package netdisk

import (
	"context"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestAdaptersBaiduShareSignatureAndPathRecovery(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := adapterSession("baidu")
	s.Capture.Tokens = map[string]string{}
	s.Capture.Share = map[string]string{"bdstoken": "browser-csrf"}
	src := model.Source{Provider: "baidu", URL: "https://pan.baidu.com/s/1share", ShareID: "1share", ParentID: "99", Password: "code"}
	paths := []string{}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/share/wxlist":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			dir := r.Form.Get("dir")
			paths = append(paths, dir)
			if dir == "" {
				return adapterJSON(`{"errno":0,"data":{"uk":123,"shareid":456,"seckey":"sekey","list":[{"fs_id":99,"server_filename":"folder","isdir":1,"path":"/folder"}]}}`), nil
			}
			if dir != "/folder" {
				t.Fatal(r.Form)
			}
			return adapterJSON(`{"errno":0,"data":{"uk":123,"shareid":456,"seckey":"sekey","list":[{"fs_id":100,"server_filename":"file","size":8,"path":"/folder/file"}]}}`), nil
		case "/share/tplconfig":
			if r.URL.Query().Get("surl") != "1share" || r.URL.Query().Get("bdstoken") != "browser-csrf" {
				t.Fatal(r.URL)
			}
			return adapterJSON(`{"errno":0,"data":{"sign":"signature","timestamp":123456}}`), nil
		case "/api/sharedownload":
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			if r.URL.Query().Get("sign") != "signature" || r.URL.Query().Get("timestamp") != "123456" || r.Form.Get("fid_list") != "[100]" || r.Form.Get("primaryid") != "456" || r.Form.Get("extra") != `{"sekey":"sekey"}` {
				t.Fatal(r.URL, r.Form)
			}
			return adapterJSON(`{"errno":0,"list":[{"fs_id":42,"dlink":"https://d.pcs.baidu.com/wrong"},{"fs_id":100,"dlink":"https://d.pcs.baidu.com/right"}]}`), nil
		}
		t.Fatal(r.URL)
		return nil, nil
	})
	l, e := p.List(context.Background(), c, s, src, "")
	if e != nil || len(l.Files) != 1 || l.Files[0].ID != "100" || !reflect.DeepEqual(paths, []string{"", "/folder"}) {
		t.Fatal(l, e, paths)
	}
	d, e := p.Resolve(context.Background(), c, s, src, l.Files[0])
	if e != nil || d.URL != "https://d.pcs.baidu.com/right" {
		t.Fatal(d, e)
	}
}

func TestAdaptersBaiduPersonalDirectoryIDAndDownloadAuth(t *testing.T) {
	p := adapterProvider(t, "baidu")
	s := adapterSession("baidu")
	src := model.Source{URL: "https://pan.baidu.com/disk/main", ParentID: "99"}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/rest/2.0/xpan/multimedia" {
			if r.URL.Query().Get("dlink") == "1" {
				return adapterJSON(`{"errno":0,"list":[{"fs_id":100,"dlink":"https://d.pcs.baidu.com/file"}]}`), nil
			}
			if r.URL.Query().Get("fsids") != "[99]" {
				t.Fatal(r.URL)
			}
			return adapterJSON(`{"errno":0,"list":[{"fs_id":99,"isdir":1,"path":"/folder"}]}`), nil
		}
		if r.URL.Path != "/rest/2.0/xpan/file" || r.URL.Query().Get("dir") != "/folder" {
			t.Fatal(r.URL)
		}
		return adapterJSON(`{"errno":0,"list":[{"fs_id":100,"server_filename":"file","path":"/folder/file"}]}`), nil
	})
	l, e := p.List(context.Background(), c, s, src, "")
	if e != nil {
		t.Fatal(e)
	}
	d, e := p.Resolve(context.Background(), c, s, src, l.Files[0])
	if e != nil || d.URL != "https://d.pcs.baidu.com/file?access_token=token" {
		t.Fatal(d, e)
	}
}

func TestAdaptersInvalidListsNeverFakeEmptySuccess(t *testing.T) {
	fixtures := map[string]string{"baidu": `{"errno":0,"list":null}`, "aliyun": `{"items":"bad"}`, "mcloud": `{"success":true,"data":{"items":{}}}`, "tcloud": `{"res_code":0,"fileListAO":{}}`, "xunlei": `{"files":null}`, "quark": `{"code":0,"data":{"list":"bad"}}`, "uc": `{"code":0,"data":{"list":{}}}`, "123pan": `{"code":0,"data":{"InfoList":"bad"}}`, "guangya": `{"code":0,"data":{"list":null}}`}
	for id, raw := range fixtures {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			c := adapterClient(p, s, func(*http.Request) (*http.Response, error) { return adapterJSON(raw), nil })
			l, e := p.List(context.Background(), c, s, model.Source{URL: "https://" + p.Info().Hosts[0] + "/drive"}, "")
			if l != nil {
				t.Fatal("invalid list accepted", l)
			}
			adapterError(t, e, "invalid_response")
		})
	}
}

func TestAdaptersPendingTransferFailureStillTracksIDs(t *testing.T) {
	for _, id := range []string{"quark", "uc", "xunlei"} {
		t.Run(id, func(t *testing.T) {
			p := adapterProvider(t, id)
			s := adapterSession(id)
			s.Capture.Share = map[string]string{"stoken": "s", "share_fid_token.a": "s", "pass_code_token": "s"}
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/1/clouddrive/file":
					return adapterJSON(`{"code":0,"data":{"fid":"folder"}}`), nil
				case "/1/clouddrive/share/sharepage/save":
					return adapterJSON(`{"code":0,"data":{"task_id":"task"}}`), nil
				case "/1/clouddrive/task":
					return adapterJSON(`{"code":0,"data":{"status":-1,"save_as":{"save_as_top_fids":["copy"]}}}`), nil
				case "/drive/v1/files":
					return adapterJSON(`{"id":"folder"}`), nil
				case "/drive/v1/share/restore":
					return adapterJSON(`{"restore_status":"RESTORE_ERROR","trace_file_ids":{"a":["copy"]}}`), nil
				}
				t.Fatal(r.URL)
				return nil, nil
			})
			r, e := p.Transfer(context.Background(), c, s, model.Source{URL: "https://" + p.Info().Hosts[0] + "/s/s", ShareID: "s"}, []model.File{{ID: "a", Name: "a"}}, "batch")
			adapterError(t, e, "transfer_failed")
			if r == nil || !reflect.DeepEqual(r.CreatedIDs, []string{"copy"}) {
				t.Fatal("created copy lost on task failure", r)
			}
		})
	}
}

func TestAdaptersEmptyFailedFolderCleanup(t *testing.T) {
	p := adapterProvider(t, "quark")
	s := adapterSession("quark")
	r := receipt(p, s, model.Source{URL: "https://pan.quark.cn/s/history", ShareID: "history"}, "batch", "folder", []model.File{})
	deleted := []string{}
	c := adapterClient(p, s, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/1/clouddrive/file/sort":
			if req.URL.Query().Get("pdir_fid") == "0" {
				return adapterJSON(`{"code":0,"data":{"list":[{"fid":"folder","file_name":"__gopeed_batch","file":false}]},"metadata":{"_total":1}}`), nil
			}
			return adapterJSON(`{"code":0,"data":{"list":[]},"metadata":{"_total":0}}`), nil
		case "/1/clouddrive/file/delete":
			b := adapterBody(t, req)
			a := items(b["filelist"])
			if len(a) == 0 {
				t.Fatal("empty deletion scope")
			}
			for _, v := range a {
				deleted = append(deleted, str(v))
			}
			return adapterJSON(`{"code":0,"data":{"task_id":"delete"}}`), nil
		case "/1/clouddrive/task":
			return adapterJSON(`{"code":0,"data":{"status":2}}`), nil
		}
		t.Fatal(req.URL)
		return nil, nil
	})
	if e := p.Cleanup(context.Background(), c, s, r); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(deleted, []string{"folder"}) {
		t.Fatal(deleted)
	}
}

func TestAdaptersCookieRedirectNeverLeavesAPIWhitelist(t *testing.T) {
	p := adapterProvider(t, "quark")
	s := adapterSession("quark")
	calls := 0
	c := adapterClient(p, s, func(*http.Request) (*http.Response, error) {
		calls++
		r := adapterJSON(`{}`)
		r.StatusCode = 302
		r.Header.Set("Location", "https://drive-pc.quark.cn.evil.example/steal")
		return r, nil
	})
	_, e := c.Do(context.Background(), "GET", "https://drive-pc.quark.cn/test", nil, map[string]string{"Authorization": "secret"})
	if e == nil || calls != 1 {
		t.Fatal("redirect escaped domain boundary", e, calls)
	}
}

func TestAdaptersPublicDownloadMetaDropsSecrets(t *testing.T) {
	f := model.File{ID: "a", Name: "a", Meta: map[string]string{"downloadUrl": "https://secret.example/token", "share_fid_token": "opaque", "sharefid": "opaque", "Authorization": "Bearer secret", "drive_id": "drive", "parent_id": "p"}}
	d, e := download(nil, nil, f, "https://cdn.example/a", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	for k, v := range d.File.Meta {
		if strings.Contains(v, "secret") || strings.Contains(v, "opaque") || strings.Contains(k, "token") {
			t.Fatal(d.File.Meta)
		}
	}
	if d.File.Meta["drive_id"] != "drive" {
		t.Fatal(d.File)
	}
}
