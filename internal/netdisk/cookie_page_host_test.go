package netdisk

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

func TestCaptureCookiesForVerifiedPageHost(t *testing.T) {
	for _, id := range []string{"aliyun", "123pan", "guangya"} {
		t.Run(id, func(t *testing.T) {
			info := adapterProvider(t, id).Info()
			parent := info.Hosts[0]
			page := "https://www." + parent + "/s/fixture"
			for _, tc := range []struct {
				name, domain string
				accepted     bool
			}{
				{"page_host_only", "www." + parent, true},
				{"page_domain", ".www." + parent, true},
				{"parent_domain", "." + parent, true},
				{"api_host", info.APIHosts[0], true},
				{"sibling", "other." + parent, false},
				{"child", "child.www." + parent, false},
				{"unrelated", "foreign.example", false},
				{"suffix_attack", "www." + parent + ".foreign.example", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s := NewService()
					r, err := s.RequestBrowser(model.Source{URL: page}, "fixture-browser")
					if err != nil {
						t.Fatal(err)
					}
					claim, err := s.Claim(page, "fixture-browser", "Chrome", "fixture-tab", r.ID)
					if err != nil || claim == nil {
						t.Fatalf("claim: %v, %v", claim, err)
					}
					_, err = s.Capture(r.ID, claim.Nonce, model.Capture{
						Provider: id, SourcePage: page, InstanceID: "fixture-browser", ContextID: "fixture-tab",
						Cookies: []model.Cookie{{Name: "session", Value: "fixture", Domain: tc.domain, Path: "/", Secure: true}},
					})
					if (err == nil) != tc.accepted {
						t.Fatalf("accepted = %v, want %v: %v", err == nil, tc.accepted, err)
					}
					want := "failed"
					if tc.accepted {
						want = "ready"
					}
					status, err := s.BrowserStatus(r.ID)
					if err != nil || status.State != want {
						t.Fatalf("status = %+v, %v; want %s", status, err, want)
					}
				})
			}
		})
	}
}

func TestAPIClientPageHostCookieIsolation(t *testing.T) {
	for _, id := range []string{"aliyun", "123pan", "guangya"} {
		t.Run(id, func(t *testing.T) {
			info := adapterProvider(t, id).Info()
			parent := info.Hosts[0]
			host := "www." + parent
			page := "https://" + host + "/drive/list"
			for _, source := range []string{"request", "capture", "both"} {
				t.Run(source, func(t *testing.T) {
					req := &base.Request{}
					capture := model.Capture{Provider: id, Cookies: []model.Cookie{
						{Name: "www", Value: "fixture", Domain: host, Path: "/", Secure: true},
						{Name: "parent", Value: "fixture", Domain: "." + parent, Path: "/", Secure: true},
						{Name: "path", Value: "fixture", Domain: host, Path: "/drive", Secure: true},
						{Name: "expired", Value: "fixture", Domain: host, Path: "/", Expires: float64(time.Now().Add(-time.Hour).Unix())},
						{Name: "future", Value: "fixture", Domain: host, Path: "/", Expires: float64(time.Now().Add(time.Hour).Unix())},
					}}
					if source != "capture" {
						req.URL = page
					}
					if source != "request" {
						capture.SourcePage = page
					}
					c := NewAPIClient(nil, req, info, &Session{Capture: capture})
					originalHosts := append(append([]string{}, info.Hosts...), info.APIHosts...)
					if !reflect.DeepEqual(c.Hosts, originalHosts) {
						t.Fatalf("API allowlist changed: %v", c.Hosts)
					}
					var got map[string]bool
					calls := 0
					c.Client.Transport = adapterTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						got = map[string]bool{}
						for _, cookie := range r.Cookies() {
							if got[cookie.Name] {
								t.Fatalf("duplicate cookie %s", cookie.Name)
							}
							got[cookie.Name] = true
						}
						return adapterJSON(`{}`), nil
					})
					type cookieCase struct {
						url  string
						want map[string]bool
					}
					cases := []cookieCase{
						{page, map[string]bool{"www": true, "parent": true, "path": true, "future": true}},
						{"https://" + host + "/drive-other", map[string]bool{"www": true, "parent": true, "future": true}},
						{"http://" + host + "/drive/list", map[string]bool{"future": true}},
						{"https://" + parent + "/drive/list", map[string]bool{"parent": true}},
						{"https://other." + parent + "/drive/list", map[string]bool{"parent": true}},
						{"https://child." + host + "/drive/list", map[string]bool{"parent": true}},
					}
					for _, api := range info.APIHosts {
						want := map[string]bool{}
						if api == "api.alipan.com" || api == "yun.123pan.com" || api == "api.guangyapan.com" {
							want["parent"] = true
						}
						cases = append(cases, cookieCase{"https://" + api + "/drive/list", want})
					}
					for _, tc := range cases {
						res, err := c.Do(context.Background(), "GET", tc.url, nil, map[string]string{"Cookie": "raw=must-not-forward"})
						if err != nil {
							t.Fatal(err)
						}
						res.Body.Close()
						if !reflect.DeepEqual(got, tc.want) {
							t.Fatalf("%s: cookies = %v, want %v", tc.url, got, tc.want)
						}
					}
					before := calls
					if _, err := c.Do(context.Background(), "GET", "https://foreign.example/", nil, map[string]string{"Cookie": "raw=secret"}); err == nil || calls != before {
						t.Fatal("foreign request reached transport")
					}
				})
			}
		})
	}
}

func TestAPIClientPageSeedDoesNotAuthorizeForeignHost(t *testing.T) {
	info := adapterProvider(t, "aliyun").Info()
	for _, raw := range []string{
		"https://foreign.example/", "https://www.alipan.com.foreign.example/",
		"https://user:pass@www.alipan.com/", "file://www.alipan.com/", ":invalid",
	} {
		t.Run(raw, func(t *testing.T) {
			capture := model.Capture{Provider: info.ID, SourcePage: raw, Cookies: []model.Cookie{
				{Name: "foreign", Domain: "foreign.example", Value: "fixture", Path: "/"},
				{Name: "page", Domain: "www.alipan.com", Value: "fixture", Path: "/"},
			}}
			c := NewAPIClient(nil, &base.Request{URL: raw}, info, &Session{Capture: capture})
			for _, host := range []string{"foreign.example", "www.alipan.com"} {
				u := &url.URL{Scheme: "https", Host: host, Path: "/"}
				if cookies := c.Client.Jar.Cookies(u); len(cookies) != 0 {
					t.Fatalf("invalid page seeded cookies for %s", host)
				}
			}
			foreign, _ := url.Parse("https://foreign.example/")
			if c.Allowed(foreign) {
				t.Fatal("foreign page expanded API allowlist")
			}
		})
	}
}
