package identitymigration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy")
	target := filepath.Join(dir, "current")
	os.Mkdir(source, 0700)
	db, err := bolt.Open(filepath.Join(source, "gopeed.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"task", "save", "protocol_state", "config", "extension", "extension_storage"} {
			b, e := tx.CreateBucket([]byte(name))
			if e != nil {
				return e
			}
			if e = b.Put([]byte("fixture-id"), []byte(`{"status":"pause","path":"/fixture","newField":{"operationId":"fixture-op"}}`)); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.WriteFile(filepath.Join(source, "database.hive"), []byte("synthetic hive bytes"), 0600)
	os.WriteFile(filepath.Join(source, "database.lock"), []byte(""), 0600)
	os.MkdirAll(filepath.Join(source, "extensions", "fixture"), 0700)
	os.WriteFile(filepath.Join(source, "extensions", "fixture", "index.js"), []byte("fixture"), 0600)
	os.WriteFile(filepath.Join(source, "host"), []byte("old executable"), 0700)
	return source, target
}
func TestImportAndIdempotentBootstrap(t *testing.T) {
	source, target := fixture(t)
	before, err := summarize(source, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Import(Options{Source: source, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if got != before || got.Files != 3 {
		t.Fatal("incomplete copy", got, before)
	}
	if _, err = os.Stat(filepath.Join(target, "host")); !os.IsNotExist(err) {
		t.Fatal("old host copied")
	}
	again, err := Import(Options{Source: source, Target: target})
	if err != nil || again != got {
		t.Fatal(err)
	}
	after, err := summarize(source, true)
	if err != nil || after != before {
		t.Fatal("legacy changed", err)
	}
}
func TestImportInterruptRecovery(t *testing.T) {
	for _, phase := range []string{"staging-scratch", "staging", "verified", "published"} {
		t.Run(phase, func(t *testing.T) {
			source, target := fixture(t)
			_, err := Import(Options{Source: source, Target: target, Hook: func(p string) error {
				if p == phase {
					return errors.New("fixture_interrupt")
				}
				return nil
			}})
			if err == nil {
				t.Fatal("did not interrupt")
			}
			if _, err = Import(Options{Source: source, Target: target}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRefuseOccupiedAndLockedAndSymlink(t *testing.T) {
	source, target := fixture(t)
	os.Mkdir(target, 0700)
	os.WriteFile(filepath.Join(target, "existing"), []byte("keep"), 0600)
	if _, err := Import(Options{Source: source, Target: target}); err == nil || err.Error() != "target_has_unmarked_data" {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(target, "existing"))
	db, err := bolt.Open(filepath.Join(source, "gopeed.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(Options{Source: source, Target: target}); err == nil || err.Error() != "legacy_database_locked_or_invalid" {
		t.Fatal(err)
	}
	db.Close()
	os.Symlink("/tmp", filepath.Join(source, "outside"))
	if _, err = Import(Options{Source: source, Target: target}); err == nil || err.Error() != "source_contains_symlink" {
		t.Fatal(err)
	}
}
func TestFreshAndExistingEmptyTarget(t *testing.T) {
	for _, empty := range []bool{true, false} {
		dir := t.TempDir()
		source := filepath.Join(dir, "absent")
		target := filepath.Join(dir, "current")
		if empty {
			os.Mkdir(target, 0700)
		}
		s, err := Import(Options{Source: source, Target: target})
		if err != nil || s.Imported || s.Files != 0 {
			t.Fatal(s, err)
		}
	}
}
func TestPendingCannotHideChangedData(t *testing.T) {
	source, target := fixture(t)
	Import(Options{Source: source, Target: target, Hook: func(phase string) error {
		if phase == "published" {
			return errors.New("interrupt")
		}
		return nil
	}})
	os.WriteFile(filepath.Join(target, "database.hive"), []byte("changed"), 0600)
	if _, err := Import(Options{Source: source, Target: target}); err == nil || err.Error() != "pending_verification_failed" {
		t.Fatal(err)
	}
}

func scratchEntries(t *testing.T, target string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); n != filepath.Base(target) && len(n) > len(filepath.Base(target)) && n[:len(filepath.Base(target))] == filepath.Base(target) && n != filepath.Base(target)+".migration-lock" {
			names = append(names, n)
		}
	}
	return names
}

// The staging path must never exist without its owner token, including when
// the importer stops right after writing the token.
func TestStagingNeverAppearsWithoutOwner(t *testing.T) {
	source, target := fixture(t)
	_, err := Import(Options{Source: source, Target: target, Hook: func(p string) error {
		if p == "staging-scratch" {
			return errors.New("fixture_interrupt")
		}
		if p == "staging" {
			data, err := os.ReadFile(filepath.Join(target+".importing", owner))
			if err != nil || len(data) == 0 {
				t.Fatal("staging published without its owner token")
			}
		}
		return nil
	}})
	if err == nil {
		t.Fatal("did not interrupt")
	}
	if _, err = os.Lstat(target + ".importing"); !os.IsNotExist(err) {
		t.Fatal("staging path exists before its token was published")
	}
	if _, err = Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	if left := scratchEntries(t, target); len(left) != 0 {
		t.Fatal("scratch directories left behind", left)
	}
}

// Older versions created the staging directory before its token; a crash in
// that window (or while removing a staging directory) must not block import.
func TestOwnerlessOrForeignStagingIsDiscardedWhenNothingPublished(t *testing.T) {
	for _, token := range []string{"", "another-import"} {
		source, target := fixture(t)
		os.Mkdir(target+".importing", 0700)
		os.WriteFile(filepath.Join(target+".importing", "database.hive"), []byte("partial copy"), 0600)
		if token != "" {
			os.WriteFile(filepath.Join(target+".importing", owner), []byte(token), 0600)
		}
		for i := 0; i < 2; i++ {
			got, err := Import(Options{Source: source, Target: target})
			if err != nil || !got.Imported {
				t.Fatal(token, i, got, err)
			}
		}
		data, _ := os.ReadFile(filepath.Join(target, "database.hive"))
		if string(data) != "synthetic hive bytes" {
			t.Fatal("stale staging content reached the target")
		}
		if _, err := os.Lstat(target + ".importing"); !os.IsNotExist(err) {
			t.Fatal("staging not removed")
		}
	}
}

func TestInterruptedDiscardAndSymlinkStaging(t *testing.T) {
	source, target := fixture(t)
	aside := target + scratchOld + "fixture"
	os.MkdirAll(filepath.Join(aside, "extensions"), 0700)
	os.WriteFile(filepath.Join(aside, "database.hive"), []byte("half removed"), 0600)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	if left := scratchEntries(t, target); len(left) != 0 {
		t.Fatal("interrupted discard left behind", left)
	}

	source, target = fixture(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600)
	os.Symlink(outside, target+".importing")
	if _, err := Import(Options{Source: source, Target: target}); err == nil || err.Error() != "unknown_staging_directory" {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(data) != "keep" {
		t.Fatal("symlinked staging target modified")
	}
}

// useLegacy stands in for build 25 running on its root after the import.
func useLegacy(t *testing.T, source string) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(source, "gopeed.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("task")).Put([]byte("created-after-rollback"), []byte(`{"status":"done"}`))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMarkerRecordsLegacyDatabaseFingerprint(t *testing.T) {
	source, target := fixture(t)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, Marker))
	if err != nil {
		t.Fatal(err)
	}
	var m record
	if err = json.Unmarshal(data, &m); err != nil || m.SourceDatabase == nil {
		t.Fatal("marker lacks the legacy fingerprint", string(data))
	}
	want, err := fingerprintFile(filepath.Join(source, "gopeed.db"))
	if err != nil || *m.SourceDatabase != want || !want.Present || want.Size == 0 {
		t.Fatal(m.SourceDatabase, want, err)
	}
}

// Reviewer reproduction M1: after a rollback build 25 keeps using the legacy
// root; reopening the earlier import would silently show the old state.
func TestLegacyUseAfterImportIsRefusedWithoutChangingEitherRoot(t *testing.T) {
	source, target := fixture(t)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal("an unchanged legacy root must reopen the import", err)
	}
	useLegacy(t, source)
	current, _ := summarize(target, true)
	legacy, _ := fingerprintFile(filepath.Join(source, "gopeed.db"))
	for i := 0; i < 2; i++ {
		if _, err := Import(Options{Source: source, Target: target}); !errors.Is(err, ErrLegacyChanged) {
			t.Fatal(i, err)
		}
	}
	currentAfter, _ := summarize(target, true)
	legacyAfter, _ := fingerprintFile(filepath.Join(source, "gopeed.db"))
	if current != currentAfter || legacy != legacyAfter {
		t.Fatal("a refused start changed one of the roots")
	}
	// Moving the legacy root aside keeps using the imported data.
	if err := os.Rename(source, source+".kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
}

// Moving the imported root aside (what the rollback script now does) makes
// the next start import the legacy root again, including its newer records.
func TestMovedAsideImportReimportsCurrentLegacyData(t *testing.T) {
	source, target := fixture(t)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	useLegacy(t, source)
	if err := os.Rename(target, target+".rolled-back-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(target, "gopeed.db"), 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("task")).Get([]byte("created-after-rollback")) == nil {
			t.Fatal("re-import missed records written by the legacy app")
		}
		return nil
	})
}

func TestRunningLegacyAppIsNotCompared(t *testing.T) {
	source, target := fixture(t)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	useLegacy(t, source)
	running, err := bolt.Open(filepath.Join(source, "gopeed.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal("the store lock of a running legacy app must not block startup", err)
	}
	running.Close()
	if _, err = Import(Options{Source: source, Target: target}); !errors.Is(err, ErrLegacyChanged) {
		t.Fatal(err)
	}
}

// upstreamWrite stands in for upstream Gopeed, which uses the same data root
// and database file name as the legacy kownload.
func upstreamWrite(t *testing.T, source, value string) {
	t.Helper()
	os.MkdirAll(source, 0700)
	db, err := bolt.Open(filepath.Join(source, "gopeed.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("task"))
		if err != nil {
			return err
		}
		return b.Put([]byte("upstream"), []byte(value))
	}); err != nil {
		t.Fatal(err)
	}
}

// Review finding R2-1 (reviewer reproduction E1): after a fresh setup the
// legacy root that appears later belongs to upstream Gopeed, not to a
// migrated kownload, so its changes never block startup.
func TestUpstreamGopeedRootAfterFreshSetupIsIgnored(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "com.gopeed.gopeed"), filepath.Join(dir, "current")
	if s, err := Import(Options{Source: source, Target: target}); err != nil || s.Imported {
		t.Fatal(s, err)
	}
	upstreamWrite(t, source, "gopeed-upstream-task")
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal("upstream Gopeed starting must not block kownload", err)
	}
	upstreamWrite(t, source, "gopeed-again")
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal("upstream Gopeed running again must not block kownload", err)
	}
}

// Review finding R2-1: a user who imported from the legacy root and keeps
// using upstream Gopeed there can choose once to keep this root and stop the
// check; the choice is stored in the marker and survives later changes.
func TestIgnoreLegacyChangesIsPersistent(t *testing.T) {
	source, target := fixture(t)
	first, err := Import(Options{Source: source, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	upstreamWrite(t, source, "after-import")
	if _, err = Import(Options{Source: source, Target: target}); !errors.Is(err, ErrLegacyChanged) {
		t.Fatal(err)
	}
	kept, err := Import(Options{Source: source, Target: target, IgnoreLegacyChanges: true})
	if err != nil || kept != first {
		t.Fatal("the persistent choice must keep the imported data", kept, err)
	}
	data, _ := os.ReadFile(filepath.Join(target, Marker))
	var m record
	if json.Unmarshal(data, &m) != nil || !m.IgnoreLegacyChanges || m.Summary != first || m.SourceDatabase == nil {
		t.Fatal("marker did not keep the choice and the import record", string(data))
	}
	if _, err = os.Stat(filepath.Join(target, markerTemporary)); !os.IsNotExist(err) {
		t.Fatal("marker rewrite left its temporary file")
	}
	upstreamWrite(t, source, "later-change")
	if _, err = Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal("later changes must be ignored after the choice", err)
	}
	if current, _ := summarize(target, true); current != first {
		t.Fatal("the choice changed the imported data")
	}
}

func TestIgnoreChoiceCompletesPendingImport(t *testing.T) {
	source, target := fixture(t)
	Import(Options{Source: source, Target: target, Hook: func(phase string) error {
		if phase == "published" {
			return errors.New("fixture_interrupt")
		}
		return nil
	}})
	upstreamWrite(t, source, "after-import")
	if _, err := Import(Options{Source: source, Target: target}); !errors.Is(err, ErrLegacyChanged) {
		t.Fatal(err)
	}
	if _, err := Import(Options{Source: source, Target: target, IgnoreLegacyChanges: true}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(target, Marker))
	var m record
	if json.Unmarshal(data, &m) != nil || !m.IgnoreLegacyChanges {
		t.Fatal("pending import published without the choice", string(data))
	}
}

func TestMarkerWithoutFingerprintAndPendingImport(t *testing.T) {
	source, target := fixture(t)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(target, Marker))
	var m map[string]any
	json.Unmarshal(data, &m)
	delete(m, "sourceDatabase")
	data, _ = json.Marshal(m)
	os.WriteFile(filepath.Join(target, Marker), data, 0600)
	useLegacy(t, source)
	if _, err := Import(Options{Source: source, Target: target}); err != nil {
		t.Fatal("markers from before fingerprints are not compared", err)
	}

	source, target = fixture(t)
	Import(Options{Source: source, Target: target, Hook: func(phase string) error {
		if phase == "published" {
			return errors.New("fixture_interrupt")
		}
		return nil
	}})
	useLegacy(t, source)
	if _, err := Import(Options{Source: source, Target: target}); !errors.Is(err, ErrLegacyChanged) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, Marker)); !os.IsNotExist(err) {
		t.Fatal("a changed legacy root must not complete a pending import")
	}
}
