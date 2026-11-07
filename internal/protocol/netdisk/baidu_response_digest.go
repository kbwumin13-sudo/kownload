package netdisk

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Baidu's listing MD5 can be stale even after deobfuscation. BaiduPCS-Go's
// GetFileRawInfo instead reads Content-MD5 from the file download response.
// Accept it only for a verified, complete representation from Baidu itself.
type baiduResponseDigest struct {
	enabled bool
	total   int64
	mu      sync.Mutex
	digest  string
}

func trustedBaiduDownload(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil &&
		(cloud.HostMatches(u.Hostname(), "baidu.com") || cloud.HostMatches(u.Hostname(), "baidupcs.com"))
}

func (d *baiduResponseDigest) policy(req *http.Request, res *http.Response) error {
	if err := rangeResponse(d.total)(req, res); err != nil {
		return err
	}
	if !d.enabled || d.total <= 0 || req.Method != http.MethodGet || !trustedBaiduDownload(req.URL) ||
		(res.Request != nil && !trustedBaiduDownload(res.Request.URL)) ||
		res.TLS == nil || len(res.TLS.VerifiedChains) == 0 ||
		req.Header.Get("Range") != "" || res.StatusCode != http.StatusOK ||
		res.ContentLength != d.total || res.Header.Get("Content-Range") != "" || res.Uncompressed {
		return nil
	}
	if encoding := res.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil
	}
	values := res.Header.Values("Content-MD5")
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return errors.New("网盘下载响应包含冲突的MD5校验信息")
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return nil
	}
	digest, ok := parseResponseMD5(raw)
	if !ok {
		return errors.New("网盘下载响应的MD5校验信息无效")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.digest != "" && d.digest != digest {
		return errors.New("网盘下载响应的MD5校验信息不一致")
	}
	d.digest = digest
	return nil
}

func (d *baiduResponseDigest) value() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.digest
}

func parseResponseMD5(raw string) (string, bool) {
	if len(raw) == 32 {
		if b, err := hex.DecodeString(raw); err == nil {
			return hex.EncodeToString(b), true
		}
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) != 16 {
		return "", false
	}
	return hex.EncodeToString(b), true
}

func storedResponseMD5(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "md5:") || len(raw) != 36 {
		return "", false
	}
	return parseResponseMD5(raw[4:])
}
