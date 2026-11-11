package base

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/mattn/go-ieproxy"
	"golang.org/x/net/http/httpproxy"
)

func snapshotProxyURL(t *testing.T, handler func(*http.Request) (*url.URL, error), target string) string {
	t.Helper()
	if handler == nil {
		return "direct"
	}
	req, _ := http.NewRequest("GET", target, nil)
	value, err := handler(req)
	if err != nil {
		t.Fatal(err)
	}
	if value == nil {
		return "direct"
	}
	return value.String()
}
func TestProxySnapshotManualIsStableAndOwnsValues(t *testing.T) {
	global := &DownloaderProxyConfig{Enable: true, Scheme: "http", Host: "proxy-a.invalid:8080", Usr: "synthetic-A", Pwd: "synthetic-A"}
	first, id, ok := CaptureProxySnapshot(nil, global)
	if !ok || id == "" {
		t.Fatal("manual unsupported")
	}
	_, same, ok := CaptureProxySnapshot(nil, &DownloaderProxyConfig{Enable: true, Scheme: "http", Host: "proxy-a.invalid:8080", Usr: "synthetic-A", Pwd: "synthetic-A"})
	if !ok || same != id {
		t.Fatal("ID is not deterministic across reconstructed config")
	}
	req, _ := http.NewRequest("GET", "https://target.example/file", nil)
	exposed, _ := first(req)
	exposed.Host = "mutated.invalid"
	global.Host = "proxy-b.invalid:9090"
	_, different, _ := CaptureProxySnapshot(nil, global)
	if different == id {
		t.Fatal("effective manual config change did not change ID")
	}
	if snapshotProxyURL(t, first, "https://target.example/file") != "http://synthetic-A:synthetic-A@proxy-a.invalid:8080" {
		t.Fatal("capture reread mutated global config")
	}
	_, noneA, _ := CaptureProxySnapshot(&RequestProxy{Mode: RequestProxyModeNone}, global)
	global.Host = "proxy-c.invalid:9999"
	_, noneB, _ := CaptureProxySnapshot(&RequestProxy{Mode: RequestProxyModeNone}, global)
	if noneA != noneB {
		t.Fatal("explicit none followed global config")
	}
}
func TestProxySnapshotSystemStaticEnvironmentAndPACEligibility(t *testing.T) {
	conf := ieproxy.ProxyConf{Static: ieproxy.StaticProxyConf{Active: true, Protocols: map[string]string{"https": "http://static-a.invalid:8080"}, NoProxy: "bypass.example"}}
	static, id, ok := captureSystemProxySnapshot(conf, httpproxy.Config{})
	if !ok || id == "" {
		t.Fatal("static unsupported")
	}
	conf.Static.Protocols["https"] = "http://static-b.invalid:8080"
	target, _ := http.NewRequest("GET", "https://target.example/file", nil)
	exposed, err := static(target)
	if err != nil || exposed == nil {
		t.Fatal("missing static proxy", err)
	}
	exposed.Host = "mutated.invalid"
	if snapshotProxyURL(t, static, "https://target.example/file") != "http://static-a.invalid:8080" || snapshotProxyURL(t, static, "https://bypass.example/file") != "direct" {
		t.Fatal("static snapshot changed with source map")
	}
	conf.Automatic.Active = true
	if _, _, ok = captureSystemProxySnapshot(conf, httpproxy.Config{}); ok {
		t.Fatal("PAC was certified by static fallback")
	}
	env := httpproxy.Config{HTTPSProxy: "http://env-a.invalid:7070", NoProxy: "bypass.example"}
	handler, envID, ok := captureSystemProxySnapshot(conf, env)
	if !ok || envID == id {
		t.Fatal("environment precedence snapshot missing")
	}
	env.HTTPSProxy = "http://env-b.invalid:7070"
	if snapshotProxyURL(t, handler, "https://target.example/file") != "http://env-a.invalid:7070" {
		t.Fatal("environment snapshot reread mutable values")
	}
	_, next, _ := captureSystemProxySnapshot(conf, env)
	if next == envID {
		t.Fatal("effective environment change not reflected")
	}
}
