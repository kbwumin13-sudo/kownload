package transfer

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func setCompletionDescriptorTimes(file *os.File, accessedAt, modifiedAt time.Time) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return completionError(CompletionIO, err)
	}
	if stat.Nlink != 1 {
		return completionError(CompletionIdentity, nil)
	}
	var values [2]unix.Timespec
	for i, value := range []time.Time{accessedAt, modifiedAt} {
		ts, err := unix.TimeToTimespec(value)
		if err != nil {
			return completionError(CompletionInvalid, err)
		}
		values[i] = ts
	}
	// x/sys Futimes uses /proc/self/fd on Linux. Use the descriptor-only
	// utimensat form instead; older kernels/filesystems fail without fallback.
	if err := unix.UtimesNanoAt(int(file.Fd()), "", values[:], unix.AT_EMPTY_PATH); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOENT) {
			return completionError(CompletionUnsupported, err)
		}
		return completionError(CompletionIO, err)
	}
	return nil
}
