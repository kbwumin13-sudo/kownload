//go:build darwin || linux

package transfer

import (
	"golang.org/x/sys/unix"
	"os"
)

func openPathWorkspaceDirectory(root *CompletionDirectory, name string, previous *CompletionFileIdentity) (*CompletionDirectory, error) {
	if previous == nil {
		// An unacknowledged pre-existing directory is never adopted.
		if err := unix.Mkdirat(int(root.handle.Fd()), name, 0700); err != nil {
			return nil, completionError(CompletionConflict, err)
		}
		if err := root.handle.Sync(); err != nil {
			return nil, err
		}
	}
	d, err := openLayoutDirectory(root, []string{name}, false)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err = unix.Fstat(int(d.handle.Fd()), &stat); err != nil || stat.Uid != uint32(os.Getuid()) || stat.Mode&0022 != 0 || (previous != nil && d.Identity() != *previous) {
		d.Close()
		return nil, completionError(CompletionIdentity, err)
	}
	return d, nil
}
func linkPathWorkspaceFile(work, root *CompletionDirectory, name string) error {
	if err := unix.Linkat(int(work.handle.Fd()), name, int(root.handle.Fd()), name, 0); err != nil {
		return completionError(CompletionConflict, err)
	}
	return nil
}
