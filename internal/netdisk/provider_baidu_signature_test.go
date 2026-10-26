package netdisk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

func baiduSignatureSession() *Session {
	s := adapterSession("baidu")
	s.Capture.Tokens = nil
	s.Capture.Cookies = []model.Cookie{{Name: "BDUSS", Value: "mock-private-cookie", Domain: ".baidu.com", Path: "/", Secure: true}}
	s.Capture.Share = map[string]string{"sign": "stale-share-sign", "sign1": "stale-sign1", "sign3": "stale-sign3", "timestamp": "1", "bdstoken": "mock-private-csrf"}
	return s
}

func TestBaiduSignatureCurrentAPIResolveIdentityAndSecrets(t *testing.T) {
	for _, timestamp := range []string{`1700000000`, `"1700000000"`} {
		t.Run(timestamp, func(t *testing.T) {
			p := adapterProvider(t, "baidu")
			s := baiduSignatureSession()
			before, _ := json.Marshal(s)
			// A retained share URL must not change personal-file identity.
			src := model.Source{Provider: "baidu", URL: "https://pan.baidu.com/s/1history?pwd=mock-private-password"}
			f := model.File{ID: "9007199254740993", Name: "f", Path: "nested", Size: 15360, Meta: map[string]string{"sign": "mock-private-sign", "remote_path": "/nested/f"}}
			paths := []string{}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				if r.URL.Host != "pan.baidu.com" || r.URL.Scheme != "https" {
					t.Fatal("credentials sent outside personal API host")
				}
				cookie, err := r.Cookie("BDUSS")
				if err != nil || cookie.Value != "mock-private-cookie" {
					t.Fatal("missing cookiejar authentication")
				}
				if r.Header.Get("Authorization") != "" || strings.Contains(r.URL.RawQuery, "mock-private") || strings.Contains(r.Header.Get("Referer"), "pwd=") {
					t.Fatal("sensitive data copied into request URL/headers")
				}
				switch r.URL.Path {
				case "/disk/home":
					res := adapterJSON("")
					res.StatusCode = http.StatusFound
					res.Header.Set("Location", "/disk/main")
					res.Header.Add("Set-Cookie", "PANPSC=mock-home-cookie; Path=/; Secure")
					return res, nil
				case "/disk/main":
					res := adapterJSON(`<html><a href="https://passport.baidu.com/">账号</a><script>{"sign1":"partial-html-sign"}</script></html>`)
					res.Header.Set("Content-Type", "text/html")
					return res, nil
				case "/api/gettemplatevariable":
					var fields []string
					if err := json.Unmarshal([]byte(r.URL.Query().Get("fields")), &fields); err != nil || !reflect.DeepEqual(fields, []string{"sign1", "sign3", "timestamp"}) {
						t.Fatal("incorrect template fields")
					}
					q := r.URL.Query()
					if r.Method != "GET" || q.Get("web") != "1" || q.Get("app_id") != "250528" || q.Get("clienttype") != "0" || q.Get("channel") != "chunlei" || len(q) != 5 {
						t.Fatal("incorrect personal signature request")
					}
					if r.Header.Get("Referer") != "https://pan.baidu.com/disk/home" || !strings.Contains(r.Header.Get("User-Agent"), "Mozilla/") {
						t.Fatal("missing personal browser context")
					}
					if _, err := r.Cookie("PANPSC"); err != nil {
						t.Fatal("home cookie not preserved")
					}
					return adapterJSON(`{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":` + timestamp + `,"sign2":"throw new Error('must never execute')"}}`), nil
				case "/api/download":
					if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("sign") != "u/MW6NlArwrT" || r.Form.Get("timestamp") != "1700000000" || r.Form.Get("fidlist") != "[9007199254740993]" {
						t.Fatal("incorrect personal file signature/identity")
					}
					return adapterJSON(`{"errno":0,"dlink":[{"fs_id":"other","dlink":"https://unrelated.example/wrong"},{"fs_id":9007199254740993,"dlink":"https://d.baidupcs.com/f"}]}`), nil
				default:
					t.Fatal("unexpected endpoint; only home, template and download permitted")
					return nil, nil
				}
			})
			d, err := p.Resolve(context.Background(), c, s, src, f)
			if err != nil {
				t.Fatal(err)
			}
			if d.URL != "https://d.baidupcs.com/f" || d.File.ID != f.ID || d.File.Name != f.Name || d.File.Path != f.Path || d.File.Size != f.Size {
				t.Fatal("selected file identity was not preserved")
			}
			public, _ := json.Marshal(d)
			if strings.Contains(string(public), "mock-private") || strings.Contains(string(public), "stale-") || strings.Contains(string(public), "u/MW6NlArwrT") || len(d.Cookies) != 0 {
				t.Fatal("signature or Baidu credentials exported to unrelated download domain")
			}
			after, _ := json.Marshal(s)
			if string(before) != string(after) {
				t.Fatal("signature persisted into capture")
			}
			if !reflect.DeepEqual(paths, []string{"/disk/home", "/disk/main", "/api/gettemplatevariable", "/api/download"}) {
				t.Fatal("unexpected request sequence")
			}
		})
	}
}

func TestBaiduSignatureLegacyHTMLWithPassportLink(t *testing.T) {
	p := adapterProvider(t, "baidu").(*baiduProvider)
	s := baiduSignatureSession()
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/disk/home" {
			t.Fatal("legacy HTML should remain sufficient")
		}
		return adapterJSON(`<a href="https://passport.baidu.com/">账号</a><script>{"sign1":"Plain\u0074ext","sign3":"Key","timestamp":"1700000000"}</script>`), nil
	})
	sign, timestamp, err := p.webSignature(context.Background(), c, s, model.Source{})
	if err != nil || sign != "u/MW6NlArwrT" || timestamp != "1700000000" {
		t.Fatal("legacy signature failed", err)
	}
}

func TestBaiduSignatureRefreshesPerResolveWithoutCaching(t *testing.T) {
	p := adapterProvider(t, "baidu").(*baiduProvider)
	s := baiduSignatureSession()
	before, _ := json.Marshal(s)
	timestamps := []string{"1700000000", "1700000001"}
	requests := 0
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/disk/home" {
			return adapterJSON(`<html>current application</html>`), nil
		}
		if r.URL.Path != "/api/gettemplatevariable" || requests >= len(timestamps) {
			t.Fatal("unexpected signature request")
		}
		timestamp := timestamps[requests]
		requests++
		return adapterJSON(`{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":` + timestamp + `}}`), nil
	})
	for _, want := range timestamps {
		sign, timestamp, err := p.webSignature(context.Background(), c, s, model.Source{})
		if err != nil || sign != "u/MW6NlArwrT" || timestamp != want {
			t.Fatal("signature reused stale context", err)
		}
	}
	after, _ := json.Marshal(s)
	if requests != 2 || string(before) != string(after) {
		t.Fatal("signature cached in session")
	}
}

func TestBaiduSignatureResolveRequiresPersonalAuthentication(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := baiduSignatureSession()
	s.Capture.Cookies = nil
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		t.Fatal("unauthenticated personal resolve made a request")
		return nil, nil
	})
	d, err := p.Resolve(context.Background(), c, s, model.Source{}, model.File{ID: "1", Name: "f"})
	adapterError(t, err, "login_required")
	if d != nil {
		t.Fatal("unauthenticated resolve returned download")
	}
}

func TestBaiduSignatureErrorsAndNoPassportFalsePositive(t *testing.T) {
	for _, tc := range []struct{ name, raw, code string }{
		{"missing_fields", `{"errno":0,"result":{}}`, "invalid_response"},
		{"partial_not_mixed", `{"errno":0,"result":{"sign3":"Key","timestamp":1700000000}}`, "invalid_response"},
		{"wrong_nesting", `{"errno":0,"sign1":"Plaintext","sign3":"Key","timestamp":1700000000}`, "invalid_response"},
		{"missing_status", `{"result":{"sign1":"Plaintext","sign3":"Key","timestamp":1700000000}}`, "invalid_response"},
		{"invalid_json", `<html>mock-private-response passport.baidu.com</html>`, "invalid_response"},
		{"expired", `{"errno":-6,"errmsg":"mock-private-response"}`, "expired"},
		{"login", `{"errno":31001,"errmsg":"mock-private-response"}`, "login_required"},
		{"verification", `{"errno":132,"errmsg":"mock-private-response"}`, "verification_required"},
		{"unknown_code", `{"errno":"mock-private-response","errmsg":"mock-private-response"}`, "api_error"},
		{"numeric_sign", `{"errno":0,"result":{"sign1":123,"sign3":"Key","timestamp":1700000000}}`, "invalid_response"},
		{"invalid_key", `{"errno":0,"result":{"sign1":"Plaintext","sign3":"","timestamp":1700000000}}`, "invalid_response"},
		{"zero_time", `{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":0}}`, "invalid_response"},
		{"negative_time", `{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":-1}}`, "invalid_response"},
		{"fractional_time", `{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":1700000000.5}}`, "invalid_response"},
		{"signed_time", `{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":"+1700000000"}}`, "invalid_response"},
		{"overflow_time", `{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":99999999999999999999999}}`, "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "baidu").(*baiduProvider)
			s := baiduSignatureSession()
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.URL.Path {
				case "/disk/home":
					return adapterJSON(`<a href="https://passport.baidu.com/">账号</a><script>{"sign1":"partial-html-sign"}</script>`), nil
				case "/api/gettemplatevariable":
					return adapterJSON(tc.raw), nil
				default:
					t.Fatal("signature error must stop before download or mutation")
					return nil, nil
				}
			})
			d, err := p.Resolve(context.Background(), c, s, model.Source{Provider: "baidu"}, model.File{ID: "1", Name: "f"})
			adapterError(t, err, tc.code)
			if d != nil || calls != 2 || strings.Contains(err.Error(), "mock-private") || strings.Contains(err.Error(), "partial-html-sign") {
				t.Fatal("unexpected success, requests or sensitive error")
			}
		})
	}
}

func TestBaiduSignatureResolveRejectsMismatchedDownloadID(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := baiduSignatureSession()
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/disk/home":
			return adapterJSON(`<a href="https://passport.baidu.com/">账号</a>`), nil
		case "/api/gettemplatevariable":
			return adapterJSON(`{"errno":0,"result":{"sign1":"Plaintext","sign3":"Key","timestamp":1700000000}}`), nil
		case "/api/download":
			return adapterJSON(`{"errno":0,"dlink":[{"fs_id":9007199254740992,"dlink":"https://d.baidupcs.com/wrong"}]}`), nil
		default:
			t.Fatal("unexpected endpoint")
			return nil, nil
		}
	})
	d, err := p.Resolve(context.Background(), c, s, model.Source{}, model.File{ID: "9007199254740993", Name: "f"})
	adapterError(t, err, "file_mismatch")
	if d != nil {
		t.Fatal("mismatched file returned")
	}
}

func TestBaiduSignatureTransportFailuresStayTypedAndPrivate(t *testing.T) {
	for _, tc := range []struct{ name, code string }{{"network", "network_error"}, {"foreign_redirect", "network_error"}, {"verification_http", "verification_required"}, {"unavailable", "api_error"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "baidu").(*baiduProvider)
			s := baiduSignatureSession()
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "pan.baidu.com" {
					t.Fatal("cross-host credential request")
				}
				if r.URL.Path == "/disk/home" {
					return adapterJSON(`<a href="https://passport.baidu.com/">账号</a>`), nil
				}
				if r.URL.Path != "/api/gettemplatevariable" {
					t.Fatal("unexpected endpoint")
				}
				switch tc.name {
				case "network":
					return nil, errors.New("mock-private-response")
				case "foreign_redirect":
					res := adapterJSON("")
					res.StatusCode = http.StatusFound
					res.Header.Set("Location", "https://unrelated.example/?token=mock-private-response")
					return res, nil
				case "verification_http":
					res := adapterJSON(`{"errno":132,"errmsg":"mock-private-response"}`)
					res.StatusCode = http.StatusForbidden
					return res, nil
				default:
					res := adapterJSON("mock-private-response")
					res.StatusCode = http.StatusNotFound
					return res, nil
				}
			})
			sign, timestamp, err := p.webSignature(context.Background(), c, s, model.Source{})
			adapterError(t, err, tc.code)
			if sign != "" || timestamp != "" || strings.Contains(err.Error(), "mock-private") {
				t.Fatal("failed signature exposed data")
			}
		})
	}
}
