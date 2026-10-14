package netdisk

import (
	"context"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"strings"
)

type aliyunProvider struct{ providerBase }

func (p *aliyunProvider) endpoint(src model.Source) string {
	u, _ := url.Parse(src.URL)
	if u != nil && HostMatches(u.Hostname(), "aliyundrive.com") {
		return "https://api.aliyundrive.com"
	}
	return "https://api.alipan.com"
}
func (p *aliyunProvider) headers(s *Session, src model.Source) map[string]string {
	h := requestHeaders(s, src)
	if b := bearer(s); b != "" {
		h["Authorization"] = b
	}
	h["X-Canary"] = "client=windows,app=adrive,version=v6.0.0"
	for _, k := range []string{"X-Signature", "X-Device-Id"} {
		if v := sessionValue(s, k); v != "" {
			h[k] = v
		}
	}
	return h
}
func (p *aliyunProvider) call(ctx context.Context, c *APIClient, s *Session, src model.Source, path string, body object) (object, error) {
	r, e := apiJSON(ctx, c, "POST", p.endpoint(src)+path, body, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if code := str(r["code"]); code != "" {
		return nil, responseError(code)
	}
	return r, nil
}
func (p *aliyunProvider) shareToken(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	if v := sessionValue(s, "share_token", "shareToken", "x-share-token"); v != "" {
		return v, nil
	}
	r, e := p.call(ctx, c, s, src, "/v2/share_link/get_share_token", object{"share_id": src.ShareID, "share_pwd": src.Password})
	if e != nil {
		return "", e
	}
	v := str(r["share_token"])
	if v == "" {
		return "", Fail("invalid_response", "阿里云盘未返回分享令牌")
	}
	remember(s, "share_token", v)
	return v, nil
}
func (p *aliyunProvider) drive(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (string, error) {
	if v := first(objectString(f.Meta), "drive_id", "driveId"); v != "" {
		return v, nil
	}
	if v := sessionValue(s, "default_drive_id", "drive_id", "driveId", "resource_drive_id"); v != "" {
		return v, nil
	}
	if bearer(s) == "" {
		return "", LoginRequired(p.Info().Name)
	}
	r, e := p.call(ctx, c, s, src, "/v2/user/get", object{})
	if e != nil {
		return "", e
	}
	v := first(r, "resource_drive_id", "default_drive_id", "backup_drive_id")
	if v == "" {
		return "", Fail("invalid_response", "阿里云盘未返回个人网盘身份")
	}
	remember(s, "default_drive_id", v)
	return v, nil
}
func (p *aliyunProvider) shareDrive(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (string, error) {
	if v := first(objectString(f.Meta), "drive_id", "driveId"); v != "" {
		return v, nil
	}
	if s != nil {
		for _, cf := range s.Capture.Files {
			if cf.ID == f.ID {
				if v := first(objectString(cf.Meta), "drive_id", "driveId"); v != "" {
					return v, nil
				}
			}
		}
	}
	sub := src
	if f.Meta["parent_id"] != "" {
		sub.ParentID = f.Meta["parent_id"]
	}
	cur := ""
	seen := map[string]bool{}
	for {
		if seen[cur] {
			return "", Fail("invalid_pagination", "阿里分享目录分页重复")
		}
		seen[cur] = true
		l, e := p.List(ctx, c, s, sub, cur)
		if e != nil {
			return "", e
		}
		for _, cf := range l.Files {
			if cf.ID == f.ID && cf.Meta["drive_id"] != "" {
				return cf.Meta["drive_id"], nil
			}
		}
		cur = l.NextPage
		if cur == "" {
			return "", Fail("invalid_context", "阿里分享文件缺少源网盘身份，请在浏览器打开文件所在目录再同步")
		}
	}
}
func (p *aliyunProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	parent := src.ParentID
	if parent == "" {
		parent = "root"
	}
	body := object{"parent_file_id": parent, "limit": 200, "marker": cursor, "order_by": "name", "order_direction": "ASC"}
	h := p.headers(s, src)
	path := "/v2/file/list"
	if src.ShareID != "" {
		tok, e := p.shareToken(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		body["share_id"] = src.ShareID
		h["x-share-token"] = tok
		path = "/adrive/v3/file/list"
	} else {
		drive, e := p.drive(ctx, c, s, src, model.File{})
		if e != nil {
			return nil, e
		}
		body["drive_id"] = drive
	}
	r, e := apiJSON(ctx, c, "POST", p.endpoint(src)+path, body, h)
	if e != nil {
		return nil, e
	}
	if code := str(r["code"]); code != "" {
		return nil, responseError(code)
	}
	if _, ok := r["items"].([]any); !ok {
		return nil, Fail("invalid_response", "阿里云盘响应缺少目录列表")
	}
	l := &model.Listing{Source: src, Files: []model.File{}, NextPage: str(r["next_marker"])}
	for _, v := range items(r["items"]) {
		m := obj(v)
		f := model.File{ID: str(m["file_id"]), Name: str(m["name"]), Size: num(m["size"]), Hash: str(m["content_hash"]), IsDir: str(m["type"]) == "folder", Meta: map[string]string{"drive_id": str(m["drive_id"]), "parent_id": parent}}
		if e := validateFile(f); e != nil {
			return nil, e
		}
		l.Files = append(l.Files, f)
	}
	return l, nil
}
func (p *aliyunProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	body := object{"file_id": f.ID, "expire_sec": 600}
	h := p.headers(s, src)
	path := "/v2/file/get_download_url"
	if src.ShareID != "" {
		t, e := p.shareToken(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		h["x-share-token"] = t
		h["X-Canary"] = "client=web,app=share,version=v2.3.1"
		body["share_id"] = src.ShareID
		drive, e := p.shareDrive(ctx, c, s, src, f)
		if e != nil {
			return nil, e
		}
		body["drive_id"] = drive
		path = "/v2/file/get_share_link_download_url"
	} else {
		d, e := p.drive(ctx, c, s, src, f)
		if e != nil {
			return nil, e
		}
		body["drive_id"] = d
	}
	r, e := apiJSON(ctx, c, "POST", p.endpoint(src)+path, body, h)
	if e != nil {
		return nil, e
	}
	if code := str(r["code"]); code != "" {
		return nil, responseError(code)
	}
	if id := str(r["file_id"]); id != "" && id != f.ID {
		return nil, Fail("file_mismatch", "阿里云盘返回了另一文件的下载链接")
	}
	dh := map[string]string{"Referer": h["Referer"], "User-Agent": h["User-Agent"]}
	return download(c, s, f, first(r, "url", "download_url"), dh, r["expiration"])
}

// Copy of a share into an isolated folder uses one batch request per file so
// returned bodies cannot overwrite another file's identity or URL.
func (p *aliyunProvider) Transfer(ctx context.Context, c *APIClient, s *Session, src model.Source, files []model.File, batch string) (result *model.TransferReceipt, err error) {
	if e := transferInput(s, src, files, batch); e != nil {
		return nil, e
	}
	// Share drive_id belongs to the sharer. Obtain the destination from the
	// account endpoint unless Capture explicitly supplied its default drive.
	drive := sessionValue(s, "default_drive_id")
	var e error
	if drive == "" {
		var u object
		u, e = p.call(ctx, c, s, src, "/v2/user/get", object{})
		if e == nil {
			drive = first(u, "resource_drive_id", "default_drive_id", "backup_drive_id")
			remember(s, "default_drive_id", drive)
		}
	}
	if e != nil {
		return nil, e
	}
	if drive == "" {
		return nil, Fail("invalid_context", "阿里云盘未确认目标账号的个人网盘身份")
	}
	t, e := p.shareToken(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	r, e := p.call(ctx, c, s, src, "/adrive/v2/file/createWithFolders", object{"drive_id": drive, "parent_file_id": "root", "name": folderName(batch), "type": "folder", "check_name_mode": "refuse"})
	if e != nil {
		return nil, e
	}
	folder := str(r["file_id"])
	if folder == "" || folder == "root" {
		return nil, Fail("invalid_transfer", "阿里云盘未返回新目录身份")
	}
	result = receipt(p, s, src, batch, folder, []model.File{})
	h := p.headers(s, src)
	h["x-share-token"] = t
	for _, f := range files {
		r, e = apiJSON(ctx, c, "POST", p.endpoint(src)+"/v3/batch", object{"resource": "file", "requests": []object{{"id": f.ID, "method": "POST", "url": "/file/copy", "headers": object{"Content-Type": "application/json"}, "body": object{"share_id": src.ShareID, "file_id": f.ID, "to_drive_id": drive, "to_parent_file_id": folder, "auto_rename": false}}}}, h)
		if e != nil {
			return result, e
		}
		if code := str(r["code"]); code != "" {
			return result, responseError(code)
		}
		rs := items(r["responses"])
		if len(rs) != 1 || str(at(rs[0], "id")) != f.ID {
			return result, Fail("invalid_transfer", "阿里云盘转存响应无法映射所选文件，目录已保留")
		}
		v := obj(rs[0])
		if n := num(v["status"]); n < 200 || n >= 300 {
			return result, responseError(first(obj(v["body"]), "code"))
		}
		id := str(at(v, "body", "file_id"))
		if e = requireMapped(f, id); e != nil {
			return result, e
		}
		if e = recordCreated(result, f, id); e != nil {
			return result, e
		}
		result.Files[len(result.Files)-1].Meta["drive_id"] = drive
	}
	return result, nil
}
func (p *aliyunProvider) Cleanup(ctx context.Context, c *APIClient, s *Session, r *model.TransferReceipt) error {
	if s != nil && r != nil && len(r.Files) > 0 && r.Files[0].Meta["drive_id"] != "" {
		s = clone(s)
		if s.Capture.Tokens == nil {
			s.Capture.Tokens = map[string]string{}
		}
		s.Capture.Tokens["default_drive_id"] = r.Files[0].Meta["drive_id"]
	}
	if e := ownedCleanup(ctx, p, c, s, r, "root"); e != nil {
		return e
	}
	drive := ""
	if len(r.Files) > 0 {
		drive = r.Files[0].Meta["drive_id"]
	} else {
		var e error
		own := r.Source
		own.ShareID = ""
		drive, e = p.drive(ctx, c, s, own, model.File{})
		if e != nil {
			return e
		}
	}
	if drive == "" {
		return Fail("ownership_error", "转存记录缺少目标网盘身份")
	}
	for _, id := range append(append([]string{}, r.CreatedIDs...), r.FolderID) {
		v, e := p.call(ctx, c, s, r.Source, "/v2/recyclebin/trash", object{"drive_id": drive, "file_id": id})
		if e != nil {
			return e
		}
		if code := strings.TrimSpace(str(v["code"])); code != "" {
			return responseError(code)
		}
	}
	return nil
}
