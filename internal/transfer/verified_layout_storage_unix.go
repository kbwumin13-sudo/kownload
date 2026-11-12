//go:build darwin || linux

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Every traversal starts at the borrowed root fd. No pathname of that root is
// reopened for output, and no symlink component is followed.
func openLayoutDirectory(root *CompletionDirectory, parts []string, create bool) (*CompletionDirectory, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	current, err := unix.Openat(int(root.handle.Fd()), ".", flags, 0)
	if err != nil {
		return nil, completionError(CompletionIO, err)
	}
	defer func() {
		if current >= 0 {
			_ = unix.Close(current)
		}
	}()
	for _, part := range parts {
		next, openErr := unix.Openat(current, part, flags, 0)
		created := false
		if create && errors.Is(openErr, unix.ENOENT) {
			if err := unix.Mkdirat(current, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, completionError(CompletionIO, err)
			}
			next, openErr = unix.Openat(current, part, flags, 0)
			created = true
		}
		if openErr != nil {
			return nil, completionError(CompletionIdentity, openErr)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(next, &stat); err != nil || stat.Ino == 0 || uint64(stat.Dev) != root.identity.Device {
			_ = unix.Close(next)
			return nil, completionError(CompletionIdentity, err)
		}
		if created {
			if err := unix.Fsync(next); err != nil {
				_ = unix.Close(next)
				return nil, completionError(CompletionIO, err)
			}
			if err := unix.Fsync(current); err != nil {
				_ = unix.Close(next)
				return nil, completionError(CompletionIO, err)
			}
		}
		_ = unix.Close(current)
		current = next
	}
	file := os.NewFile(uintptr(current), "verified-layout-directory")
	current = -1
	identity, err := CompletionIdentityOf(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &CompletionDirectory{handle: file, identity: identity}, nil
}

func checkLayoutFile(file *os.File, expected CompletionFileIdentity) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return completionError(CompletionIdentity, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || uint64(stat.Dev) != expected.Device || uint64(stat.Ino) != expected.Inode {
		return completionError(CompletionIdentity, nil)
	}
	return nil
}
