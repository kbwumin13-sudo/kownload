//go:build darwin || linux

package transfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func stagingDirectoryFixture(t *testing.T) (string, *CompletionDirectory) {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := OpenCompletionDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	return path, directory
}

func pinnedStagingRecord(t *testing.T, directory *CompletionDirectory, name string, file *os.File) VerifiedCompletionRecord {
	t.Helper()
	identity, err := CompletionIdentityOf(file)
	if err != nil {
		t.Fatal(err)
	}
	return VerifiedCompletionRecord{Version: 1, ID: "staging-fixture", Phase: CompletionPrepared,
		DirectoryIdentity: directory.Identity(), StagingIdentity: identity, StagingName: name, FinalName: "final.bin",
		Size: -1, ExpectedSHA256: strings.Repeat("a", 64)}
}

func TestVerifiedStagingCreateAndWriterPreserveBytes(t *testing.T) {
	path, directory := stagingDirectoryFixture(t)
	file, err := directory.CreateStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0600 {
		t.Fatal("not an empty private file", err)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_RDWR {
		t.Fatal("writer access mode", err)
	}
	fdFlags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil || fdFlags&unix.FD_CLOEXEC == 0 {
		t.Fatal("writer is not close-on-exec", err)
	}
	record := pinnedStagingRecord(t, directory, ".stage", file)
	// Stand in for caller pin+Save ACK, then HTTP consumes this same fd.
	body := []byte("partial checkpoint bytes")
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := directory.OpenStagingWriter(record)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	after, err := writer.Stat()
	if err != nil || after.Size() != int64(len(body)) {
		t.Fatal("reopen truncated checkpoint", err)
	}
	actual, err := os.ReadFile(filepath.Join(path, ".stage"))
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("reopen changed contents", err)
	}
	identity, err := CompletionIdentityOf(writer)
	if err != nil || identity != record.StagingIdentity {
		t.Fatal("reopen switched inode", err)
	}
}

func TestVerifiedStagingExistingAndInvalidNamesArePreserved(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "dangling", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path, d := stagingDirectoryFixture(t)
			target := filepath.Join(path, "external")
			if err := os.WriteFile(target, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(path, ".stage")
			var err error
			switch kind {
			case "regular":
				err = os.WriteFile(name, []byte("existing"), 0600)
			case "symlink":
				err = os.Symlink(target, name)
			case "dangling":
				err = os.Symlink("missing", name)
			case "directory":
				err = os.Mkdir(name, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(name)
			if err != nil {
				t.Fatal(err)
			}
			opened, openErr := d.CreateStaging(".stage")
			if opened != nil {
				_ = opened.Close()
			}
			if kind == "regular" {
				data, err := os.ReadFile(name)
				if err != nil || string(data) != "existing" {
					t.Fatal("existing bytes were changed", err)
				}
			}
			after, err := os.Lstat(name)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("existing entry replaced", err)
			}
			body, err := os.ReadFile(target)
			if err != nil || string(body) != "sentinel" {
				t.Fatal("external target modified", err)
			}
			if openErr == nil {
				t.Fatal("existing entry was adopted")
			}
		})
	}
	path, d := stagingDirectoryFixture(t)
	for _, name := range []string{"", ".", "..", "../outside", "a/b", "a\\b"} {
		if f, err := d.CreateStaging(name); err == nil {
			f.Close()
			t.Fatal("invalid name accepted")
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid names created files", err)
	}
}

func TestVerifiedStagingWriterRejectsSealedAndForeignObjects(t *testing.T) {
	path, d := stagingDirectoryFixture(t)
	file, err := d.CreateStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	r := pinnedStagingRecord(t, d, ".stage", file)
	file.Close()
	for _, phase := range []VerifiedCompletionPhase{CompletionWaiting, CompletionVerifying, CompletionVerified, CompletionPublished, CompletionCommitted, "unknown"} {
		sealed := r
		sealed.Phase = phase
		if f, err := d.OpenStagingWriter(sealed); err == nil {
			f.Close()
			t.Fatal("sealed phase reopened writer", phase)
		}
	}
	for _, mutate := range []func(*VerifiedCompletionRecord){func(v *VerifiedCompletionRecord) { v.StagingIdentity.Inode++ }, func(v *VerifiedCompletionRecord) { v.DirectoryIdentity.Inode++ }, func(v *VerifiedCompletionRecord) { v.StagingIdentity = CompletionFileIdentity{} }, func(v *VerifiedCompletionRecord) { v.StagingName = "../outside" }} {
		wrong := r
		mutate(&wrong)
		if f, err := d.OpenStagingWriter(wrong); err == nil {
			f.Close()
			t.Fatal("foreign/unbound writer accepted")
		}
	}
	if err := os.Link(filepath.Join(path, ".stage"), filepath.Join(path, "published")); err != nil {
		t.Fatal(err)
	}
	if f, err := d.OpenStagingWriter(r); err == nil {
		f.Close()
		t.Fatal("published hard-link alias allowed writer")
	}
}

func TestVerifiedStagingWriterRejectsSymlinkFIFOAndReplacement(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			path, d := stagingDirectoryFixture(t)
			file, err := d.CreateStaging(".stage")
			if err != nil {
				t.Fatal(err)
			}
			r := pinnedStagingRecord(t, d, ".stage", file)
			file.Close()
			if err := os.Rename(filepath.Join(path, ".stage"), filepath.Join(path, "original")); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(path, ".stage")
			switch kind {
			case "symlink":
				err = os.Symlink("original", name)
			case "fifo":
				err = unix.Mkfifo(name, 0600)
			case "directory":
				err = os.Mkdir(name, 0700)
			case "replacement":
				err = os.WriteFile(name, []byte("foreign"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if f, err := d.OpenStagingWriter(r); err == nil {
				f.Close()
				t.Fatal("nonowned/nonregular entry accepted")
			}
			body, err := os.ReadFile(filepath.Join(path, "original"))
			if err != nil || len(body) != 0 {
				t.Fatal("original inode modified", err)
			}
		})
	}
}

func TestVerifiedStagingPinnedDirectoryAndAbortPreserveState(t *testing.T) {
	path, d := stagingDirectoryFixture(t)
	moved := path + "-moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := d.CreateStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The caller aborts before pin Save ACK/first byte. These methods have no
	// ctx/Task authority; cancellation belongs to its outer lifecycle Fence.
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("fixture cancellation missing")
	}
	file.Close()
	if _, err := os.Lstat(filepath.Join(path, ".stage")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absolute-path replacement was followed", err)
	}
	info, err := os.Stat(filepath.Join(moved, ".stage"))
	if err != nil || info.Size() != 0 {
		t.Fatal("abort removed or wrote staging", err)
	}
	unbound := VerifiedCompletionRecord{Version: 1, ID: "unbound", Phase: CompletionPrepared, DirectoryIdentity: d.Identity(), StagingName: ".stage", FinalName: "final", Size: -1, ExpectedSHA256: strings.Repeat("a", 64)}
	if f, err := d.OpenStagingWriter(unbound); err == nil {
		f.Close()
		t.Fatal("unconfirmed pin was inferred from path")
	}
}

func TestVerifiedStagingSyncFailureClosesFDKeepsUnknownEntry(t *testing.T) {
	path, d := stagingDirectoryFixture(t)
	file, err := d.createStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	returned, err := finishCreatedStaging(file, func() error { return unix.EIO })
	if err == nil || returned != nil {
		t.Fatal("sync failure returned a writer")
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("failed descriptor remained open", err)
	}
	info, err := os.Stat(filepath.Join(path, ".stage"))
	if err != nil || info.Size() != 0 {
		t.Fatal("unknown entry was removed or written", err)
	}
	if f, err := d.CreateStaging(".stage"); err == nil {
		f.Close()
		t.Fatal("unknown entry was silently reclaimed")
	}
}
