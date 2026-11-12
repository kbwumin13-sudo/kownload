//go:build darwin && cgo

package transfer

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// Every chmod, rename and cleanup in these tests targets only this test's tree.
func verifiedRootFixture(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		t.Fatal(err)
	}
	var name []byte
	for _, b := range fs.Fstypename {
		if b == 0 {
			break
		}
		name = append(name, byte(b))
	}
	if string(name) != "apfs" {
		t.Fatal("real ACL fixture requires APFS")
	}
	verifiedRootChmod(t, path, "-N")
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func verifiedRootChmod(t *testing.T, path string, args ...string) {
	t.Helper()
	args = append(args, path)
	// Do not print a username/qualifier or OS diagnostic on failure.
	if err := exec.Command("/bin/chmod", args...).Run(); err != nil {
		t.Fatal("private ACL fixture chmod failed")
	}
}

func verifiedRootAddACL(t *testing.T, path, entry string) {
	t.Helper()
	verifiedRootChmod(t, path, "+a", entry)
	t.Cleanup(func() { verifiedRootChmod(t, path, "-N") })
}

func verifiedRootOpenFixture(t *testing.T, path string) (*VerifiedPolicyRoot, *VerifiedRootLease) {
	t.Helper()
	r, err := OpenVerifiedPolicyRoot("synthetic-root", path)
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.Borrow()
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r, l
}

func TestVerifiedRootActualExistingModeAndIdentity(t *testing.T) {
	path := verifiedRootFixture(t)
	r, l := verifiedRootOpenFixture(t, path)
	binding := r.Binding()
	if binding.RootID != "synthetic-root" || binding.Path != path || binding.Identity.Inode == 0 {
		t.Fatal("incomplete binding")
	}
	binding.Path = "untrusted"
	binding.RootID = "untrusted"
	binding.Identity.Inode++
	if r.Binding().Path != path || r.Binding().RootID != "synthetic-root" {
		t.Fatal("binding aliases caller")
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Fatal("0022 contract incorrectly rejects readable root", err)
	}
	if err := os.Chmod(path, 0770); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); !errors.Is(err, ErrVerifiedRootUnsafe) {
		t.Fatalf("group write accepted: %v", err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyRootHandle(l.directory.handle, CompletionFileIdentity{Device: r.Binding().Identity.Device, Inode: r.Binding().Identity.Inode + 1}); !errors.Is(err, ErrVerifiedRootIdentity) {
		t.Fatalf("wrong inode accepted: %v", err)
	}
	if _, err := OpenVerifiedPolicyRoot("", path); !errors.Is(err, ErrVerifiedRootInvalid) {
		t.Fatal("empty root ID accepted")
	}
	missing := filepath.Join(path, "missing")
	if _, err := OpenVerifiedPolicyRoot("root", missing); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("root opener created directory")
	}
	file := filepath.Join(path, "regular")
	if err := os.WriteFile(file, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVerifiedPolicyRoot("root", file); err == nil {
		t.Fatal("regular file accepted as directory")
	}
	link := filepath.Join(path, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVerifiedPolicyRoot("root", link); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestVerifiedRootActualACL(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal("cannot resolve fixture user")
	}
	cases := []struct {
		name, entry string
		want        error
	}{
		{"deny", "everyone deny delete", nil},
		{"read", "everyone allow read,readattr,readextattr,readsecurity", nil},
		{"current-write", "user:" + me.Username + " allow write,append,delete,delete_child,writeattr,writeextattr,writesecurity,chown", nil},
		{"foreign-write", "everyone allow write", ErrVerifiedRootUnsafe},
		{"foreign-delete-child", "everyone allow delete_child", ErrVerifiedRootUnsafe},
		{"foreign-inherit-only", "everyone allow write,file_inherit,directory_inherit,only_inherit", ErrVerifiedRootUnsafe},
		{"safe-read-inherit", "everyone allow read,file_inherit,directory_inherit", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := verifiedRootFixture(t)
			verifiedRootAddACL(t, path, tc.entry)
			r, err := OpenVerifiedPolicyRoot("root", path)
			if !errors.Is(err, tc.want) {
				if r != nil {
					_ = r.Close()
				}
				t.Fatalf("actual ACL got %v want %v", err, tc.want)
			}
			if r != nil {
				l, err := r.Borrow()
				if err != nil {
					_ = r.Close()
					t.Fatal(err)
				}
				if err := l.Check(); err != nil {
					t.Error(err)
				}
				_ = l.Close()
				if err := r.Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	t.Run("change-after-open", func(t *testing.T) {
		path := verifiedRootFixture(t)
		r, l := verifiedRootOpenFixture(t, path)
		verifiedRootAddACL(t, path, "everyone allow write")
		if err := l.Check(); !errors.Is(err, ErrVerifiedRootUnsafe) {
			t.Fatalf("changed ACL not rejected: %v", err)
		}
		if other, err := r.Borrow(); !errors.Is(err, ErrVerifiedRootUnsafe) {
			if other != nil {
				_ = other.Close()
			}
			t.Fatalf("new lease escaped changed ACL: %v", err)
		}
		verifiedRootChmod(t, path, "-N")
		if err := l.Check(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("actual-inherited-entry", func(t *testing.T) {
		parent := verifiedRootFixture(t)
		verifiedRootAddACL(t, parent, "everyone allow write,file_inherit,directory_inherit")
		child := filepath.Join(parent, "child")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { verifiedRootChmod(t, child, "-N") })
		if r, err := OpenVerifiedPolicyRoot("root", child); !errors.Is(err, ErrVerifiedRootUnsafe) {
			if r != nil {
				_ = r.Close()
			}
			t.Fatalf("inherited ACL escaped: %v", err)
		}
	})
}

func TestVerifiedRootActualRenameAndReplacement(t *testing.T) {
	parent := verifiedRootFixture(t)
	path := filepath.Join(parent, "root")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	_, l := verifiedRootOpenFixture(t, path)
	original := filepath.Join(parent, "original")
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := l.WithDirectory(func(*CompletionDirectory) error { called = true; return nil }); !errors.Is(err, ErrVerifiedRootIdentity) || called {
		t.Fatalf("foreign replacement: err=%v callback=%v", err, called)
	}
	if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
		t.Fatal("foreign directory changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, path); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); !errors.Is(err, ErrVerifiedRootIdentity) {
		t.Fatalf("symlink replacement accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); !errors.Is(err, ErrVerifiedRootIdentity) {
		t.Fatalf("missing path accepted: %v", err)
	}
	if err := os.Rename(original, path); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	// Same fd IO uses the original directory object, never a reopened IO path.
	if err := l.WithDirectory(func(d *CompletionDirectory) error {
		f, err := d.CreateStaging("owned-stage")
		if err != nil {
			return err
		}
		return f.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "owned-stage")); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedRootActualLeaseLifetime(t *testing.T) {
	path := verifiedRootFixture(t)
	r, l := verifiedRootOpenFixture(t, path)
	for _, d := range []*CompletionDirectory{r.directory, l.directory} {
		flags, err := unix.FcntlInt(d.handle.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("descriptor lacks CLOEXEC")
		}
	}
	if r.directory.handle.Fd() == l.directory.handle.Fd() {
		t.Fatal("lease shares owner's fd number")
	}
	if err := r.Close(); !errors.Is(err, ErrVerifiedRootBusy) {
		t.Fatalf("owner did not report busy: %v", err)
	}
	if _, err := r.Borrow(); !errors.Is(err, ErrVerifiedRootClosed) {
		t.Fatal("closing owner admitted new borrow")
	}
	if err := l.Check(); err != nil {
		t.Fatal("closing owner broke active lease", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	result := make(chan error, 1)
	received := false
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if !received {
			<-result
		}
	}()
	go func() {
		result <- l.WithDirectory(func(d *CompletionDirectory) error { close(entered); <-release; _, err := d.handle.Stat(); return err })
	}()
	select {
	case <-entered:
	case err := <-result:
		received = true
		t.Fatalf("callback was not entered: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); !errors.Is(err, ErrVerifiedRootBusy) {
		t.Fatal("active snapshot not retained", err)
	}
	releaseOnce.Do(func() { close(release) })
	err := <-result
	received = true
	if !errors.Is(err, ErrVerifiedRootClosed) {
		t.Fatalf("closed lease result=%v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); !errors.Is(err, ErrVerifiedRootClosed) {
		t.Fatal("closed lease checked successfully")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("close deleted root", err)
	}
}

func TestVerifiedRootActualConcurrentChecksAndPanic(t *testing.T) {
	path := verifiedRootFixture(t)
	r, l := verifiedRootOpenFixture(t, path)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- l.Check() }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("callback did not panic")
			}
		}()
		_ = l.WithDirectory(func(*CompletionDirectory) error { panic("synthetic callback") })
	}()
	r.mu.Lock()
	operations := r.operations
	r.mu.Unlock()
	if operations != 0 {
		t.Fatal("panic leaked active snapshot")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A failed same-fd ACL read is unsupported, not a no-ACL grant.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := verifyRootHandle(f, r.Binding().Identity); !errors.Is(err, ErrVerifiedRootUnsupported) {
		t.Fatalf("failed read accepted: %v", err)
	}
}
