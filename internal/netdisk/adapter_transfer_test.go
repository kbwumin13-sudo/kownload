package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestAdaptersQuarkUCTransferReceiptAndCleanupOwnership(t *testing.T) {
	for _, id := range []string{"quark", "uc"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share = map[string]string{"stoken": "ephemeral", "share_fid_token.a": "token-a", "share_fid_token.b": "token-b"}
			src := model.Source{Provider: id, URL: "https://" + p.Info().Hosts[0] + "/s/history", ShareID: "history"}
			files := []model.File{{ID: "a", Name: "first", Path: "sub/one", Size: 5}, {ID: "b", Name: "second", Path: "sub/two", Size: 6}}
			deletes := []string{}
			foreign := false
			saveCount := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/1/clouddrive/file":
					b := adapterBody(t, r)
					if b["file_name"] != "__gopeed_batch" || b["pdir_fid"] != "0" {
						t.Fatal(b)
					}
					return adapterJSON(`{"code":0,"data":{"fid":"created-folder"}}`), nil
				case "/1/clouddrive/share/sharepage/save":
					b := adapterBody(t, r)
					saveCount++
					if b["to_pdir_fid"] != "created-folder" || b["pwd_id"] != "history" {
						t.Fatal(b)
					}
					id := str(items(b["fid_list"])[0])
					if str(items(b["fid_token_list"])[0]) != "token-"+id {
						t.Fatal(b)
					}
					return adapterJSON(`{"code":0,"data":{"task_id":"save-` + id + `"}}`), nil
				case "/1/clouddrive/task":
					task := r.URL.Query().Get("task_id")
					if strings.HasPrefix(task, "save-") {
						id := strings.TrimPrefix(task, "save-")
						return adapterJSON(`{"code":0,"data":{"status":2,"save_as":{"save_as_top_fids":["copy-` + id + `"]}}}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"status":2}}`), nil
				case "/1/clouddrive/file/sort":
					if r.URL.Query().Get("pdir_fid") == "0" {
						return adapterJSON(`{"code":0,"data":{"list":[{"fid":"created-folder","file_name":"__gopeed_batch","file":false}]},"metadata":{"_total":1}}`), nil
					}
					if foreign {
						return adapterJSON(`{"code":0,"data":{"list":[{"fid":"someone-elses","file_name":"other","file":true}]},"metadata":{"_total":1}}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"list":[{"fid":"copy-a","file_name":"first","file":true},{"fid":"copy-b","file_name":"second","file":true}]},"metadata":{"_total":2}}`), nil
				case "/1/clouddrive/file/delete":
					b := adapterBody(t, r)
					for _, v := range items(b["filelist"]) {
						deletes = append(deletes, str(v))
					}
					return adapterJSON(`{"code":0,"data":{"task_id":"delete"}}`), nil
				}
				t.Fatalf("unexpected endpoint %s", r.URL)
				return nil, nil
			})
			r, e := p.Transfer(context.Background(), c, s, src, files, "batch")
			if e != nil {
				t.Fatal(e)
			}
			if r.FolderID != "created-folder" || !reflect.DeepEqual(r.CreatedIDs, []string{"copy-a", "copy-b"}) || r.Files[0].Path != "sub/one" || r.Files[1].Path != "sub/two" || saveCount != 2 {
				t.Fatal(r)
			}
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), "ephemeral") || strings.Contains(string(raw), "token-a") {
				t.Fatal("receipt leaked credentials", string(raw))
			}
			other := clone(s)
			other.Capture.AccountID = "other-account"
			adapterError(t, p.Cleanup(context.Background(), c, other, r), "ownership_error")
			if len(deletes) != 0 {
				t.Fatal(deletes)
			}
			foreign = true
			adapterError(t, p.Cleanup(context.Background(), c, s, r), "ownership_error")
			if len(deletes) != 0 {
				t.Fatal(deletes)
			}
			foreign = false
			if e = p.Cleanup(context.Background(), c, s, r); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(deletes, []string{"copy-a", "copy-b", "created-folder"}) {
				t.Fatal("cleanup deleted source files", deletes)
			}
		})
	}
}

func TestAdaptersPartialTransferPersistsOnlyActualCreatedIDs(t *testing.T) {
	for _, id := range []string{"aliyun", "quark", "uc", "xunlei"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share = map[string]string{"stoken": "share", "share_token": "share", "pass_code_token": "share", "share_fid_token.a": "a-token", "share_fid_token.b": "b-token"}
			src := model.Source{Provider: id, URL: "https://" + p.Info().Hosts[0] + "/s/share", ShareID: "share"}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				path := r.URL.Path
				switch path {
				case "/adrive/v2/file/createWithFolders":
					return adapterJSON(`{"file_id":"new-folder"}`), nil
				case "/v3/batch":
					b := adapterBody(t, r)
					requests := items(b["requests"])
					if len(requests) != 1 {
						t.Fatal("copy must map one file", b)
					}
					original := str(at(requests[0], "id"))
					calls++
					if calls == 2 {
						return adapterJSON(`{"code":"AccessTokenInvalid"}`), nil
					}
					if at(requests[0], "body", "to_parent_file_id") != "new-folder" {
						t.Fatal(b)
					}
					return adapterJSON(`{"responses":[{"id":"` + original + `","status":201,"body":{"file_id":"copy-` + original + `"}}]}`), nil
				case "/1/clouddrive/file":
					return adapterJSON(`{"code":0,"data":{"fid":"new-folder"}}`), nil
				case "/1/clouddrive/share/sharepage/save":
					calls++
					if calls == 2 {
						return adapterJSON(`{"code":401}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"task_id":"task"}}`), nil
				case "/1/clouddrive/task":
					return adapterJSON(`{"code":0,"data":{"status":2,"save_as":{"save_as_top_fids":["copy-a"]}}}`), nil
				case "/drive/v1/files":
					return adapterJSON(`{"id":"new-folder","kind":"drive#folder"}`), nil
				case "/drive/v1/share/restore":
					b := adapterBody(t, r)
					if b["parent_id"] != "new-folder" || !yes(b["specify_parent_id"]) {
						t.Fatal(b)
					}
					calls++
					if calls == 2 {
						return adapterJSON(`{"error_code":9}`), nil
					}
					return adapterJSON(`{"restore_status":"RESTORE_COMPLETE","trace_file_ids":"{\"a\":[\"copy-a\"]}"}`), nil
				}
				t.Fatal(r.URL)
				return nil, nil
			})
			fs := []model.File{{ID: "a", Name: "a", Path: "nested/a"}, {ID: "b", Name: "b", Path: "nested/b"}}
			r, e := p.Transfer(context.Background(), c, s, src, fs, "batch")
			adapterError(t, e, "expired")
			if r == nil || r.FolderID != "new-folder" || !reflect.DeepEqual(r.CreatedIDs, []string{"copy-a"}) || len(r.Files) != 1 || r.Files[0].ID != "copy-a" || r.Files[0].Path != "nested/a" || r.Files[0].Meta["source_id"] != "a" || r.Source.ShareID != "share" {
				t.Fatalf("lost partial receipt: %#v", r)
			}
		})
	}
}

func TestAdaptersTransferNeverSubstitutesNameForCreatedID(t *testing.T) {
	for _, id := range []string{"aliyun", "quark", "uc", "xunlei"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share = map[string]string{"share_token": "x", "stoken": "x", "share_fid_token.a": "x", "pass_code_token": "x"}
			calls := 0
			c := adapterClient(p, s, func(*http.Request) (*http.Response, error) {
				calls++
				if id == "quark" || id == "uc" {
					return adapterJSON(`{"code":0,"data":{"file_name":"__gopeed_batch"}}`), nil
				}
				return adapterJSON(`{"name":"__gopeed_batch"}`), nil
			})
			r, e := p.Transfer(context.Background(), c, s, model.Source{URL: "https://" + p.Info().Hosts[0] + "/s/x", ShareID: "x"}, []model.File{{ID: "a", Name: "a"}}, "batch")
			adapterError(t, e, "invalid_transfer")
			if r != nil || calls != 1 {
				t.Fatal("looked up existing folder by name", r, calls)
			}
		})
	}
}

func TestAdaptersUnsupportedTransfersDoNotTouchAPI(t *testing.T) {
	for _, id := range []string{"mcloud", "123pan"} {
		p := adapterProvider(t, id)
		r, e := p.Transfer(context.Background(), nil, adapterSession(id), model.Source{ShareID: "s"}, []model.File{{ID: "a", Name: "a"}}, "b")
		if r != nil {
			t.Fatal(r)
		}
		adapterError(t, e, "unsupported")
		adapterError(t, p.Cleanup(context.Background(), nil, nil, nil), "unsupported")
	}
}

func TestAdaptersXunleiSuccessfulTransferAndCleanup(t *testing.T) {
	p := adapterProvider(t, "xunlei")
	s := adapterSession("xunlei")
	s.Capture.Share["pass_code_token"] = "secret"
	src := model.Source{Provider: "xunlei", URL: "https://pan.xunlei.com/s/source", ShareID: "source"}
	deletes := []string{}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/drive/v1/share/restore":
			b := adapterBody(t, r)
			if b["parent_id"] != "folder" || str(items(b["file_ids"])[0]) != "source-id" {
				t.Fatal(b)
			}
			return adapterJSON(`{"restore_status":"RESTORE_COMPLETE","trace_file_ids":{"source-id":["own-id"]}}`), nil
		case "/drive/v1/files":
			if r.Method == "POST" {
				b := adapterBody(t, r)
				if b["name"] != "__gopeed_batch" || b["parent_id"] != "" {
					t.Fatal(b)
				}
				return adapterJSON(`{"id":"folder"}`), nil
			}
			if r.URL.Query().Get("parent_id") == "" {
				return adapterJSON(`{"files":[{"id":"folder","name":"__gopeed_batch","kind":"drive#folder"}]}`), nil
			}
			return adapterJSON(`{"files":[{"id":"own-id","name":"a","kind":"drive#file"}]}`), nil
		case "/drive/v1/files:batchDelete":
			b := adapterBody(t, r)
			for _, v := range items(b["ids"]) {
				deletes = append(deletes, str(v))
			}
			return adapterJSON(`{}`), nil
		}
		t.Fatal(r.URL)
		return nil, nil
	})
	r, e := p.Transfer(context.Background(), c, s, src, []model.File{{ID: "source-id", Name: "a", Path: "nested"}}, "batch")
	if e != nil {
		t.Fatal(e)
	}
	if r.Files[0].ID != "own-id" || r.Files[0].Path != "nested" {
		t.Fatal(r)
	}
	if e = p.Cleanup(context.Background(), c, s, r); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(deletes, []string{"own-id", "folder"}) {
		t.Fatal(deletes)
	}
}
