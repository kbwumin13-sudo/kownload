package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"reflect"
	"testing"
)

func TestAdaptersBaiduCookiePersonalResolveAndTransferCleanup(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := adapterSession("baidu")
	s.Capture.Tokens = nil
	s.Capture.Cookies = []model.Cookie{{Name: "BDUSS", Value: "private-cookie", Domain: ".baidu.com", Path: "/", Secure: true}}
	s.Capture.Share = map[string]string{"bdstoken": "csrf", "shareid": "22", "uk": "11"}
	src := model.Source{Provider: "baidu", URL: "https://pan.baidu.com/s/1history"}
	deleted := []string{}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		cookie, e := r.Cookie("BDUSS")
		if e != nil || cookie.Value != "private-cookie" || r.URL.Query().Get("access_token") != "" {
			t.Fatal("web request lost cookie or used OAuth", r.URL, e)
		}
		switch r.URL.Path {
		case "/api/list":
			if r.URL.Query().Get("num") != "100" || r.URL.Query().Get("page") != "1" {
				t.Fatal(r.URL)
			}
			if r.URL.Query().Get("dir") == "/" {
				return adapterJSON(`{"errno":0,"list":[{"fs_id":1000,"isdir":1,"server_filename":"__gopeed_batch","path":"/__gopeed_batch"}]}`), nil
			}
			return adapterJSON(`{"errno":0,"list":[{"fs_id":9007199254740993,"server_filename":"f","size":5,"path":"/__gopeed_batch/f"}]}`), nil
		case "/disk/home":
			return adapterJSON(`<script>var context={"sign1":"Plaintext","sign3":"Key","timestamp":1700000000};</script>`), nil
		case "/api/download":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			// Published RC4 vector: Key / Plaintext -> BBF316E8D940AF0AD3.
			if r.Form.Get("sign") != "u/MW6NlArwrT" || r.Form.Get("fidlist") != "[9007199254740993]" || r.Form.Get("timestamp") != "1700000000" {
				t.Fatal(r.Form)
			}
			return adapterJSON(`{"errno":0,"dlink":[{"fs_id":"other","dlink":"https://cdn.example/incorrect"},{"fs_id":"9007199254740993","dlink":"https://d.baidupcs.com/f"}]}`), nil
		case "/api/create":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			if r.URL.Query().Get("bdstoken") != "csrf" || r.Form.Get("path") != "/__gopeed_batch" || r.Form.Get("block_list") != "[]" || r.Form.Get("rtype") != "1" {
				t.Fatal(r.URL, r.Form)
			}
			return adapterJSON(`{"errno":0,"fs_id":1000,"path":"/__gopeed_batch"}`), nil
		case "/share/transfer":
			return adapterJSON(`{"errno":0,"taskid":7}`), nil
		case "/share/taskquery":
			if r.URL.Query().Get("taskid") == "7" {
				if r.URL.Query().Get("shareid") != "22" || r.URL.Query().Get("from") != "11" {
					t.Fatal(r.URL)
				}
				return adapterJSON(`{"errno":0,"status":"success","list":[{"from":"/shared/f","to":"/__gopeed_batch/f","to_fs_id":9007199254740993}]}`), nil
			}
			if r.URL.Query().Get("shareid") != "" || r.URL.Query().Get("from") != "" {
				t.Fatal("cleanup used share task identity", r.URL)
			}
			return adapterJSON(`{"status":"success","task_errno":0}`), nil
		case "/api/filemanager":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			if r.URL.Query().Get("opera") != "delete" || r.URL.Query().Get("async") != "2" || r.URL.Query().Get("bdstoken") != "csrf" {
				t.Fatal(r.URL)
			}
			var a []string
			if e := json.Unmarshal([]byte(r.Form.Get("filelist")), &a); e != nil {
				t.Fatal(e)
			}
			deleted = append(deleted, a...)
			return adapterJSON(`{"errno":0,"taskid":8}`), nil
		}
		t.Fatal("unexpected endpoint", r.URL)
		return nil, nil
	})
	// Historical share URL must still use personal APIs after ShareID is cleared.
	l, e := p.List(context.Background(), c, s, src, "")
	if e != nil || len(l.Files) != 1 || l.Files[0].ID != "1000" {
		t.Fatal(l, e)
	}
	share := src
	share.ShareID = "1history"
	r, e := p.Transfer(context.Background(), c, s, share, []model.File{{ID: "100", Name: "f", Size: 5, Path: "nested", Meta: map[string]string{"remote_path": "/shared/f"}}}, "batch")
	if e != nil || !reflect.DeepEqual(r.CreatedIDs, []string{"9007199254740993"}) || r.Files[0].Path != "nested" {
		t.Fatal(r, e)
	}
	d, e := p.Resolve(context.Background(), c, s, src, r.Files[0])
	if e != nil || d.URL != "https://d.baidupcs.com/f" || d.File.ID != "9007199254740993" {
		t.Fatal(d, e)
	}
	if e = p.Cleanup(context.Background(), c, s, r); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(deleted, []string{"/__gopeed_batch/f", "/__gopeed_batch"}) {
		t.Fatal(deleted)
	}
}

func TestAdaptersBaiduAsyncFailureKeepsActualIDs(t *testing.T) {
	for _, tc := range []struct{ task, code string }{
		{`{"errno":0,"status":"failed","list":[{"from_fs_id":100,"to_fs_id":900,"to":"/__gopeed_batch/f"}]}`, "transfer_failed"},
		{`{"errno":0,"status":"success","list":[{"from_fs_id":100,"to_fs_id":901,"to":"/__gopeed_batch/f"}]}`, "invalid_transfer"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			p := adapterProvider(t, "baidu")
			s := adapterSession("baidu")
			s.Capture.Share = map[string]string{"bdstoken": "csrf", "shareid": "22", "uk": "11"}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("method") == "create" {
					return adapterJSON(`{"errno":0,"fs_id":1000,"path":"/__gopeed_batch"}`), nil
				}
				if r.URL.Path == "/share/transfer" {
					return adapterJSON(`{"errno":0,"taskid":7,"extra":{"list":[{"from_fs_id":100,"to_fs_id":900,"to":"/__gopeed_batch/f"}]}}`), nil
				}
				if r.URL.Path == "/share/taskquery" {
					return adapterJSON(tc.task), nil
				}
				t.Fatal(r.URL)
				return nil, nil
			})
			r, e := p.Transfer(context.Background(), c, s, model.Source{ShareID: "1s"}, []model.File{{ID: "100", Name: "f"}}, "batch")
			adapterError(t, e, tc.code)
			if r == nil || r.FolderID != "1000" || len(r.CreatedIDs) == 0 || r.CreatedIDs[0] != "900" {
				t.Fatal(r)
			}
			if tc.code == "invalid_transfer" && !reflect.DeepEqual(r.CreatedIDs, []string{"900", "901"}) {
				t.Fatal("lost second response's created ID", r)
			}
		})
	}
}
