package netdisk

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"sort"
	"strings"
	"time"
)

type mcloudProvider struct{ providerBase }

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
func encodeURIComponent(s string) string {
	const hx = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.!~*'()", rune(c)) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hx[c>>4])
			b.WriteByte(hx[c&15])
		}
	}
	return b.String()
}
func mcloudSign(body []byte, stamp, nonce string) string {
	cs := strings.Split(encodeURIComponent(string(body)), "")
	sort.Strings(cs)
	b := base64.StdEncoding.EncodeToString([]byte(strings.Join(cs, "")))
	return strings.ToUpper(md5hex(md5hex(b) + md5hex(stamp+":"+nonce)))
}
func (p *mcloudProvider) endpoint(s *Session) string {
	v := sessionValue(s, "personalCloudHost", "personal_cloud_host")
	if v == "" {
		return "https://personal-kd-njs.yun.139.com/hcy"
	}
	return strings.TrimRight(v, "/")
}
func (p *mcloudProvider) headers(s *Session, body []byte) map[string]string {
	stamp := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
	nonce := randomID()[:16]
	auth := sessionValue(s, "Authorization", "authorization")
	if auth == "" {
		auth = cookieValue(s, "authorization")
	}
	if auth != "" && !strings.HasPrefix(strings.ToLower(auth), "basic ") {
		auth = "Basic " + auth
	}
	// The personal API also requires the common web client headers. In
	// particular, omitting X-Yun-Client-Info produces 04000003 (invalid headers)
	// even when the other client headers and the signed body are present.
	// Keep these aligned with the fixed LinkSwift web request contract.
	return map[string]string{
		"Authorization":        auth,
		"Caller":               "web",
		"Content-Type":         "application/json;charset=UTF-8",
		"CMS-DEVICE":           "default",
		"Mcloud-Channel":       "1000101",
		"Mcloud-Client":        "10701",
		"Mcloud-Route":         "001",
		"Mcloud-Sign":          stamp + "," + nonce + "," + mcloudSign(body, stamp, nonce),
		"Mcloud-Version":       "7.14.2",
		"X-DeviceInfo":         "||9|7.17.0|edge||||windows 10||zh-CN|||",
		"X-Huawei-ChannelSrc":  "10000034",
		"X-Inner-Ntwk":         "2",
		"X-M4C-Caller":         "PC",
		"X-M4C-Src":            "10002",
		"X-SvcType":            "1",
		"X-Yun-Api-Version":    "v1",
		"X-Yun-App-Channel":    "10000034",
		"X-Yun-Channel-Source": "10000034",
		"X-Yun-Client-Info":    "||9|7.17.0|edge||||windows 10||zh-CN|||||",
		"X-Yun-Module-Type":    "100",
		"X-Yun-Svc-Type":       "1",
		"User-Agent":           browserUA,
		"Referer":              "https://yun.139.com/",
		"Origin":               "https://yun.139.com",
	}
}
func mcloudCheck(r object) error {
	if v, ok := r["success"]; ok {
		if yes(v) {
			return nil
		}
		return responseError(str(r["code"]))
	}
	return checkResponse(r, "code", "0", "0000")
}
func (p *mcloudProvider) personal(ctx context.Context, c *APIClient, s *Session, path string, body object) (object, error) {
	b, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	h := p.headers(s, b)
	if h["Authorization"] == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	r, e := apiJSON(ctx, c, "POST", p.endpoint(s)+path, body, h)
	if e != nil {
		return nil, e
	}
	if e = mcloudCheck(r); e != nil {
		return nil, e
	}
	return r, nil
}
func mcloudEncrypt(b []byte) (string, error) {
	key := []byte("PVGDwmcvfs1uV3d1")
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	iv := make([]byte, aes.BlockSize)
	if _, e = rand.Read(iv); e != nil {
		return "", e
	}
	n := aes.BlockSize - len(b)%aes.BlockSize
	b = append(b, bytes.Repeat([]byte{byte(n)}, n)...)
	out := make([]byte, len(b))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, b)
	return base64.StdEncoding.EncodeToString(append(iv, out...)), nil
}
func mcloudDecrypt(s string) ([]byte, error) {
	b, e := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if e != nil || len(b) < 32 || (len(b)-16)%16 != 0 {
		return nil, Fail("invalid_response", "移动分享响应加密格式无效")
	}
	block, _ := aes.NewCipher([]byte("PVGDwmcvfs1uV3d1"))
	out := make([]byte, len(b)-16)
	cipher.NewCBCDecrypter(block, b[:16]).CryptBlocks(out, b[16:])
	n := int(out[len(out)-1])
	if n < 1 || n > 16 || !bytes.Equal(out[len(out)-n:], bytes.Repeat([]byte{byte(n)}, n)) {
		return nil, Fail("invalid_response", "移动分享响应填充无效")
	}
	return out[:len(out)-n], nil
}
func (p *mcloudProvider) share(ctx context.Context, c *APIClient, s *Session, path string, body object) (object, error) {
	b, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	enc, e := mcloudEncrypt(b)
	if e != nil {
		return nil, e
	}
	h := p.headers(s, b)
	h["hcy-cool-flag"] = "1"
	h["Content-Type"] = "application/json;charset=UTF-8"
	raw, e := c.Text(ctx, "POST", "https://share-kd-njs.yun.139.com/yun-share"+path, []byte(enc), h)
	if e != nil {
		return nil, e
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		b, e = mcloudDecrypt(raw)
		if e != nil {
			return nil, e
		}
	} else {
		b = []byte(raw)
	}
	r, e := decodeObject(b)
	if e != nil {
		return nil, e
	}
	if e = mcloudCheck(r); e != nil {
		return nil, e
	}
	return r, nil
}
func (p *mcloudProvider) List(ctx context.Context, c *APIClient, s *Session, src model.Source, cursor string) (*model.Listing, error) {
	parent := src.ParentID
	l := &model.Listing{Source: src, Files: []model.File{}}
	if src.ShareID != "" {
		if cursor != "" {
			return nil, Fail("invalid_pagination", "移动分享接口一次返回完整目录，无后续页")
		}
		if parent == "" {
			parent = "root"
		}
		r, e := p.share(ctx, c, s, "/richlifeApp/devapp/IOutLink/getOutLinkInfoV6", object{"getOutLinkInfoReq": object{"account": sessionValue(s, "account", "accountName"), "linkID": src.ShareID, "passwd": src.Password, "pCaID": parent}})
		if e != nil {
			return nil, e
		}
		d := obj(r["data"])
		if e = validCollections(d, "caLst", "coLst"); e != nil {
			return nil, e
		}
		l.Title = str(d["lkName"])
		for _, v := range items(d["caLst"]) {
			l.Files = append(l.Files, model.File{ID: str(at(v, "caId")), Name: str(at(v, "caName")), IsDir: true})
		}
		for _, v := range items(d["coLst"]) {
			l.Files = append(l.Files, model.File{ID: str(at(v, "coId")), Name: str(at(v, "coName")), Size: num(at(v, "coSize"))})
		}
		return l, nil
	}
	if parent == "" {
		parent = "/"
	}
	r, e := p.personal(ctx, c, s, "/file/list", object{"parentFileId": parent, "orderBy": "updated_at", "orderDirection": "DESC", "pageInfo": object{"pageCursor": cursor, "pageSize": 100}, "imageThumbnailStyleList": []string{"Small", "Large"}})
	if e != nil {
		return nil, e
	}
	d := obj(r["data"])
	if _, ok := d["items"].([]any); !ok {
		return nil, Fail("invalid_response", "移动云盘缺少文件列表")
	}
	for _, v := range items(d["items"]) {
		l.Files = append(l.Files, model.File{ID: str(at(v, "fileId")), Name: str(at(v, "name")), Size: num(at(v, "size")), IsDir: str(at(v, "type")) == "folder"})
	}
	l.NextPage = str(d["nextPageCursor"])
	return l, nil
}
func (p *mcloudProvider) Resolve(ctx context.Context, c *APIClient, s *Session, src model.Source, f model.File) (*Download, error) {
	if e := checkFile(f); e != nil {
		return nil, e
	}
	var r object
	var e error
	raw := ""
	if src.ShareID == "" {
		r, e = p.personal(ctx, c, s, "/file/getDownloadUrl", object{"fileId": f.ID})
		if e != nil {
			return nil, e
		}
		d := obj(r["data"])
		raw = str(d["url"])
		if yes(d["cdnSwitch"]) && str(d["cdnUrl"]) != "" {
			raw = str(d["cdnUrl"])
		}
	} else {
		account := sessionValue(s, "account", "accountName")
		if account == "" {
			auth := sessionValue(s, "Authorization", "authorization")
			if auth == "" {
				auth = cookieValue(s, "authorization")
			}
			b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
			parts := strings.Split(string(b), ":")
			if len(parts) > 1 {
				account = parts[1]
			}
		}
		if account == "" {
			return nil, LoginRequired(p.Info().Name)
		}
		r, e = p.share(ctx, c, s, "/richlifeApp/devapp/IOutLink/dlFromOutLinkV3", object{"dlFromOutLinkReqV3": object{"account": account, "linkID": src.ShareID, "passwd": src.Password, "coIDLst": object{"item": []string{f.ID}}}})
		if e != nil {
			return nil, e
		}
		d := obj(r["data"])
		raw = str(at(d, "extInfo", "cdnDownloadUrl"))
		if raw == "" {
			raw = first(d, "redrUrl", "downloadURL")
		}
	}
	return download(c, s, f, raw, map[string]string{"User-Agent": browserUA, "Referer": "https://yun.139.com/"}, nil)
}
