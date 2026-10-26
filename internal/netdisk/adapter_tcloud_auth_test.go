package netdisk

import (
	"context"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"strconv"
	"testing"
)

// Endpoint evidence (public sources, retrieved 2026-10-15):
// cloud.dlife.cn/web/main/js/index.9f3753d6.js loads /web/subapp/main.html,
// which still references the following chunk-common (not a retired bundle).
// cloud.dlife.cn/web/main/subapp/js/chunk-common.c41232a0.js exports
// listCloudFiles({params:{folderId}}) at /api/open/file/listFiles.action;
// its request interceptor adds Browser-Id from initialized Vuex browserId
// (FingerprintJS visitorId). getFileInfo uses /api/portal/getFileInfo.action.
// OpenList drivers/189/{driver,types}.go
// at 482464c2affc5eeba5d4a25bb272aeb352ec5918 reads downloadUrl there.
// The fixed LinkSwift-stable.user.js uses access-token MD5 signing only
// for api.cloud.189.cn/open/file/getFileDownloadUrl.action.
func TestTcloudPersonalListBrowserCookieAuth(t *testing.T) {
	for _, token := range []string{"", "download-token"} {
		t.Run("token="+strconv.FormatBool(token != ""), func(t *testing.T) {
			p := adapterProvider(t, "tcloud")
			s := &Session{Capture: model.Capture{
				Provider: "tcloud",
				Tokens:   map[string]string{"accessToken": token, "browserId": "mock-initialized-browser"},
				// These are synthetic cookies, not browser credentials.
				Cookies: []model.Cookie{
					{Name: "mock_web_session", Value: "web-session", Domain: ".cloud.189.cn", Path: "/", Secure: true},
					{Name: "mock_auth_only", Value: "auth-session", Domain: "open.e.189.cn", Path: "/", Secure: true},
					{Name: "mock_wrong_path", Value: "other-path", Domain: "cloud.189.cn", Path: "/web", Secure: true},
				},
			}}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "cloud.189.cn" || r.URL.Path != "/api/open/file/listFiles.action" {
					t.Fatal("unexpected browser listing endpoint", r.URL)
				}
				if r.URL.Query().Has("fileId") || r.Header.Get("AccessToken") != "" || r.Header.Get("Signature") != "" {
					t.Fatal("browser listing mixed portal parameters or token authentication")
				}
				cookie, err := r.Cookie("mock_web_session")
				if err != nil || cookie.Value != "web-session" {
					t.Fatal("browser session cookie was not sent")
				}
				for _, name := range []string{"mock_auth_only", "mock_wrong_path"} {
					if _, err := r.Cookie(name); err != http.ErrNoCookie {
						t.Fatal("cookie scope was not preserved", name)
					}
				}
				if r.Header.Get("Referer") != "https://cloud.189.cn/" || r.Header.Get("Accept") != "application/json;charset=UTF-8" {
					t.Fatal("missing browser referer or JSON negotiation")
				}
				if r.Header.Get("Browser-Id") != "mock-initialized-browser" || r.Header.Get("Sign-Type") != "1" || r.URL.Query().Get("noCache") == "" {
					t.Fatal("missing official web interceptor headers or cache bypass")
				}
				for _, name := range []string{"mediaType", "iconOption", "orderBy", "descending", "pageSize"} {
					want := map[string]string{"mediaType": "0", "iconOption": "5", "orderBy": "lastOpTime", "descending": "true", "pageSize": "60"}[name]
					if r.URL.Query().Get(name) != want {
						t.Fatal("incorrect browser list parameter", name)
					}
				}
				if r.URL.Query().Get("folderId") == "-11" && r.URL.Query().Get("pageNum") == "1" {
					return adapterJSON(`{"res_code":0,"fileListAO":{"count":61,"folderList":[{"id":9007199254740993,"name":"nested"}],"fileList":[]}}`), nil
				}
				if r.URL.Query().Get("folderId") != "9007199254740993" || r.URL.Query().Get("pageNum") != "2" {
					t.Fatal("incorrect folder or page")
				}
				return adapterJSON(`{"res_code":0,"fileListAO":{"count":61,"folderList":[],"fileList":[{"id":9007199254740995,"name":"file","size":7}]}}`), nil
			})
			// A former share URL must still use personal cookie auth after its
			// explicit ShareID has been cleared by the caller.
			src := model.Source{Provider: "tcloud", URL: "https://cloud.189.cn/t/old-share"}
			l, err := p.List(context.Background(), c, s, src, "")
			if err != nil || len(l.Files) != 1 || l.Files[0].ID != "9007199254740993" || !l.Files[0].IsDir || l.NextPage != "2" {
				t.Fatal(l, err)
			}
			src.ParentID = l.Files[0].ID
			l, err = p.List(context.Background(), c, s, src, "2")
			if err != nil || len(l.Files) != 1 || l.Files[0].ID != "9007199254740995" || l.Files[0].Size != 7 || l.Files[0].Meta["parent_id"] != src.ParentID || l.NextPage != "" {
				t.Fatal(l, err)
			}
		})
	}
}

func TestTcloudPersonalListAuthDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"expired-json", `{"errorCode":"InvalidSessionKey"}`, "expired", 200},
		{"expired-lowercase", `{"errorcode":"InvalidSessionKey"}`, "expired", 200},
		{"expired-http-error", `{"errorCode":"InvalidSessionKey"}`, "expired", 400},
		{"expired-res-code", `{"res_code":"InvalidSessionKey"}`, "expired", 200},
		{"html-login", `<html>Login page</html>`, "invalid_response", 200},
		{"xml-error", `<?xml version="1.0"?><error><code>InvalidSessionKey</code></error>`, "invalid_response", 200},
		{"missing-list", `{"res_code":0}`, "invalid_response", 200},
		{"null-list", `{"res_code":0,"fileListAO":{"fileList":null}}`, "invalid_response", 200},
		{"object-list", `{"res_code":0,"fileListAO":{"fileList":{}}}`, "invalid_response", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "tcloud")
			s := &Session{Capture: model.Capture{Provider: "tcloud"}}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				res := adapterJSON(tc.body)
				res.StatusCode = tc.status
				return res, nil
			})
			_, err := p.List(context.Background(), c, s, model.Source{Provider: "tcloud", URL: "https://cloud.189.cn/web/main"}, "")
			adapterError(t, err, tc.code)
			if calls != 1 {
				t.Fatal("listing unexpectedly attempted another authentication protocol")
			}
		})
	}
}

func TestTcloudPersonalListConcurrentSession(t *testing.T) {
	p := adapterProvider(t, "tcloud")
	s := &Session{Capture: model.Capture{Provider: "tcloud", Tokens: map[string]string{"browserId": "mock-initialized-browser"}, Cookies: []model.Cookie{
		{Name: "mock_web_session", Value: "web-session", Domain: ".cloud.189.cn", Path: "/", Secure: true},
	}}}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if _, err := r.Cookie("mock_web_session"); err != nil {
			t.Error("concurrent listing lost browser cookie")
		}
		return adapterJSON(`{"res_code":0,"fileListAO":{"count":0,"folderList":[],"fileList":[]}}`), nil
	})
	for i := 0; i < 8; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			l, err := p.List(context.Background(), c, s, model.Source{Provider: "tcloud", URL: "https://cloud.189.cn/web/main"}, "")
			if err != nil || len(l.Files) != 0 || l.NextPage != "" {
				t.Fatal(l, err)
			}
		})
	}
}

func TestTcloudPersonalCookieDownload(t *testing.T) {
	p := adapterProvider(t, "tcloud")
	s := &Session{Capture: model.Capture{Provider: "tcloud", Cookies: []model.Cookie{
		{Name: "mock_web_session", Value: "web-session", Domain: ".cloud.189.cn", Path: "/", Secure: true},
	}}}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "cloud.189.cn" || r.URL.Path != "/api/portal/getFileInfo.action" || r.URL.Query().Get("fileId") != "9007199254740995" || r.URL.Query().Has("shareId") {
			t.Fatal("incorrect cookie download endpoint", r.URL)
		}
		if _, err := r.Cookie("mock_web_session"); err != nil {
			t.Fatal("missing browser session cookie")
		}
		if r.Header.Get("AccessToken") != "" || r.Header.Get("Signature") != "" {
			t.Fatal("portal download unexpectedly signed with access token")
		}
		return adapterJSON(`{"res_code":0,"fileId":9007199254740995,"downloadUrl":"//download.cloud.189.cn/file?x=1&amp;y=2"}`), nil
	})
	f := model.File{ID: "9007199254740995", Name: "file", Size: 7, Path: "nested"}
	d, err := p.Resolve(context.Background(), c, s, model.Source{Provider: "tcloud", URL: "https://cloud.189.cn/t/old-share"}, f)
	if err != nil || d.URL != "https://download.cloud.189.cn/file?x=1&y=2" || d.File.ID != f.ID || d.File.Path != f.Path || len(d.Cookies) != 1 {
		t.Fatal(d, err)
	}
	if d.Header["Cookie"] != "" || d.Header["AccessToken"] != "" || d.Header["Signature"] != "" {
		t.Fatal("API credentials escaped into download headers")
	}
}

func TestTcloudPersonalCookieDownloadErrors(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"errorCode":"InvalidSessionKey"}`, "expired"},
		{`{"res_code":0}`, "invalid_response"},
		{`{"res_code":0,"fileDownloadUrl":"https://download.cloud.189.cn/wrong-field"}`, "invalid_response"},
		{`{"res_code":0,"fileId":"other","downloadUrl":"https://download.cloud.189.cn/wrong-file"}`, "file_mismatch"},
	} {
		p := adapterProvider(t, "tcloud")
		s := &Session{Capture: model.Capture{Provider: "tcloud"}}
		calls := 0
		c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
			calls++
			return adapterJSON(tc.body), nil
		})
		_, err := p.Resolve(context.Background(), c, s, model.Source{Provider: "tcloud"}, model.File{ID: "chosen", Name: "file"})
		adapterError(t, err, tc.code)
		if calls != 1 {
			t.Fatal("download attempted another authentication protocol")
		}
	}
}

func TestTcloudPersonalTokenDownloadWithCookies(t *testing.T) {
	p := adapterProvider(t, "tcloud")
	s := &Session{Capture: model.Capture{Tokens: map[string]string{"accessToken": "mock-token"}, Cookies: []model.Cookie{
		{Name: "mock_web_session", Value: "web-session", Domain: ".cloud.189.cn", Path: "/", Secure: true},
	}}}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		stamp := r.Header.Get("Timestamp")
		if r.URL.Host != "api.cloud.189.cn" || r.URL.Path != "/open/file/getFileDownloadUrl.action" || r.URL.Query().Get("fileId") != "chosen" || r.URL.Query().Has("shareId") || r.Header.Get("AccessToken") != "mock-token" || r.Header.Get("Signature") != md5hex("AccessToken=mock-token&Timestamp="+stamp+"&fileId=chosen") {
			t.Fatal("captured cookie changed the existing signed personal download contract")
		}
		return adapterJSON(`{"res_code":0,"fileDownloadUrl":"https://download.cloud.189.cn/file"}`), nil
	})
	if _, err := p.Resolve(context.Background(), c, s, model.Source{Provider: "tcloud"}, model.File{ID: "chosen", Name: "file"}); err != nil {
		t.Fatal(err)
	}
}

func TestTcloudCapturedBrowserID(t *testing.T) {
	for _, capture := range []model.Capture{
		{Headers: map[string]string{"Browser-Id": "mock-initialized-browser"}},
		{Tokens: map[string]string{"browserId": "mock-initialized-browser"}},
		{},
	} {
		p := adapterProvider(t, "tcloud")
		s := &Session{Capture: capture}
		calls := 0
		c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
			calls++
			want := "mock-initialized-browser"
			if capture.Headers == nil && capture.Tokens == nil {
				want = ""
			}
			if r.Header.Get("Browser-Id") != want || r.Header.Get("Sign-Type") != "1" {
				t.Fatal("web request lost the captured browser identity or invented one")
			}
			switch r.URL.Path {
			case "/api/open/file/listFiles.action":
				return adapterJSON(`{"res_code":0,"fileListAO":{"count":0,"folderList":[],"fileList":[]}}`), nil
			case "/api/portal/getFileInfo.action":
				return adapterJSON(`{"res_code":0,"downloadUrl":"https://download.cloud.189.cn/file"}`), nil
			default:
				t.Fatal("unexpected authentication request", r.URL.Path)
				return nil, nil
			}
		})
		src := model.Source{Provider: "tcloud", URL: "https://cloud.189.cn/web/main"}
		if _, err := p.List(context.Background(), c, s, src, ""); err != nil {
			t.Fatal(err)
		}
		d, err := p.Resolve(context.Background(), c, s, src, model.File{ID: "chosen", Name: "file"})
		if err != nil || d.Header["Browser-Id"] != "" || calls != 2 {
			t.Fatal("browser identity escaped into download headers", err)
		}
	}
}
