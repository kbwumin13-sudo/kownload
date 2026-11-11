package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Each child exits at the actual Bolt transaction boundary. The receipt is
// durable data, while the one-shot authorization exists only in its process.
type explicitRetryExitStore struct {
	AtomicStorage
	point string
}
type explicitRetryExitTx struct {
	StorageTransaction
	point string
}

func (s explicitRetryExitTx) Put(bucket, key string, value any) error {
	if err := s.StorageTransaction.Put(bucket, key, value); err != nil {
		return err
	}
	if s.point == bucket {
		os.Exit(78)
	}
	return nil
}
func (s explicitRetryExitStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		return fn(explicitRetryExitTx{tx, s.point})
	})
	if err == nil && s.point == "commit-before-ack" {
		os.Exit(78)
	}
	return err
}

func explicitRetryRestartFixture(t *testing.T, dir string) (*BoltStorage, string, *Task, *taskRecoveryOwner, RecoveryRetryRequest) {
	t.Helper()
	store := NewBoltStorage(filepath.Join(dir, "db"))
	dataset, task, owner, request := explicitRetryFixture(t, store)
	task.Meta.Opts.Path = dir
	task.Meta.Opts.Name = "preserved.bin"
	if err := store.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(bucketSave, task.ID, json.RawMessage(`{"opaqueSavedCheckpoint":9007199254740993,"spans":[[0,4095]]}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preserved.bin"), []byte("already downloaded prefix"), 0600); err != nil {
		t.Fatal(err)
	}
	return store, dataset, task, owner, request
}

func TestExplicitRecoveryRetryCrashWorker(t *testing.T) {
	dir := os.Getenv("K15_EXPLICIT_RETRY_CRASH_DIR")
	if dir == "" {
		return
	}
	store, dataset, _, owner, request := explicitRetryRestartFixture(t, dir)
	point := os.Getenv("K15_EXPLICIT_RETRY_CRASH_POINT")
	if point != "ack-before-io" {
		owner.store = explicitRetryExitStore{store, point}
	}
	_, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
	if err != nil || !fresh {
		t.Fatal("failed to reach acknowledged retry", err)
	}
	// Deliberately do not call begin, Start or any network operation.
	os.Exit(78)
}

func TestExplicitRecoveryRetryBoltCrashBoundaries(t *testing.T) {
	for _, point := range []string{bucketTask, bucketTaskRecovery, bucketRecoveryRequired, "commit-before-ack", "ack-before-io"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(binary, "-test.run=^TestExplicitRecoveryRetryCrashWorker$")
			child.Dir = dir
			child.Env = append(os.Environ(), "K15_EXPLICIT_RETRY_CRASH_DIR="+dir, "K15_EXPLICIT_RETRY_CRASH_POINT="+point)
			raw, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 78 {
				t.Fatalf("crash worker failed: %v %s", err, raw)
			}
			store := NewBoltStorage(filepath.Join(dir, "db"))
			defer store.Close()
			task, record, err := readRecovery(store, "recovery-fixture-task")
			if err != nil {
				t.Fatal("partial triplet on reopen", err)
			}
			committed := point == "commit-before-ack" || point == "ack-before-io"
			wantVersion := 1
			if committed {
				wantVersion = 2
			}
			if record.Version != wantVersion || task.Recovery.Version != wantVersion || task.Meta.Opts.Path != dir || task.Meta.Opts.Name != "preserved.bin" {
				t.Fatal("reopen changed task/path or transaction boundary", record.Version, task.Meta.Opts)
			}
			var checkpoint json.RawMessage
			if ok, err := store.Get(bucketSave, task.ID, &checkpoint); err != nil || !ok || !bytes.Equal(checkpoint, []byte(`{"opaqueSavedCheckpoint":9007199254740993,"spans":[[0,4095]]}`)) {
				t.Fatal("checkpoint changed", err, string(checkpoint))
			}
			file, err := os.ReadFile(filepath.Join(dir, "preserved.bin"))
			if err != nil || string(file) != "already downloaded prefix" {
				t.Fatal("output changed", err)
			}
			owner, err := newTaskRecoveryOwner(store, &task)
			if err != nil || owner.retryAuthorization.Load() != nil {
				t.Fatal("reopen reconstructed authority", err)
			}
			before := owner.record.TotalUsed
			if lease, err := owner.begin(record.Generation+1, ""); err != nil || lease == nil || owner.record.TotalUsed != before+1 || owner.record.Episode != record.Episode {
				t.Fatal("restart granted free attempt or new episode", err, owner.record)
			}
		})
	}
}

// A separate executable compiled from the unmodified frozen v1 reader opens
// this database; its rejection is recorded by the verification command.
func TestExplicitRecoveryRetryWriteCompatibilityDatabase(t *testing.T) {
	dir := os.Getenv("K15_EXPLICIT_RETRY_COMPAT_DIR")
	if dir == "" {
		return
	}
	store, dataset, task, owner, request := explicitRetryRestartFixture(t, dir)
	defer store.Close()
	if _, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1); err != nil || !fresh {
		t.Fatal(err)
	}
	owner.invalidate()
	if _, _, err := readRecovery(store, task.ID); err != nil {
		t.Fatal(err)
	}
	if owner.CheckRecovery(transfer.RecoveryToken{}) == nil {
		t.Fatal("fixture issued an execution token")
	}
}
