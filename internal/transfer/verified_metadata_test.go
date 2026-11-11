//go:build darwin

package transfer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func metadataFixture(t *testing.T) (string, *CompletionDirectory, *os.File, VerifiedCompletionRecord) {
	t.Helper()
	root, directory := stagingDirectoryFixture(t)
	writer, err := directory.CreateStaging(".stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	record := pinnedStagingRecord(t, directory, ".stage", writer)
	body := []byte("owned staged bytes")
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	record.Phase, record.Size = CompletionWaiting, int64(len(body))
	file, err := directory.OpenStaging(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return root, directory, file, record
}

func metadataStat(t *testing.T, file *os.File) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	return stat
}

func TestVerifiedMetadataMatchesLegacyUnixTimesAndBirthtimeBehavior(t *testing.T) {
	root, directory, file, record := metadataFixture(t)
	accessed, modified := time.Unix(1700000200, 0), time.Unix(1600000100, 0)
	before := metadataStat(t, file)
	if err := directory.SetStagingServerTimes(file, record, accessed, modified); err != nil {
		t.Fatal(err)
	}
	after := metadataStat(t, file)
	if after.Atim.Sec != accessed.Unix() || after.Atim.Nsec != 0 ||
		after.Mtim.Sec != modified.Unix() || after.Mtim.Nsec != 0 {
		t.Fatal("fd access/modification times differ")
	}
	if before.Ino != after.Ino || before.Size != after.Size || before.Mode != after.Mode {
		t.Fatal("inode, size or permissions changed")
	}
	// setft's locked non-Windows source delegates exactly to os.Chtimes.
	legacy := filepath.Join(root, "legacy-reference")
	if err := os.WriteFile(legacy, []byte("reference"), 0600); err != nil {
		t.Fatal(err)
	}
	reference, err := os.Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	beforeReference := metadataStat(t, reference)
	if err := os.Chtimes(legacy, accessed, modified); err != nil {
		t.Fatal(err)
	}
	want := metadataStat(t, reference)
	if after.Atim != want.Atim || after.Mtim != want.Mtim {
		t.Fatal("fd times differ from legacy non-Windows semantics")
	}
	// Darwin/APFS can clamp birthtime down to an earlier mtime even though
	// neither interface explicitly sets creation time. Preserve the observed
	// legacy behavior; do not falsely promise that birthtime never changes.
	changed, referenceChanged := before.Btim != after.Btim, beforeReference.Btim != want.Btim
	if changed != referenceChanged || changed && after.Btim != want.Btim {
		t.Fatal("birthtime behavior differs from legacy Chtimes")
	}
	t.Logf("fd birthtime %v -> %v; legacy birthtime %v -> %v", before.Btim, after.Btim, beforeReference.Btim, want.Btim)
	body, err := os.ReadFile(filepath.Join(root, ".stage"))
	if err != nil || !bytes.Equal(body, []byte("owned staged bytes")) {
		t.Fatal("metadata operation changed bytes", err)
	}
}

func TestVerifiedMetadataReplacementCannotRedirectDescriptor(t *testing.T) {
	for _, replacement := range []string{"file", "symlink", "root"} {
		t.Run(replacement, func(t *testing.T) {
			root, directory, file, record := metadataFixture(t)
			foreignFinal := filepath.Join(root, "final.bin")
			if err := os.WriteFile(foreignFinal, []byte("external final"), 0600); err != nil {
				t.Fatal(err)
			}
			final, err := os.Open(foreignFinal)
			if err != nil {
				t.Fatal(err)
			}
			defer final.Close()
			beforeFinal := metadataStat(t, final)
			stage := filepath.Join(root, ".stage")
			if replacement == "root" {
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(moved) })
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(stage, filepath.Join(root, "owned-moved")); err != nil {
				t.Fatal(err)
			}
			if replacement == "symlink" {
				err = os.Symlink(foreignFinal, stage)
			} else {
				err = os.WriteFile(stage, []byte("foreign replacement"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			replacementFile, err := os.Open(stage)
			if err != nil {
				t.Fatal(err)
			}
			defer replacementFile.Close()
			beforeReplacement := metadataStat(t, replacementFile)
			accessed, modified := time.Unix(1700000200, 0), time.Unix(1600000100, 0)
			if err := directory.SetStagingServerTimes(file, record, accessed, modified); err != nil {
				t.Fatal(err)
			}
			after := metadataStat(t, file)
			if after.Atim.Sec != accessed.Unix() || after.Mtim.Sec != modified.Unix() {
				t.Fatal("lost original descriptor after replacement")
			}
			if beforeFinal != metadataStat(t, final) || beforeReplacement != metadataStat(t, replacementFile) {
				t.Fatal("changed metadata on foreign final or replacement")
			}
			body, err := os.ReadFile(stage)
			want := "foreign replacement"
			if replacement == "symlink" {
				want = "external final"
			}
			if err != nil || string(body) != want {
				t.Fatal("foreign bytes changed", err)
			}
		})
	}
}

func TestVerifiedMetadataRejectsUnsealedForeignAndPublishedHandles(t *testing.T) {
	root, directory, file, record := metadataFixture(t)
	before := metadataStat(t, file)
	accessed, modified := time.Unix(1700000200, 0), time.Unix(1600000100, 0)
	for _, phase := range []VerifiedCompletionPhase{CompletionPrepared, CompletionVerifying, CompletionVerified, CompletionPublished, CompletionCommitted, "unknown"} {
		other := record
		other.Phase = phase
		if err := directory.SetStagingServerTimes(file, other, accessed, modified); err == nil {
			t.Fatal("phase accepted", phase)
		}
	}
	for _, mutate := range []func(*VerifiedCompletionRecord){
		func(r *VerifiedCompletionRecord) { r.StagingIdentity.Inode++ },
		func(r *VerifiedCompletionRecord) { r.DirectoryIdentity.Inode++ },
		func(r *VerifiedCompletionRecord) { r.StagingIdentity.Device++ },
		func(r *VerifiedCompletionRecord) { r.Size++ },
	} {
		other := record
		mutate(&other)
		if err := directory.SetStagingServerTimes(file, other, accessed, modified); err == nil {
			t.Fatal("foreign identity/size accepted")
		}
	}
	if err := directory.SetStagingServerTimes(file, record, accessed, time.Time{}); err == nil {
		t.Fatal("invented missing server time")
	}
	writer, err := os.OpenFile(filepath.Join(root, ".stage"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := directory.SetStagingServerTimes(writer, record, accessed, modified); err == nil {
		t.Fatal("writable descriptor accepted")
	}
	if after := metadataStat(t, file); after != before {
		t.Fatal("rejected operation changed metadata")
	}
	if err := os.Link(filepath.Join(root, ".stage"), filepath.Join(root, "published")); err != nil {
		t.Fatal(err)
	}
	before = metadataStat(t, file)
	if err := directory.SetStagingServerTimes(file, record, accessed, modified); err == nil {
		t.Fatal("published alias accepted")
	}
	if after := metadataStat(t, file); after != before {
		t.Fatal("rejection changed published alias metadata")
	}
}

func TestVerifiedMetadataClosedDescriptorFailsWithoutTouchingFinal(t *testing.T) {
	root, directory, file, record := metadataFixture(t)
	name := filepath.Join(root, "final.bin")
	if err := os.WriteFile(name, []byte("existing final"), 0600); err != nil {
		t.Fatal(err)
	}
	final, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	before := metadataStat(t, final)
	file.Close()
	err = directory.SetStagingServerTimes(file, record, time.Now(), time.Unix(1600000100, 0))
	var typed *VerifiedCompletionError
	if err == nil || !errors.As(err, &typed) || err.Error() != "completion: "+string(typed.Kind) {
		t.Fatal("missing fixed redacted error", err)
	}
	if before != metadataStat(t, final) {
		t.Fatal("failure modified external final")
	}
	body, err := os.ReadFile(name)
	if err != nil || string(body) != "existing final" {
		t.Fatal("failure changed bytes", err)
	}
}
