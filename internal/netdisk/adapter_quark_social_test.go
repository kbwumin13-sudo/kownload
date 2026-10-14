package netdisk

import (
	"context"
	"encoding/json"
	"errors"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const quarkMockPCUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 QuarkPC/6.9.7.761 QuarkCloudDrivePC/6.9.7.761 quark-cloud-drive/2.5.40"

func TestAdaptersQuarkSocialAcquisitionAndScopedDownload(t *testing.T) {
	p := adapterProvider(t, "quark")
	s := adapterSession("quark")
	s.Capture.Tokens = nil
	s.Capture.Cookies = []model.Cookie{{Name: "session", Value: "mock-cookie", Domain: ".quark.cn", Path: "/", Secure: true}, {Name: "foreign", Value: "never-send", Domain: ".uc.cn", Path: "/"}}
	src := model.Source{Provider: "quark", URL: "https://pan.quark.cn/s/original"}
	f := model.File{ID: "chosen", Name: "a", Path: "nested", Size: 123}
	expiry := str(time.Now().Add(time.Hour).UnixMilli())
	socialCalls, downloads := 0, 0
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("User-Agent") != quarkMockPCUA || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cookie") != "session=mock-cookie" {
			t.Fatal("PC headers or scoped API cookies differ", r.URL, r.Header)
		}
		q := r.URL.Query()
		if r.Method != "POST" || q.Get("pr") != "ucpro" || q.Get("sys") != "win32" || q.Get("ve") != "6.9.7.761" {
			t.Fatal(r.Method, r.URL)
		}
		b := adapterBody(t, r)
		switch r.URL.Path {
		case "/1/clouddrive/chat/conv/file/acquire_dl_token":
			socialCalls++
			if r.URL.Host != "drive-social-api.quark.cn" || !reflect.DeepEqual(q["fr"], []string{"pc", "win"}) || q.Get("la") != "zh-CN" || q.Get("ch") != "pckk@product_guanwan" {
				t.Fatal(r.URL)
			}
			stamp := strings.TrimPrefix(str(b["conversation_id"]), "300000")
			seconds, e := strconv.ParseInt(stamp, 10, 64)
			if e != nil || seconds < time.Now().Unix()-5 || seconds > time.Now().Unix() || str(b["msg_id"]) != stamp+"000" || num(b["conversation_type"]) != 3 {
				t.Fatal("social request identity", b)
			}
			return adapterJSON(`{"code":0,"data":{"token":"memory-social-secret","expired_timestamp":` + expiry + `}}`), nil
		case "/1/clouddrive/file/download":
			downloads++
			if r.URL.Host != "drive-pc.quark.cn" || q.Get("fr") != "pc" || b["token"] != "memory-social-secret" || b["speedup_session"] != "" || !reflect.DeepEqual(items(b["fids"]), []any{"chosen"}) {
				t.Fatal(r.URL, b)
			}
			for _, key := range []string{"pwd_id", "stoken", "fids_token"} {
				if _, ok := b[key]; ok {
					t.Fatal("personal request reused share context", key)
				}
			}
			return adapterJSON(`{"code":0,"data":[{"fid":"chosen","download_url":"https://cdn.quark.cn/chosen"}]}`), nil
		default:
			t.Fatal("unexpected API", r.URL)
			return nil, nil
		}
	})
	u, _ := url.Parse("https://drive-social-api.quark.cn/1/clouddrive/chat/conv/file/acquire_dl_token")
	listed := false
	for _, host := range p.Info().Hosts {
		listed = listed || host == u.Host
	}
	if !c.Allowed(u) || !listed {
		t.Fatal("social host missing from provider allowlist", p.Info())
	}
	for i := 0; i < 2; i++ {
		d, e := p.Resolve(context.Background(), c, s, src, f)
		if e != nil || d == nil || d.File.ID != f.ID || d.File.Path != f.Path || d.File.Size != f.Size {
			t.Fatal(d, e)
		}
		if d.Header["User-Agent"] != quarkMockPCUA || d.Header["Cookie"] != "" || d.Header["Authorization"] != "" || len(d.Cookies) != 1 || d.Cookies[0].Domain != ".quark.cn" {
			t.Fatal("download headers or cookies escaped scope", d)
		}
		public, e := json.Marshal(d.File)
		if e != nil || strings.Contains(string(public), "memory-social-secret") || strings.Contains(string(public), "mock-cookie") {
			t.Fatal("credentials escaped into public file", string(public), e)
		}
	}
	if socialCalls != 1 || downloads != 2 || s.Capture.Share["social_token"] != "memory-social-secret" || s.Capture.Share["social_token_expired_timestamp"] != expiry {
		t.Fatal("token not cached in operation Capture", socialCalls, downloads)
	}
}

func TestAdaptersQuarkSocialCacheExpiry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ttl        time.Duration
		omitExpiry bool
		wantCalls  int
	}{
		{"valid", time.Hour, false, 0},
		{"expired", -time.Minute, false, 1},
		{"sixty_second_margin", 30 * time.Second, false, 1},
		{"unknown_expiry", 0, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "quark").(*quarkProvider)
			s := adapterSession("quark")
			s.Capture.Tokens["social_token"] = "captured-token"
			if tc.omitExpiry {
				delete(s.Capture.Tokens, "social_token_expired_timestamp")
			} else {
				s.Capture.Tokens["social_token_expired_timestamp"] = str(time.Now().Add(tc.ttl).UnixMilli())
			}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "drive-social-api.quark.cn" {
					t.Fatal(r.URL)
				}
				return adapterJSON(`{"code":0,"data":{"token":"refreshed-token","expired_timestamp":` + str(time.Now().Add(time.Hour).UnixMilli()) + `}}`), nil
			})
			want := "refreshed-token"
			if tc.wantCalls == 0 {
				want = "captured-token"
			}
			for i := 0; i < 2; i++ {
				got, e := p.socialToken(context.Background(), c, s, model.Source{})
				if e != nil || got != want {
					t.Fatal(got, e)
				}
			}
			if calls != tc.wantCalls {
				t.Fatal("cache reused expired captured token or failed to reuse refreshed token", calls)
			}
		})
	}
	t.Run("response_without_expiry", func(t *testing.T) {
		p := adapterProvider(t, "quark").(*quarkProvider)
		s := adapterSession("quark")
		s.Capture.Tokens = nil
		s.Capture.Share["social_token_expired_timestamp"] = str(time.Now().Add(time.Hour).UnixMilli())
		calls := 0
		c := adapterClient(p, s, func(*http.Request) (*http.Response, error) {
			calls++
			return adapterJSON(`{"code":0,"data":{"token":"unknown-expiry-token"}}`), nil
		})
		for i := 0; i < 2; i++ {
			got, e := p.socialToken(context.Background(), c, s, model.Source{})
			if e != nil || got != "unknown-expiry-token" {
				t.Fatal(got, e)
			}
		}
		if calls != 2 || s.Capture.Share["social_token_expired_timestamp"] != "" {
			t.Fatal("unknown expiry reused old timestamp", calls)
		}
	})
}

func TestAdaptersQuarkSocialErrorsAndAnonymousShare(t *testing.T) {
	for _, tc := range []struct {
		name, raw, code string
		guestAllowed    bool
	}{
		{"login", `{"code":31001}`, "login_required", true},
		{"expired", `{"code":112}`, "expired", true},
		{"expired_response_token", `{"code":0,"data":{"token":"stale","expired_timestamp":1}}`, "expired", true},
		{"verification", `{"code":8001}`, "verification_required", false},
		{"quota", `{"code":5113}`, "quota_exceeded", false},
		{"other_api_error", `{"code":5000}`, "api_error", false},
		{"missing_token", `{"code":0,"data":{}}`, "invalid_response", false},
		{"malformed", `not-json`, "invalid_response", false},
		{"network", "", "network_error", false},
	} {
		for _, share := range []bool{false, true} {
			mode := "personal"
			if share {
				mode = "share"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				p := adapterProvider(t, "quark")
				s := adapterSession("quark")
				s.Capture.Tokens = nil
				s.Capture.AccountID = ""
				s.Capture.Share = map[string]string{"stoken": "share-token", "share_fid_token.chosen": "file-token"}
				src := model.Source{Provider: "quark", URL: "https://pan.quark.cn/s/share"}
				if share {
					src.ShareID = "share"
				}
				downloads := 0
				c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/1/clouddrive/chat/conv/file/acquire_dl_token":
						if tc.raw == "" {
							return nil, errors.New("mock network failure")
						}
						return adapterJSON(tc.raw), nil
					case "/1/clouddrive/file/download":
						downloads++
						b := adapterBody(t, r)
						if !share || !tc.guestAllowed || b["token"] != "" || b["speedup_session"] != "" || b["pwd_id"] != "share" || b["stoken"] != "share-token" || !reflect.DeepEqual(items(b["fids_token"]), []any{"file-token"}) {
							t.Fatal("unexpected anonymous fallback", b)
						}
						return adapterJSON(`{"code":0,"data":[{"fid":"chosen","download_url":"https://cdn.quark.cn/chosen"}]}`), nil
					default:
						t.Fatal("unexpected API", r.URL)
						return nil, nil
					}
				})
				d, e := p.Resolve(context.Background(), c, s, src, model.File{ID: "chosen", Name: "a"})
				if share && tc.guestAllowed {
					if e != nil || d == nil || downloads != 1 {
						t.Fatal(d, e, downloads)
					}
				} else {
					adapterError(t, e, tc.code)
					if d != nil || downloads != 0 {
						t.Fatal("error swallowed", d, downloads)
					}
				}
			})
		}
	}
}

func TestAdaptersUCShareDirectDownloadAndTypedErrors(t *testing.T) {
	for _, tc := range []struct{ name, raw, code string }{
		{"direct", `{"code":0,"data":[{"fid":"chosen","download_url":"https://cdn.uc.cn/chosen"}]}`, ""},
		{"requires_transfer", `{"code":"save_required"}`, "transfer_required"},
		{"login", `{"code":31001}`, "login_required"},
		{"expired", `{"code":112}`, "expired"},
		{"quota", `{"code":5113}`, "quota_exceeded"},
		{"api_error", `{"code":5000}`, "api_error"},
		{"mismatch", `{"code":0,"data":[{"fid":"other","download_url":"https://cdn.uc.cn/other"}]}`, "file_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "uc")
			s := adapterSession("uc")
			s.Capture.AccountID = ""
			s.Capture.Share = map[string]string{"stoken": "share-token", "share_fid_token.chosen": "file-token"}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "pc-api.uc.cn" || r.URL.Path != "/1/clouddrive/file/download" || r.Method != "POST" {
					t.Fatal("share used transfer or social API", r.URL)
				}
				q := r.URL.Query()
				if q.Get("pr") != "UCBrowser" || q.Get("fr") != "pc" || q.Get("sys") != "win32" || q.Get("ve") != "1.8.8" || r.Header.Get("User-Agent") != "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) uc-cloud-drive/1.8.8 Chrome/100.0.4896.160 Electron/18.3.5.16-b62cf9c50d Safari/537.36 Channel/ucpan_other_ch" || r.Header.Get("Sec-Ch-Ua") != `"Not=A?Brand";v="99", "Chromium";v="100", "Google Chrome";v="100"` {
					t.Fatal("UC PC identity differs", r.URL, r.Header)
				}
				b := adapterBody(t, r)
				if b["pwd_id"] != "share" || b["stoken"] != "share-token" || !reflect.DeepEqual(items(b["fids"]), []any{"chosen"}) || !reflect.DeepEqual(items(b["fids_token"]), []any{"file-token"}) {
					t.Fatal(b)
				}
				if _, ok := b["token"]; ok {
					t.Fatal("Quark social token sent to UC", b)
				}
				return adapterJSON(tc.raw), nil
			})
			d, e := p.Resolve(context.Background(), c, s, model.Source{Provider: "uc", URL: "https://drive.uc.cn/s/share", ShareID: "share"}, model.File{ID: "chosen", Name: "a"})
			if calls != 1 {
				t.Fatal("download not attempted exactly once", calls)
			}
			if tc.code == "" {
				if e != nil || d == nil || d.URL != "https://cdn.uc.cn/chosen" {
					t.Fatal(d, e)
				}
			} else {
				adapterError(t, e, tc.code)
				if d != nil {
					t.Fatal(d)
				}
			}
		})
	}
}
