package download

import (
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func (i VerifiedHTTPIntegrityIntent) valid() bool {
	if i.Version != 1 || i.RequiredCapability != verifiedHTTPCapability || i.Algorithm != "sha256" || len(i.ExpectedSHA256) != 64 || strings.ToLower(i.ExpectedSHA256) != i.ExpectedSHA256 {
		return false
	}
	_, err := hex.DecodeString(i.ExpectedSHA256)
	return err == nil
}

func validateVerifiedHTTPRequest(r VerifiedHTTPRequest) error {
	if r.Version != 1 || !operationIDPattern.MatchString(r.OperationID) || !r.Integrity.valid() || r.Request == nil || r.Request.Req == nil {
		return ErrVerifiedHTTPInvalid
	}
	req := r.Request.Req
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || (req.Protocol != "" && req.Protocol != "http") {
		return ErrVerifiedHTTPInvalid
	}
	if r.Request.Opts != nil && (len(r.Request.Opts.SelectFiles) > 1 || (len(r.Request.Opts.SelectFiles) == 1 && r.Request.Opts.SelectFiles[0] != 0)) {
		return ErrVerifiedHTTPInvalid
	}
	return nil
}

// Freeze resolved execution policy only on a new R. The canonical fingerprint
// still binds the raw caller intent. Existing R never reinterprets defaults.
func (d *Downloader) freezeVerifiedOptions(input *base.Options) (*base.Options, error) {
	opts, err := cloneOperationJSON(input)
	if err != nil || opts == nil {
		return nil, ErrVerifiedHTTPInvalid
	}
	opts.Path, err = d.freezeVerifiedRoot(opts.Path)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{}
	if opts.Extra != nil {
		raw, err := json.Marshal(opts.Extra)
		if err != nil {
			return nil, ErrVerifiedHTTPInvalid
		}
		if err = decodeStoredJSON(raw, &extra); err != nil || extra == nil {
			return nil, ErrVerifiedHTTPInvalid
		}
	}
	d.configSnapshotMu.RLock()
	torrent, extract := false, false
	serverTime := false
	var timeErr error
	if cfg := d.cfg.DownloaderStoreConfig; cfg != nil {
		if cfg.AutoTorrent != nil {
			torrent = cfg.AutoTorrent.Enable
		}
		if cfg.Archive != nil {
			extract = cfg.Archive.AutoExtract
		}
		if value := cfg.ProtocolConfig["http"]; value != nil {
			raw, err := json.Marshal(value)
			var protocol struct {
				UseServerCtime bool `json:"useServerCtime"`
			}
			if err != nil {
				timeErr = err
			} else if err = json.Unmarshal(raw, &protocol); err != nil {
				timeErr = err
			} else {
				serverTime = protocol.UseServerCtime
			}
		}

	}
	d.configSnapshotMu.RUnlock()
	if timeErr != nil {
		return nil, ErrVerifiedHTTPUnsupportedOption
	}
	extra["verifiedUseServerCtime"] = serverTime
	for _, item := range []struct {
		key       string
		inherited bool
	}{{"autoTorrent", torrent}, {"autoExtract", extract}} {
		enabled := item.inherited
		if value, exists := extra[item.key]; exists && value != nil {
			b, ok := value.(bool)
			if !ok {
				return nil, ErrVerifiedHTTPUnsupportedOption
			}
			enabled = b
		}
		if enabled {
			return nil, ErrVerifiedHTTPUnsupportedOption
		}
		extra[item.key] = false
	}
	opts.Extra = extra
	return opts, nil
}

// Capture only interpreted configuration; no Resolve, Restore, wire request,
// protocol Store or output file operation takes place at this pre-R check.
// Actual execution captures again at its safe run boundary, and holds if the
// context has since become unsupported. Existing R bypasses this new-R check.
func (d *Downloader) checkVerifiedHTTPContext(req *base.Request) error {
	f, err := d.buildFetcher(req)
	if err != nil {
		return ErrVerifiedHTTPUnsupportedOption
	}
	capture, ok := f.(interface {
		CaptureRecoveryContext(*base.Request) (*transfer.HTTPExecutionContext, bool, error)
	})
	if !ok {
		return ErrVerifiedHTTPUnsupportedOption
	}
	snapshot, supported, err := capture.CaptureRecoveryContext(req)
	if err != nil || !supported || snapshot == nil {
		return ErrVerifiedHTTPUnsupportedOption
	}
	return nil
}
