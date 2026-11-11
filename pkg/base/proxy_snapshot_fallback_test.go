package base

import (
	"github.com/mattn/go-ieproxy"
	"golang.org/x/net/http/httpproxy"
	"testing"
)

func TestProxySnapshotCommonSystemFallback(t *testing.T) {
	conf := ieproxy.ProxyConf{Static: ieproxy.StaticProxyConf{Active: true, Protocols: map[string]string{"": "http://common-a.invalid:8080"}, NoProxy: "bypass.example"}}
	first, id, ok := captureSystemProxySnapshot(conf, httpproxy.Config{})
	if !ok {
		t.Fatal("common static proxy unsupported")
	}
	for _, target := range []string{"http://target.example/file", "https://target.example/file"} {
		if got := snapshotProxyURL(t, first, target); got != "http://common-a.invalid:8080" {
			t.Fatalf("common proxy lost for %s: %s", target, got)
		}
	}
	if got := snapshotProxyURL(t, first, "https://bypass.example/file"); got != "direct" {
		t.Fatal("bypass lost", got)
	}
	conf.Static.Protocols[""] = "http://common-b.invalid:9090"
	_, changed, _ := captureSystemProxySnapshot(conf, httpproxy.Config{})
	if changed == id {
		t.Fatal("common proxy change did not change identity")
	}
	if got := snapshotProxyURL(t, first, "https://target.example/file"); got != "http://common-a.invalid:8080" {
		t.Fatal("snapshot reread mutable map", got)
	}
	conf.Static.Protocols["https"] = "http://secure-only.invalid:7070"
	conf.Static.Protocols["http"] = "" // Explicitly present empty value disables fallback.
	explicit, explicitID, ok := captureSystemProxySnapshot(conf, httpproxy.Config{})
	if !ok {
		t.Fatal("protocol override unsupported")
	}
	if got := snapshotProxyURL(t, explicit, "http://target.example/file"); got != "direct" {
		t.Fatal("explicit direct overridden", got)
	}
	if got := snapshotProxyURL(t, explicit, "https://target.example/file"); got != "http://secure-only.invalid:7070" {
		t.Fatal("protocol override ignored", got)
	}
	conf.Static.Protocols[""] = "http://unused-common.invalid:6060"
	_, unused, _ := captureSystemProxySnapshot(conf, httpproxy.Config{})
	if unused != explicitID {
		t.Fatal("unused common setting changed effective snapshot")
	}
}
