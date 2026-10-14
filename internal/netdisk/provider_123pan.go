package netdisk

import (
	"context"
	"encoding/base64"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"hash/crc32"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type pan123Provider struct{ providerBase }

func pan123Sign(raw string, stamp time.Time, nonce string) string {
	u, _ := url.Parse(raw)
	table := "adefghlmyijnopkqrstubcvwsz"
	digits := stamp.In(time.FixedZone("CST", 8*3600)).Format("200601021504")
	b := make([]byte, len(digits))
	for i, d := range []byte(digits) {
		b[i] = table[d-'0']
	}
	key := strconv.FormatUint(uint64(crc32.ChecksumIEEE(b)), 10)
	ts := strconv.FormatInt(stamp.Unix(), 10)
	data := strings.Join([]string{ts, nonce, u.Path, "web", "3", key}, "|")
	val := strings.Join([]string{ts, nonce, strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(data))), 10)}, "-")
	return query(raw, values(map[string]string{key: val}))
}
func (p *pan123Provider) headers(s *Session, src model.Source) map[string]string {
	h := requestHeaders(s, src)
	h["Platform"], h["App-Version"], h["Platform_os"] = "web", "3", "Windows"
	if b := bearer(s); b != "" {
		h["Authorization"] = b
	}
	h["LoginUuid"] = sessionValue(s, "LoginUuid", "loginUuid")
	return h
}
func (p *pan123Provider) call(ctx context.Context, c *APIClient, s *Session, src model.Source, method, raw string, body any) (object, error) {
	r, e := apiJSON(ctx, c, method, pan123Sign(raw, time.Now(), strconv.FormatInt(time.Now().UnixNano()%10000000, 10)), body, p.headers(s, src))
	if e != nil {
		return nil, e
	}
	if e = checkResponse(r, "code", "0", "200"); e != nil {
		return nil, e
	}
	return r, nil
}
func (p *pan123Provider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	n, e := pageNumber(cursor, 1)
	if e != nil {
		return nil, e
	}
	parent := src.ParentID
	if parent == "" {
		parent = "0"
	}
	q := map[string]string{"parentFileId": parent, "limit": "100", "next": "0", "Page": strconv.Itoa(n), "orderBy": "file_id", "orderDirection": "desc"}
	path := "/file/list/new"
	if src.ShareID != "" {
		path = "/share/get"
		q["shareKey"], q["SharePwd"] = src.ShareID, src.Password
	} else {
		if bearer(s) == "" {
			return nil, LoginRequired(p.Info().Name)
		}
		q["driveId"], q["trashed"], q["SearchData"], q["event"], q["operateType"] = "0", "false", "", "homeListFile", "4"
	}
	r, e := p.call(ctx, c, s, src, "GET", query("https://yun.123pan.com/b/api"+path, values(q)), nil)
	if e != nil {
		return nil, e
	}
	d := obj(r["data"])
	if _, ok := d["InfoList"].([]any); !ok {
		return nil, Fail("invalid_response", "123云盘未返回文件列表")
	}
	l := &model.Listing{Source: src, Files: []model.File{}}
	for _, v := range items(d["InfoList"]) {
		m := obj(v)
		f := model.File{ID: first(m, "FileId", "FileID"), Name: str(m["FileName"]), Size: num(m["Size"]), Hash: str(m["Etag"]), IsDir: num(m["Type"]) == 1, Meta: map[string]string{"s3key_flag": str(m["S3KeyFlag"]), "etag": str(m["Etag"]), "parent_id": parent}}
		if e = validateFile(f); e != nil {
			return nil, e
		}
		l.Files = append(l.Files, f)
	}
	next := str(d["Next"])
	if len(l.Files) > 0 && next != "-1" && (len(l.Files) == 100 || int64(n*100) < num(d["Total"]) || next != "") {
		l.NextPage = strconv.Itoa(n + 1)
	}
	return l, nil
}
func (p *pan123Provider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	id, e := strconv.ParseInt(f.ID, 10, 64)
	if e != nil {
		return nil, Fail("invalid_file", "123文件身份必须是数字")
	}
	etag := f.Meta["etag"]
	if etag == "" {
		etag = f.Hash
	}
	flag := f.Meta["s3key_flag"]
	if flag == "" {
		flag = f.Meta["S3KeyFlag"]
	}
	if flag == "" || etag == "" {
		return nil, Fail("invalid_context", "123文件缺少Etag或S3KeyFlag，请重新同步目录")
	}
	body := object{"driveId": 0, "fileId": id, "fileName": f.Name, "etag": etag, "s3keyFlag": flag, "size": f.Size, "type": 0}
	path := "/api/v2/file/download_info"
	if src.ShareID != "" {
		path = "/api/v2/share/download/info"
		body = object{"ShareKey": src.ShareID, "SharePwd": src.Password, "FileID": id, "S3keyFlag": flag, "Size": f.Size, "Etag": etag}
	} else if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	r, e := p.call(ctx, c, s, src, "POST", "https://api.123pan.cn"+path, body)
	if e != nil {
		return nil, e
	}
	d := obj(r["data"])
	raw := first(d, "DownloadUrl", "DownloadURL", "downloadUrl")
	if raw == "" {
		a := items(d["dispatchList"])
		if len(a) > 0 {
			raw = str(at(a[0], "prefix")) + str(d["downloadPath"])
		}
	}
	if u, e := url.Parse(raw); e == nil {
		if params := u.Query().Get("params"); params != "" {
			decoded, e := base64.StdEncoding.DecodeString(params)
			if e != nil {
				decoded, e = base64.RawStdEncoding.DecodeString(params)
			}
			if e != nil {
				return nil, Fail("invalid_response", "123直链参数解码失败")
			}
			raw = string(decoded)
		}
	}
	return download(c, s, f, raw, map[string]string{"User-Agent": browserUA, "Referer": "https://www.123pan.com/"}, d["expire_at"])
}
