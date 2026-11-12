//go:build darwin && cgo

package transfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
)

func TestVerifiedLayoutStorageACKAndResume(t *testing.T) {
	root := verifiedRootFixture(t)
	_, lease := verifiedRootOpenFixture(t, root)
	layout, err := pathpolicy.BuildLayoutV1(root, []pathpolicy.Member{{Index: 0, RelativePath: "tree/一.bin", Selected: true}, {Index: 1, RelativePath: "tree/empty", Selected: false}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := func() error { return ErrLayoutStorageHeld }
	if s, b, err := CreateVerifiedLayoutStorage(lease, layout, []int64{3, 0}, held); s != nil || b.Version != 0 || !errors.Is(err, ErrLayoutStorageHeld) {
		t.Fatalf("F held = %v/%v", b, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("held F touched output")
	}
	s, binding, err := CreateVerifiedLayoutStorage(lease, layout, []int64{3, 0}, ctx.Err)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(binding.Files) != 2 || binding.Files[1].Identity.Inode == 0 {
		t.Fatal("missing zero/unselected identity")
	}
	if n, err := s.WriteAt(0, []byte("abc"), 0); n != 0 || !errors.Is(err, ErrLayoutStorageHeld) {
		t.Fatalf("before identity ACK %d/%v", n, err)
	}
	bad := s.Binding()
	bad.Files[0].Identity.Inode++
	if err := s.Activate(bad); !errors.Is(err, ErrLayoutStorageHeld) {
		t.Fatal("accepted changed identity ACK")
	}
	if err := s.Activate(binding); err != nil {
		t.Fatal(err)
	}
	binding.Files[0].RelativePath = "mutated caller"
	if n, err := s.WriteAt(0, []byte("abc"), 0); n != 3 || err != nil {
		t.Fatalf("write %d/%v", n, err)
	}
	if n, err := s.WriteAt(1, []byte("x"), 0); n != 0 || err == nil {
		t.Fatal("wrote past zero-length member")
	}
	want := s.Binding()
	cancel()
	if n, err := s.WriteAt(0, []byte("x"), 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled run still writes")
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, resumed, err := OpenVerifiedLayoutStorage(lease, layout, []int64{3, 0}, want, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Activate(resumed); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3)
	if n, err := r.ReadAt(0, data, 0); n != 3 || err != nil || !bytes.Equal(data, []byte("abc")) {
		t.Fatalf("resumed bytes %q %d/%v", data, n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "tree/empty")); err != nil {
		t.Fatal(err)
	}
	if opened, _, err := OpenVerifiedLayoutStorage(lease, layout, []int64{3, 0}, want, func() error { return nil }); opened != nil || err == nil {
		t.Fatal("resume created missing native file")
	}
	if _, err := os.Stat(filepath.Join(root, "tree/empty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing file was created")
	}
}

func TestVerifiedLayoutStorageRejectsUnsafeTargets(t *testing.T) {
	root := verifiedRootFixture(t)
	_, lease := verifiedRootOpenFixture(t, root)
	guard := func() error { return nil }
	bad, _ := pathpolicy.BuildLayoutV1(root, []pathpolicy.Member{{Index: 0, RelativePath: "file"}, {Index: 1, RelativePath: "file/child"}})
	if s, _, err := CreateVerifiedLayoutStorage(lease, bad, []int64{0, 0}, guard); s != nil || err == nil {
		t.Fatal("accepted file/directory collision")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("invalid shape performed IO")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	layout, _ := pathpolicy.BuildLayoutV1(root, []pathpolicy.Member{{Index: 0, RelativePath: "escape/file"}})
	if s, _, err := CreateVerifiedLayoutStorage(lease, layout, []int64{3}, guard); s != nil || err == nil {
		t.Fatal("followed directory symlink")
	}
	entries, _ = os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside pinned root")
	}
	layout, _ = pathpolicy.BuildLayoutV1(root, []pathpolicy.Member{{Index: 0, RelativePath: "owned"}})
	s, binding, err := CreateVerifiedLayoutStorage(lease, layout, []int64{3}, guard)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "owned"), filepath.Join(root, "preserved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "owned"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, _, err := OpenVerifiedLayoutStorage(lease, layout, []int64{3}, binding, guard); s != nil || err == nil {
		t.Fatal("adopted foreign inode")
	}
	if s, _, err := CreateVerifiedLayoutStorage(lease, layout, []int64{3}, guard); s != nil || err == nil {
		t.Fatal("overwrote existing file")
	}
	data, _ := os.ReadFile(filepath.Join(root, "owned"))
	if string(data) != "old" {
		t.Fatal("foreign contents changed")
	}
	layout, _ = pathpolicy.BuildLayoutV1(root, []pathpolicy.Member{{Index: 0, RelativePath: "lease-file"}})
	s, binding, err = CreateVerifiedLayoutStorage(lease, layout, []int64{1}, guard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Activate(binding); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := s.WriteAt(0, []byte("x"), 0); n != 0 || !errors.Is(err, ErrVerifiedRootClosed) {
		t.Fatal("closed lease still writes through retained fd")
	}
}
