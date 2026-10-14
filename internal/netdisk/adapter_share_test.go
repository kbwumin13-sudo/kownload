package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAdaptersShareDirectoriesAndEphemeralTokens(t *testing.T) {
	for _, id := range []string{"baidu", "aliyun", "mcloud", "tcloud", "xunlei", "quark", "uc", "123pan"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share = map[string]string{"share_token": "ali-share", "stoken": "quark-share", "pass_code_token": "thunder-share"}
			src := model.Source{Provider: id, URL: "https://" + p.Info().Hosts[0] + "/s/share", ShareID: "share", ParentID: "parent", Password: "pwd"}
			if id == "baidu" {
				src.ParentID = "/subdir"
			}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				switch id {
				case "baidu":
					if r.URL.Path != "/share/wxlist" {
						t.Fatal(r.URL)
					}
					if e := r.ParseForm(); e != nil {
						t.Fatal(e)
					}
					if r.Form.Get("dir") != "/subdir" || r.Form.Get("root") != "0" || r.Form.Get("page") != "2" || r.Form.Get("pwd") != "pwd" {
						t.Fatal(r.Form)
					}
					return adapterJSON(`{"errno":0,"data":{"uk":42,"shareid":123,"seckey":"secret","has_more":1,"list":[{"fs_id":234,"server_filename":"f","size":12,"isdir":0,"path":"/subdir/f"}]}}`), nil
				case "aliyun":
					b := adapterBody(t, r)
					if r.URL.Path != "/adrive/v3/file/list" || b["parent_file_id"] != "parent" || b["share_id"] != "share" || r.Header.Get("x-share-token") != "ali-share" {
						t.Fatal(r.URL, b, r.Header)
					}
					return adapterJSON(`{"items":[{"file_id":"234","name":"f","size":12,"type":"file","drive_id":"share-drive"}],"next_marker":"marker"}`), nil
				case "mcloud":
					if r.URL.Path != "/yun-share/richlifeApp/devapp/IOutLink/getOutLinkInfoV6" {
						t.Fatal(r.URL)
					}
					b := make([]byte, r.ContentLength)
					_, _ = r.Body.Read(b)
					plain, e := mcloudDecrypt(string(b))
					if e != nil {
						t.Fatal(e)
					}
					req, e := decodeObject(plain)
					if e != nil || at(req, "getOutLinkInfoReq", "pCaID") != "parent" {
						t.Fatal(req, e)
					}
					enc, e := mcloudEncrypt([]byte(`{"success":true,"data":{"lkName":"title","caLst":[],"coLst":[{"coId":"234","coName":"f","coSize":12}]}}`))
					if e != nil {
						t.Fatal(e)
					}
					return adapterJSON(enc), nil
				case "tcloud":
					if r.URL.Path == "/api/open/share/getShareInfoByCode.action" {
						return adapterJSON(`{"res_code":0,"shareId":123,"fileId":12,"isFolder":true,"shareMode":1}`), nil
					}
					if r.URL.Path != "/api/open/share/listShareDir.action" || r.URL.Query().Get("fileId") != "parent" || r.URL.Query().Get("shareId") != "123" || r.URL.Query().Get("pageNum") != "2" {
						t.Fatal(r.URL)
					}
					return adapterJSON(`{"res_code":0,"fileListAO":{"count":180,"folderList":[],"fileList":[{"id":234,"name":"f","size":12}]}}`), nil
				case "xunlei":
					if r.URL.Path != "/drive/v1/share/detail" || r.URL.Query().Get("parent_id") != "parent" || r.URL.Query().Get("pass_code_token") != "thunder-share" {
						t.Fatal(r.URL)
					}
					return adapterJSON(`{"share_status":"OK","files":[{"id":"234","name":"f","size":"12","kind":"drive#file"}],"next_page_token":"next"}`), nil
				case "quark", "uc":
					if r.URL.Path != "/1/clouddrive/share/sharepage/detail" || r.URL.Query().Get("pwd_id") != "share" || r.URL.Query().Get("pdir_fid") != "parent" || r.URL.Query().Get("_page") != "2" {
						t.Fatal(r.URL)
					}
					return adapterJSON(`{"code":0,"data":{"list":[{"fid":"234","file_name":"f","size":12,"file":true,"share_fid_token":"NEVER-PERSIST"}]},"metadata":{"_total":240}}`), nil
				case "123pan":
					if r.URL.Path != "/b/api/share/get" || r.URL.Query().Get("shareKey") != "share" || r.URL.Query().Get("SharePwd") != "pwd" {
						t.Fatal(r.URL)
					}
					return adapterJSON(`{"code":0,"data":{"InfoList":[{"FileId":234,"FileName":"f","Size":12,"Type":0,"Etag":"etag","S3KeyFlag":"flag"}],"Next":"0"}}`), nil
				}
				t.Fatal(id)
				return nil, nil
			})
			cursor := "2"
			if id == "mcloud" {
				cursor = ""
			}
			l, e := p.List(context.Background(), c, s, src, cursor)
			if e != nil {
				t.Fatal(e)
			}
			if len(l.Files) != 1 || l.Files[0].ID != "234" || l.Files[0].Name != "f" || l.Files[0].Size != 12 || l.Files[0].IsDir {
				t.Fatal(l)
			}
			b, _ := json.Marshal(l)
			if strings.Contains(string(b), "NEVER-PERSIST") || strings.Contains(string(b), "secret") {
				t.Fatal("ephemeral token persisted", string(b))
			}
			if (id == "quark" || id == "uc") && s.Capture.Share["share_fid_token.234"] != "NEVER-PERSIST" {
				t.Fatal("token wasn't retained in Capture")
			}
			if calls == 0 {
				t.Fatal("no API request")
			}
		})
	}
}

func TestAdaptersSignedRequests(t *testing.T) {
	if got := mcloudSign([]byte(`{"fileId":"abc"}`), "2026-10-14 12:34:56", "0123456789abcdef"); got != "E9D75263EA38FB1858D2D7151A1BF523" {
		t.Fatal(got)
	}
	stamp := time.Date(2026, 10, 14, 4, 34, 56, 0, time.UTC)
	raw := pan123Sign("https://yun.123pan.com/b/api/share/get?shareKey=foo", stamp, "1234567")
	u, _ := url.Parse(raw)
	if u.Query().Get("215578447") != "1791952496-1234567-3884289918" || u.Query().Get("shareKey") != "foo" {
		t.Fatal(raw)
	}
	p := adapterProvider(t, "tcloud")
	s := adapterSession("tcloud")
	s.Capture.Share["tcloud_share_id"] = "456"
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		ts := r.Header.Get("Timestamp")
		want := md5hex("AccessToken=token&Timestamp=" + ts + "&dt=1&fileId=123&shareId=456")
		if r.Header.Get("Signature") != want || r.Header.Get("AccessToken") != "token" || r.Header.Get("Sign-Type") != "1" {
			t.Fatal(r.Header)
		}
		return adapterJSON(`{"res_code":0,"fileDownloadUrl":"https://cdn.example/file"}`), nil
	})
	if _, e := p.Resolve(context.Background(), c, s, model.Source{URL: "https://cloud.189.cn/t/abc", ShareID: "abc"}, model.File{ID: "123", Name: "file"}); e != nil {
		t.Fatal(e)
	}
}

func TestAdaptersShareResolveSelection(t *testing.T) {
	for _, id := range []string{"quark", "uc"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share["stoken"] = "stoken"
			s.Capture.Files = []model.File{{ID: "chosen", Name: "a", Meta: map[string]string{"share_fid_token": "secret"}}}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				b := adapterBody(t, r)
				if r.URL.Path != "/1/clouddrive/file/download" || !reflect.DeepEqual(items(b["fids"]), []any{"chosen"}) || !reflect.DeepEqual(items(b["fids_token"]), []any{"secret"}) || b["stoken"] != "stoken" || b["pwd_id"] != "share" {
					t.Fatal(r.URL, b)
				}
				return adapterJSON(`{"code":0,"data":[{"fid":"wrong","download_url":"https://cdn.example/wrong"},{"fid":"chosen","download_url":"https://cdn.example/chosen"}]}`), nil
			})
			d, e := p.Resolve(context.Background(), c, s, model.Source{URL: "https://" + p.Info().Hosts[0] + "/s/share", ShareID: "share"}, model.File{ID: "chosen", Name: "a"})
			if e != nil || d == nil || d.URL != "https://cdn.example/chosen" || calls != 1 {
				t.Fatal(d, e, calls)
			}
		})
	}
}

func TestAdaptersShareResolveOtherPlatforms(t *testing.T) {
	cases := []struct{ id, path, raw string }{
		{"aliyun", "/v2/file/get_share_link_download_url", `{"file_id":"123","download_url":"https://cdn.example/ali"}`},
		{"xunlei", "/drive/v1/share/file_info", `{"file_info":{"id":"123","web_content_link":"https://cdn.example/thunder"}}`},
		{"123pan", "/api/v2/share/download/info", `{"code":0,"data":{"dispatchList":[{"prefix":"https://cdn.example"}],"downloadPath":"/123"}}`},
		{"guangya", "/userres/v1/get_share_download_url", `{"code":0,"data":{"signedURL":"https://cdn.example/duck"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p := adapterProvider(t, tc.id)
			s := adapterSession(tc.id)
			s.Capture.Share = map[string]string{"share_token": "ali-share", "pass_code_token": "thunder-share", "share_access_token": "duck-share"}
			src := model.Source{Provider: tc.id, URL: "https://" + p.Info().Hosts[0] + "/s/share", ShareID: "share"}
			f := model.File{ID: "123", Name: "file", Hash: "etag", Meta: map[string]string{"s3key_flag": "flag", "drive_id": "shared-drive"}}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != tc.path {
					t.Fatal(r.URL)
				}
				if r.Method == "POST" {
					b := adapterBody(t, r)
					switch tc.id {
					case "aliyun":
						if b["file_id"] != "123" || b["share_id"] != "share" || r.Header.Get("x-share-token") != "ali-share" {
							t.Fatal(b, r.Header)
						}
					case "123pan":
						if str(b["FileID"]) != "123" || b["ShareKey"] != "share" {
							t.Fatal(b)
						}
					case "guangya":
						if b["fileId"] != "123" || b["accessToken"] != "duck-share" || r.Header.Get("X-Captcha-Token") != "captcha" {
							t.Fatal(b, r.Header)
						}
					}
				} else if r.URL.Query().Get("file_id") != "123" || r.URL.Query().Get("share_id") != "share" {
					t.Fatal(r.URL)
				}
				return adapterJSON(tc.raw), nil
			})
			d, e := p.Resolve(context.Background(), c, s, src, f)
			if e != nil || d == nil || d.File.ID != "123" {
				t.Fatal(d, e)
			}
		})
	}
}
