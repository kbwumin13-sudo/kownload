package netdisk

import (
	"context"
	"crypto/rc4"
	"encoding/base64"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type baiduProvider struct{ providerBase }

func baiduCookieAuth(c *APIClient) bool {
	if c == nil || c.Client == nil || c.Client.Jar == nil {
		return false
	}
	u, _ := url.Parse("https://pan.baidu.com/")
	for _, v := range c.Client.Jar.Cookies(u) {
		if (v.Name == "BDUSS" || v.Name == "BDUSS_BFESS") && v.Value != "" {
			return true
		}
	}
	return false
}

// The personal web download API uses base64(RC4(sign3, sign1)), as in
// BaiduPCS-Go panhome.Signature; the share tplconfig sign is a separate value.
func baiduWebSign(key, input string) (string, error) {
	cipher, e := rc4.NewCipher([]byte(key))
	if e != nil || input == "" {
		return "", Fail("invalid_response", "百度个人盘下载签名缺失")
	}
	out := make([]byte, len(input))
	cipher.XORKeyStream(out, []byte(input))
	return base64.StdEncoding.EncodeToString(out), nil
}

var baiduHomeSignFields = regexp.MustCompile(`"(sign1|sign3|timestamp)"\s*:\s*("(?:[^"\\]|\\.)*"|[0-9]+)`)

func (p *baiduProvider) webSignature(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, string, error) {
	h := p.headers(s, src)
	h["User-Agent"] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36"
	h["Referer"] = "https://pan.baidu.com/disk/home"
	h["Origin"] = "https://pan.baidu.com"
	// Keep the home request (including its redirects/cookies) and the legacy
	// inline fields. Current /disk/main pages obtain these variables via API.
	raw, e := c.Text(ctx, "GET", "https://pan.baidu.com/disk/home", nil, h)
	if e != nil {
		return "", "", e
	}
	fields := map[string]string{}
	for _, m := range baiduHomeSignFields.FindAllStringSubmatch(raw, -1) {
		v := m[2]
		if strings.HasPrefix(v, `"`) {
			if e = json.Unmarshal([]byte(v), &v); e != nil {
				return "", "", Fail("invalid_response", "百度个人盘签名字段无法解析")
			}
		}
		fields[m[1]] = v
	}
	if fields["sign1"] == "" || fields["sign3"] == "" || fields["timestamp"] == "" {
		// Primary references (pinned revisions):
		// Aqr-K/web-mount-packs@877e61ab529ba092e8c30501431e8e0ed38042b1
		// python-dupan-client/dupan/client.py:get_sign_and_timestamp/get_templatevariable
		// canoziia/baidunetdisk@e8504001900bae3ec072a5fcc7011c633ce4de70
		// download/download.go:GetMyParams. Never execute the returned sign2 JS.
		r, err := apiJSON(ctx, c, "GET", query("https://pan.baidu.com/api/gettemplatevariable", values(map[string]string{
			"fields": `["sign1","sign3","timestamp"]`, "channel": "chunlei", "web": "1", "app_id": "250528", "clienttype": "0",
		})), nil, h)
		if err != nil {
			return "", "", err
		}
		if err = checkResponse(r, "errno", "0"); err != nil {
			// Unknown upstream codes may contain arbitrary sensitive text.
			if typed, ok := err.(*Error); ok && typed.Code == "api_error" {
				return "", "", Fail("api_error", "百度个人盘签名接口拒绝请求")
			}
			return "", "", err
		}
		// Do not mix partial HTML fields (or cached share signatures) with API
		// fields. A passport link in ordinary HTML is not evidence of logout.
		result := obj(r["result"])
		fields["sign1"], _ = result["sign1"].(string)
		fields["sign3"], _ = result["sign3"].(string)
		fields["timestamp"] = str(result["timestamp"])
	}
	timestamp, err := strconv.ParseInt(fields["timestamp"], 10, 64)
	if fields["sign1"] == "" || fields["sign3"] == "" || err != nil || timestamp <= 0 || strings.Trim(fields["timestamp"], "0123456789") != "" {
		return "", "", Fail("invalid_response", "百度个人盘未提供有效下载签名")
	}
	sign, e := baiduWebSign(fields["sign3"], fields["sign1"])
	return sign, fields["timestamp"], e
}

func (p *baiduProvider) headers(s *Session, src model.Source) map[string]string {
	h := requestHeaders(s, src)
	h["User-Agent"] = "pan.baidu.com"
	return h
}
func baiduDir(s *Session, src model.Source) (string, error) {
	if src.ParentID == "" || src.ParentID == "0" {
		return "/", nil
	}
	if strings.HasPrefix(src.ParentID, "/") {
		return src.ParentID, nil
	}
	if v := sessionValue(s, "baidu_path."+src.ParentID); v != "" {
		return v, nil
	}
	if s != nil {
		for _, f := range s.Capture.Files {
			if f.ID == src.ParentID && f.Meta["remote_path"] != "" {
				return f.Meta["remote_path"], nil
			}
		}
	}
	return "", Fail("invalid_context", "百度网盘目录路径缺失，请重新同步目录")
}
func (p *baiduProvider) directory(ctx context.Context, c *APIClient, s *Session, src model.Source) (string, error) {
	if d, e := baiduDir(s, src); e == nil {
		return d, nil
	}
	if src.ShareID == "" && sessionValue(s, "access_token", "accessToken", "baidu_access_token") != "" {
		token := sessionValue(s, "access_token", "accessToken", "baidu_access_token")
		b, e := json.Marshal([]json.Number{json.Number(src.ParentID)})
		if e != nil {
			return "", Fail("invalid_file", "百度目录身份无效")
		}
		r, e := apiJSON(ctx, c, "GET", query("https://pan.baidu.com/rest/2.0/xpan/multimedia", values(map[string]string{"method": "filemetas", "fsids": string(b), "access_token": token})), nil, p.headers(s, src))
		if e != nil {
			return "", e
		}
		if e = checkResponse(r, "errno", "0"); e != nil {
			return "", e
		}
		for _, v := range items(r["list"]) {
			if str(at(v, "fs_id")) == src.ParentID && yes(at(v, "isdir")) {
				d := str(at(v, "path"))
				if d != "" {
					remember(s, "baidu_path."+src.ParentID, d)
					return d, nil
				}
			}
		}
		return "", Fail("not_found", "百度目录不存在或已移动")
	}
	// The web APIs address directories by path, while the public model
	// preserves fs_id. Rediscover that path through share listings after a fresh
	// service-session clone rather than requiring persisted opaque page state.
	queue := []string{"/"}
	if src.ShareID != "" {
		queue[0] = ""
	}
	seen := map[string]bool{}
	requests := 0
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		if seen[parent] {
			continue
		}
		seen[parent] = true
		sub := src
		sub.ParentID = parent
		cur := ""
		pages := map[string]bool{}
		for {
			requests++
			if requests > 10000 {
				return "", Fail("too_many_files", "百度目录查找超出范围，请在浏览器打开目标目录再同步")
			}
			if pages[cur] {
				return "", Fail("invalid_pagination", "百度目录分页重复")
			}
			pages[cur] = true
			l, e := p.List(ctx, c, s, sub, cur)
			if e != nil {
				return "", e
			}
			for _, f := range l.Files {
				if !f.IsDir {
					continue
				}
				d := f.Meta["remote_path"]
				if f.ID == src.ParentID && d != "" {
					return d, nil
				}
				if d != "" {
					queue = append(queue, d)
				}
			}
			cur = l.NextPage
			if cur == "" {
				break
			}
		}
	}
	return "", Fail("not_found", "百度网盘中找不到所选目录")
}
func (p *baiduProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	n, e := pageNumber(cursor, 1)
	if e != nil {
		return nil, e
	}
	dir, e := p.directory(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	var r object
	if src.ShareID != "" {
		root := "0"
		if src.ParentID == "" || src.ParentID == "0" {
			root = "1"
			dir = ""
		}
		r, e = apiForm(ctx, c, "https://pan.baidu.com/share/wxlist?channel=weixin&version=2.2.2&clienttype=25&web=1", values(map[string]string{"shorturl": strings.TrimPrefix(src.ShareID, "1"), "pwd": src.Password, "root": root, "dir": dir, "num": "100", "order": "time", "page": strconv.Itoa(n)}), p.headers(s, src))
	} else {
		token := sessionValue(s, "access_token", "accessToken", "baidu_access_token")
		if token == "" {
			if !baiduCookieAuth(c) {
				return nil, LoginRequired(p.Info().Name)
			}
			r, e = apiJSON(ctx, c, "GET", query("https://pan.baidu.com/api/list", values(map[string]string{"dir": dir, "page": strconv.Itoa(n), "num": "100", "order": "name", "desc": "0", "clienttype": "0", "web": "1", "app_id": "250528"})), nil, p.headers(s, src))
		} else {
			r, e = apiJSON(ctx, c, "GET", query("https://pan.baidu.com/rest/2.0/xpan/file", values(map[string]string{"method": "list", "showempty": "1", "dir": dir, "start": strconv.Itoa((n - 1) * 100), "limit": "100", "access_token": token})), nil, p.headers(s, src))
		}
	}
	if e != nil {
		return nil, e
	}
	if e = checkResponse(r, "errno", "0"); e != nil {
		return nil, e
	}
	d := r
	if src.ShareID != "" {
		d = obj(r["data"])
		for _, k := range []string{"uk", "shareid", "seckey"} {
			remember(s, k, str(d[k]))
		}
	}
	if _, ok := d["list"].([]any); !ok {
		return nil, Fail("invalid_response", "百度网盘响应缺少文件列表")
	}
	l := &model.Listing{Source: src, Files: []model.File{}}
	for _, v := range items(d["list"]) {
		m := obj(v)
		f := model.File{ID: str(m["fs_id"]), Name: str(m["server_filename"]), Size: num(m["size"]), Hash: str(m["md5"]), IsDir: yes(m["isdir"]), Meta: map[string]string{"remote_path": str(m["path"])}}
		if e = validateFile(f); e != nil {
			return nil, e
		}
		remember(s, "baidu_path."+f.ID, str(m["path"]))
		l.Files = append(l.Files, f)
	}
	if yes(d["has_more"]) || src.ShareID == "" && len(l.Files) == 100 {
		l.NextPage = strconv.Itoa(n + 1)
	}
	return l, nil
}
func (p *baiduProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	ids, e := json.Marshal([]json.Number{json.Number(f.ID)})
	if e != nil {
		return nil, Fail("invalid_file", "百度文件身份必须是数字")
	}
	h := p.headers(s, src)
	var r object
	if src.ShareID == "" {
		tok := sessionValue(s, "access_token", "accessToken", "baidu_access_token")
		if tok == "" {
			if !baiduCookieAuth(c) {
				return nil, LoginRequired(p.Info().Name)
			}
			sign, timestamp, sigErr := p.webSignature(ctx, c, s, src)
			if sigErr != nil {
				return nil, sigErr
			}
			r, e = apiForm(ctx, c, "https://pan.baidu.com/api/download", values(map[string]string{"sign": sign, "timestamp": timestamp, "fidlist": string(ids)}), h)
		} else {
			r, e = apiJSON(ctx, c, "GET", query("https://pan.baidu.com/rest/2.0/xpan/multimedia", values(map[string]string{"method": "filemetas", "dlink": "1", "fsids": string(ids), "access_token": tok})), nil, h)
		}
	} else {
		if sessionValue(s, "uk") == "" || sessionValue(s, "shareid", "share_id") == "" {
			if _, e = p.List(ctx, c, s, model.Source{Provider: src.Provider, URL: src.URL, ShareID: src.ShareID, Password: src.Password}, ""); e != nil {
				return nil, e
			}
		}
		sig, e := apiJSON(ctx, c, "GET", query("https://pan.baidu.com/share/tplconfig", values(map[string]string{"surl": src.ShareID, "fields": "sign,timestamp", "channel": "chunlei", "web": "1", "app_id": "250528", "clienttype": "0", "bdstoken": sessionValue(s, "bdstoken")})), nil, h)
		if e != nil {
			return nil, e
		}
		if e = checkResponse(sig, "errno", "0"); e != nil {
			return nil, e
		}
		sign, timestamp := str(at(sig, "data", "sign")), str(at(sig, "data", "timestamp"))
		if sign == "" || timestamp == "" {
			return nil, Fail("invalid_response", "百度分享缺少有效下载签名")
		}
		body := values(map[string]string{"encrypt": "0", "product": "share", "uk": sessionValue(s, "uk"), "primaryid": sessionValue(s, "shareid", "share_id"), "fid_list": string(ids)})
		if sekey := sessionValue(s, "seckey", "sekey"); sekey != "" {
			b, _ := json.Marshal(object{"sekey": sekey})
			body.Set("extra", string(b))
		}
		r, e = apiForm(ctx, c, query("https://pan.baidu.com/api/sharedownload", values(map[string]string{"channel": "chunlei", "clienttype": "0", "web": "1", "app_id": "250528", "sign": sign, "timestamp": timestamp, "bdstoken": sessionValue(s, "bdstoken")})), body, h)
	}
	if e != nil {
		return nil, e
	}
	if e = checkResponse(r, "errno", "0"); e != nil {
		return nil, e
	}
	if _, ok := r["list"].(string); ok {
		if src.ShareID != "" {
			return nil, TransferRequired()
		}
		return nil, Fail("invalid_response", "百度个人盘未返回文件下载信息")
	}
	links := items(r["list"])
	if src.ShareID == "" && len(links) == 0 {
		// /api/download returns dlink[], unlike xpan's list[].
		links = items(r["dlink"])
	}
	for _, v := range links {
		m := obj(v)
		if str(m["fs_id"]) == f.ID {
			raw := str(m["dlink"])
			if token := sessionValue(s, "access_token", "accessToken", "baidu_access_token"); token != "" && raw != "" {
				u, e := url.Parse(raw)
				if e != nil || !(HostMatches(u.Hostname(), "baidu.com") || HostMatches(u.Hostname(), "baidupcs.com")) {
					return nil, Fail("invalid_response", "百度返回了不支持认证参数的下载域名")
				}
				raw = query(raw, values(map[string]string{"access_token": token}))
			}
			return download(c, s, f, raw, h, m["expires"])
		}
	}
	return nil, Fail("file_mismatch", "百度未返回所选文件的下载地址")
}
