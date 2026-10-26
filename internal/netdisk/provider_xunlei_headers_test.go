package netdisk

import (
	"context"
	"net/http"
	"testing"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

func TestXunleiHeadersPreferCapturedClientIdentity(t *testing.T) {
	p := adapterProvider(t, "xunlei").(*xunleiProvider)
	for _, tc := range []struct {
		name    string
		tokens  map[string]string
		headers map[string]string
		want    string
	}{
		{name: "default", want: "Xqp0kJBXWhwaTpB6"},
		{name: "credentials client_id", tokens: map[string]string{"client_id": "captured-web-client"}, want: "captured-web-client"},
		{name: "captured header", headers: map[string]string{"X-Client-Id": "captured-header-client"}, want: "captured-header-client"},
		{name: "header case", headers: map[string]string{"x-client-id": "lowercase-header-client"}, want: "lowercase-header-client"},
		{name: "header takes precedence", tokens: map[string]string{"client_id": "storage-client"}, headers: map[string]string{"X-Client-Id": "request-client"}, want: "request-client"},
		{name: "empty header uses token", tokens: map[string]string{"client_id": "storage-client"}, headers: map[string]string{"X-Client-Id": ""}, want: "storage-client"},
		{name: "empty capture uses default", tokens: map[string]string{"client_id": ""}, headers: map[string]string{"X-Client-Id": ""}, want: "Xqp0kJBXWhwaTpB6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := adapterSession("xunlei")
			for k, v := range tc.tokens {
				s.Capture.Tokens[k] = v
			}
			s.Capture.Headers = tc.headers
			h := p.headers(s, model.Source{URL: "https://pan.xunlei.com/"})
			if h["X-Client-Id"] != tc.want {
				t.Fatalf("want captured client %q, got %q", tc.want, h["X-Client-Id"])
			}
			if h["Authorization"] != "Bearer token" || h["X-Captcha-Token"] != "captcha" || h["X-Device-Id"] != "device" {
				t.Fatal("captured authentication headers changed")
			}
			if h["X-Client-Version"] != "1.92.89" || h["Origin"] != "https://pan.xunlei.com" {
				t.Fatal("existing web headers changed")
			}
		})
	}
	if got := p.headers(nil, model.Source{})["X-Client-Id"]; got != "Xqp0kJBXWhwaTpB6" {
		t.Fatal("nil capture lost default client")
	}
}

func TestXunleiListIdentityAndCookieDomainBoundary(t *testing.T) {
	p := adapterProvider(t, "xunlei")
	s := adapterSession("xunlei")
	s.Capture.Tokens["client_id"] = "captured-web-client"
	s.Capture.Headers = map[string]string{"Cookie": "raw-cookie=never-send"}
	s.Capture.Cookies = []model.Cookie{
		{Name: "root", Value: "root-cookie", Domain: ".xunlei.com", Path: "/", Secure: true},
		{Name: "api", Value: "api-cookie", Domain: "api-pan.xunlei.com", Path: "/", Secure: true},
		{Name: "web", Value: "never-send", Domain: "pan.xunlei.com", Path: "/", Secure: true},
		{Name: "foreign", Value: "never-send", Domain: "xunlei.com.attacker.example", Path: "/", Secure: true},
	}
	calls := 0
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api-pan.xunlei.com" || r.URL.Path != "/drive/v1/files" || r.Header.Get("X-Client-Id") != "captured-web-client" {
			t.Fatal("list lost captured web identity or API endpoint")
		}
		cookies := map[string]string{}
		for _, cookie := range r.Cookies() {
			cookies[cookie.Name] = cookie.Value
		}
		if len(cookies) != 2 || cookies["root"] != "root-cookie" || cookies["api"] != "api-cookie" {
			t.Fatal("cookies escaped their domain boundary")
		}
		return adapterJSON(`{"files":[]}`), nil
	})
	listing, err := p.List(context.Background(), c, s, model.Source{URL: "https://pan.xunlei.com/"}, "")
	if err != nil || listing == nil || calls != 1 {
		t.Fatal("list failed", err)
	}
	for _, raw := range []string{"https://api-pan.xunlei.com.attacker.example/files", "https://user@api-pan.xunlei.com/files", "file:///tmp/files"} {
		if _, err := c.Do(context.Background(), "GET", raw, nil, p.(*xunleiProvider).headers(s, model.Source{})); err == nil {
			t.Fatal("untrusted endpoint accepted")
		}
	}
	if calls != 1 {
		t.Fatal("untrusted host reached transport")
	}
}

func TestXunleiDeviceIDFromHelperStorage(t *testing.T) {
	p := adapterProvider(t, "xunlei").(*xunleiProvider)
	s := &Session{Capture: model.Capture{Tokens: map[string]string{"deviceid": "helper-device"}}}
	if p.headers(s, model.Source{})["X-Device-Id"] != "helper-device" {
		t.Fatal("helper device identity lost")
	}
}
