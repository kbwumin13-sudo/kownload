//go:build darwin || linux

package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// Real Bolt test adapter for the module's callback contract. These are private
// fixture buckets, not production Task/S0 schema or proof of Core integration.
var completionTestTask = []byte("fixture-task")
var completionTestRecord = []byte("fixture-record")
var completionTestRequired = []byte("fixture-required")
var completionTestKey = []byte("same-task")

func completionTestPut(tx *bolt.Tx, bucket, value []byte) error {
	b, err := tx.CreateBucketIfNotExists(bucket)
	if err != nil {
		return err
	}
	return b.Put(completionTestKey, value)
}

func writeCompletionTestDB(db *bolt.DB, r VerifiedCompletionRecord, committed bool) error {
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	status := []byte("running")
	if committed {
		status = []byte("done")
	}
	return db.Update(func(tx *bolt.Tx) error {
		if err := completionTestPut(tx, completionTestTask, status); err != nil {
			return err
		}
		if err := completionTestPut(tx, completionTestRecord, encoded); err != nil {
			return err
		}
		return completionTestPut(tx, completionTestRequired, []byte(VerifiedCompletionCapability))
	})
}

func readCompletionTestDB(db *bolt.DB) (r VerifiedCompletionRecord, done bool, err error) {
	err = db.View(func(tx *bolt.Tx) error {
		get := func(bucket []byte) []byte {
			if b := tx.Bucket(bucket); b != nil {
				return b.Get(completionTestKey)
			}
			return nil
		}
		if string(get(completionTestRequired)) != VerifiedCompletionCapability {
			return errors.New("missing or unsupported required-state")
		}
		if err := json.Unmarshal(get(completionTestRecord), &r); err != nil {
			return err
		}
		if r.Version != 1 || r.ID == "" {
			return errors.New("unsupported completion record")
		}
		switch string(get(completionTestTask)) {
		case "done":
			done = true
		case "running":
		default:
			return errors.New("missing Task")
		}
		if done != (r.Phase == CompletionCommitted) {
			return errors.New("Task/completion transaction is inconsistent")
		}
		return nil
	})
	return
}

func completionBoltHooks(db *bolt.DB, crashAt string) VerifiedCompletionHooks {
	crash := func(point string) {
		if crashAt == point {
			// An actual process exit: no deferred Close/cleanup. Updates before this
			// point used Bolt's normal synchronous commit, not an in-memory mock.
			os.Exit(97)
		}
	}
	return VerifiedCompletionHooks{
		CheckRun: func() error {
			_, _, err := readCompletionTestDB(db)
			return err
		},
		Fence: func(ctx context.Context, body func() error) error {
			// Each fixture process has one completion and no competing lifecycle
			// mutation. Racing invalidation is tested with the shared-gate fixture.
			return body()
		},
		Save: func(r VerifiedCompletionRecord) error {
			crash("before_" + string(r.Phase))
			previous, done, err := readCompletionTestDB(db)
			if err != nil || done || previous.ID != r.ID {
				return errors.New("save requires live matching fixture intent")
			}
			if err := writeCompletionTestDB(db, r, false); err != nil {
				return err
			}
			crash("after_" + string(r.Phase))
			return nil
		},
		Commit: func(r VerifiedCompletionRecord) (bool, error) {
			crash("before_commit")
			previous, done, err := readCompletionTestDB(db)
			if err != nil || previous.ID != r.ID {
				return false, errors.New("commit requires matching fixture intent")
			}
			if done {
				return false, nil
			}
			if err := writeCompletionTestDB(db, r, true); err != nil {
				return false, err
			}
			crash("after_commit")
			return true, nil
		},
	}
}

func TestVerifiedCompletionCrashHelper(t *testing.T) {
	root := os.Getenv("K12_COMPLETION_CRASH_ROOT")
	if root == "" {
		return
	}
	db, err := bolt.Open(filepath.Join(root, "fixture.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, _, err := readCompletionTestDB(db)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := OpenCompletionDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	file, err := dir.OpenStaging(r)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	point := os.Getenv("K12_COMPLETION_CRASH_AT")
	permit, err := completerForTest(t).Complete(context.Background(), dir, file, r, completionBoltHooks(db, point))
	if err != nil {
		t.Fatal(err)
	}
	if point == "after_return" {
		os.Exit(97)
	}
	if permit.Fresh {
		if err := os.WriteFile(filepath.Join(root, "success-admission"), []byte("one"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifiedCompletionRealProcessCrashes(t *testing.T) {
	points := []struct {
		point     string
		persisted VerifiedCompletionPhase
		published bool
		committed bool
	}{
		{"before_verifying", CompletionWaiting, false, false},
		{"after_verifying", CompletionVerifying, false, false},
		{"before_verified", CompletionVerifying, false, false},
		{"after_verified", CompletionVerified, false, false},
		{"before_published_pending_commit", CompletionVerified, true, false},
		{"after_published_pending_commit", CompletionPublished, true, false},
		{"before_commit", CompletionPublished, true, false},
		{"after_commit", CompletionCommitted, true, true},
		{"after_return", CompletionCommitted, true, true},
	}
	for _, point := range points {
		t.Run(point.point, func(t *testing.T) {
			f := newCompletionFixture(t, []byte("same-task durable crash fixture"))
			dbPath := filepath.Join(f.path, "fixture.db")
			db, err := bolt.Open(dbPath, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeCompletionTestDB(db, f.record, false); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			// Only this private fixture root is given to the same test executable.
			cmd := exec.Command(os.Args[0], "-test.run=^TestVerifiedCompletionCrashHelper$", "-test.timeout=15s")
			cmd.Dir = f.path
			cmd.Env = append(os.Environ(), "K12_COMPLETION_CRASH_ROOT="+f.path, "K12_COMPLETION_CRASH_AT="+point.point)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 97 {
				t.Fatalf("fixture did not exit at crash boundary: %v\n%s", err, output)
			}
			db, err = bolt.Open(dbPath, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			r, done, err := readCompletionTestDB(db)
			if err != nil || r.ID != f.record.ID || r.Phase != point.persisted || done != point.committed {
				t.Fatalf("unexpected durable state after process exit: %+v %v %v", r, done, err)
			}
			_, finalErr := os.Lstat(filepath.Join(f.path, r.FinalName))
			if (finalErr == nil) != point.published {
				t.Fatalf("publication disagrees with crash boundary: %v", finalErr)
			}
			if _, err := os.Stat(filepath.Join(f.path, "success-admission")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("crashed child dispatched success", err)
			}
			// Reopen the same physical directory/staging and Task/completion ID.
			reopened, err := OpenCompletionDirectory(f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			file, err := reopened.OpenStaging(r)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			permit, err := completerForTest(t).Complete(context.Background(), reopened, file, r, completionBoltHooks(db, ""))
			if err != nil || permit.Fresh == point.committed {
				t.Fatalf("same-ID recovery permission: %+v %v", permit, err)
			}
			after, done, err := readCompletionTestDB(db)
			if err != nil || !done || after.Phase != CompletionCommitted || after.ID != f.record.ID {
				t.Fatalf("recovered transaction was not committed: %+v %v", after, err)
			}
			if err := reopened.checkFinal(file, after); err != nil {
				t.Fatal("recovery lost final ownership", err)
			}
		})
	}
}

func TestVerifiedCompletionRealBoltUnknownCommitACK(t *testing.T) {
	f := newCompletionFixture(t, []byte("unknown ACK fixture"))
	db, err := bolt.Open(filepath.Join(f.path, "fixture.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCompletionTestDB(db, f.record, false); err != nil {
		t.Fatal(err)
	}
	hooks := completionBoltHooks(db, "")
	commit := hooks.Commit
	hooks.Commit = func(r VerifiedCompletionRecord) (bool, error) {
		_, err := commit(r)
		if err != nil {
			return false, err
		}
		return false, errors.New("durable commit succeeded; ACK lost")
	}
	v := completerForTest(t)
	permit, err := v.Complete(context.Background(), f.dir, f.file, f.record, hooks)
	if err == nil || permit.Fresh {
		t.Fatal("unknown ACK granted success permission", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = bolt.Open(filepath.Join(f.path, "fixture.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, done, err := readCompletionTestDB(db)
	if err != nil || !done {
		t.Fatal("authoritative reopen lost acknowledged-to-disk commit", err)
	}
	permit, err = v.Complete(context.Background(), f.dir, f.file, r, completionBoltHooks(db, ""))
	if err != nil || permit.Fresh {
		t.Fatal("reopen replayed success after unknown ACK", err)
	}
}
