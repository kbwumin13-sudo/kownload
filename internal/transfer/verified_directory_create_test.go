//go:build darwin || linux

package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func directoryCreateFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func directoryCreateOps() completionDirectoryCreateOps {
	return completionDirectoryCreateOps{openat: unix.Openat, mkdirat: unix.Mkdirat, sync: unix.Fsync}
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatal("unexpected external or invalid-path writes", err, len(entries))
	}
}

func assertDirectoryCreateFDsClosed(t *testing.T, descriptors []int) {
	t.Helper()
	for _, fd := range descriptors {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatal("failure leaked descriptor", fd, err)
		}
	}
}

func TestVerifiedDirectoryCreateNestedSyncAndStagingVolume(t *testing.T) {
	base := directoryCreateFixture(t)
	root := filepath.Join(base, "new", "leaf")
	ops := directoryCreateOps()
	var synced []CompletionFileIdentity
	ops.sync = func(fd int) error {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		synced = append(synced, CompletionFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)})
		return unix.Fsync(fd)
	}
	d, err := createCompletionDirectoryWith(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(synced) != 4 || synced[0] != synced[3] || synced[2] != d.Identity() || synced[0] == synced[1] {
		t.Fatal("new child/parent sync order or final identity", synced)
	}
	for _, path := range []string{filepath.Join(base, "new"), root} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			t.Fatal("new directory is not private", err)
		}
	}
	flags, err := unix.FcntlInt(d.handle.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("directory descriptor is not close-on-exec", err)
	}
	stage, err := d.CreateStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	id, err := CompletionIdentityOf(stage)
	if err != nil || id.Inode == 0 || id.Device != d.Identity().Device {
		t.Fatal("staging did not use pinned same-volume directory", err)
	}
	r := pinnedStagingRecord(t, d, ".stage", stage)
	writer, err := d.OpenStagingWriter(r)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
}

func TestVerifiedDirectoryCreateExistingPermissionsAndInvalidPaths(t *testing.T) {
	base := directoryCreateFixture(t)
	root := filepath.Join(base, "existing")
	if err := os.Mkdir(root, 0751); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := CreateCompletionDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	after, err := os.Stat(root)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("existing directory was replaced or chmodded", err)
	}
	empty := directoryCreateFixture(t)
	for _, path := range []string{"", "relative/leaf", empty + "/new/../leaf", empty + "/new/", empty + "/new/\x00/leaf"} {
		if d, err := CreateCompletionDirectory(path); err == nil {
			d.Close()
			t.Fatal("invalid root accepted")
		}
	}
	assertDirectoryEmpty(t, empty)
}

func TestVerifiedDirectoryCreateSymlinkReplacementsPreserveExternal(t *testing.T) {
	for _, position := range []string{"parent", "leaf"} {
		t.Run(position, func(t *testing.T) {
			base := directoryCreateFixture(t)
			approved, outside := filepath.Join(base, "approved"), filepath.Join(base, "outside")
			for _, path := range []string{approved, outside} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			root, replaced := filepath.Join(approved, "missing", "leaf"), approved
			if position == "leaf" {
				root = filepath.Join(approved, "leaf")
				replaced = root
				if err := os.Mkdir(replaced, 0700); err != nil {
					t.Fatal(err)
				}
			}
			// Canonical root was frozen before this namespace replacement.
			if err := os.Rename(replaced, replaced+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, replaced); err != nil {
				t.Fatal(err)
			}
			d, createErr := CreateCompletionDirectory(root)
			if d != nil {
				d.Close()
			}
			// A failure is insufficient if MkdirAll already wrote outside.
			assertDirectoryEmpty(t, outside)
			if createErr == nil || d != nil {
				t.Fatal("symlink replacement was accepted")
			}
		})
	}
}

func TestVerifiedDirectoryCreatePinnedParentCannotRedirectWrites(t *testing.T) {
	base := directoryCreateFixture(t)
	parent, retained, outside := filepath.Join(base, "parent"), filepath.Join(base, "retained"), filepath.Join(base, "outside")
	for _, path := range []string{parent, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ops := directoryCreateOps()
	replaced := false
	ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		opened, err := unix.Openat(fd, name, flags, mode)
		if err == nil && name == "parent" && !replaced {
			replaced = true
			if err := os.Rename(parent, retained); err != nil {
				unix.Close(opened)
				return -1, err
			}
			if err := os.Symlink(outside, parent); err != nil {
				unix.Close(opened)
				return -1, err
			}
		}
		return opened, err
	}
	d, err := createCompletionDirectoryWith(filepath.Join(parent, "new", "leaf"), ops)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stage, err := d.CreateStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	stage.Close()
	assertDirectoryEmpty(t, outside)
	info, err := os.Stat(filepath.Join(retained, "new", "leaf", ".stage"))
	if !replaced || err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatal("did not keep the anchored parent", err)
	}
}

func TestVerifiedDirectoryCreateConcurrentEntryWinner(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			base := directoryCreateFixture(t)
			root, outside := filepath.Join(base, "winner"), filepath.Join(base, "outside")
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			ops := directoryCreateOps()
			var winnerMode os.FileMode
			ops.mkdirat = func(fd int, name string, mode uint32) error {
				// The competing creator wins between our ENOENT and mkdir.
				done := make(chan error, 1)
				go func() {
					var err error
					switch kind {
					case "directory":
						err = os.Mkdir(root, 0751)
					case "symlink":
						err = os.Symlink(outside, root)
					case "file":
						err = os.WriteFile(root, []byte("foreign"), 0600)
					}
					done <- err
				}()
				if err := <-done; err != nil {
					return err
				}
				info, err := os.Lstat(root)
				if err != nil {
					return err
				}
				winnerMode = info.Mode()
				return unix.EEXIST
			}
			syncs := 0
			ops.sync = func(fd int) error { syncs++; return unix.Fsync(fd) }
			d, err := createCompletionDirectoryWith(root, ops)
			if kind == "directory" {
				if err != nil || d == nil || syncs != 2 {
					t.Fatal("ordinary concurrent directory was not durably pinned", err, syncs)
				}
				d.Close()
			} else if err == nil || d != nil || syncs != 0 {
				if d != nil {
					d.Close()
				}
				t.Fatal("non-directory concurrent winner accepted", err)
			}
			info, statErr := os.Lstat(root)
			if statErr != nil || info.Mode() != winnerMode {
				t.Fatal("concurrent winner removed or chmodded", statErr)
			}
			assertDirectoryEmpty(t, outside)
		})
	}
}

func TestVerifiedDirectoryCreateSyncFailureRetainsAndCloses(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run([]string{"child", "parent"}[failAt-1], func(t *testing.T) {
			base := directoryCreateFixture(t)
			root := filepath.Join(base, "new")
			ops := directoryCreateOps()
			var descriptors []int
			ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
				opened, err := unix.Openat(fd, name, flags, mode)
				if err == nil {
					descriptors = append(descriptors, opened)
				}
				return opened, err
			}
			calls := 0
			ops.sync = func(fd int) error {
				calls++
				if calls == failAt {
					return unix.EIO
				}
				return unix.Fsync(fd)
			}
			d, err := createCompletionDirectoryWith(root, ops)
			var typed *VerifiedCompletionError
			if d != nil || !errors.As(err, &typed) || typed.Kind != CompletionIO {
				t.Fatal("sync failure returned directory permission", err)
			}
			assertDirectoryCreateFDsClosed(t, descriptors)
			info, err := os.Stat(root)
			if err != nil || !info.IsDir() {
				t.Fatal("unconfirmed directory was removed", err)
			}
			assertDirectoryEmpty(t, root)
		})
	}
}

func TestVerifiedDirectoryCreateLaterMkdirFailureRetainsParent(t *testing.T) {
	base := directoryCreateFixture(t)
	ops := directoryCreateOps()
	ops.mkdirat = func(fd int, name string, mode uint32) error {
		if name == "leaf" {
			return unix.EIO
		}
		return unix.Mkdirat(fd, name, mode)
	}
	d, err := createCompletionDirectoryWith(filepath.Join(base, "new", "leaf"), ops)
	if d != nil || err == nil {
		t.Fatal("mkdir failure returned directory permission", err)
	}
	assertDirectoryEmpty(t, filepath.Join(base, "new"))
}
