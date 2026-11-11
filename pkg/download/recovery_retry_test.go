package download

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func explicitRetryFixture(t *testing.T, store AtomicStorage) (string, *Task, *taskRecoveryOwner, RecoveryRetryRequest) {
	t.Helper()
	task, owner := recoveryFixtureStore(t, store, t.TempDir())
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	var dataset datasetRecord
	if ok, err := store.Get(bucketConfig, datasetIdentityKey, &dataset); err != nil || !ok {
		t.Fatal(ok, err)
	}
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lease.Retry(context.Background(), 0, "worker/0", []string{"worker/0"}); err != nil {
		t.Fatal(err)
	}
	var saved Task
	if _, err = store.Get(bucketTask, task.ID, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Status = base.DownloadStatusError
	if err = store.Put(bucketTask, task.ID, &saved); err != nil {
		t.Fatal(err)
	}
	return dataset.DatasetID, &saved, owner, RecoveryRetryRequest{1, "user-retry-1", owner.record.Episode, owner.record.SourceRevision, "retry"}
}

func TestExplicitRecoveryRetryConcurrentAndSingleAuthority(t *testing.T) {
	store := NewMemStorage()
	dataset, task, owner, request := explicitRetryFixture(t, store)
	old := owner.record
	var winners atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				winners.Add(1)
			}
		}()
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatalf("fresh winners=%d", winners.Load())
	}
	saved, record, err := readRecovery(store, task.ID)
	if err != nil || record.Version != 2 || saved.Recovery.Version != 2 || record.Episode == old.Episode || record.TotalUsed != 0 || record.SourceHash != old.SourceHash || record.ExecutionDefaultsHash != old.ExecutionDefaultsHash || saved.Meta.Opts.Path != task.Meta.Opts.Path {
		t.Fatalf("reset changed binding or failed: %v %+v", err, record)
	}
	if _, err = owner.begin(2, ""); err != nil || owner.record.TotalUsed != 0 {
		t.Fatal("fresh ACK did not fund one attempt", err)
	}
	if _, err = owner.begin(3, ""); err != nil || owner.record.TotalUsed != 1 {
		t.Fatal("authorization repeated", err)
	}
}

func TestExplicitRecoveryRetryHistoricalReplayAndDefaults(t *testing.T) {
	store := NewMemStorage()
	dataset, task, owner, request := explicitRetryFixture(t, store)
	first, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	var saved Task
	store.Get(bucketTask, task.ID, &saved)
	defaults, err := transfer.HTTPExecutionDefaultsHash(saved.Meta.Req, "new inherited UA", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.persistSnapshot(&saved, nil, false, defaults); err != nil {
		t.Fatal(err)
	}
	if owner.record.SourceRevision <= first.SourceRevision || !owner.record.valid(task.ID) || owner.record.LastExplicitRetry.SourceRevision != first.SourceRevision {
		t.Fatal("legitimate defaults CAS invalidated history")
	}
	if _, fresh, err = owner.commitExplicitRetry(context.Background(), dataset, request, 1); err != nil || fresh {
		t.Fatal("history replay issued IO", err)
	}
	conflict := request
	conflict.ExpectedSourceRevision++
	if _, _, err = owner.commitExplicitRetry(context.Background(), dataset, conflict, 1); !errors.Is(err, ErrRecoveryRetryConflict) {
		t.Fatal("same scoped action changed intent", err)
	}
	next := RecoveryRetryRequest{1, "user-retry-2", owner.record.Episode, owner.record.SourceRevision, "retry"}
	if _, fresh, err = owner.commitExplicitRetry(context.Background(), dataset, next, 2); err != nil || !fresh {
		t.Fatal(err)
	}
	if _, fresh, err = owner.commitExplicitRetry(context.Background(), dataset, request, 2); !errors.Is(err, ErrRecoveryRetryStale) || fresh {
		t.Fatal("old overwritten receipt reset episode", err)
	}
}

func TestExplicitRecoveryRetryUnknownAckAndReloadHaveNoFreeAttempt(t *testing.T) {
	store := NewMemStorage()
	dataset, task, owner, request := explicitRetryFixture(t, store)
	fault := &recoveryAckFault{AtomicStorage: store}
	fault.armed.Store(true)
	owner.store = fault
	if _, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1); err == nil || fresh || owner.retryAuthorization.Load() != nil {
		t.Fatal("unknown ACK produced authority", err)
	}
	var saved Task
	store.Get(bucketTask, task.ID, &saved)
	restored, err := newTaskRecoveryOwner(store, &saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := restored.commitExplicitRetry(context.Background(), dataset, request, 1); err != nil || fresh || restored.retryAuthorization.Load() != nil {
		t.Fatal("receipt query restored authority", err)
	}
	if _, err = restored.begin(2, ""); err != nil || restored.record.TotalUsed != 1 {
		t.Fatal("reload got free initial IO", err)
	}
}

func TestExplicitRecoveryRetryThreeWayVersions(t *testing.T) {
	for _, mode := range []string{"valid", "missing-last", "bad-last-version", "future-source", "mixed-task", "mixed-required", "unknown-version"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemStorage()
			dataset, task, owner, request := explicitRetryFixture(t, store)
			record, _, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
			if err != nil {
				t.Fatal(err)
			}
			var saved Task
			store.Get(bucketTask, task.ID, &saved)
			required := recoveryRequirement{2, recoveryCapability}
			switch mode {
			case "missing-last":
				record.LastExplicitRetry = nil
			case "bad-last-version":
				record.LastExplicitRetry.Version = 3
			case "future-source":
				record.LastExplicitRetry.SourceRevision = record.SourceRevision + 1
			case "mixed-task":
				saved.Recovery = &TaskRecoveryRef{Version: 1}
			case "mixed-required":
				required.Version = 1
			case "unknown-version":
				record.Version = 3
			}
			store.Put(bucketTask, task.ID, &saved)
			store.Put(bucketTaskRecovery, task.ID, &record)
			store.Put(bucketRecoveryRequired, task.ID, &required)
			_, _, err = readRecovery(store, task.ID)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, transfer.ErrRecoveryHeld) {
				t.Fatal("incomplete v2 was not held", err)
			}
		})
	}
}

type retryPutFaultStore struct {
	AtomicStorage
	bucket string
}
type retryPutFaultTx struct {
	StorageTransaction
	bucket string
}

func (t retryPutFaultTx) Put(bucket, key string, value any) error {
	if bucket == t.bucket {
		return errors.New("synthetic triplet write failure")
	}
	return t.StorageTransaction.Put(bucket, key, value)
}
func (s retryPutFaultStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(retryPutFaultTx{tx, s.bucket}) })
}

func TestExplicitRecoveryRetryTripletWriteFailuresRollBack(t *testing.T) {
	for _, bucket := range []string{bucketTask, bucketTaskRecovery, bucketRecoveryRequired} {
		t.Run(bucket, func(t *testing.T) {
			store := NewMemStorage()
			dataset, task, owner, request := explicitRetryFixture(t, store)
			before := owner.record
			owner.store = retryPutFaultStore{store, bucket}
			if _, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1); err == nil || fresh || owner.retryAuthorization.Load() != nil {
				t.Fatal("failed write granted IO", err)
			}
			saved, record, err := readRecovery(store, task.ID)
			if err != nil || record.Version != 1 || saved.Recovery.Version != 1 || record.Episode != before.Episode || record.TotalUsed != before.TotalUsed {
				t.Fatal("partial v2 transaction", err, record)
			}
		})
	}
}
