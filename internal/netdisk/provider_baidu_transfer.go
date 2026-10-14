package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"path"
	"strings"
)

func (p *baiduProvider) csrf(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	if t := sessionValue(s, "bdstoken"); t != "" {
		return t, nil
	}
	r, e := apiJSON(ctx, c, "GET", "https://pan.baidu.com/api/gettemplatevariable?fields=%5B%22bdstoken%22%5D", nil, p.headers(s, src))
	if e != nil {
		return "", e
	}
	if e = checkResponse(r, "errno", "0"); e != nil {
		return "", e
	}
	t := str(at(r, "result", "bdstoken"))
	if t == "" {
		return "", LoginRequired(p.Info().Name)
	}
	remember(s, "bdstoken", t)
	return t, nil
}
func (p *baiduProvider) task(ctx context.Context, c *APIClient, s *Session, src model.Source, id string, transfer bool) (object, error) {
	q := map[string]string{"taskid": id, "app_id": "250528", "clienttype": "0", "web": "1"}
	if transfer {
		q["shareid"], q["from"], q["bdstoken"] = sessionValue(s, "shareid", "share_id"), sessionValue(s, "uk"), sessionValue(s, "bdstoken")
	}
	var last object
	for i := 0; i < 120; i++ {
		r, e := apiJSON(ctx, c, "GET", query("https://pan.baidu.com/share/taskquery", values(q)), nil, p.headers(s, src))
		if e != nil {
			return last, e
		}
		last = r
		if code, ok := r["errno"]; ok && num(code) != 0 {
			return r, responseError(str(code))
		}
		if code := str(r["task_errno"]); code != "" && code != "0" {
			return r, responseError(code)
		}
		switch str(r["status"]) {
		case "success":
			return r, nil
		case "running", "pending":
		case "failed":
			return r, Fail("transfer_failed", "百度异步任务失败，已保留临时目录")
		default:
			return r, Fail("invalid_response", "百度返回了未知异步任务状态")
		}
		if e = pollPause(ctx); e != nil {
			return last, e
		}
	}
	return last, Fail("transfer_timeout", "百度异步任务超时，临时目录已保留")
}
func baiduCreatedMappings(r object) []any {
	if a := items(at(r, "extra", "list")); len(a) > 0 {
		return a
	}
	return items(r["list"])
}
func recordBaiduCopies(result *model.TransferReceipt, f model.File, r object, dest string) error {
	a := baiduCreatedMappings(r)
	found := false
	for _, v := range a {
		from := str(at(v, "from_fs_id"))
		if from != "" && from != f.ID {
			continue
		}
		if from == "" && len(a) != 1 {
			continue
		}
		if from == "" && f.Meta["remote_path"] != "" && str(at(v, "from")) != "" && str(at(v, "from")) != f.Meta["remote_path"] {
			continue
		}
		id := str(at(v, "to_fs_id"))
		if id == "" {
			continue
		}
		to := str(at(v, "to"))
		if path.Dir(to) != dest {
			return Fail("ownership_error", "百度转存文件不在新建专用目录内")
		}
		if e := recordCreated(result, f, id); e != nil {
			return e
		}
		for i := range result.Files {
			if result.Files[i].ID == id {
				result.Files[i].Meta["remote_path"] = to
			}
		}
		found = true
	}
	if !found {
		return Fail("invalid_transfer", "百度转存未返回所选文件的实际 to_fs_id，专用目录已保留")
	}
	n := 0
	for _, v := range result.Files {
		if v.Meta["source_id"] == fSourceID(f) {
			n++
		}
	}
	if n != 1 {
		return Fail("invalid_transfer", "百度单文件任务返回多个副本身份，已保留创建记录")
	}
	return nil
}
func (p *baiduProvider) Transfer(ctx context.Context, c *APIClient, s *Session, src model.Source, files []model.File, batch string) (result *model.TransferReceipt, err error) {
	if e := transferInput(s, src, files, batch); e != nil {
		return nil, e
	}
	token := sessionValue(s, "access_token", "accessToken", "baidu_access_token")
	if token == "" && !baiduCookieAuth(c) {
		return nil, LoginRequired(p.Info().Name)
	}
	csrf, e := p.csrf(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	if sessionValue(s, "uk") == "" || sessionValue(s, "shareid", "share_id") == "" {
		root := src
		root.ParentID = ""
		if _, e = p.List(ctx, c, s, root, ""); e != nil {
			return nil, e
		}
	}
	dest := "/" + folderName(batch)
	createURL := query("https://pan.baidu.com/rest/2.0/xpan/file", values(map[string]string{"method": "create", "access_token": token}))
	body := values(map[string]string{"path": dest, "size": "0", "isdir": "1", "rtype": "1"})
	if token == "" {
		createURL = query("https://pan.baidu.com/api/create", values(map[string]string{"a": "commit", "clienttype": "0", "app_id": "250528", "web": "1", "bdstoken": csrf}))
		body.Set("block_list", "[]")
	}
	r, e := apiForm(ctx, c, createURL, body, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = checkResponse(r, "errno", "0"); e != nil {
		return nil, e
	}
	folder := str(r["fs_id"])
	if folder == "" {
		return nil, Fail("invalid_transfer", "百度没有返回新建目录 fs_id")
	}
	result = receipt(p, s, src, batch, folder, []model.File{})
	if actual := str(r["path"]); actual != dest {
		return result, Fail("ownership_error", "百度返回的新目录路径与本批次不一致，目录已保留")
	}
	remember(s, "baidu_path."+folder, dest)
	for _, f := range files {
		ids, e := json.Marshal([]json.Number{json.Number(f.ID)})
		if e != nil {
			return result, Fail("invalid_file", "百度源文件身份无效")
		}
		q := map[string]string{"shareid": sessionValue(s, "shareid", "share_id"), "from": sessionValue(s, "uk"), "bdstoken": csrf, "ondup": "fail", "async": "0", "channel": "chunlei", "web": "1", "app_id": "250528", "clienttype": "0", "sekey": sessionValue(s, "seckey", "sekey")}
		r, e = apiForm(ctx, c, query("https://pan.baidu.com/share/transfer", values(q)), values(map[string]string{"fsidlist": string(ids), "path": dest}), p.headers(s, src))
		if e != nil {
			return result, e
		}
		mappingErr := recordBaiduCopies(result, f, r, dest)
		if e = checkResponse(r, "errno", "0"); e != nil {
			return result, e
		}
		if id := first(r, "taskid", "task_id"); id != "" && id != "0" {
			done, taskErr := p.task(ctx, c, s, src, id, true)
			if len(baiduCreatedMappings(done)) > 0 {
				mappingErr = recordBaiduCopies(result, f, done, dest)
			}
			if taskErr != nil {
				return result, taskErr
			}
		}
		if mappingErr != nil {
			return result, mappingErr
		}
	}
	return result, nil
}
func (p *baiduProvider) Cleanup(ctx context.Context, c *APIClient, s *Session, r *model.TransferReceipt) error {
	if e := ownedCleanup(ctx, p, c, s, r, "/"); e != nil {
		return e
	}
	token := sessionValue(s, "access_token", "accessToken", "baidu_access_token")
	if token == "" && !baiduCookieAuth(c) {
		return LoginRequired(p.Info().Name)
	}
	dest := "/" + folderName(r.BatchID)
	src := r.Source
	src.ShareID = ""
	src.ParentID = dest
	csrf := ""
	if token == "" {
		var e error
		csrf, e = p.csrf(ctx, c, s, src)
		if e != nil {
			return e
		}
	}
	paths := []string{}
	cur := ""
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, id := range r.CreatedIDs {
		ids[id] = true
	}
	for {
		if seen[cur] {
			return Fail("invalid_pagination", "百度清理分页重复")
		}
		seen[cur] = true
		l, e := p.List(ctx, c, s, src, cur)
		if e != nil {
			return e
		}
		for _, f := range l.Files {
			remote := f.Meta["remote_path"]
			if !ids[f.ID] || f.IsDir || path.Dir(remote) != dest || strings.ContainsRune(remote, 0) {
				return Fail("ownership_error", "百度专用目录身份或路径发生变化")
			}
			paths = append(paths, remote)
		}
		cur = l.NextPage
		if cur == "" {
			break
		}
	}
	for _, group := range [][]string{paths, {dest}} {
		if len(group) == 0 {
			continue
		}
		b, _ := json.Marshal(group)
		deleteURL := query("https://pan.baidu.com/rest/2.0/xpan/file", values(map[string]string{"method": "filemanager", "opera": "delete", "access_token": token}))
		body := url.Values{"async": {"0"}, "filelist": {string(b)}, "ondup": {"fail"}}
		if token == "" {
			deleteURL = query("https://pan.baidu.com/api/filemanager", values(map[string]string{"opera": "delete", "async": "2", "onnest": "fail", "bdstoken": csrf, "newVerify": "1", "clienttype": "0", "app_id": "250528", "web": "1"}))
			body = url.Values{"filelist": {string(b)}}
		}
		v, e := apiForm(ctx, c, deleteURL, body, p.headers(s, src))
		if e != nil {
			return e
		}
		if e = checkResponse(v, "errno", "0"); e != nil {
			return e
		}
		for _, x := range items(v["info"]) {
			if n := num(at(x, "errno")); n != 0 {
				return responseError(str(at(x, "errno")))
			}
		}
		if task := first(v, "taskid", "task_id"); task != "" && task != "0" {
			if _, e = p.task(ctx, c, s, src, task, false); e != nil {
				return e
			}
		}
	}
	return nil
}
