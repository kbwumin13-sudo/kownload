package rest

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// Missing keys in old clients preserve the one stored setting. Unknown settings
// can round-trip unchanged while another setting is edited, never be silently
// reset or reinterpreted as this version's rule snapshot.
func preparePathPolicyConfigUpdate(previous, next *base.DownloaderStoreConfig) error {
	if next == nil {
		return errors.New("invalid_path_policy")
	}
	var old any
	var present bool
	if previous != nil {
		old, present = previous.Extra["pathPolicyV1"]
	}
	if _, provided := next.Extra["pathPolicyV1"]; !provided && present {
		extra := make(map[string]any, len(next.Extra)+1)
		for key, value := range next.Extra {
			extra[key] = value
		}
		extra["pathPolicyV1"] = old
		next.Extra = extra
	}
	if previous != nil {
		if _, err := pathpolicy.ReadConfig(previous.Extra); err != nil {
			a, ea := json.Marshal(old)
			b, eb := json.Marshal(next.Extra["pathPolicyV1"])
			if ea != nil || eb != nil || !bytes.Equal(a, b) {
				return errors.New("path_policy_unsupported")
			}
			return nil
		}
	}
	if _, err := pathpolicy.ReadConfig(next.Extra); err != nil {
		return errors.New("invalid_path_policy")
	}
	return nil
}
