package netdisk

import (
	"context"
	"errors"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"strconv"
	"time"
)

type quarkProvider struct {
	providerBase
	uc bool
}

func newQuarkProvider(uc bool) *quarkProvider {
	if uc {
		return &quarkProvider{providerBase{model.ProviderInfo{ID: "uc", Name: "UC网盘", Hosts: []string{"drive.uc.cn"}, APIHosts: []string{"pc-api.uc.cn", "drive.uc.cn"}}}, true}
	}
	return &quarkProvider{providerBase{model.ProviderInfo{ID: "quark", Name: "夸克网盘", Hosts: []string{"pan.quark.cn", "drive-social-api.quark.cn"}, APIHosts: []string{"drive-pc.quark.cn", "drive-h.quark.cn", "drive-social-api.quark.cn"}}}, false}
}
func (p *quarkProvider) endpoint(path string) string {
	host, pr, version := "drive-pc.quark.cn", "ucpro", "6.9.7.761"
	if p.uc {
		host, pr, version = "pc-api.uc.cn", "UCBrowser", "1.8.8"
	}
	return query("https://"+host+"/1/clouddrive"+path, values(map[string]string{"pr": pr, "fr": "pc", "sys": "win32", "ve": version}))
}
func (p *quarkProvider) headers(s *Session, src model.Source) map[string]string {
	h := requestHeaders(s, src)
	if p.uc {
		h["User-Agent"] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) uc-cloud-drive/1.8.8 Chrome/100.0.4896.160 Electron/18.3.5.16-b62cf9c50d Safari/537.36 Channel/ucpan_other_ch"
		h["Sec-Ch-Ua"] = `"Not=A?Brand";v="99", "Chromium";v="100", "Google Chrome";v="100"`
	} else {
		h["User-Agent"] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 QuarkPC/6.9.7.761 QuarkCloudDrivePC/6.9.7.761 quark-cloud-drive/2.5.40"
	}
	return h
}
func (p *quarkProvider) call(ctx context.Context, c *APIClient, s *Session, src model.Source, method, path string, body any) (object, error) {
	r, e := apiJSON(ctx, c, method, p.endpoint(path), body, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = checkResponse(r, "code", "0"); e != nil {
		return nil, e
	}
	return r, nil
}

// Social credentials stay in the operation's in-memory Capture. An expiry is
// required to reuse them, with the same 60-second margin as LinkSwift dev.
func (p *quarkProvider) socialToken(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	now := time.Now()
	token := sessionValue(s, "social_token", "socialToken")
	expiry := sessionValue(s, "social_token_expired_timestamp")
	if s != nil && s.Capture.Share["social_token"] != "" {
		token, expiry = s.Capture.Share["social_token"], s.Capture.Share["social_token_expired_timestamp"]
	}
	if timestamp, e := strconv.ParseInt(expiry, 10, 64); e == nil && token != "" && timestamp > now.Add(time.Minute).UnixMilli() {
		return token, nil
	}
	stamp := strconv.FormatInt(now.Unix(), 10)
	// Both fr values are present in the fixed upstream social endpoint.
	r, e := apiJSON(ctx, c, "POST", "https://drive-social-api.quark.cn/1/clouddrive/chat/conv/file/acquire_dl_token?pr=ucpro&fr=pc&sys=win32&ve=6.9.7.761&fr=win&la=zh-CN&ch=pckk%40product_guanwan", object{"conversation_id": "300000" + stamp, "conversation_type": 3, "msg_id": stamp + "000"}, p.headers(s, src))
	if e != nil {
		return "", e
	}
	if e = checkResponse(r, "code", "0"); e != nil {
		return "", e
	}
	token = str(at(r, "data", "token"))
	if token == "" {
		return "", Fail("invalid_response", "网盘未返回社交下载令牌")
	}
	expiry = str(at(r, "data", "expired_timestamp"))
	if timestamp, e := strconv.ParseInt(expiry, 10, 64); e == nil && timestamp <= time.Now().UnixMilli() {
		return "", Fail("expired", "网盘社交下载令牌已过期，请重新同步浏览器")
	}
	remember(s, "social_token", token)
	if s != nil {
		s.Capture.Share["social_token_expired_timestamp"] = expiry
	}
	return token, nil
}

func (p *quarkProvider) token(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	if t := sessionValue(s, "stoken"); t != "" {
		return t, nil
	}
	r, e := p.call(ctx, c, s, src, "POST", "/share/sharepage/token", object{"pwd_id": src.ShareID, "passcode": src.Password})
	if e != nil {
		return "", e
	}
	t := str(at(r, "data", "stoken"))
	if t == "" {
		return "", Fail("invalid_response", "分享未返回访问令牌")
	}
	remember(s, "stoken", t)
	return t, nil
}
func (p *quarkProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	n, e := pageNumber(cursor, 1)
	if e != nil {
		return nil, e
	}
	parent := src.ParentID
	if parent == "" {
		parent = "0"
	}
	q := map[string]string{"pdir_fid": parent, "_page": strconv.Itoa(n), "_size": "100", "_fetch_total": "1", "_sort": "file_type:asc,file_name:asc", "fetch_all_file": "1"}
	path := "/file/sort"
	if src.ShareID != "" {
		t, e := p.token(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		q["pwd_id"], q["stoken"], q["force"] = src.ShareID, t, "0"
		path = "/share/sharepage/detail"
	}
	r, e := apiJSON(ctx, c, "GET", query(p.endpoint(path), values(q)), nil, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = checkResponse(r, "code", "0"); e != nil {
		return nil, e
	}
	data := obj(r["data"])
	if _, ok := data["list"].([]any); !ok {
		return nil, Fail("invalid_response", "网盘未返回目录列表")
	}
	l := &model.Listing{Source: src, Files: []model.File{}}
	for _, v := range items(data["list"]) {
		m := obj(v)
		id := str(m["fid"])
		dir := str(m["file_type"]) == "0"
		if b, ok := m["file"].(bool); ok {
			dir = !b
		}
		f := model.File{ID: id, Name: str(m["file_name"]), Size: num(m["size"]), Hash: first(m, "md5", "sha1"), IsDir: dir, Meta: map[string]string{"parent_id": parent}}
		if e = validateFile(f); e != nil {
			return nil, e
		}
		if src.ShareID != "" {
			remember(s, "share_fid_token."+id, str(m["share_fid_token"]))
		}
		l.Files = append(l.Files, f)
	}
	total := num(at(r, "metadata", "_total"))
	if total == 0 {
		total = num(at(r, "metadata", "total"))
	}
	if int64(n*100) < total || total == 0 && len(l.Files) == 100 {
		l.NextPage = strconv.Itoa(n + 1)
	}
	return l, nil
}

// The opaque share-fid token is only held in Capture. Browser snapshots may
// carry it in Capture.Files; no provider result or receipt copies it to Meta.
func shareFID(s *Session, id string) string {
	if v := sessionValue(s, "share_fid_token."+id, "shareFidToken:"+id); v != "" {
		return v
	}
	if s != nil {
		for _, f := range s.Capture.Files {
			if f.ID == id {
				v := first(objectString(f.Meta), "share_fid_token", "shareFidToken")
				remember(s, "share_fid_token."+id, v)
				return v
			}
		}
	}
	return ""
}
func objectString(m map[string]string) object {
	o := object{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func (p *quarkProvider) fidToken(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (string, error) {
	if t := shareFID(s, f.ID); t != "" {
		return t, nil
	}
	sub := src
	if v := f.Meta["parent_id"]; v != "" {
		sub.ParentID = v
	}
	cur := ""
	seen := map[string]bool{}
	for {
		if seen[cur] {
			return "", Fail("invalid_pagination", "分享目录分页重复")
		}
		seen[cur] = true
		l, e := p.List(ctx, c, s, sub, cur)
		if e != nil {
			return "", e
		}
		if t := shareFID(s, f.ID); t != "" {
			return t, nil
		}
		cur = l.NextPage
		if cur == "" {
			return "", Fail("invalid_context", "分享文件令牌缺失，请在浏览器重新同步所选文件")
		}
	}
}
func (p *quarkProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	body := object{"fids": []string{f.ID}}
	if src.ShareID != "" {
		t, e := p.token(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		ft, e := p.fidToken(ctx, c, s, src, f)
		if e != nil {
			return nil, e
		}
		body["pwd_id"], body["stoken"], body["fids_token"] = src.ShareID, t, []string{ft}
	}
	if !p.uc {
		token, e := p.socialToken(ctx, c, s, src)
		if e != nil {
			var typed *Error
			if src.ShareID == "" || !errors.As(e, &typed) || typed.Code != "login_required" && typed.Code != "expired" {
				return nil, e
			}
		}
		body["speedup_session"], body["token"] = "", token
	}
	r, e := p.call(ctx, c, s, src, "POST", "/file/download", body)
	if e != nil {
		return nil, e
	}
	for _, v := range items(r["data"]) {
		m := obj(v)
		if str(m["fid"]) == f.ID {
			return download(c, s, f, str(m["download_url"]), p.headers(s, src), m["expire_at"])
		}
	}
	return nil, Fail("file_mismatch", "网盘未返回所选文件的下载链接")
}
func (p *quarkProvider) task(ctx context.Context, c *APIClient, s *Session, src model.Source, id string) (object, error) {
	if id == "" {
		return nil, Fail("invalid_response", "网盘未返回异步任务身份")
	}
	var last object
	for i := 0; i < 80; i++ {
		r, e := apiJSON(ctx, c, "GET", query(p.endpoint("/task"), values(map[string]string{"task_id": id, "retry_index": strconv.Itoa(i)})), nil, p.headers(s, src))
		if e != nil {
			return last, e
		}
		if e = checkResponse(r, "code", "0"); e != nil {
			return last, e
		}
		d := obj(r["data"])
		last = d
		switch str(d["status"]) {
		case "2":
			return d, nil
		case "0", "1":
		default:
			return d, Fail("transfer_failed", "网盘异步操作失败，临时副本已保留")
		}
		if e = pollPause(ctx); e != nil {
			return last, e
		}
	}
	return last, Fail("transfer_timeout", "网盘转存或清理超时，临时副本已保留")
}
func (p *quarkProvider) Transfer(ctx context.Context, c *APIClient, s *Session, src model.Source, files []model.File, batch string) (result *model.TransferReceipt, err error) {
	if e := transferInput(s, src, files, batch); e != nil {
		return nil, e
	}
	t, e := p.token(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	tokens := []string{}
	for _, f := range files {
		ft, e := p.fidToken(ctx, c, s, src, f)
		if e != nil {
			return nil, e
		}
		tokens = append(tokens, ft)
	}
	r, e := p.call(ctx, c, s, src, "POST", "/file", object{"file_name": folderName(batch), "pdir_fid": "0", "dir_init_lock": false, "dir_path": ""})
	if e != nil {
		return nil, e
	}
	folder := str(at(r, "data", "fid"))
	if folder == "" || folder == "0" {
		return nil, Fail("invalid_transfer", "网盘未返回专用目录身份")
	}
	result = receipt(p, s, src, batch, folder, []model.File{})
	for i, f := range files {
		parent := f.Meta["parent_id"]
		if parent == "" {
			parent = src.ParentID
		}
		if parent == "" {
			parent = "0"
		}
		r, e = p.call(ctx, c, s, src, "POST", "/share/sharepage/save", object{"scene": "link", "pdir_fid": parent, "pwd_id": src.ShareID, "stoken": t, "fid_list": []string{f.ID}, "fid_token_list": []string{tokens[i]}, "to_pdir_fid": folder})
		if e != nil {
			return result, e
		}
		d, e := p.task(ctx, c, s, src, str(at(r, "data", "task_id")))
		if e != nil {
			if a := items(at(d, "save_as", "save_as_top_fids")); len(a) == 1 {
				if own := str(a[0]); own != "" {
					_ = recordCreated(result, f, own)
				}
			}
			return result, e
		}
		a := items(at(d, "save_as", "save_as_top_fids"))
		if len(a) != 1 {
			return result, Fail("invalid_transfer", "网盘未返回单文件转存映射，专用目录已保留")
		}
		id := str(a[0])
		if e = requireMapped(f, id); e != nil {
			return result, e
		}
		if e = recordCreated(result, f, id); e != nil {
			return result, e
		}
	}
	return result, nil
}
func (p *quarkProvider) Cleanup(ctx context.Context, c *APIClient, s *Session, r *model.TransferReceipt) error {
	if e := ownedCleanup(ctx, p, c, s, r, "0"); e != nil {
		return e
	}
	for _, ids := range [][]string{r.CreatedIDs, {r.FolderID}} {
		if len(ids) == 0 {
			continue
		}
		v, e := p.call(ctx, c, s, r.Source, "POST", "/file/delete", object{"action_type": 2, "exclude_fids": []string{}, "filelist": ids})
		if e != nil {
			return e
		}
		if _, e = p.task(ctx, c, s, r.Source, str(at(v, "data", "task_id"))); e != nil {
			return e
		}
	}
	return nil
}
