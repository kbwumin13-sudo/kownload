package transfer

import (
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
	var values [2]unix.Timeval
	for i, value := range []time.Time{accessedAt, modifiedAt} {
		ts, err := unix.TimeToTimespec(value)
		if err != nil {
			return completionError(CompletionInvalid, err)
		}
		values[i] = unix.Timeval{Sec: ts.Sec, Usec: int32(ts.Nsec / 1000)}
	}
	// On Darwin Futimes is the fd syscall, not a /dev/fd pathname adapter.
	if err := unix.Futimes(int(file.Fd()), values[:]); err != nil {
		return completionError(CompletionIO, err)
	}
	return nil
}
