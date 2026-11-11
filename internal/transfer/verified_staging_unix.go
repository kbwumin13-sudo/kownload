//go:build darwin || linux

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func (d *CompletionDirectory) createStaging(name string) (*os.File, error) {
	fd, err := unix.Openat(int(d.handle.Fd()), name,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, completionError(CompletionConflict, err)
		}
		return nil, completionError(CompletionIO, err)
	}
	return d.singleLinkStagingHandle(fd)
}

func (d *CompletionDirectory) openStagingWriter(name string) (*os.File, error) {
	fd, err := unix.Openat(int(d.handle.Fd()), name,
		unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, completionError(CompletionIdentity, err)
	}
	return d.singleLinkStagingHandle(fd)
}

func (d *CompletionDirectory) singleLinkStagingHandle(fd int) (*os.File, error) {
	file := os.NewFile(uintptr(fd), "completion-staging")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, completionError(CompletionIO, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Ino == 0 || uint64(stat.Dev) != d.identity.Device {
		_ = file.Close()
		return nil, completionError(CompletionIdentity, nil)
	}
	return file, nil
}
