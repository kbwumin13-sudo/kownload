//go:build darwin || linux

package transfer

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Per-call syscall functions allow deterministic filesystem races and Sync
// failures in tests; no global hooks or mutable production policy are used.
type completionDirectoryCreateOps struct {
	openat  func(int, string, int, uint32) (int, error)
	mkdirat func(int, string, uint32) error
	sync    func(int) error
}

func createCompletionDirectory(root string) (*CompletionDirectory, error) {
	return createCompletionDirectoryWith(root, completionDirectoryCreateOps{
		openat: unix.Openat, mkdirat: unix.Mkdirat, sync: unix.Fsync,
	})
}

func createCompletionDirectoryWith(root string, ops completionDirectoryCreateOps) (*CompletionDirectory, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	current, err := ops.openat(unix.AT_FDCWD, "/", flags, 0)
	if err != nil {
		return nil, completionError(CompletionIO, err)
	}
	defer func() {
		if current >= 0 {
			_ = unix.Close(current)
		}
	}()
	for _, component := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if component == "" {
			continue
		}
		next, openErr := ops.openat(current, component, flags, 0)
		needsSync := false
		if errors.Is(openErr, unix.ENOENT) {
			if err := ops.mkdirat(current, component, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, completionError(CompletionIO, err)
			}
			// A concurrent mkdir winner is accepted only after the same
			// NOFOLLOW directory open. A file/symlink winner is rejected.
			next, openErr = ops.openat(current, component, flags, 0)
			needsSync = true
		}
		if openErr != nil {
			return nil, completionError(CompletionIdentity, openErr)
		}
		if needsSync {
			if err := ops.sync(next); err != nil {
				_ = unix.Close(next)
				return nil, completionError(CompletionIO, err)
			}
			if err := ops.sync(current); err != nil {
				_ = unix.Close(next)
				return nil, completionError(CompletionIO, err)
			}
		}
		_ = unix.Close(current)
		current = next
	}
	file := os.NewFile(uintptr(current), "completion-directory")
	current = -1 // Ownership moves to file, and then to the caller on success.
	identity, err := CompletionIdentityOf(file)
	if err != nil || identity.Inode == 0 {
		_ = file.Close()
		return nil, completionError(CompletionIdentity, err)
	}
	return &CompletionDirectory{handle: file, identity: identity}, nil
}
