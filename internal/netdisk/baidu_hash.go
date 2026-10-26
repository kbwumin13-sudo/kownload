package netdisk

import (
	"encoding/hex"
	"strings"
)

// BaiduMD5 converts the web API's obfuscated digest to its ordinary MD5.
// Reference: OpenListTeam/OpenList drivers/baidu_netdisk/util.go DecryptMd5.
// The marker at byte 9 distinguishes this format from an ordinary hex digest.
func BaiduMD5(raw string) (string, error) {
	if len(raw) != 32 {
		return "", Fail("invalid_response", "百度文件校验信息无效")
	}
	if _, err := hex.DecodeString(raw); err == nil {
		return strings.ToLower(raw), nil
	}
	raw = strings.ToLower(raw)
	if raw[9] < 'g' || raw[9] > 'v' {
		return "", Fail("invalid_response", "百度文件校验信息无效")
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 32)
	for i := range raw {
		n := strings.IndexByte(digits, raw[i])
		if i == 9 {
			n = int(raw[i] - 'g')
		}
		if n < 0 {
			return "", Fail("invalid_response", "百度文件校验信息无效")
		}
		out[i] = digits[n^(i&15)]
	}
	return string(out[8:16]) + string(out[:8]) + string(out[24:]) + string(out[16:24]), nil
}
