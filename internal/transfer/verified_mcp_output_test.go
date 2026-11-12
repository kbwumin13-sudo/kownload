//go:build darwin && cgo

package transfer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// This observer counts only this process's handles to our synthetic identities;
// it does not open or read the contents of files referred to by other descriptors.
func verifiedMCPTestHandleCount(t *testing.T, identities ...CompletionFileIdentity) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) != nil {
			continue
		}
		actual := CompletionFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
		for _, identity := range identities {
			if actual == identity {
				count++
				break
			}
		}
	}
	return count
}

func verifiedMCPTestStage(t *testing.T, lease *VerifiedRootLease, root CompletionFileIdentity, body []byte) MCPOutputBinding {
	t.Helper()
	file, binding, err := CreateVerifiedMCPOutput(lease, "synthetic-owner", root, "owned-stage", "final.bin")
	if file != nil {
		t.Cleanup(func() { _ = file.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	if n, err := file.Write(body); err != nil || n != len(body) {
		t.Fatal("same-fd stage write failed", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return binding
}

func verifiedMCPTestClosed(t *testing.T, root *VerifiedPolicyRoot, lease *VerifiedRootLease, identities ...CompletionFileIdentity) {
	t.Helper()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if n := verifiedMCPTestHandleCount(t, identities...); n != 0 {
		t.Fatalf("synthetic root/stage handles remain: %d", n)
	}
}

func TestVerifiedMCPOutputBridgeCompleteBytesAndLifetime(t *testing.T) {
	path := verifiedRootFixture(t)
	root, lease := verifiedRootOpenFixture(t, path)
	identity := root.Binding().Identity
	body := bytes.Repeat([]byte{0, 1, 2, 0xff, 0x45}, 4096)
	writer, binding, err := CreateVerifiedMCPOutput(lease, "synthetic-owner", identity, "owned-stage", "final.bin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if binding.DirectoryIdentity != identity || binding.StagingIdentity.Inode == 0 {
		t.Fatal("returned binding is not the actual leased stage")
	}
	// The short directory callback has ended. The separate writer fd is valid,
	// and the retained lease still prevents registry-owner teardown.
	if err := root.Close(); !errors.Is(err, ErrVerifiedRootBusy) {
		t.Fatal("registry did not stay busy for caller's lease", err)
	}
	if n, err := writer.Write(body); err != nil || n != len(body) {
		t.Fatal("writer fd did not outlive short callback", err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenVerifiedMCPOutput(lease, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	seal := MCPOutputSeal{Binding: binding, SealedSize: int64(len(body))}
	outcome, err := PublishVerifiedMCPOutput(lease, readonly, seal)
	if err != nil || outcome != MCPOutputConfirmed {
		t.Fatalf("original publisher not confirmed: outcome=%d err=%v", outcome, err)
	}
	for _, name := range []string{binding.StagingName, binding.FinalName} {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatal("complete bytes not preserved", err)
		}
	}
	if err := readonly.Close(); err != nil {
		t.Fatal(err)
	}
	verifiedMCPTestClosed(t, root, lease, identity, binding.StagingIdentity)
	if _, err := os.Stat(filepath.Join(path, binding.FinalName)); err != nil {
		t.Fatal("close removed published output", err)
	}
}

func TestVerifiedMCPOutputBridgeWrongBinding(t *testing.T) {
	path := verifiedRootFixture(t)
	root, lease := verifiedRootOpenFixture(t, path)
	identity := root.Binding().Identity
	t.Run("wrong-root-zero-new-output", func(t *testing.T) {
		wrong := identity
		wrong.Inode++
		fd, binding, err := CreateVerifiedMCPOutput(lease, "synthetic-owner", wrong, "rejected-stage", "rejected-final")
		if err == nil || fd != nil || binding != (MCPOutputBinding{}) {
			if fd != nil {
				_ = fd.Close()
			}
			t.Fatal("wrong leased-root identity accepted")
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatal("wrong-root call created output", err)
		}
	})
	t.Run("invalid-metadata-zero-new-output", func(t *testing.T) {
		fd, binding, err := CreateVerifiedMCPOutput(lease, "", identity, "rejected-stage", "rejected-final")
		if err == nil || fd != nil || binding != (MCPOutputBinding{}) {
			if fd != nil {
				_ = fd.Close()
			}
			t.Fatal("invalid metadata accepted")
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatal("invalid metadata created output", err)
		}
	})
	body := []byte("unchanged original stage")
	binding := verifiedMCPTestStage(t, lease, identity, body)
	wrong := binding
	wrong.StagingIdentity.Inode++
	if fd, err := OpenVerifiedMCPOutput(lease, wrong); err == nil || fd != nil {
		if fd != nil {
			_ = fd.Close()
		}
		t.Fatal("wrong staging identity opened")
	}
	readonly, err := OpenVerifiedMCPOutput(lease, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	outcome, err := PublishVerifiedMCPOutput(lease, readonly, MCPOutputSeal{Binding: wrong, SealedSize: int64(len(body))})
	if err == nil || outcome != MCPOutputNotAttempted {
		t.Fatal("wrong staging binding reached publication", outcome, err)
	}
	if _, err := os.Lstat(filepath.Join(path, binding.FinalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected binding produced final")
	}
	got, err := os.ReadFile(filepath.Join(path, binding.StagingName))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("rejected binding changed original stage", err)
	}
	_ = readonly.Close()
	verifiedMCPTestClosed(t, root, lease, identity, binding.StagingIdentity)
}

func TestVerifiedMCPOutputBridgeReplacedRoot(t *testing.T) {
	parent := verifiedRootFixture(t)
	path := filepath.Join(parent, "root")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, lease := verifiedRootOpenFixture(t, path)
	identity := root.Binding().Identity
	body := []byte("original object only")
	binding := verifiedMCPTestStage(t, lease, identity, body)
	readonly, err := OpenVerifiedMCPOutput(lease, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	original := filepath.Join(parent, "original")
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if fd, _, err := CreateVerifiedMCPOutput(lease, "synthetic-owner", identity, "rejected-stage", "rejected-final"); err == nil || fd != nil {
		if fd != nil {
			_ = fd.Close()
		}
		t.Fatal("create accepted replaced root")
	}
	if fd, err := OpenVerifiedMCPOutput(lease, binding); err == nil || fd != nil {
		if fd != nil {
			_ = fd.Close()
		}
		t.Fatal("open accepted replaced root")
	}
	outcome, err := PublishVerifiedMCPOutput(lease, readonly, MCPOutputSeal{Binding: binding, SealedSize: int64(len(body))})
	if err == nil || outcome != MCPOutputNotAttempted {
		t.Fatal("publish accepted replaced root", outcome, err)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatal("foreign replacement changed", err)
	}
	if _, err := os.Lstat(filepath.Join(original, binding.FinalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied call published into renamed original")
	}
	got, err := os.ReadFile(filepath.Join(original, binding.StagingName))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("original stage changed", err)
	}
	_ = readonly.Close()
	verifiedMCPTestClosed(t, root, lease, identity, binding.StagingIdentity)
}

func TestVerifiedMCPOutputBridgeConflictAndUncertain(t *testing.T) {
	path := verifiedRootFixture(t)
	root, lease := verifiedRootOpenFixture(t, path)
	identity := root.Binding().Identity
	body := []byte("owned staging bytes")
	foreign := []byte("preexisting final must survive")
	binding := verifiedMCPTestStage(t, lease, identity, body)
	if err := os.WriteFile(filepath.Join(path, binding.FinalName), foreign, 0600); err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenVerifiedMCPOutput(lease, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	outcome, err := PublishVerifiedMCPOutput(lease, readonly, MCPOutputSeal{Binding: binding, SealedSize: int64(len(body))})
	if err == nil || outcome != MCPOutputConflict {
		t.Fatal("existing final not reported as conflict", outcome, err)
	}
	for name, want := range map[string][]byte{binding.FinalName: foreign, binding.StagingName: body} {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("conflict overwrote or removed file", err)
		}
	}
	// This is pure OUTCOME classification, not an injected runtime publisher or
	// a claim that a real post-link directory Sync/Close failure was reproduced.
	t.Run("outer-close-after-inner-confirmation", func(t *testing.T) {
		got, err := verifiedMCPPublishResult(MCPOutputConfirmed, ErrVerifiedRootClosed)
		if got != MCPOutputUncertain || !errors.Is(err, ErrVerifiedRootClosed) {
			t.Fatal("lifecycle error retained confirmed result")
		}
	})
	t.Run("original-uncertain-retained", func(t *testing.T) {
		got, err := verifiedMCPPublishResult(MCPOutputUncertain, ErrVerifiedRootClosed)
		if got != MCPOutputUncertain || !errors.Is(err, ErrVerifiedRootClosed) {
			t.Fatal("uncertain result lost")
		}
	})
	_ = readonly.Close()
	verifiedMCPTestClosed(t, root, lease, identity, binding.StagingIdentity)
}
