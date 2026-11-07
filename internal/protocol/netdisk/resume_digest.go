package netdisk

import (
	"strings"

	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// The HTTP child may use the same digest that verifyFile enforces at completion.
// A provider file ID or a matching length alone is not a content validator.
func resumeDigest(e *model.ReqExtra, resource *base.Resource) string {
	digest := e.File.Hash
	if e.Provider == "baidu" {
		if value, ok := storedResponseMD5(resource.Hash); ok {
			return "md5:" + value
		}
		if len(digest) == 32 {
			value, err := cloud.BaiduMD5(digest)
			if err != nil {
				return ""
			}
			digest = value
		}
	}
	if strings.Contains(digest, ":") {
		return digest
	}
	if len(digest) == 32 && (e.Provider == "baidu" || e.Provider == "quark" || e.Provider == "uc" || e.Provider == "tcloud") {
		return "md5:" + digest
	}
	if len(digest) == 40 && e.Provider == "aliyun" {
		return "sha1:" + digest
	}
	return ""
}
