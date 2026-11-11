package nativehost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/appidentity"
)

const IdentityProtocol = 1

// BridgeBinding identifies the chosen IPC route, not a publisher or a secret.
// The caller must persist it before sending the first modifying request.
type BridgeBinding struct {
	Version    int    `json:"version"`
	HostName   string `json:"hostName"`
	RouteScope string `json:"routeScope"`
}

type HostBuild struct {
	SourceCommit   string `json:"sourceCommit"`
	ReleaseVersion string `json:"releaseVersion"`
	Preview        bool   `json:"preview"`
}

// HostIdentity is initialized from the actual executable route and browser
// argv. None of these values are taken from the request body.
type HostIdentity struct {
	Socket       string
	CallerOrigin string
	Build        HostBuild
}

type HostInfo struct {
	Version            int       `json:"version"`
	Product            string    `json:"product"`
	NativeProtocol     int       `json:"nativeProtocol"`
	IdentityProtocol   int       `json:"identityProtocol"`
	Build              HostBuild `json:"build"`
	SupportedHostNames []string  `json:"supportedHostNames"`
	CallerOrigin       string    `json:"callerOrigin"`
	RouteScope         string    `json:"routeScope"`
	App                struct {
		Reachable bool `json:"reachable"`
		Identity  any  `json:"identity"`
	} `json:"app"`
	Core struct {
		Reachable      *bool `json:"reachable"`
		HandoffVersion *int  `json:"handoffVersion"`
	} `json:"core"`
}

var sourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var releaseVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$`)

// routeScope resolves existing ancestors too, since a cold App may not yet
// have created ipc/host.sock. Re-evaluate it before each actual IPC operation;
// do not trust a previous short-lived hostInfo process or a cached JS probe.
func (h *HostIdentity) routeScope() (string, error) {
	if h == nil || !AllowedOrigin(h.CallerOrigin) || !filepath.IsAbs(h.Socket) {
		return "", errors.New("identity_unavailable")
	}
	path := filepath.Clean(h.Socket)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			sum := sha256.Sum256([]byte(resolved))
			return hex.EncodeToString(sum[:]), nil
		}
		if !os.IsNotExist(err) || path == filepath.Dir(path) {
			return "", errors.New("identity_unavailable")
		}
		suffix = append(suffix, filepath.Base(path))
		path = filepath.Dir(path)
	}
}

func (d Dispatcher) hostInfo(ctx context.Context, req Request) Response {
	var params struct {
		Version         int    `json:"version"`
		ReportedBrowser string `json:"reportedBrowser"`
		ClientID        string `json:"clientId"`
	}
	if json.Unmarshal(req.Params, &params) != nil || params.Version != IdentityProtocol ||
		(params.ReportedBrowser != "chrome" && params.ReportedBrowser != "edge") || !operationID.MatchString(params.ClientID) {
		return failure(req.OperationID, "invalid_request", nil, nil)
	}
	scope, err := d.Identity.routeScope()
	if err != nil {
		return failure(req.OperationID, "identity_unavailable", nil, nil)
	}
	build := d.Identity.Build
	if !sourceCommitPattern.MatchString(build.SourceCommit) {
		build.SourceCommit = "unknown"
	}
	if !releaseVersionPattern.MatchString(build.ReleaseVersion) {
		build.ReleaseVersion = "unknown"
	}
	info := HostInfo{Version: IdentityProtocol, Product: appidentity.ProductName, NativeProtocol: 1,
		IdentityProtocol: IdentityProtocol, Build: build, CallerOrigin: d.Identity.CallerOrigin,
		SupportedHostNames: []string{appidentity.HostName, appidentity.LegacyHostName}, RouteScope: scope}
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	info.App.Reachable = d.Client.Available(probe)
	// Reachable is only the socket probe. App identity and Core capability are
	// untested here, rather than inferred from host build metadata or a ping.
	return Response{Data: info, OperationID: req.OperationID, Message: ""}
}

func (d Dispatcher) validateBinding(req Request) string {
	if req.IdentityProtocol == nil && req.BridgeBinding == nil {
		return "" // Existing official/legacy clients retain their wire contract.
	}
	if req.IdentityProtocol == nil || *req.IdentityProtocol != IdentityProtocol {
		return "identity_unsupported"
	}
	if req.Method == "hostInfo" || req.Method == "ping" {
		return ""
	}
	b := req.BridgeBinding
	if b == nil || b.Version != IdentityProtocol ||
		(b.HostName != appidentity.HostName && b.HostName != appidentity.LegacyHostName) ||
		len(b.RouteScope) != sha256.Size*2 || strings.ToLower(b.RouteScope) != b.RouteScope {
		return "binding_required"
	}
	if _, err := hex.DecodeString(b.RouteScope); err != nil {
		return "binding_required"
	}
	scope, err := d.Identity.routeScope()
	if err != nil {
		return "identity_unavailable"
	}
	if scope != b.RouteScope {
		return "route_mismatch"
	}
	return ""
}
