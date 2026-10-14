package netdisk

import (
	"context"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type tcloudProvider struct{ providerBase }

func (p *tcloudProvider) get(ctx context.Context, c *APIClient, s *Session, src model.Source, path string, q map[string]string) (object, error) {
	r, e := apiJSON(ctx, c, "GET", query("https://cloud.189.cn/api/open"+path, values(q)), nil, requestHeaders(s, src))
	if e != nil {
		return nil, e
	}
	if code := first(r, "errorCode", "errorcode"); code != "" {
		return nil, responseError(code)
	}
	if e = checkResponse(r, "res_code", "0"); e != nil {
		return nil, e
	}
	return r, nil
}
func (p *tcloudProvider) shareInfo(ctx context.Context, c *APIClient, s *Session, src model.Source) (object, error) {
	code := src.ShareID
	if u, e := url.Parse(src.URL); e == nil {
		if v := u.Query().Get("code"); v != "" {
			code = v
		} else if strings.HasPrefix(u.Path, "/t/") {
			code = strings.TrimPrefix(u.Path, "/t/")
		}
	}
	r, e := p.get(ctx, c, s, src, "/share/getShareInfoByCode.action", map[string]string{"shareCode": code})
	if e != nil {
		return nil, e
	}
	if str(r["shareId"]) == "" {
		return nil, Fail("invalid_response", "天翼分享未返回内部分享身份")
	}
	return r, nil
}
func (p *tcloudProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	n, e := pageNumber(cursor, 1)
	if e != nil {
		return nil, e
	}
	parent := src.ParentID
	if parent == "" {
		parent = "-11"
	}
	q := map[string]string{"folderId": parent, "pageNum": strconv.Itoa(n), "pageSize": "60", "mediaType": "0", "iconOption": "5", "orderBy": "lastOpTime", "descending": "true"}
	path := "/file/listFiles.action"
	if src.ShareID != "" {
		info, e := p.shareInfo(ctx, c, s, src)
		if e != nil {
			return nil, e
		}
		parent = src.ParentID
		if parent == "" {
			parent = str(info["fileId"])
		}
		q = map[string]string{"fileId": parent, "shareDirFileId": str(info["fileId"]), "shareId": str(info["shareId"]), "shareMode": str(info["shareMode"]), "accessCode": src.Password, "isFolder": str(info["isFolder"]), "pageNum": strconv.Itoa(n), "pageSize": "60", "iconOption": "5", "orderBy": "lastOpTime", "descending": "true"}
		if yes(info["isFolder"]) {
			q["isFolder"] = "true"
		} else {
			q["isFolder"] = "false"
		}
		path = "/share/listShareDir.action"
		remember(s, "tcloud_share_id", str(info["shareId"]))
	}
	r, e := p.get(ctx, c, s, src, path, q)
	if e != nil {
		return nil, e
	}
	d := obj(r["fileListAO"])
	if d == nil {
		return nil, Fail("invalid_response", "天翼网盘缺少文件列表")
	}
	if e=validCollections(d,"fileList","folderList");e!=nil{return nil,e}
	l := &model.Listing{Source: src, Files: []model.File{}}
	for _, v := range items(d["folderList"]) {
		l.Files = append(l.Files, model.File{ID: str(at(v, "id")), Name: str(at(v, "name")), IsDir: true, Meta: map[string]string{"parent_id": parent}})
	}
	for _, v := range items(d["fileList"]) {
		l.Files = append(l.Files, model.File{ID: str(at(v, "id")), Name: str(at(v, "name")), Size: num(at(v, "size")), Hash: first(v, "md5", "fileMd5"), Meta: map[string]string{"parent_id": parent}})
	}
	if len(l.Files) == 60 || int64(n*60) < num(d["count"]) {
		l.NextPage = strconv.Itoa(n + 1)
	}
	return l, nil
}
func (p *tcloudProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	token := sessionValue(s, "accessToken", "access_token", "AccessToken")
	q := map[string]string{"fileId": f.ID}
	share := ""
	if src.ShareID != "" {
		share = sessionValue(s, "tcloud_share_id", "shareId")
		if share == "" {
			info, e := p.shareInfo(ctx, c, s, src)
			if e != nil {
				return nil, e
			}
			share = str(info["shareId"])
		}
		q["dt"], q["shareId"] = "1", share
	}
	h := requestHeaders(s, src)
	raw := "https://cloud.189.cn/api/open/file/getFileDownloadUrl.action"
	if token != "" {
		raw = "https://api.cloud.189.cn/open/file/getFileDownloadUrl.action"
		stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
		sign := "AccessToken=" + token + "&Timestamp=" + stamp
		if share != "" {
			sign += "&dt=1"
		}
		sign += "&fileId=" + f.ID
		if share != "" {
			sign += "&shareId=" + share
		}
		h["Sign-Type"], h["AccessToken"], h["Timestamp"], h["Signature"] = "1", token, stamp, md5hex(sign)
	}
	r, e := apiJSON(ctx, c, "GET", query(raw, values(q)), nil, h)
	if e != nil {
		return nil, e
	}
	if code := first(r, "errorCode", "errorcode"); code != "" {
		return nil, responseError(code)
	}
	if e = checkResponse(r, "res_code", "0"); e != nil {
		return nil, e
	}
	return download(c, s, f, str(r["fileDownloadUrl"]), requestHeaders(s, src), nil)
}
