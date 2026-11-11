package transfer

import (
	"net/http"
	"testing"
)

func TestCredentialOriginsAndTargetScope(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://EXAMPLE.test/a", "https://example.test:443/b", true},
		{"http://example.test", "http://example.test:80/a", true},
		{"https://例子.测试/a", "https://xn--fsqu00a.xn--0zwm56d/b", true},
		{"http://[0:0:0:0:0:0:0:1]/a", "http://[::1]:80/b", true},
		{"https://a.example.test", "https://b.example.test", false},
		{"https://example.test", "https://example.test:444", false},
		{"https://example.test", "http://example.test", false},
		{"http://example.test", "http://example.test.", false},
	} {
		a, e := CredentialOrigin(tc.a)
		if e != nil {
			t.Fatal(e)
		}
		b, e := CredentialOrigin(tc.b)
		if e != nil {
			t.Fatal(e)
		}
		if (a == b) != tc.same {
			t.Fatalf("origin equality %q/%q", a, b)
		}
	}
	for _, raw := range []string{"file:///a", "https:///a", "http://host:0", "https://host:65536", "http://host:bad"} {
		if _, e := CredentialOrigin(raw); e == nil {
			t.Fatal("invalid origin accepted")
		}
	}
	headers := map[string]string{"authorization": "Bearer B", "X-Device": "B", "Host": "explicit-target.test", "Proxy-Authorization": "target-must-not-control-proxy"}
	c, e := NewTargetCredentials("https://b.test/file", headers, nil)
	if e != nil {
		t.Fatal(e)
	}
	headers["X-Device"] = "mutated"
	req, _ := http.NewRequest("GET", "https://b.test:443/file", nil)
	if e = c.Apply(req); e != nil {
		t.Fatal(e)
	}
	if req.Header.Get("X-Device") != "B" || req.Header.Get("Proxy-Authorization") != "" || req.Host != "explicit-target.test" {
		t.Fatal("context not detached/proxy separated")
	}
	other, _ := http.NewRequest("GET", "https://sub.b.test/file", nil)
	if e = c.Apply(other); e != ErrScopedContext || len(other.Header) != 0 {
		t.Fatal("target context crossed origin")
	}
}
