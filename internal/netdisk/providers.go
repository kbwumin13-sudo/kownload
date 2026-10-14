package netdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Protocols are derived from the pinned LinkSwift sources and OpenList drivers;
// see NETDISK_PROVIDERS.md for upstream attribution and operation boundaries.
func BuiltinProviders() []Provider {
	return []Provider{
		&baiduProvider{providerBase{model.ProviderInfo{ID: "baidu", Name: "百度网盘", Hosts: []string{"pan.baidu.com", "yun.baidu.com"}, APIHosts: []string{"pcs.baidu.com"}}}},
		&aliyunProvider{providerBase{model.ProviderInfo{ID: "aliyun", Name: "阿里云盘", Hosts: []string{"alipan.com", "aliyundrive.com"}, APIHosts: []string{"api.alipan.com", "api.aliyundrive.com"}}}},
		&mcloudProvider{providerBase{model.ProviderInfo{ID: "mcloud", Name: "中国移动云盘", Hosts: []string{"yun.139.com", "caiyun.139.com"}, APIHosts: []string{"caiyun.139.com"}}}},
		&tcloudProvider{providerBase{model.ProviderInfo{ID: "tcloud", Name: "天翼云盘", Hosts: []string{"cloud.189.cn"}, APIHosts: []string{"api.cloud.189.cn"}}}},
		&xunleiProvider{providerBase{model.ProviderInfo{ID: "xunlei", Name: "迅雷云盘", Hosts: []string{"pan.xunlei.com"}, APIHosts: []string{"api-pan.xunlei.com"}}}},
		newQuarkProvider(false), newQuarkProvider(true),
		&pan123Provider{providerBase{model.ProviderInfo{ID: "123pan", Name: "123云盘", Hosts: []string{"123pan.com", "123pan.cn", "123684.com", "123865.com", "123952.com", "123912.com"}, APIHosts: []string{"api.123pan.cn", "yun.123pan.com"}}}},
		&guangyaProvider{providerBase{model.ProviderInfo{ID: "guangya", Name: "光鸭云盘", Hosts: []string{"guangyapan.com"}, APIHosts: []string{"api.guangyapan.com"}}}},
	}
}

type providerBase struct{ info model.ProviderInfo }

func (p providerBase) Info() model.ProviderInfo { return p.info }
func (p providerBase) Transfer(context.Context, *APIClient, *Session, model.Source, []model.File, string) (*model.TransferReceipt, error) {
	return nil, Fail("unsupported", "此网盘暂不支持可核验所有权的自动转存，请在浏览器转存后同步个人网盘")
}
func (p providerBase) Cleanup(context.Context, *APIClient, *Session, *model.TransferReceipt) error {
	return Fail("unsupported", "此网盘暂不支持自动清理转存副本")
}

type object map[string]any

func obj(v any) object {
	switch x := v.(type) {
	case map[string]any:
		return object(x)
	case object:
		return x
	}
	return nil
}
func at(v any, keys ...string) any {
	for _, k := range keys {
		v = obj(v)[k]
	}
	return v
}
func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}
func num(v any) int64   { n, _ := strconv.ParseInt(str(v), 10, 64); return n }
func yes(v any) bool    { return v == true || str(v) == "1" || str(v) == "true" }
func items(v any) []any { a, _ := v.([]any); return a }
func validCollections(v object, keys ...string) error {
	found := false
	for _, k := range keys {
		if x, ok := v[k]; ok {
			found = true
			if _, ok = x.([]any); !ok {
				return Fail("invalid_response", "网盘目录集合格式无效")
			}
		}
	}
	if !found {
		return Fail("invalid_response", "网盘响应缺少目录集合")
	}
	return nil
}
func first(v any, keys ...string) string {
	for _, k := range keys {
		if s := str(obj(v)[k]); s != "" {
			return s
		}
	}
	return ""
}
func decodeObject(b json.RawMessage) (object, error) {
	var v object
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if d.Decode(&v) != nil || v == nil {
		return nil, Fail("invalid_response", "网盘未返回有效对象")
	}
	return v, nil
}
func apiJSON(ctx context.Context, c *APIClient, method, raw string, body any, h map[string]string) (object, error) {
	var b json.RawMessage
	if e := c.JSON(ctx, method, raw, body, h, &b); e != nil {
		return nil, e
	}
	return decodeObject(b)
}
func apiForm(ctx context.Context, c *APIClient, raw string, body url.Values, h map[string]string) (object, error) {
	var b json.RawMessage
	if e := c.Form(ctx, raw, body, h, &b); e != nil {
		var typed *Error
		if errors.As(e, &typed) {
			return nil, e
		}
		return nil, Fail("invalid_response", "网盘表单请求失败或响应无效")
	}
	return decodeObject(b)
}
func query(raw string, v url.Values) string {
	u, _ := url.Parse(raw)
	q := u.Query()
	for k, vs := range v {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}
func values(m map[string]string) url.Values {
	v := url.Values{}
	for k, x := range m {
		v.Set(k, x)
	}
	return v
}
func pageNumber(cursor string, base int) (int, error) {
	if cursor == "" {
		return base, nil
	}
	n, e := strconv.Atoi(cursor)
	if e != nil || n < base || n > 1000000 {
		return 0, Fail("invalid_pagination", "无效的网盘分页游标")
	}
	return n, nil
}
func sessionValue(s *Session, keys ...string) string {
	if s == nil {
		return ""
	}
	for _, k := range keys {
		if x := s.Capture.Tokens[k]; x != "" {
			return x
		}
		if x := s.Capture.Share[k]; x != "" {
			return x
		}
		for h, x := range s.Capture.Headers {
			if strings.EqualFold(h, k) && x != "" {
				return x
			}
		}
	}
	return ""
}
func remember(s *Session, k, v string) {
	if s != nil && v != "" {
		if s.Capture.Share == nil {
			s.Capture.Share = map[string]string{}
		}
		s.Capture.Share[k] = v
	}
}
func cookieValue(s *Session, name string) string {
	if s != nil {
		for _, c := range s.Capture.Cookies {
			if c.Name == name {
				return c.Value
			}
		}
	}
	return ""
}
func bearer(s *Session) string {
	v := sessionValue(s, "Authorization", "access_token", "accessToken", "token")
	if v != "" && !strings.ContainsAny(v, " \t") {
		v = "Bearer " + v
	}
	return v
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func requestHeaders(s *Session, src model.Source) map[string]string {
	h := map[string]string{"User-Agent": browserUA}
	if u, e := url.Parse(src.URL); e == nil && u.Host != "" {
		h["Referer"] = u.Scheme + "://" + u.Host + "/"
		h["Origin"] = u.Scheme + "://" + u.Host
	}
	return h
}
func checkFile(f model.File) error {
	if f.IsDir {
		return Fail("not_file", "请选择单个文件获取下载链接")
	}
	return validateFile(f)
}
func checkResponse(r object, field string, success ...string) error {
	v, ok := r[field]
	if !ok {
		return Fail("invalid_response", "网盘响应缺少状态码")
	}
	code := str(v)
	for _, s := range success {
		if code == s {
			return nil
		}
	}
	return responseError(code)
}
func responseError(code string) error {
	switch strings.ToLower(code) {
	case "112", "9019", "401", "-6", "accesstokeninvalid", "sharelinktokeninvalid", "invalidsessionkey", "9", "invalid_token":
		return Fail("expired", "网盘登录或分享凭据已过期，请重新同步浏览器")
	case "31001", "23018", "5112", "207", "40101", "unauthorized":
		return Fail("login_required", "请在浏览器登录网盘后重新同步")
	case "sharenotfoundflatdir", "save_required":
		return TransferRequired()
	case "sharelinknotfound", "sharelinkcancelled", "sharelinkexpired", "not_found":
		return Fail("share_expired", "分享不存在、已取消或已过期")
	case "-9", "sharepwdnotmatch", "password_error":
		return Fail("password_required", "提取码不正确，请在浏览器验证分享")
	case "5113":
		return Fail("quota_exceeded", "网盘下载流量不足")
	case "132", "8001":
		return Fail("verification_required", "网盘需要浏览器验证，请完成验证后重新同步")
	}
	return Fail("api_error", "网盘接口拒绝操作（状态码 "+code+"）")
}
func download(c *APIClient, s *Session, f model.File, raw string, h map[string]string, expiry any) (*Download, error) {
	raw = strings.ReplaceAll(raw, "&amp;", "&")
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, Fail("invalid_response", "网盘未返回有效的单文件下载链接")
	}
	d := &Download{URL: u.String(), Header: h, File: adapterPublicFile(f)}
	if d.Header == nil {
		d.Header = map[string]string{}
	}
	if t, e := time.Parse(time.RFC3339, str(expiry)); e == nil {
		d.ExpiresAt = t
	} else if n := num(expiry); n > 0 {
		if n > 1e12 {
			n /= 1000
		}
		d.ExpiresAt = time.Unix(n, 0)
	}
	if d.ExpiresAt.IsZero() {
		for _, k := range []string{"Expires", "expires", "expire"} {
			if n, e := strconv.ParseInt(u.Query().Get(k), 10, 64); e == nil && n >= 0 {
				if n > 1e12 {
					n /= 1000
				}
				d.ExpiresAt = time.Unix(n, 0)
				break
			}
		}
		if d.ExpiresAt.IsZero() {
			for _, pfx := range []string{"x-oss-", "X-Amz-"} {
				ttl := u.Query().Get(pfx + "expires")
				if ttl == "" {
					ttl = u.Query().Get(pfx + "Expires")
				}
				if n, e := strconv.ParseInt(ttl, 10, 64); e == nil && n >= 0 {
					stamp := u.Query().Get(pfx + "date")
					if stamp == "" {
						stamp = u.Query().Get(pfx + "Date")
					}
					if start, e := time.Parse("20060102T150405Z", stamp); e == nil {
						d.ExpiresAt = start.Add(time.Duration(n) * time.Second)
					}
				}
			}
		}
	}
	if !d.ExpiresAt.IsZero() && !time.Now().Before(d.ExpiresAt) {
		return nil, Fail("expired", "网盘返回的下载链接已过期，请重新同步浏览器")
	}
	// Keep cookies structured and scoped. Merge updated jar cookies only on their
	// own host, rather than exporting a raw Cookie header to an unrelated CDN.
	if s != nil {
		for _, x := range s.Capture.Cookies {
			if x.Domain != "" && HostMatches(u.Hostname(), x.Domain) {
				d.Cookies = append(d.Cookies, x)
			}
		}
	}
	if c != nil && c.Client != nil && c.Client.Jar != nil {
		for _, x := range c.Client.Jar.Cookies(u) {
			found := false
			for i := range d.Cookies {
				if d.Cookies[i].Name == x.Name {
					d.Cookies[i].Value = x.Value
					found = true
				}
			}
			if !found {
				d.Cookies = append(d.Cookies, model.Cookie{Name: x.Name, Value: x.Value, Domain: u.Hostname(), Path: "/", Secure: u.Scheme == "https"})
			}
		}
	}
	return d, nil
}
func mappedFile(f model.File, id, folder string) model.File {
	original := fSourceID(f)
	f.ID = id
	f.Meta = map[string]string{"source_id": original, "parent_id": folder}
	return f
}
func fSourceID(f model.File) string {
	if f.Meta["source_id"] != "" {
		return f.Meta["source_id"]
	}
	return f.ID
}
func receipt(p Provider, s *Session, src model.Source, batch, folder string, files []model.File) *model.TransferReceipt {
	r := &model.TransferReceipt{Provider: p.Info().ID, AccountID: s.Capture.AccountID, Source: src, BatchID: batch, FolderID: folder, Files: files}
	for _, f := range files {
		r.CreatedIDs = append(r.CreatedIDs, f.ID)
	}
	return r
}
func transferInput(s *Session, src model.Source, files []model.File, batch string) error {
	if s == nil || s.Capture.AccountID == "" {
		return Fail("login_required", "转存需要同步可确认的登录账号")
	}
	if src.ShareID == "" || len(files) == 0 || batch == "" {
		return Fail("invalid_transfer", "转存缺少分享、文件或批次身份")
	}
	seen := map[string]bool{}
	for _, f := range files {
		if e := checkFile(f); e != nil {
			return e
		}
		if seen[f.ID] {
			return Fail("invalid_transfer", "转存文件身份重复")
		}
		seen[f.ID] = true
	}
	return nil
}
func folderName(batch string) string { return "__gopeed_" + batch }
func pollPause(ctx context.Context) error {
	t := time.NewTimer(250 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Verify receipt identity and remote directory contents before any deletion.
// A folder with foreign additions is preserved in full for manual review.
func ownedCleanup(ctx context.Context, p Provider, c *APIClient, s *Session, r *model.TransferReceipt, root string) error {
	if r == nil || s == nil || r.Provider != p.Info().ID || r.AccountID == "" || r.AccountID != s.Capture.AccountID || r.FolderID == "" || r.BatchID == "" || len(r.Files) != len(r.CreatedIDs) {
		return Fail("ownership_error", "无法确认临时副本属于当前账号和批次")
	}
	if len(r.Remaining) > 0 {
		return Fail("in_use", "临时副本仍有下载未完成")
	}
	ids := map[string]bool{}
	for _, id := range r.CreatedIDs {
		if id == "" || id == r.FolderID || ids[id] {
			return Fail("ownership_error", "转存记录中的创建身份无效")
		}
		ids[id] = true
	}
	for _, f := range r.Files {
		if !ids[f.ID] || f.Meta["source_id"] == "" || f.Meta["source_id"] == f.ID || f.Meta["parent_id"] != r.FolderID {
			return Fail("ownership_error", "清理记录未证明文件是本批次创建的副本")
		}
	}
	src := r.Source
	src.ShareID = ""
	src.ParentID = root
	found := false
	cur := ""
	seen := map[string]bool{}
	for {
		if seen[cur] {
			return Fail("invalid_pagination", "目录分页重复")
		}
		seen[cur] = true
		l, e := p.List(ctx, c, s, src, cur)
		if e != nil {
			return e
		}
		for _, f := range l.Files {
			if f.ID == r.FolderID {
				if !f.IsDir || f.Name != folderName(r.BatchID) {
					return Fail("ownership_error", "临时目录名称或身份已变化")
				}
				found = true
			}
		}
		cur = l.NextPage
		if cur == "" {
			break
		}
	}
	if !found {
		return Fail("ownership_error", "当前网盘中找不到本批次专用目录")
	}
	src.ParentID = r.FolderID
	cur = ""
	seen = map[string]bool{}
	for {
		if seen[cur] {
			return Fail("invalid_pagination", "目录分页重复")
		}
		seen[cur] = true
		l, e := p.List(ctx, c, s, src, cur)
		if e != nil {
			return e
		}
		for _, f := range l.Files {
			if !ids[f.ID] || f.IsDir {
				return Fail("ownership_error", "专用目录包含非本批次文件，已保留全部副本")
			}
		}
		cur = l.NextPage
		if cur == "" {
			break
		}
	}
	return nil
}
func requireMapped(original model.File, id string) error {
	if id == "" || id == original.ID {
		return Fail("invalid_transfer", fmt.Sprintf("转存未返回文件 %s 的实际新身份，临时目录已保留", original.Name))
	}
	return nil
}

func adapterPublicFile(f model.File) model.File {
	meta := map[string]string{}
	for _, k := range []string{"drive_id", "parent_id", "source_id", "remote_path", "space", "etag", "s3key_flag"} {
		if v := f.Meta[k]; v != "" {
			meta[k] = v
		}
	}
	f.Meta = meta
	return f
}
func recordCreated(r *model.TransferReceipt, f model.File, id string) error {
	if e := requireMapped(f, id); e != nil {
		return e
	}
	for _, x := range r.Files {
		if x.ID == id {
			if x.Meta["source_id"] == fSourceID(f) {
				return nil
			}
			return Fail("invalid_transfer", "不同分享文件返回了相同的副本身份，临时目录已保留")
		}
	}
	r.Files = append(r.Files, mappedFile(f, id, r.FolderID))
	r.CreatedIDs = append(r.CreatedIDs, id)
	return nil
}
