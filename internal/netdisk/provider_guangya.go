package netdisk

import (
	"context"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"strconv"
	"strings"
)

// The fixed LinkSwift dev script and OpenList 482464c2 guangyapan driver
// use /nd.bizuserres.s for download/token and mutation/task APIs.
// The official https://www.guangyapan.com/static/js/index.b1e7c5bb.js
// (SHA256 f5eeb594b6c622c87d87e91dfc22110076d3351e137b46f577cb4630e06c2347)
// uses /userres for share paging/restore; OpenList's normal personal listing
// also uses /userres. Keep this distinct from its transfer-directory lookup.
// Keep those independently verified paths explicit; do not infer aliases or
// retry write operations against another service prefix.
type guangyaProvider struct{ providerBase }

func (p *guangyaProvider) headers(s *Session, src model.Source) map[string]string {
	h := requestHeaders(s, src)
	if b := bearer(s); b != "" {
		h["Authorization"] = b
	}
	for _, k := range []string{"X-Captcha-Token", "X-Device-Id"} {
		v := sessionValue(s, k)
		if v == "" && k == "X-Captcha-Token" {
			v = sessionValue(s, "captcha_token", "captchaToken")
		}
		if v == "" && k == "X-Device-Id" {
			v = sessionValue(s, "device_id", "deviceId", "deviceid")
		}
		if v != "" {
			h[k] = v
		}
	}
	h["Did"] = h["X-Device-Id"]
	h["Dt"] = "4"
	if v := sessionValue(s, "Did"); v != "" {
		h["Did"] = v
	}
	if v := sessionValue(s, "Dt"); v != "" {
		h["Dt"] = v
	}
	return h
}
func (p *guangyaProvider) call(ctx context.Context, c *APIClient, s *Session, src model.Source, path string, body object) (object, error) {
	r, e := apiJSON(ctx, c, "POST", "https://api.guangyapan.com"+path, body, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = guangyaResponse(r); e != nil {
		return nil, e
	}
	return r, nil
}
func (p *guangyaProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	if src.ShareID != "" {
		token, e := p.shareToken(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		r, e := p.call(ctx, c, s, src, "/userres/v1/get_share_page_files_list", object{"accessToken": token, "parentId": src.ParentID, "cursor": cursor, "pageSize": 100, "orderBy": 0, "sortType": 0})
		if e != nil {
			return nil, e
		}
		d := obj(r["data"])
		if e = validCollections(d, "list"); e != nil {
			return nil, e
		}
		l := &model.Listing{Source: src, Files: []model.File{}}
		for _, v := range items(d["list"]) {
			f := model.File{ID: str(at(v, "fileId")), Name: str(at(v, "fileName")), Size: num(at(v, "fileSize")), Hash: str(at(v, "md5")), IsDir: num(at(v, "resType")) == 2, Meta: map[string]string{"parent_id": src.ParentID, "gcid": str(at(v, "gcid"))}}
			if e = validateFile(f); e != nil {
				return nil, e
			}
			l.Files = append(l.Files, f)
		}
		if yes(d["hasMore"]) {
			l.NextPage = str(d["cursor"])
			if l.NextPage == "" || l.NextPage == cursor {
				return nil, Fail("invalid_pagination", "光鸭分享分页没有返回新的游标")
			}
		}
		return l, nil
	}
	if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	n, e := pageNumber(cursor, 0)
	if e != nil {
		return nil, e
	}
	parent := src.ParentID
	r, e := p.call(ctx, c, s, src, "/userres/v1/file/get_file_list", object{"parentId": parent, "page": n, "pageSize": 100, "orderBy": 3, "sortType": 1})
	if e != nil {
		return nil, e
	}
	d := obj(r["data"])
	if _, ok := d["list"].([]any); !ok {
		// Protobuf JSON omits an empty repeated list and its zero total.
		// The observed empty personal response is {"msg":"success","data":{}}.
		_, explicitCode := r["code"]
		if d == nil || len(d) != 0 || explicitCode || !strings.EqualFold(strings.TrimSpace(str(r["msg"])), "success") {
			return nil, Fail("invalid_response", "光鸭网盘缺少目录列表")
		}
	}
	l := &model.Listing{Source: src, Files: []model.File{}}
	for _, v := range items(d["list"]) {
		f := model.File{ID: str(at(v, "fileId")), Name: str(at(v, "fileName")), Size: num(at(v, "fileSize")), Hash: str(at(v, "md5")), IsDir: num(at(v, "resType")) == 2, Meta: map[string]string{"parent_id": parent, "gcid": str(at(v, "gcid"))}}
		if e = validateFile(f); e != nil {
			return nil, e
		}
		l.Files = append(l.Files, f)
	}
	total := num(d["total"])
	if int64((n+1)*100) < total || total == 0 && len(l.Files) == 100 {
		l.NextPage = strconv.Itoa(n + 1)
	}
	return l, nil
}
func (p *guangyaProvider) shareToken(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	if t := sessionValue(s, "share_access_token", "shareAccessToken"); t != "" {
		return t, nil
	}
	r, e := p.call(ctx, c, s, src, "/nd.bizuserres.s/v1/get_share_access_token", object{"shareId": src.ShareID, "code": src.Password})
	if e != nil {
		return "", e
	}
	t := str(at(r, "data", "accessToken"))
	if t == "" {
		return "", Fail("invalid_response", "光鸭分享未返回访问令牌")
	}
	remember(s, "share_access_token", t)
	return t, nil
}
func (p *guangyaProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	path := "/nd.bizuserres.s/v1/get_res_download_url"
	body := object{"fileId": f.ID}
	if src.ShareID != "" {
		tok, e := p.shareToken(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		path = "/nd.bizuserres.s/v1/get_share_download_url"
		body["accessToken"] = tok
	} else if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	r, e := p.call(ctx, c, s, src, path, body)
	if e != nil {
		return nil, e
	}
	if id := str(at(r, "data", "fileId")); id != "" && id != f.ID {
		return nil, Fail("file_mismatch", "光鸭返回了另一文件的下载信息")
	}
	return download(c, s, f, first(obj(r["data"]), "signedURL", "downloadUrl"), map[string]string{"User-Agent": browserUA, "Referer": "https://www.guangyapan.com/"}, at(r, "data", "expireAt"))
}

// Guangya omits the zero code in successful protobuf JSON responses. Its
// public OpenList offline.go contract accepts an empty or "success" message.
// Requiring that marker avoids treating an arbitrary missing status as success;
// each caller still validates its endpoint-specific result fields.
func guangyaResponse(r object) error {
	if _, ok := r["code"]; ok {
		return checkResponse(r, "code", "0")
	}
	msg, ok := r["msg"].(string)
	if ok && (strings.TrimSpace(msg) == "" || strings.EqualFold(strings.TrimSpace(msg), "success")) {
		return nil
	}
	return Fail("invalid_response", "光鸭网盘响应缺少有效状态")
}
