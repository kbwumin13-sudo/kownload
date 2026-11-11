package nativehost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/appidentity"
)

type identityFixture struct {
	fixture
	probes int
	onWake func()
}

func (f *identityFixture) Available(ctx context.Context) bool {
	f.probes++
	return f.fixture.Available(ctx)
}

func (f *identityFixture) Ensure(ctx context.Context, silent bool) error {
	if f.onWake != nil {
		f.onWake()
	}
	return f.fixture.Ensure(ctx, silent)
}

func testIdentity(t *testing.T) *HostIdentity {
	t.Helper()
	return &HostIdentity{Socket: filepath.Join(t.TempDir(), "ipc", "host.sock"),
		CallerOrigin: "chrome-extension://" + appidentity.ChromiumExtensionIds[0] + "/",
		Build:        HostBuild{SourceCommit: strings.Repeat("a", 40), ReleaseVersion: "1.9.3-kownload.26"}}
}

func boundRequest(t *testing.T, identity *HostIdentity, method string) Request {
	t.Helper()
	scope, err := identity.routeScope()
	if err != nil {
		t.Fatal(err)
	}
	version := IdentityProtocol
	params := json.RawMessage(`{"method":"POST","path":"/api/v1/tasks","data":{}}`)
	if method == "browser" {
		params = json.RawMessage(`{"action":"handoffPrepare","clientId":"profile-A","browser":"edge"}`)
	}
	return Request{Method: method, OperationID: "op-route-1", Params: params, IdentityProtocol: &version,
		BridgeBinding: &BridgeBinding{Version: 1, HostName: appidentity.HostName, RouteScope: scope}}
}

func dispatchRequest(t *testing.T, d Dispatcher, req Request) Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return d.Dispatch(context.Background(), body)
}

func TestHostInfoWithoutIdentityDoesNotProbeOrWake(t *testing.T) {
	f := &identityFixture{fixture: fixture{available: true}}
	r := (Dispatcher{Client: f, Wakeup: f}).Dispatch(context.Background(), []byte(`{"method":"hostInfo","params":{"version":1,"reportedBrowser":"edge","clientId":"profile-A"}}`))
	if r.Error != "identity_unavailable" || f.calls != 0 || f.wakes != 0 || f.probes != 0 {
		payload, _ := json.Marshal(r)
		t.Fatalf("missing identity must fail without side effects: %s calls=%d wakes=%d", payload, f.calls, f.wakes)
	}
}

func TestHostInfoIsReadOnlyAndDoesNotClaimAppOrCoreIdentity(t *testing.T) {
	identity := testIdentity(t)
	f := &identityFixture{fixture: fixture{available: true}}
	r := (Dispatcher{Client: f, Wakeup: f, Identity: identity}).Dispatch(context.Background(), []byte(`{"method":"hostInfo","params":{"version":1,"reportedBrowser":"edge","clientId":"profile-A","callerOrigin":"spoofed","routeScope":"spoofed"}}`))
	info, ok := r.Data.(HostInfo)
	if r.Code != 0 || !ok || info.CallerOrigin != identity.CallerOrigin || info.RouteScope == "" ||
		info.Build != identity.Build || !info.App.Reachable || info.App.Identity != nil || info.Core.Reachable != nil || info.Core.HandoffVersion != nil ||
		f.probes != 1 || f.wakes != 0 || f.calls != 0 {
		t.Fatalf("incorrect read-only handshake: %+v counters=%+v", r, f)
	}
	payload, _ := json.Marshal(r)
	if strings.Contains(string(payload), filepath.Dir(identity.Socket)) || strings.Contains(string(payload), "spoofed") {
		t.Fatal("handshake leaked a path or trusted request identity")
	}
	identity.Build.SourceCommit = "/Users/private/source"
	identity.Build.ReleaseVersion = "https://private.invalid/?token=secret"
	r = (Dispatcher{Client: f, Wakeup: f, Identity: identity}).Dispatch(context.Background(), []byte(`{"method":"hostInfo","params":{"version":1,"reportedBrowser":"chrome","clientId":"profile-A"}}`))
	if info = r.Data.(HostInfo); info.Build.SourceCommit != "unknown" || info.Build.ReleaseVersion != "unknown" {
		t.Fatal("untrusted build metadata must remain unknown")
	}
}

func TestHostInfoRejectsUnsupportedOrInvalidRequestsWithoutProbe(t *testing.T) {
	for _, params := range []string{`null`, `{}`, `{"version":2,"reportedBrowser":"edge","clientId":"a"}`, `{"version":1,"reportedBrowser":"other","clientId":"a"}`, `{"version":1,"reportedBrowser":"edge","clientId":""}`} {
		f := &identityFixture{}
		r := (Dispatcher{Client: f, Wakeup: f, Identity: testIdentity(t)}).Dispatch(context.Background(), []byte(`{"method":"hostInfo","params":`+params+`}`))
		if r.Error != "invalid_request" || f.probes+f.wakes+f.calls != 0 {
			t.Fatalf("invalid hostInfo had effects: %+v", r)
		}
	}
}

func TestBindingMustMatchActualWritingHostNotPriorProbe(t *testing.T) {
	probed := testIdentity(t)
	otherHost := testIdentity(t)
	for _, method := range []string{"create", "forward", "browser", "wakeup"} {
		t.Run(method, func(t *testing.T) {
			f := &identityFixture{fixture: fixture{available: true, status: 200, body: `{"code":0,"operationId":"op-route-1","data":"task-1","submission":{"state":"created","taskIds":["task-1"]}}`}}
			req := boundRequest(t, probed, method)
			r := dispatchRequest(t, Dispatcher{Client: f, Wakeup: f, Identity: otherHost}, req)
			if r.Error != "route_mismatch" || f.probes+f.calls+f.wakes != 0 {
				t.Fatalf("another native process used stale handshake: %+v counters=%+v", r, f)
			}
			// Reconnect with the SAME operation and route. The guard forwards
			// once; it does not select another host, retry or recreate the op.
			r = dispatchRequest(t, Dispatcher{Client: f, Wakeup: f, Identity: probed}, req)
			if r.Error != "" || (method != "wakeup" && (f.calls != 1 || f.last.OperationID != req.OperationID || f.last.BridgeBinding.RouteScope != req.BridgeBinding.RouteScope)) || (method == "wakeup" && f.wakes != 1) {
				t.Fatalf("same binding did not preserve original request: %+v", r)
			}
		})
	}
}

func TestBindingMissingInvalidOrUnsupportedFailsBeforeSideEffects(t *testing.T) {
	identity := testIdentity(t)
	for _, change := range []func(*Request){
		func(r *Request) { r.BridgeBinding = nil },
		func(r *Request) { r.IdentityProtocol = nil },
		func(r *Request) { *r.IdentityProtocol = 2 },
		func(r *Request) { r.BridgeBinding.Version = 2 },
		func(r *Request) { r.BridgeBinding.HostName = "foreign.native" },
		func(r *Request) { r.BridgeBinding.RouteScope = strings.Repeat("z", 64) },
	} {
		req := boundRequest(t, identity, "create")
		change(&req)
		f := &identityFixture{}
		r := dispatchRequest(t, Dispatcher{Client: f, Wakeup: f, Identity: identity}, req)
		if r.Code == 0 || f.calls+f.wakes+f.probes != 0 {
			t.Fatalf("invalid binding executed: %+v", r)
		}
	}
}

func TestRouteCheckedAgainAfterWakeBeforeForward(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "active")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	identity := testIdentity(t)
	identity.Socket = filepath.Join(link, "ipc", "host.sock")
	req := boundRequest(t, identity, "create")
	f := &identityFixture{onWake: func() {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(b, link); err != nil {
			t.Fatal(err)
		}
	}}
	r := dispatchRequest(t, Dispatcher{Client: f, Wakeup: f, Identity: identity}, req)
	if r.Error != "route_mismatch" || f.wakes != 1 || f.calls != 0 {
		t.Fatalf("route changed during wake but POST executed: %+v", r)
	}
}

func TestColdSocketCreationKeepsScopeAndInvalidOriginCannotBind(t *testing.T) {
	identity := testIdentity(t)
	before, err := identity.routeScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(identity.Socket), 0700); err != nil {
		t.Fatal(err)
	}
	after, err := identity.routeScope()
	if err != nil || before != after {
		t.Fatal("creating ipc directory changed route")
	}
	identity.CallerOrigin = "chrome-extension://not-allowed/"
	if _, err := identity.routeScope(); err == nil {
		t.Fatal("invalid actual origin accepted")
	}
}
