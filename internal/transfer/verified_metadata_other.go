//go:build !darwin && !linux

package transfer

import (
	"os"
	"time"
)

func setCompletionDescriptorTimes(*os.File, time.Time, time.Time) error {
	return completionError(CompletionUnsupported, nil)
}
