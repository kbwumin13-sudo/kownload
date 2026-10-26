package netdisk

import (
	"context"
	"net/http"
	"testing"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// Request fixtures are independent absolute URLs, from the fixed LinkSwift dev
// script and OpenList 482464c2 drivers/guangyapan/{driver,util}.go. The official
// https://www.guangyapan.com/static/js/index.b1e7c5bb.js provides the share
// paging path and code/data schema. It still uses /userres, including in vO's
// final URL concatenation; that prefix must not be blindly replaced everywhere.
func TestAdaptersGuangyaAbsoluteEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, path, raw string
		share, resolve  bool
	}{
		{"personal_list", "https://api.guangyapan.com/userres/v1/file/get_file_list", `{"code":0,"data":{"list":[{"fileId":"chosen","fileName":"file","fileSize":12,"resType":1}],"total":1}}`, false, false},
		{"share_list", "https://api.guangyapan.com/userres/v1/get_share_page_files_list", `{"code":0,"data":{"list":[{"fileId":"chosen","fileName":"file","fileSize":12,"resType":1}],"hasMore":false}}`, true, false},
		{"personal_resolve", "https://api.guangyapan.com/nd.bizuserres.s/v1/get_res_download_url", `{"code":0,"data":{"fileId":"chosen","signedURL":"https://cdn.example/chosen"}}`, false, true},
		{"share_resolve", "https://api.guangyapan.com/nd.bizuserres.s/v1/get_share_download_url", `{"code":0,"data":{"fileId":"chosen","downloadUrl":"https://cdn.example/chosen"}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "guangya")
			s := adapterSession("guangya")
			src := model.Source{Provider: "guangya", URL: "https://www.guangyapan.com/", ParentID: "parent"}
			if tc.share {
				src.ShareID, src.Password = "share", "password"
			}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost {
					t.Fatal(r.Method, r.URL)
				}
				b := adapterBody(t, r)
				if tc.share && calls == 1 {
					if r.URL.String() != "https://api.guangyapan.com/nd.bizuserres.s/v1/get_share_access_token" || b["shareId"] != "share" || b["code"] != "password" {
						t.Fatal(r.URL, b)
					}
					return adapterJSON(`{"code":0,"data":{"accessToken":"share-token"}}`), nil
				}
				if r.URL.String() != tc.path || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Did") != "device" || r.Header.Get("Dt") != "4" {
					t.Fatal(r.URL, r.Header)
				}
				if tc.share && b["accessToken"] != "share-token" {
					t.Fatal(b)
				}
				if !tc.share && b["accessToken"] != nil {
					t.Fatal("personal request contains share token", b)
				}
				if tc.resolve {
					if b["fileId"] != "chosen" {
						t.Fatal(b)
					}
				} else if b["parentId"] != "parent" || str(b["pageSize"]) != "100" {
					t.Fatal(b)
				} else if tc.share {
					if b["cursor"] != "" || str(b["orderBy"]) != "0" || str(b["sortType"]) != "0" {
						t.Fatal(b)
					}
				} else if str(b["page"]) != "0" || str(b["orderBy"]) != "3" || str(b["sortType"]) != "1" {
					t.Fatal(b)
				}
				return adapterJSON(tc.raw), nil
			})
			if tc.resolve {
				d, err := p.Resolve(context.Background(), c, s, src, model.File{ID: "chosen", Name: "file", Size: 12})
				if err != nil || d == nil || d.URL != "https://cdn.example/chosen" || d.File.ID != "chosen" {
					t.Fatal(d, err)
				}
			} else {
				l, err := p.List(context.Background(), c, s, src, "")
				if err != nil || l == nil || len(l.Files) != 1 || l.Files[0].ID != "chosen" || l.NextPage != "" {
					t.Fatal(l, err)
				}
			}
			wantCalls := 1
			if tc.share {
				wantCalls++
			}
			if calls != wantCalls {
				t.Fatal("unexpected request count", calls)
			}
		})
	}
}

func TestAdaptersGuangyaRejectsUnknownResponseSchema(t *testing.T) {
	// Include apparently usable payloads: missing/invalid status must never turn
	// a gateway response or unrecognized API schema into successful data.
	const data = `"data":{"list":[],"signedURL":"https://cdn.example/chosen","accessToken":"share-token","fileId":"folder","status":2}`
	for _, tc := range []struct{ name, raw, code string }{
		{"missing_code", `{` + data + `}`, "invalid_response"},
		{"message_only", `{"msg":"OK",` + data + `}`, "invalid_response"},
		{"unknown_schema", `{"status":200,"result":true,` + data + `}`, "invalid_response"},
		{"null_code", `{"code":null,` + data + `}`, "api_error"},
		{"unknown_code", `{"code":9999,` + data + `}`, "api_error"},
		{"expired_code", `{"code":9,` + data + `}`, "expired"},
		{"missing_data", `{"code":0}`, "invalid_response"},
	} {
		for _, operation := range []string{"list", "share_list", "resolve", "share_resolve", "share_token", "task"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				p := adapterProvider(t, "guangya").(*guangyaProvider)
				s := adapterSession("guangya")
				src := model.Source{Provider: "guangya", URL: "https://www.guangyapan.com/"}
				if operation == "share_list" || operation == "share_resolve" {
					src.ShareID = "share"
					s.Capture.Share["share_access_token"] = "cached-token"
				}
				calls := 0
				c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
					calls++
					return adapterJSON(tc.raw), nil
				})
				var err error
				switch operation {
				case "list", "share_list":
					_, err = p.List(context.Background(), c, s, src, "")
				case "resolve", "share_resolve":
					_, err = p.Resolve(context.Background(), c, s, src, model.File{ID: "chosen", Name: "file"})
				case "share_token":
					_, err = p.shareToken(context.Background(), c, s, model.Source{ShareID: "share"})
				case "task":
					err = p.task(context.Background(), c, s, src, "task")
				}
				adapterError(t, err, tc.code)
				if calls != 1 {
					t.Fatal("unexpected retry against a different endpoint", calls)
				}
			})
		}
	}
}

func TestAdaptersGuangyaTaskTerminalStatus(t *testing.T) {
	for _, tc := range []struct{ raw, code string }{
		{`{"code":0,"data":{"status":2,"detail":{"code":0}}}`, ""},
		{`{"code":0,"data":{"status":2}}`, ""},
		{`{"code":0,"data":{"status":2,"detail":{"code":157}}}`, "api_error"},
		{`{"code":0,"data":{"status":3}}`, "transfer_failed"},
		{`{"code":0,"data":{"status":4}}`, "invalid_response"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			p := adapterProvider(t, "guangya").(*guangyaProvider)
			s := adapterSession("guangya")
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.String() != "https://api.guangyapan.com/nd.bizuserres.s/v1/get_task_status" || adapterBody(t, r)["taskId"] != "task" {
					t.Fatal(r.Method, r.URL)
				}
				return adapterJSON(tc.raw), nil
			})
			err := p.task(context.Background(), c, s, model.Source{}, "task")
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				adapterError(t, err, tc.code)
			}
		})
	}
}

func TestGuangyaOmittedZeroStatus(t *testing.T) {
	for _, raw := range []string{`{"msg":"success","data":{"list":[],"total":0}}`, `{"msg":"success","data":{}}`, `{"msg":"","data":{"list":[],"total":0}}`} {
		p := adapterProvider(t, "guangya")
		s := adapterSession("guangya")
		c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) { return adapterJSON(raw), nil })
		l, err := p.List(context.Background(), c, s, model.Source{Provider: "guangya", URL: "https://www.guangyapan.com/"}, "")
		if err != nil || l == nil || len(l.Files) != 0 {
			t.Fatal(l, err)
		}
	}
	for _, raw := range []string{`{"data":{"list":[]}}`, `{"msg":"failed","data":{"list":[]}}`, `{"msg":"success"}`, `{"code":0,"data":{}}`} {
		p := adapterProvider(t, "guangya")
		s := adapterSession("guangya")
		c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) { return adapterJSON(raw), nil })
		if _, err := p.List(context.Background(), c, s, model.Source{Provider: "guangya", URL: "https://www.guangyapan.com/"}, ""); err == nil {
			t.Fatal("invalid list accepted")
		}
	}
}
