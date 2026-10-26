package netdisk

import (
	"context"
	"errors"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type adapterTransport func(*http.Request) (*http.Response, error)

func (f adapterTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func adapterJSON(raw string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}
}
func adapterProvider(t *testing.T, id string) Provider {
	t.Helper()
	for _, p := range BuiltinProviders() {
		if p.Info().ID == id {
			return p
		}
	}
	t.Fatal("missing provider", id)
	return nil
}
func adapterSession(id string) *Session {
	s := &Session{ID: "session", CreatedAt: time.Now(), Capture: model.Capture{Provider: id, AccountID: "account", Tokens: map[string]string{"access_token": "token", "default_drive_id": "drive", "Authorization": "Bearer token", "X-Captcha-Token": "captcha", "X-Device-Id": "device"}, Share: map[string]string{}}}
	if id == "quark" {
		s.Capture.Tokens["social_token"] = "mock-social-token"
		s.Capture.Tokens["social_token_expired_timestamp"] = str(time.Now().Add(time.Hour).UnixMilli())
	}
	return s
}
func adapterClient(p Provider, s *Session, fn adapterTransport) *APIClient {
	c := NewAPIClient(nil, &base.Request{}, p.Info(), s)
	c.Client.Transport = fn
	return c
}
func adapterBody(t *testing.T, r *http.Request) object {
	t.Helper()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	d, e := decodeObject(b)
	if e != nil {
		t.Fatal(e, string(b))
	}
	return d
}
func adapterError(t *testing.T, e error, code string) {
	t.Helper()
	var v *Error
	if !errors.As(e, &v) || v.Code != code {
		t.Fatalf("want typed %s, got %v", code, e)
	}
}

func TestAdaptersBuiltinIdentity(t *testing.T) {
	want := []string{"baidu", "aliyun", "mcloud", "tcloud", "xunlei", "quark", "uc", "123pan", "guangya"}
	ids := []string{}
	for _, p := range BuiltinProviders() {
		ids = append(ids, p.Info().ID)
		if len(p.Info().Hosts) == 0 {
			t.Fatal(p.Info())
		}
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatal(ids)
	}
}

// Fixtures use the upstream field names and deliberately include a directory,
// an integer beyond float64 precision, and an expiring credential to exercise
// the public model rather than only matching generated request bodies.
func TestAdaptersPersonalDirectoryPagination(t *testing.T) {
	cases := []struct{ id, path, raw, next string }{
		{"baidu", "/rest/2.0/xpan/file", `{"errno":0,"list":[{"fs_id":9007199254740993,"server_filename":"folder","path":"/a/folder","isdir":1}]}`, ""},
		{"aliyun", "/v2/file/list", `{"items":[{"file_id":"child","name":"folder","type":"folder","drive_id":"drive"}],"next_marker":"marker2"}`, "marker2"},
		{"mcloud", "/hcy/file/list", `{"success":true,"data":{"items":[{"fileId":"child","name":"folder","type":"folder"}],"nextPageCursor":"cursor2"}}`, "cursor2"},
		{"tcloud", "/api/open/file/listFiles.action", `{"res_code":0,"fileListAO":{"count":200,"folderList":[{"id":123,"name":"folder"}],"fileList":[]}}`, "3"},
		{"xunlei", "/drive/v1/files", `{"files":[{"id":"child","name":"folder","kind":"drive#folder"}],"next_page_token":"page2"}`, "page2"},
		{"quark", "/1/clouddrive/file/sort", `{"code":0,"data":{"list":[{"fid":"child","file_name":"folder","file":false,"file_type":0}]},"metadata":{"_total":230}}`, "3"},
		{"uc", "/1/clouddrive/file/sort", `{"code":0,"data":{"list":[{"fid":"child","file_name":"folder","file":false,"file_type":0}]},"metadata":{"_total":230}}`, "3"},
		{"123pan", "/b/api/file/list/new", `{"code":0,"data":{"InfoList":[{"FileId":123,"FileName":"folder","Type":1}],"Next":"0","Total":230}}`, "3"},
		{"guangya", "/userres/v1/file/get_file_list", `{"code":0,"data":{"list":[{"fileId":"child","fileName":"folder","resType":2}],"total":350}}`, "3"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p := adapterProvider(t, tc.id)
			s := adapterSession(tc.id)
			src := model.Source{Provider: tc.id, URL: "https://" + p.Info().Hosts[0] + "/s/history", ParentID: "parent"}
			if tc.id == "baidu" {
				src.ParentID = "/a"
			}
			s.Capture.Headers = map[string]string{"Authorization": "Basic auth"}
			if tc.id == "mcloud" {
				s.Capture.Tokens["Authorization"] = "Basic auth"
			}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != tc.path {
					t.Fatalf("unexpected endpoint %s", r.URL)
				}
				switch tc.id {
				case "aliyun":
					b := adapterBody(t, r)
					if b["parent_file_id"] != "parent" || b["marker"] != "2" || b["drive_id"] != "drive" {
						t.Fatal(b)
					}
				case "mcloud":
					b := adapterBody(t, r)
					if b["parentFileId"] != "parent" || at(b, "pageInfo", "pageCursor") != "2" {
						t.Fatal(b)
					}
					if r.Header.Get("Mcloud-Sign") == "" {
						t.Fatal("missing signature")
					}
				case "guangya":
					b := adapterBody(t, r)
					if b["parentId"] != "parent" || str(b["page"]) != "2" {
						t.Fatal(b)
					}
				case "quark", "uc":
					if r.URL.Query().Get("_page") != "2" || r.URL.Query().Get("pdir_fid") != "parent" {
						t.Fatal(r.URL)
					}
				case "123pan":
					if r.URL.Query().Get("parentFileId") != "parent" || r.URL.Query().Get("Page") != "2" {
						t.Fatal(r.URL)
					}
				case "tcloud":
					if r.URL.Query().Get("folderId") != "parent" || r.URL.Query().Get("pageNum") != "2" {
						t.Fatal(r.URL)
					}
				case "xunlei":
					if r.URL.Query().Get("parent_id") != "parent" || r.URL.Query().Get("page_token") != "2" {
						t.Fatal(r.URL)
					}
				}
				return adapterJSON(tc.raw), nil
			})
			l, e := p.List(context.Background(), c, s, src, "2")
			if e != nil {
				t.Fatal(e)
			}
			if len(l.Files) != 1 || !l.Files[0].IsDir || l.NextPage != tc.next {
				t.Fatal(l)
			}
			if tc.id == "baidu" && l.Files[0].ID != "9007199254740993" {
				t.Fatal("ID lost precision", l.Files[0])
			}
		})
	}
}

func TestAdaptersOwnResolveKeepsOriginalShareURL(t *testing.T) {
	cases := []struct{ id, path, raw string }{
		{"baidu", "/rest/2.0/xpan/multimedia", `{"errno":0,"list":[{"fs_id":123,"dlink":"https://d.pcs.baidu.com/file"}]}`},
		{"aliyun", "/v2/file/get_download_url", `{"file_id":"123","url":"https://cdn.example/ali"}`},
		{"mcloud", "/hcy/file/getDownloadUrl", `{"success":true,"data":{"url":"https://cdn.example/mobile"}}`},
		{"tcloud", "/open/file/getFileDownloadUrl.action", `{"res_code":0,"fileDownloadUrl":"//download.cloud.189.cn/file"}`},
		{"xunlei", "/drive/v1/files/123", `{"id":"123","web_content_link":"https://cdn.example/thunder"}`},
		{"quark", "/1/clouddrive/file/download", `{"code":0,"data":[{"fid":"123","download_url":"https://cdn.quark.cn/file"}]}`},
		{"uc", "/1/clouddrive/file/download", `{"code":0,"data":[{"fid":"123","download_url":"https://cdn.uc.cn/file"}]}`},
		{"123pan", "/api/v2/file/download_info", `{"code":0,"data":{"dispatchList":[{"prefix":"https://cdn.example"}],"downloadPath":"/123"}}`},
		{"guangya", "/nd.bizuserres.s/v1/get_res_download_url", `{"code":0,"data":{"signedURL":"https://cdn.example/duck"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p := adapterProvider(t, tc.id)
			s := adapterSession(tc.id)
			s.Capture.Share = map[string]string{"stoken": "oldshare", "share_token": "oldshare"}
			f := model.File{ID: "123", Name: "file.bin", Size: 17, Path: "chosen/nested", Hash: "etag", Meta: map[string]string{"s3key_flag": "flag", "etag": "etag"}}
			src := model.Source{Provider: tc.id, URL: "https://" + p.Info().Hosts[0] + "/s/history"}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != tc.path {
					t.Fatalf("unexpected share endpoint %s", r.URL)
				}
				if r.Method == "POST" {
					b := adapterBody(t, r)
					for _, key := range []string{"share_id", "pwd_id", "ShareKey", "accessToken", "stoken"} {
						if _, ok := b[key]; ok {
							t.Fatalf("own resolve reused share %s", key)
						}
					}
					if tc.id == "aliyun" && b["file_id"] != f.ID {
						t.Fatal(b)
					}
				}
				return adapterJSON(tc.raw), nil
			})
			d, e := p.Resolve(context.Background(), c, s, src, f)
			if e != nil {
				t.Fatal(e)
			}
			if d.File.ID != f.ID || d.File.Path != f.Path || d.URL == "" {
				t.Fatal(d)
			}
			if d.Header["Cookie"] != "" || d.Header["Authorization"] != "" {
				t.Fatal("auth header escaped API")
			}
		})
	}
}

func TestAdaptersAliyunConcurrentSingleFileMapping(t *testing.T) {
	p := adapterProvider(t, "aliyun")
	s := adapterSession("aliyun")
	src := model.Source{URL: "https://www.alipan.com/drive"}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		b := adapterBody(t, r)
		id := str(b["file_id"])
		return adapterJSON(`{"file_id":"` + id + `","url":"https://cdn.example/` + id + `"}`), nil
	})
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c"} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := model.File{ID: id, Name: id}
			d, e := p.Resolve(context.Background(), c, s, src, f)
			if e != nil || d == nil || d.URL != "https://cdn.example/"+id || d.File.ID != id {
				t.Errorf("file %s: %#v %v", id, d, e)
			}
		}()
	}
	wg.Wait()
}

func TestAdaptersMismatchExpiredAndUnsupported(t *testing.T) {
	p := adapterProvider(t, "aliyun")
	s := adapterSession("aliyun")
	src := model.Source{URL: "https://www.alipan.com/drive"}
	f := model.File{ID: "a", Name: "a"}
	for _, tc := range []struct{ raw, code string }{{`{"file_id":"b","url":"https://cdn.example/b"}`, "file_mismatch"}, {`{"code":"AccessTokenInvalid"}`, "expired"}, {`{"file_id":"a","url":"https://cdn.example/a?Expires=1","expiration":"2000-01-01T00:00:00Z"}`, "expired"}, {`{}`, "invalid_response"}} {
		c := adapterClient(p, s, func(*http.Request) (*http.Response, error) { return adapterJSON(tc.raw), nil })
		d, e := p.Resolve(context.Background(), c, s, src, f)
		if d != nil {
			t.Fatal("fake success", d)
		}
		adapterError(t, e, tc.code)
	}
	p = adapterProvider(t, "guangya")
	ss := adapterSession("guangya")
	ss.Capture.Share["share_access_token"] = "share"
	cc := adapterClient(p, ss, func(*http.Request) (*http.Response, error) {
		return adapterJSON(`{"code":0,"data":{"list":[],"hasMore":true,"cursor":""}}`), nil
	})
	l, e := p.List(context.Background(), cc, ss, model.Source{ShareID: "share"}, "")
	if l != nil {
		t.Fatal(l)
	}
	adapterError(t, e, "invalid_pagination")
}

func TestAdaptersCookieJarDownloadScope(t *testing.T) {
	p := adapterProvider(t, "quark")
	s := adapterSession("quark")
	s.Capture.Cookies = []model.Cookie{{Name: "original", Value: "ok", Domain: ".quark.cn", Path: "/", Secure: true}, {Name: "different", Value: "secret", Domain: "pan.quark.cn", Path: "/", Secure: true}}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if ck, e := r.Cookie("original"); e != nil || ck.Value != "ok" {
			t.Fatal("jar wasn't seeded")
		}
		res := adapterJSON(`{"code":0,"data":[{"fid":"a","download_url":"https://cdn.quark.cn/a"}]}`)
		res.Header.Set("Set-Cookie", "__pugs=new; Domain=.quark.cn; Path=/; Secure")
		return res, nil
	})
	d, e := p.Resolve(context.Background(), c, s, model.Source{URL: "https://pan.quark.cn/drive"}, model.File{ID: "a", Name: "a"})
	if e != nil {
		t.Fatal(e)
	}
	names := map[string]string{}
	for _, ck := range d.Cookies {
		names[ck.Name] = ck.Value
		if ck.Domain == "" {
			t.Fatal("missing cookie domain")
		}
	}
	if names["__pugs"] != "new" || names["original"] != "ok" || names["different"] != "" {
		t.Fatal(names)
	}
	d, e = download(c, s, model.File{ID: "a", Name: "a"}, "https://cdn.unrelated.example/a", nil, nil)
	if e != nil || len(d.Cookies) != 0 {
		t.Fatal("cookie leaked", d, e)
	}
}

func TestAdaptersAPIWhitelistRejectsBeforeTransport(t *testing.T) {
	p := adapterProvider(t, "quark")
	s := adapterSession("quark")
	called := false
	c := adapterClient(p, s, func(*http.Request) (*http.Response, error) { called = true; return adapterJSON(`{}`), nil })
	for _, raw := range []string{"https://drive-pc.quark.cn.attacker.example/a", "https://user@drive-pc.quark.cn/a", "file:///tmp/a"} {
		_, e := c.Do(context.Background(), "GET", raw, nil, nil)
		if e == nil {
			t.Fatal(raw)
		}
	}
	if called {
		t.Fatal("untrusted API host received a request")
	}
	u, _ := url.Parse("https://drive-pc.quark.cn/a")
	if !c.Allowed(u) {
		t.Fatal("known endpoint rejected")
	}
}
