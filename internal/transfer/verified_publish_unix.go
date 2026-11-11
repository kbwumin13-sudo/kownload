//go:build darwin || linux

package transfer

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// VerifiedCompletionPlatformSupported reports the compiled adapter only. A
// particular filesystem can still reject hard links or directory Sync; that
// failure never falls back to replacement or grants completion permission.
const VerifiedCompletionPlatformSupported = true

func openCompletionDirectory(path string) (*CompletionDirectory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, completionError(CompletionInvalid, nil)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, completionError(CompletionIO, err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, completionError(CompletionIdentity, openErr)
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), "completion-directory")
	identity, err := completionIdentityOf(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &CompletionDirectory{handle: file, identity: identity}, nil
}

func completionIdentityOf(file *os.File) (CompletionFileIdentity, error) {
	if file == nil {
		return CompletionFileIdentity{}, completionError(CompletionInvalid, nil)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return CompletionFileIdentity{}, completionError(CompletionIO, err)
	}
	return CompletionFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func completionReadOnly(file *os.File) error {
	if file == nil {
		return completionError(CompletionInvalid, nil)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return completionError(CompletionIO, err)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return completionError(CompletionInvalid, nil)
	}
	return nil
}

func (d *CompletionDirectory) openFile(name string) (*os.File, error) {
	if !completionName(name) {
		return nil, completionError(CompletionInvalid, nil)
	}
	fd, err := unix.Openat(int(d.handle.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, completionError(CompletionIdentity, err)
	}
	file := os.NewFile(uintptr(fd), "completion-file")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, completionError(CompletionIdentity, err)
	}
	return file, nil
}

// linkat never replaces an existing final, including a dangling symlink. Both
// names are single components relative to the pinned directory descriptor. The
// caller owns the staging namespace and must keep writers/path mutators stopped.
func (d *CompletionDirectory) link(staging, final string) error {
	if !completionName(staging) || !completionName(final) {
		return completionError(CompletionInvalid, nil)
	}
	if err := unix.Linkat(int(d.handle.Fd()), staging, int(d.handle.Fd()), final, 0); err != nil {
		if err == unix.EEXIST {
			return completionError(CompletionConflict, os.ErrExist)
		}
		if err == unix.ENOTSUP || err == unix.EOPNOTSUPP || err == unix.EXDEV {
			return completionError(CompletionUnsupported, err)
		}
		return completionError(CompletionIO, err)
	}
	return nil
}
