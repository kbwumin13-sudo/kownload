package http

import (
	"strings"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// IsExpiredLinkError also understands the completion error's status text so
// wrappers keep the build 25 refresh contract when a stream's 403 was retained.
func IsExpiredLinkError(err error) bool {
	if err == nil {
		return false
	}
	if re := extractRequestError(err); re != nil {
		return transfer.IsExpiredLinkStatus(re.Code)
	}
	text := err.Error()
	for _, marker := range []string{"status=401", "status=403", "status=410", "status=412"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
