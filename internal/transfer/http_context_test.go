package transfer

import (
	"testing"

	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestHTTPExecutionProjectionDefaultsAndOpaqueEquivalence(t *testing.T) {
	r := &base.Request{URL: "https://source.example/a%2Fb?sig=one%2Btwo&x=1", Extra: map[string]any{"opaque": map[string]any{"n": "9007199254740993"}}}
	a, err := NewHTTPExecutionContext(r, " UA-A ", "proxy-A", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Extra = map[string]any{"method": "GET", "opaque": "different-unused"}
	b, err := NewHTTPExecutionContext(r, "UA-A", "proxy-A", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceHash() != b.SourceHash() || a.DefaultsHash() != b.DefaultsHash() {
		t.Fatal("typed/default GET or ignored Extra changed projection")
	}
	for _, change := range []struct{ ua, proxy string }{{"UA-B", "proxy-A"}, {"UA-A", "proxy-B"}} {
		c, e := NewHTTPExecutionContext(r, change.ua, change.proxy, nil)
		if e != nil || c.SourceHash() == a.SourceHash() {
			t.Fatal("inherited effective value not bound", e)
		}
	}
	r.Extra = map[string]any{"header": map[string]string{"uSeR-aGeNt": ""}, "body": "exact body"}
	r.Proxy = &base.RequestProxy{Mode: base.RequestProxyModeNone}
	x, _ := NewHTTPExecutionContext(r, "UA-A", "proxy-A", nil)
	y, _ := NewHTTPExecutionContext(r, "UA-B", "proxy-B", nil)
	if x.DefaultsHash() != y.DefaultsHash() || x.SourceHash() != y.SourceHash() {
		t.Fatal("explicit empty UA/none proxy inherited unrelated defaults")
	}
	if x.Validate(r, x.SourceHash()) != nil {
		t.Fatal("explicit projection rejected")
	}
	r.URL = "https://source.example/a/b?x=1&sig=one+two"
	if x.Validate(r, x.SourceHash()) == nil {
		t.Fatal("URL escape/query identity was rewritten or ignored")
	}
}
func TestHTTPExecutionProjectionRejectsMissingAndBadDefaults(t *testing.T) {
	r := &base.Request{URL: "https://source.example/file"}
	for _, value := range []string{"", "not-hex", "A123"} {
		if _, err := HTTPExecutionSourceHash(r, value); err == nil {
			t.Fatal("invalid defaults accepted")
		}
	}
	if _, err := NewHTTPExecutionContext(r, "UA", "", nil); err == nil {
		t.Fatal("unverifiable proxy ID accepted")
	}
}
