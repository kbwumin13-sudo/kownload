package base

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/mattn/go-ieproxy"
	"golang.org/x/net/http/httpproxy"
)

func proxySnapshotID(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return "proxy-snapshot/v1:" + hex.EncodeToString(sum[:])
}
func directProxySnapshot() (func(*http.Request) (*url.URL, error), string, bool) {
	return nil, proxySnapshotID("direct"), true
}

// CaptureProxySnapshot never returns a closure that rereads mutable settings.
// A PAC that may actually execute remains legacy, not falsely certified by a
// stable PAC URL. Only digests are persisted by the recovery owner.
func CaptureProxySnapshot(request *RequestProxy, global *DownloaderProxyConfig) (func(*http.Request) (*url.URL, error), string, bool) {
	if request != nil {
		copy := *request
		switch copy.Mode {
		case RequestProxyModeNone:
			return directProxySnapshot()
		case RequestProxyModeCustom:
			handler := copy.ToHandler()
			if handler == nil {
				return directProxySnapshot()
			}
			return ownProxyURLs(handler), proxySnapshotID(struct {
				Kind  string
				Proxy RequestProxy
			}{"custom", copy}), true
		}
	}
	if global == nil || !global.Enable {
		return directProxySnapshot()
	}
	copy := *global
	if !copy.System {
		handler := copy.ToHandler()
		if handler == nil {
			return directProxySnapshot()
		}
		return ownProxyURLs(handler), proxySnapshotID(struct {
			Kind  string
			Proxy DownloaderProxyConfig
		}{"global", copy}), true
	}
	// Reuse existing OS configuration discovery, but freeze its interpreted
	// scalar values/environment rather than return ieproxy's dynamic handler.
	safeProxyReloadConf()
	return captureSystemProxySnapshot(ieproxy.GetConf(), *httpproxy.FromEnvironment())
}
func captureSystemProxySnapshot(conf ieproxy.ProxyConf, env httpproxy.Config) (func(*http.Request) (*url.URL, error), string, bool) {
	if env.HTTPProxy != "" || env.HTTPSProxy != "" {
		return frozenProxyConfig("environment", env)
	}
	if conf.Automatic.Active {
		return nil, "", false
	}
	if conf.Static.Active {
		// go-ieproxy represents a common Windows proxy under the empty key.
		// A present empty protocol value is an explicit direct override.
		protocol := func(scheme string) string {
			value, present := conf.Static.Protocols[scheme]
			if !present {
				value = conf.Static.Protocols[""]
			}
			return value
		}
		return frozenProxyConfig("system-static", httpproxy.Config{HTTPProxy: protocol("http"), HTTPSProxy: protocol("https"), NoProxy: conf.Static.NoProxy})
	}
	return frozenProxyConfig("environment", env)
}
func frozenProxyConfig(kind string, config httpproxy.Config) (func(*http.Request) (*url.URL, error), string, bool) {
	copy := config
	handler := copy.ProxyFunc()
	return ownProxyURLs(func(request *http.Request) (*url.URL, error) { return handler(request.URL) }), proxySnapshotID(struct {
		Kind   string
		Config httpproxy.Config
	}{kind, copy}), true
}

func ownProxyURLs(handler func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(request *http.Request) (*url.URL, error) {
		value, err := handler(request)
		if value == nil || err != nil {
			return value, err
		}
		copy := *value
		return &copy, nil
	}
}
