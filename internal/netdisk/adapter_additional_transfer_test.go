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

func TestAdaptersGuangyaOfficialShareCursor(t *testing.T) {
	p := adapterProvider(t, "guangya")
	s := adapterSession("guangya")
	src := model.Source{Provider: "guangya", URL: "https://www.guangyapan.com/s/share", ShareID: "share", ParentID: "parent", Password: "code"}
	calls := []string{}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "api.guangyapan.com" || r.URL.RawQuery != "" {
			t.Fatal("unexpected Guangya request", r.Method, r.URL)
		}
		calls = append(calls, r.URL.Path)
		b := adapterBody(t, r)
		switch r.URL.Path {
		case "/nd.bizuserres.s/v1/get_share_access_token":
			if b["shareId"] != "share" || b["code"] != "code" {
				t.Fatal(b)
			}
			return adapterJSON(`{"code":0,"data":{"accessToken":"private-share"}}`), nil
		case "/userres/v1/get_share_page_files_list":
			if b["accessToken"] != "private-share" || b["parentId"] != "parent" || b["cursor"] != "cursor-in" || str(b["pageSize"]) != "100" || r.Header.Get("Did") != "device" {
				t.Fatal(b, r.Header)
			}
			return adapterJSON(`{"code":0,"data":{"list":[{"fileId":"own-shared","fileName":"f","fileSize":12,"resType":1},{"fileId":"dir","fileName":"directory","resType":2}],"hasMore":true,"cursor":"cursor-out"}}`), nil
		}
		t.Fatal(r.URL)
		return nil, nil
	})
	l, e := p.List(context.Background(), c, s, src, "cursor-in")
	if e != nil || l.NextPage != "cursor-out" || len(l.Files) != 2 || !l.Files[1].IsDir {
		t.Fatal(l, e)
	}
	b, _ := json.Marshal(l)
	if strings.Contains(string(b), "private-share") {
		t.Fatal("token persisted")
	}
	if !reflect.DeepEqual(calls, []string{"/nd.bizuserres.s/v1/get_share_access_token", "/userres/v1/get_share_page_files_list"}) {
		t.Fatal(calls)
	}
}

func TestAdaptersBaiduTransferNewIDsAndOwnedPaths(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := adapterSession("baidu")
	s.Capture.Share = map[string]string{"bdstoken": "csrf", "uk": "11", "shareid": "22", "seckey": "key"}
	src := model.Source{Provider: "baidu", URL: "https://pan.baidu.com/s/1share", ShareID: "1share"}
	deleted := []string{}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/share/transfer" {
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			if r.URL.Query().Get("shareid") != "22" || r.URL.Query().Get("from") != "11" || r.URL.Query().Get("bdstoken") != "csrf" || r.Form.Get("fsidlist") != "[100]" || r.Form.Get("path") != "/__gopeed_batch" {
				t.Fatal(r.URL, r.Form)
			}
			return adapterJSON(`{"errno":0,"extra":{"list":[{"from_fs_id":100,"to_fs_id":900,"to":"/__gopeed_batch/file"}]}}`), nil
		}
		if r.URL.Path != "/rest/2.0/xpan/file" {
			t.Fatal(r.URL)
		}
		switch r.URL.Query().Get("method") {
		case "create":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			if r.Form.Get("path") != "/__gopeed_batch" || r.Form.Get("isdir") != "1" {
				t.Fatal(r.Form)
			}
			return adapterJSON(`{"errno":0,"fs_id":1000,"path":"/__gopeed_batch"}`), nil
		case "list":
			if r.URL.Query().Get("dir") == "/" {
				return adapterJSON(`{"errno":0,"list":[{"fs_id":1000,"server_filename":"__gopeed_batch","path":"/__gopeed_batch","isdir":1}]}`), nil
			}
			return adapterJSON(`{"errno":0,"list":[{"fs_id":900,"server_filename":"file","path":"/__gopeed_batch/file","size":5}]}`), nil
		case "filemanager":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			var paths []string
			if e := json.Unmarshal([]byte(r.Form.Get("filelist")), &paths); e != nil {
				t.Fatal(e)
			}
			deleted = append(deleted, paths...)
			return adapterJSON(`{"errno":0,"info":[{"errno":0}]}`), nil
		}
		t.Fatal(r.URL)
		return nil, nil
	})
	r, e := p.Transfer(context.Background(), c, s, src, []model.File{{ID: "100", Name: "file", Size: 5, Path: "nested"}}, "batch")
	if e != nil || r.FolderID != "1000" || !reflect.DeepEqual(r.CreatedIDs, []string{"900"}) || r.Files[0].Path != "nested" {
		t.Fatal(r, e)
	}
	if e = p.Cleanup(context.Background(), c, s, r); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(deleted, []string{"/__gopeed_batch/file", "/__gopeed_batch"}) {
		t.Fatal(deleted)
	}
}

func TestAdaptersBaiduPartialTransferReceipt(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := adapterSession("baidu")
	s.Capture.Share = map[string]string{"bdstoken": "csrf", "uk": "1", "shareid": "2"}
	n := 0
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("method") == "create" {
			return adapterJSON(`{"errno":0,"fs_id":1000,"path":"/__gopeed_batch"}`), nil
		}
		n++
		if n == 2 {
			return adapterJSON(`{"errno":9019}`), nil
		}
		return adapterJSON(`{"errno":0,"extra":{"list":[{"from_fs_id":100,"to_fs_id":900,"to":"/__gopeed_batch/a"}]}}`), nil
	})
	r, e := p.Transfer(context.Background(), c, s, model.Source{URL: "https://pan.baidu.com/s/1s", ShareID: "1s"}, []model.File{{ID: "100", Name: "a", Path: "one"}, {ID: "101", Name: "b", Path: "two"}}, "batch")
	adapterError(t, e, "expired")
	if r == nil || r.FolderID != "1000" || !reflect.DeepEqual(r.CreatedIDs, []string{"900"}) || r.Files[0].ID != "900" {
		t.Fatal(r)
	}
}

func TestAdaptersTcloudGuangyaSingleTaskCopyMappingAndCleanup(t *testing.T) {
	for _, id := range []string{"tcloud", "guangya"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share["share_access_token"] = "share-token"
			src := model.Source{Provider: id, URL: "https://" + p.Info().Hosts[0] + "/s/share", ShareID: "share"}
			copied := false
			deleted := []string{}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				if id == "guangya" && (r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "api.guangyapan.com" || r.URL.RawQuery != "") {
					t.Fatal("unexpected Guangya request", r.Method, r.URL)
				}
				switch r.URL.Path {
				case "/api/open/share/getShareInfoByCode.action":
					return adapterJSON(`{"res_code":0,"shareId":10,"fileId":12,"isFolder":true}`), nil
				case "/api/open/file/createFolder.action":
					if e := r.ParseForm(); e != nil {
						t.Fatal(e)
					}
					if r.Form.Get("folderName") != "__gopeed_batch" || r.Form.Get("parentFolderId") != "-11" {
						t.Fatal(r.Form)
					}
					return adapterJSON(`{"res_code":0,"id":1000}`), nil
				case "/api/open/file/listFiles.action":
					if r.URL.Query().Get("folderId") == "-11" {
						return adapterJSON(`{"res_code":0,"fileListAO":{"folderList":[{"id":1000,"name":"__gopeed_batch"}],"fileList":[],"count":1}}`), nil
					}
					if copied {
						return adapterJSON(`{"res_code":0,"fileListAO":{"folderList":[],"fileList":[{"id":900,"name":"f","size":5,"md5":"hash"}],"count":1}}`), nil
					}
					return adapterJSON(`{"res_code":0,"fileListAO":{"folderList":[],"fileList":[],"count":0}}`), nil
				case "/api/open/batch/createBatchTask.action":
					if e := r.ParseForm(); e != nil {
						t.Fatal(e)
					}
					var tasks []object
					if e := json.Unmarshal([]byte(r.Form.Get("taskInfos")), &tasks); e != nil {
						t.Fatal(e)
					}
					if r.Form.Get("type") == "SHARE_SAVE" {
						if r.Form.Get("targetFolderId") != "1000" || r.Form.Get("shareId") != "10" || str(tasks[0]["fileId"]) != "100" {
							t.Fatal(r.Form)
						}
						copied = true
					} else {
						for _, f := range tasks {
							deleted = append(deleted, str(f["fileId"]))
						}
					}
					return adapterJSON(`{"res_code":0,"taskId":"task"}`), nil
				case "/api/open/batch/checkBatchTask.action":
					return adapterJSON(`{"res_code":0,"taskStatus":4,"failedCount":0}`), nil
				case "/nd.bizuserres.s/v1/file/create_dir":
					b := adapterBody(t, r)
					if b["dirName"] != "__gopeed_batch" || !yes(b["failIfNameExist"]) {
						t.Fatal(b)
					}
					return adapterJSON(`{"code":0,"data":{"fileId":"1000"}}`), nil
				case "/userres/v1/file/get_file_list":
					b := adapterBody(t, r)
					if b["parentId"] == "" {
						return adapterJSON(`{"code":0,"data":{"list":[{"fileId":"1000","fileName":"__gopeed_batch","resType":2}],"total":1}}`), nil
					}
					if copied {
						return adapterJSON(`{"code":0,"data":{"list":[{"fileId":"900","fileName":"f","fileSize":5,"resType":1,"gcid":"hash"}],"total":1}}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"list":[],"total":0}}`), nil
				case "/userres/v1/restore_share":
					b := adapterBody(t, r)
					if b["parentId"] != "1000" || b["accessToken"] != "share-token" || str(items(b["fileIds"])[0]) != "100" {
						t.Fatal(b)
					}
					copied = true
					return adapterJSON(`{"code":0,"data":{"taskId":"task"}}`), nil
				case "/nd.bizuserres.s/v1/get_task_status":
					return adapterJSON(`{"code":0,"data":{"status":2,"detail":{"code":0}}}`), nil
				case "/nd.bizuserres.s/v1/file/delete_file":
					b := adapterBody(t, r)
					for _, x := range items(b["fileIds"]) {
						deleted = append(deleted, str(x))
					}
					return adapterJSON(`{"code":0,"data":{"taskId":"task"}}`), nil
				}
				t.Fatal(r.URL)
				return nil, nil
			})
			r, e := p.Transfer(context.Background(), c, s, src, []model.File{{ID: "100", Name: "f", Size: 5, Hash: "hash", Path: "nested"}}, "batch")
			if e != nil || r.FolderID != "1000" || r.Files[0].ID != "900" || r.Files[0].Path != "nested" {
				t.Fatal(r, e)
			}
			if e = p.Cleanup(context.Background(), c, s, r); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(deleted, []string{"900", "1000"}) {
				t.Fatal("wrong cleanup IDs", deleted)
			}
		})
	}
}

func TestAdaptersCopyDeltaRejectsForeignOrChangedFile(t *testing.T) {
	src := model.File{ID: "source", Name: "f", Size: 5, Hash: "hash"}
	for _, fs := range [][]model.File{{{ID: "one", Name: "f", Size: 5}, {ID: "foreign", Name: "f", Size: 5}}, {{ID: "one", Name: "f", Size: 6}}, {{ID: "one", Name: "different", Size: 5}}, {{ID: "one", Name: "f", Size: 5, Hash: "wrong"}}} {
		if _, e := copyDelta(nil, fs, src); e == nil {
			t.Fatal("accepted ambiguous creation", fs)
		}
	}
}

func TestAdaptersTcloudGuangyaPartialTaskFailure(t *testing.T) {
	for _, id := range []string{"tcloud", "guangya"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share["share_access_token"] = "share-token"
			tasks := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/open/share/getShareInfoByCode.action":
					return adapterJSON(`{"res_code":0,"shareId":10,"fileId":12,"isFolder":true}`), nil
				case "/api/open/file/createFolder.action":
					return adapterJSON(`{"res_code":0,"id":1000}`), nil
				case "/nd.bizuserres.s/v1/file/create_dir":
					return adapterJSON(`{"code":0,"data":{"fileId":"1000"}}`), nil
				case "/api/open/file/listFiles.action":
					if tasks > 0 {
						return adapterJSON(`{"res_code":0,"fileListAO":{"folderList":[],"fileList":[{"id":900,"name":"f","size":5}],"count":1}}`), nil
					}
					return adapterJSON(`{"res_code":0,"fileListAO":{"folderList":[],"fileList":[],"count":0}}`), nil
				case "/userres/v1/file/get_file_list":
					if tasks > 0 {
						return adapterJSON(`{"code":0,"data":{"list":[{"fileId":"900","fileName":"f","fileSize":5,"resType":1}],"total":1}}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"list":[],"total":0}}`), nil
				case "/api/open/batch/createBatchTask.action", "/userres/v1/restore_share":
					tasks++
					if id == "tcloud" {
						return adapterJSON(`{"res_code":0,"taskId":"task"}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"taskId":"task"}}`), nil
				case "/api/open/batch/checkBatchTask.action":
					if tasks == 2 {
						return adapterJSON(`{"res_code":0,"taskStatus":4,"failedCount":1}`), nil
					}
					return adapterJSON(`{"res_code":0,"taskStatus":4}`), nil
				case "/nd.bizuserres.s/v1/get_task_status":
					if tasks == 2 {
						return adapterJSON(`{"code":0,"data":{"status":3}}`), nil
					}
					return adapterJSON(`{"code":0,"data":{"status":2}}`), nil
				}
				t.Fatal(r.URL)
				return nil, nil
			})
			r, e := p.Transfer(context.Background(), c, s, model.Source{Provider: id, ShareID: "share"}, []model.File{{ID: "100", Name: "f", Size: 5, Path: "nested"}, {ID: "101", Name: "other"}}, "batch")
			adapterError(t, e, "transfer_failed")
			if r == nil || r.FolderID != "1000" || !reflect.DeepEqual(r.CreatedIDs, []string{"900"}) || r.Files[0].Path != "nested" {
				t.Fatal("lost actual first copy on second task failure", r)
			}
		})
	}
}
