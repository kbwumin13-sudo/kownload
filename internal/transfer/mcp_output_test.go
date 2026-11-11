//go:build darwin || linux

package transfer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func mcpOutputFixture(t *testing.T, body []byte) (string, *CompletionDirectory, *os.File, MCPOutputSeal) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "output")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := OpenCompletionDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	writer, err := d.CreateStaging(".owned-part")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	identity, err := CompletionIdentityOf(writer)
	if err != nil {
		t.Fatal(err)
	}
	binding := MCPOutputBinding{"caller-frozen-binding", d.Identity(), identity, ".owned-part", "final.bin"}
	// This FS fixture stands in for the caller's pin ACK, then closes its sole
	// writer before deriving a seal. It does not test Task/DB authorization.
	if _, err = writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = writer.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := d.OpenMCPOutput(binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return root, d, file, MCPOutputSeal{binding, info.Size()}
}

func mcpOutputBytes(t *testing.T, name string, want []byte) {
	t.Helper()
	actual, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(actual, want) {
		t.Fatal("file bytes changed", err)
	}
}

func TestMCPOutputPublishSealedBytesWithoutChecksum(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("ordinary bytes without a digest requirement")} {
		root, d, file, seal := mcpOutputFixture(t, body)
		outcome, err := d.PublishMCPOutput(file, seal)
		if err != nil || outcome != MCPOutputConfirmed {
			t.Fatal("filesystem publication", outcome, err)
		}
		stage, err := os.Stat(filepath.Join(root, seal.Binding.StagingName))
		if err != nil {
			t.Fatal(err)
		}
		final, err := os.Stat(filepath.Join(root, seal.Binding.FinalName))
		if err != nil || !os.SameFile(stage, final) || final.Size() != seal.SealedSize {
			t.Fatal("publication did not retain the anchored inode", err)
		}
		mcpOutputBytes(t, filepath.Join(root, seal.Binding.StagingName), body)
		mcpOutputBytes(t, filepath.Join(root, seal.Binding.FinalName), body)
		if _, err := file.Stat(); err != nil {
			t.Fatal("publish closed borrowed fd", err)
		}
		if flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0); err != nil || flags&unix.O_ACCMODE != unix.O_RDONLY {
			t.Fatal("open did not return readonly fd", err)
		}
		if outcome, err = d.PublishMCPOutput(file, seal); err == nil || outcome != MCPOutputConflict {
			t.Fatal("same-inode existing final silently adopted", outcome, err)
		}
	}
}

func TestMCPOutputExistingFinalPreservesBoth(t *testing.T) {
	for _, kind := range []string{"foreign", "same-bytes", "symlink", "dangling", "directory", "own-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			body := []byte("owned stage bytes")
			root, d, file, seal := mcpOutputFixture(t, body)
			final := filepath.Join(root, seal.Binding.FinalName)
			external := filepath.Join(root, "external")
			if err := os.WriteFile(external, []byte("external sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "foreign":
				err = os.WriteFile(final, []byte("foreign final"), 0600)
			case "same-bytes":
				err = os.WriteFile(final, body, 0600)
			case "symlink":
				err = os.Symlink(external, final)
			case "dangling":
				err = os.Symlink("missing", final)
			case "directory":
				err = os.Mkdir(final, 0700)
			case "own-hardlink":
				err = os.Link(filepath.Join(root, seal.Binding.StagingName), final)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(final)
			if err != nil {
				t.Fatal(err)
			}
			outcome, publishErr := d.PublishMCPOutput(file, seal)
			// Assert preservation before interpreting errors: an ordinary
			// Rename can destroy foreign bytes and only then fail a check.
			after, err := os.Lstat(final)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("existing final entry replaced", err)
			}
			if kind == "foreign" {
				mcpOutputBytes(t, final, []byte("foreign final"))
			}
			mcpOutputBytes(t, external, []byte("external sentinel"))
			mcpOutputBytes(t, filepath.Join(root, seal.Binding.StagingName), body)
			if publishErr == nil || outcome != MCPOutputConflict || !errors.Is(publishErr, os.ErrExist) {
				t.Fatal("existing entry not a conflict", outcome, publishErr)
			}
		})
	}
}

func TestMCPOutputConcurrentFinalBeforeLinkIsConflict(t *testing.T) {
	body := []byte("sealed before competing final")
	root, d, file, seal := mcpOutputFixture(t, body)
	final := filepath.Join(root, seal.Binding.FinalName)
	// Another creator takes the final name after our source checks/flush,
	// immediately before linkat. No stage namespace mutation is involved.
	outcome, err := d.publishMCPOutput(file, seal, func() error {
		if err := file.Sync(); err != nil {
			return err
		}
		return os.WriteFile(final, []byte("late foreign final"), 0600)
	}, d.handle.Sync)
	mcpOutputBytes(t, final, []byte("late foreign final"))
	mcpOutputBytes(t, filepath.Join(root, seal.Binding.StagingName), body)
	if err == nil || outcome != MCPOutputConflict {
		t.Fatal("concurrent final was not excluded atomically", outcome, err)
	}
}

func TestMCPOutputRejectsWrongBindingSealAndHandle(t *testing.T) {
	body := []byte("fixed sealed bytes")
	root, d, file, seal := mcpOutputFixture(t, body)
	for _, change := range []func(*MCPOutputBinding){
		func(b *MCPOutputBinding) { b.OwnerID = "" },
		func(b *MCPOutputBinding) { b.OwnerID = strings.Repeat("x", 129) },
		func(b *MCPOutputBinding) { b.DirectoryIdentity.Inode++ },
		func(b *MCPOutputBinding) { b.StagingIdentity.Inode++ },
		func(b *MCPOutputBinding) { b.StagingIdentity.Device++ },
		func(b *MCPOutputBinding) { b.StagingName = "../external" },
		func(b *MCPOutputBinding) { b.FinalName = "/external" },
		func(b *MCPOutputBinding) { b.FinalName = strings.ToUpper(b.StagingName) },
	} {
		wrong := seal
		change(&wrong.Binding)
		if opened, err := d.OpenMCPOutput(wrong.Binding); err == nil || opened != nil {
			if opened != nil {
				opened.Close()
			}
			t.Fatal("wrong binding opened")
		}
		if outcome, err := d.PublishMCPOutput(file, wrong); err == nil || outcome != MCPOutputNotAttempted {
			t.Fatal("wrong binding attempted publication", outcome, err)
		}
	}
	for _, size := range []int64{-1, seal.SealedSize + 1} {
		wrong := seal
		wrong.SealedSize = size
		if outcome, err := d.PublishMCPOutput(file, wrong); err == nil || outcome != MCPOutputNotAttempted {
			t.Fatal("wrong seal attempted publication", outcome, err)
		}
	}
	if outcome, err := d.PublishMCPOutput(nil, seal); err == nil || outcome != MCPOutputNotAttempted {
		t.Fatal("nil file accepted", outcome, err)
	}
	writer, err := os.OpenFile(filepath.Join(root, seal.Binding.StagingName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := d.PublishMCPOutput(writer, seal); err == nil || outcome != MCPOutputNotAttempted {
		writer.Close()
		t.Fatal("writable file accepted", outcome, err)
	}
	writer.Close()
	foreign := filepath.Join(root, "foreign")
	if err := os.WriteFile(foreign, body, 0600); err != nil {
		t.Fatal(err)
	}
	other, err := os.Open(foreign)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if outcome, err := d.PublishMCPOutput(other, seal); err == nil || outcome != MCPOutputNotAttempted {
		t.Fatal("same-byte foreign inode accepted", outcome, err)
	}
	mcpOutputBytes(t, foreign, body)
	mcpOutputBytes(t, filepath.Join(root, seal.Binding.StagingName), body)
	if _, err := os.Lstat(filepath.Join(root, seal.Binding.FinalName)); !os.IsNotExist(err) {
		t.Fatal("invalid evidence created final", err)
	}
}

func TestMCPOutputRejectsReplacedStageAndNonRegular(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			body := []byte("retained original")
			root, d, file, seal := mcpOutputFixture(t, body)
			stage, original := filepath.Join(root, seal.Binding.StagingName), filepath.Join(root, "original")
			if err := os.Rename(stage, original); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(stage, body, 0600)
			case "symlink":
				err = os.Symlink(original, stage)
			case "fifo":
				err = unix.Mkfifo(stage, 0600)
			case "directory":
				err = os.Mkdir(stage, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if opened, err := d.OpenMCPOutput(seal.Binding); err == nil || opened != nil {
				if opened != nil {
					opened.Close()
				}
				t.Fatal("replacement stage opened")
			}
			if outcome, err := d.PublishMCPOutput(file, seal); err == nil || outcome != MCPOutputNotAttempted {
				t.Fatal("replacement stage publication attempted", outcome, err)
			}
			mcpOutputBytes(t, original, body)
			if _, err := os.Lstat(filepath.Join(root, seal.Binding.FinalName)); !os.IsNotExist(err) {
				t.Fatal("replacement stage created final", err)
			}
		})
	}
}

func TestMCPOutputPinnedRootPreservesReplacement(t *testing.T) {
	body := []byte("pinned old directory")
	root, d, file, seal := mcpOutputFixture(t, body)
	retained := filepath.Join(filepath.Dir(root), "retained")
	if err := os.Rename(root, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, seal.Binding.FinalName)
	if err := os.WriteFile(foreign, []byte("foreign root"), 0600); err != nil {
		t.Fatal(err)
	}
	if outcome, err := d.PublishMCPOutput(file, seal); err != nil || outcome != MCPOutputConfirmed {
		t.Fatal("pinned directory lost", outcome, err)
	}
	mcpOutputBytes(t, foreign, []byte("foreign root"))
	mcpOutputBytes(t, filepath.Join(retained, seal.Binding.FinalName), body)
	mcpOutputBytes(t, filepath.Join(retained, seal.Binding.StagingName), body)
}

func TestMCPOutputSyncFailuresReportUncertaintyAndRetain(t *testing.T) {
	for _, where := range []string{"file", "directory"} {
		t.Run(where, func(t *testing.T) {
			body := []byte("kept after failed sync")
			root, d, file, seal := mcpOutputFixture(t, body)
			syncFile, syncDirectory := file.Sync, d.handle.Sync
			if where == "file" {
				syncFile = func() error { return unix.EIO }
			} else {
				syncDirectory = func() error { return unix.EIO }
			}
			outcome, err := d.publishMCPOutput(file, seal, syncFile, syncDirectory)
			want := MCPOutputNotAttempted
			if where == "directory" {
				want = MCPOutputUncertain
			}
			if err == nil || outcome != want {
				t.Fatal("sync failure lost its effect boundary", outcome, err)
			}
			mcpOutputBytes(t, filepath.Join(root, seal.Binding.StagingName), body)
			if _, err := file.Stat(); err != nil {
				t.Fatal("failed publish closed borrowed fd", err)
			}
			if where == "directory" {
				mcpOutputBytes(t, filepath.Join(root, seal.Binding.FinalName), body)
				if outcome, err := d.PublishMCPOutput(file, seal); err == nil || outcome != MCPOutputConflict {
					t.Fatal("uncertain publication silently replayed/adopted", outcome, err)
				}
			} else if _, err := os.Lstat(filepath.Join(root, seal.Binding.FinalName)); !os.IsNotExist(err) {
				t.Fatal("pre-link sync failure created final", err)
			}
		})
	}
}

func TestMCPOutputClosedHandlesFailBeforePublication(t *testing.T) {
	for _, which := range []string{"file", "directory"} {
		root, d, file, seal := mcpOutputFixture(t, []byte("closed handle bytes"))
		if which == "file" {
			file.Close()
		} else {
			d.Close()
		}
		outcome, err := d.PublishMCPOutput(file, seal)
		var typed *VerifiedCompletionError
		if outcome != MCPOutputNotAttempted || !errors.As(err, &typed) || err.Error() != "completion: "+string(typed.Kind) {
			t.Fatal("closed handle granted publication or leaked error fields", outcome, err)
		}
		mcpOutputBytes(t, filepath.Join(root, seal.Binding.StagingName), []byte("closed handle bytes"))
		if _, err := os.Lstat(filepath.Join(root, seal.Binding.FinalName)); !os.IsNotExist(err) {
			t.Fatal("closed handle created final", err)
		}
	}
}
