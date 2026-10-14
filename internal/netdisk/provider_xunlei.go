package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"strings"
)

type xunleiProvider struct{ providerBase }

const xunleiAPI = "https://api-pan.xunlei.com/drive/v1"

func (p *xunleiProvider) headers(s *Session, src model.Source) map[string]string {
	h := requestHeaders(s, src)
	h["X-Client-Id"] = "Xqp0kJBXWhwaTpB6"
	h["X-Client-Version"] = "1.92.89"
	if b := bearer(s); b != "" {
		h["Authorization"] = b
	}
	h["X-Captcha-Token"] = sessionValue(s, "X-Captcha-Token", "captcha_token", "captchaToken")
	h["X-Device-Id"] = sessionValue(s, "X-Device-Id", "device_id", "deviceId")
	return h
}
func xunleiCheck(r object) error {
	if v, ok := r["error_code"]; ok && num(v) != 0 {
		return responseError(str(v))
	}
	if s := str(r["error"]); s != "" {
		return responseError(s)
	}
	if s := str(r["share_status"]); s != "" && s != "OK" && s != "SHARE_STATUS_OK" && s != "NORMAL" {
		return Fail("share_expired", "迅雷分享不可用或已过期")
	}
	return nil
}
func (p *xunleiProvider) call(ctx context.Context, c *APIClient, s *Session, src model.Source, method, path string, body any) (object, error) {
	r, e := apiJSON(ctx, c, method, xunleiAPI+path, body, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = xunleiCheck(r); e != nil {
		return nil, e
	}
	if d := obj(r["data"]); d != nil {
		r = d
		if e = xunleiCheck(r); e != nil {
			return nil, e
		}
	}
	return r, nil
}
func (p *xunleiProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	q := map[string]string{"parent_id": src.ParentID, "page_token": cursor, "limit": "100", "space": "", "with": "url", "filters": `{"trashed":{"eq":false}}`}
	path := "/files"
	if src.ShareID != "" {
		q["share_id"], q["pass_code"], q["pass_code_token"], q["scene"], q["thumbnail_size"] = src.ShareID, src.Password, sessionValue(s, "pass_code_token", "passCodeToken"), "NORMAL", "SIZE_MEDIUM"
		path = "/share"
		if src.ParentID != "" {
			path = "/share/detail"
		}
	} else if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	u := query(xunleiAPI+path, values(q))
	r, e := apiJSON(ctx, c, "GET", u, nil, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = xunleiCheck(r); e != nil {
		return nil, e
	}
	if d := obj(r["data"]); d != nil {
		r = d
		if e = xunleiCheck(r); e != nil {
			return nil, e
		}
	}
	if _, ok := r["files"].([]any); !ok {
		return nil, Fail("invalid_response", "迅雷网盘缺少文件列表")
	}
	remember(s, "pass_code_token", first(r, "pass_code_token"))
	if t := str(at(r, "share_info", "pass_code_token")); t != "" {
		remember(s, "pass_code_token", t)
	}
	l := &model.Listing{Source: src, Files: []model.File{}, NextPage: str(r["next_page_token"])}
	for _, v := range items(r["files"]) {
		m := obj(v)
		f := model.File{ID: str(m["id"]), Name: str(m["name"]), Size: num(m["size"]), Hash: str(m["hash"]), IsDir: str(m["kind"]) == "drive#folder", Meta: map[string]string{"parent_id": src.ParentID, "space": str(m["space"])}}
		if e = validateFile(f); e != nil {
			return nil, e
		}
		l.Files = append(l.Files, f)
	}
	return l, nil
}
func (p *xunleiProvider) passToken(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	if t := sessionValue(s, "pass_code_token", "passCodeToken"); t != "" {
		return t, nil
	}
	sub := src
	sub.ParentID = ""
	if _, e := p.List(ctx, c, s, sub, ""); e != nil {
		return "", e
	}
	return sessionValue(s, "pass_code_token", "passCodeToken"), nil
}
func (p *xunleiProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	path := "/files/" + url.PathEscape(f.ID)
	q := map[string]string{"_magic": "2021", "space": f.Meta["space"], "with": "url", "thumbnail_size": "SIZE_LARGE", "usage": "CONSUME"}
	if src.ShareID != "" {
		tok, e := p.passToken(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		parent := f.Meta["parent_id"]
		if parent == "" {
			parent = src.ParentID
		}
		path = "/share/file_info"
		q = map[string]string{"share_id": src.ShareID, "file_id": f.ID, "pass_code": src.Password, "pass_code_token": tok, "folder_id": parent, "space": "", "scene": "NORMAL"}
	} else if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	r, e := apiJSON(ctx, c, "GET", query(xunleiAPI+path, values(q)), nil, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = xunleiCheck(r); e != nil {
		return nil, e
	}
	if d := obj(r["data"]); d != nil {
		r = d
	}
	if d := obj(r["file_info"]); d != nil {
		r = d
	}
	if e = xunleiCheck(r); e != nil {
		return nil, e
	}
	if id := str(r["id"]); id != "" && id != f.ID {
		return nil, Fail("file_mismatch", "迅雷返回了另一文件的直链")
	}
	raw := str(r["web_content_link"])
	if raw == "" && src.ShareID != "" {
		if str(r["id"]) == "" && str(r["kind"]) == "" && str(r["name"]) == "" {
			return nil, Fail("invalid_response", "迅雷分享没有返回文件信息")
		}
		return nil, TransferRequired()
	}
	return download(c, s, f, raw, map[string]string{"User-Agent": browserUA}, nil)
}
func restoreMapping(r object, source string) string {
	containers := []object{r, obj(r["params"]), obj(r["result"]), obj(r["data"]), obj(r["task"])}
	if t := obj(r["task"]); t != nil {
		containers = append(containers, obj(t["params"]), obj(t["result"]))
	}
	for _, m := range containers {
		for _, k := range []string{"trace_file_ids", "restore_file_ids"} {
			v := m[k]
			if b, ok := v.(string); ok {
				var j any
				d := json.NewDecoder(strings.NewReader(b))
				d.UseNumber()
				if d.Decode(&j) == nil {
					v = j
				}
			}
			x := obj(v)[source]
			if a := items(x); len(a) > 0 {
				x = a[0]
			}
			if id := str(x); id != "" {
				return id
			}
		}
		if id := first(m, "restore_file_id", "file_id"); id != "" {
			return id
		}
	}
	return ""
}
func restoreStatus(r object) string {
	for _, v := range []object{r, obj(r["params"]), obj(r["task"]), obj(r["result"])} {
		if st := first(v, "restore_status", "phase_type", "task_status", "status", "phase"); st != "" {
			return strings.ToUpper(st)
		}
	}
	return ""
}
func (p *xunleiProvider) Transfer(ctx context.Context, c *APIClient, s *Session, src model.Source, files []model.File, batch string) (result *model.TransferReceipt, err error) {
	if e := transferInput(s, src, files, batch); e != nil {
		return nil, e
	}
	if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	tok, e := p.passToken(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	r, e := p.call(ctx, c, s, src, "POST", "/files", object{"kind": "drive#folder", "name": folderName(batch), "parent_id": "", "space": ""})
	if e != nil {
		return nil, e
	}
	folder := str(r["id"])
	if folder == "" {
		return nil, Fail("invalid_transfer", "迅雷未返回新目录身份")
	}
	result = receipt(p, s, src, batch, folder, []model.File{})
	for _, f := range files {
		r, e = p.call(ctx, c, s, src, "POST", "/share/restore", object{"parent_id": folder, "share_id": src.ShareID, "pass_code_token": tok, "ancestor_ids": []string{}, "file_ids": []string{f.ID}, "specify_parent_id": true})
		if e != nil {
			return result, e
		}
		id := restoreMapping(r, f.ID)
		task := first(r, "restore_task_id", "task_id")
		if task == "" {
			if str(r["kind"]) == "drive#task" {
				task = str(r["id"])
			}
		}
		for i := 0; i < 80; i++ {
			if id != "" {
				if e = recordCreated(result, f, id); e != nil {
					return result, e
				}
			}
			status := restoreStatus(r)
			if strings.Contains(status, "ERROR") || strings.Contains(status, "FAILED") {
				return result, Fail("transfer_failed", "迅雷转存失败，专用目录已保留")
			}
			if id != "" && (status == "" || strings.Contains(status, "COMPLETE")) {
				break
			}
			if task == "" {
				return result, Fail("invalid_transfer", "迅雷未返回可靠的转存文件映射")
			}
			if e = pollPause(ctx); e != nil {
				return result, e
			}
			r, e = p.call(ctx, c, s, src, "GET", "/tasks/"+url.PathEscape(task), nil)
			if e != nil {
				return result, e
			}
			if mapped := restoreMapping(r, f.ID); mapped != "" {
				if id != "" && id != mapped {
					_ = recordCreated(result, f, mapped)
					return result, Fail("invalid_transfer", "迅雷转存身份在轮询期间变化，临时目录已保留")
				}
				id = mapped
				if e = recordCreated(result, f, id); e != nil {
					return result, e
				}
			}
			if i == 79 {
				return result, Fail("transfer_timeout", "迅雷转存超时，专用目录已保留")
			}
		}
		if e = requireMapped(f, id); e != nil {
			return result, e
		}
		if e = recordCreated(result, f, id); e != nil {
			return result, e
		}
	}
	return result, nil
}
func (p *xunleiProvider) Cleanup(ctx context.Context, c *APIClient, s *Session, r *model.TransferReceipt) error {
	if e := ownedCleanup(ctx, p, c, s, r, ""); e != nil {
		return e
	}
	for _, ids := range [][]string{r.CreatedIDs, {r.FolderID}} {
		if len(ids) == 0 {
			continue
		}
		v, e := p.call(ctx, c, s, r.Source, "POST", "/files:batchDelete", object{"ids": ids, "space": ""})
		if e != nil {
			return e
		}
		if task := str(v["task_id"]); task != "" {
			done := false
			for i := 0; i < 80; i++ {
				st, e := p.call(ctx, c, s, r.Source, "GET", "/tasks/"+url.PathEscape(task), nil)
				if e != nil {
					return e
				}
				status := restoreStatus(st)
				if strings.Contains(status, "ERROR") {
					return Fail("cleanup_failed", "迅雷清理任务失败")
				}
				if strings.Contains(status, "COMPLETE") {
					done = true
					break
				}
				if e = pollPause(ctx); e != nil {
					return e
				}
			}
			if !done {
				return Fail("cleanup_timeout", "迅雷清理任务超时")
			}
		}
	}
	return nil
}
