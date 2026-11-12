package download

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Test-only v3 rows exercise the shared Retry reader/writer. They are not the
// actual v3 enrollment writer or evidence of old-reader compatibility.
func coverageRetryFixture(t *testing.T, store AtomicStorage) (string, *Task, *taskRecoveryOwner, RecoveryRetryRequest) {
	t.Helper()
	dataset, task, owner, request := explicitRetryFixture(t, store)
	coverage, e := transfer.NewHTTPCoverageState(strings.Repeat("a", 64), 2<<20)
	if e != nil {
		t.Fatal(e)
	}
	coverage.CheckpointHash = strings.Repeat("c", 64)
	coverage.DurableRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1536}}
	coverage.SettledRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1024}}
	coverage.SettlementSequence = 4
	var record recoveryRecord
	e = store.UpdateAtomic(func(tx StorageTransaction) error {
		current, r, e := readRecovery(tx, task.ID)
		if e != nil {
			return e
		}
		r.Version = 3
		r.Coverage = &coverage
		current.Recovery = &TaskRecoveryRef{Version: 3}
		if !r.valid(task.ID) {
			t.Fatal("invalid v3 test record")
		}
		record = r
		if e = tx.Put(bucketTask, task.ID, &current); e != nil {
			return e
		}
		if e = tx.Put(bucketTaskRecovery, task.ID, &r); e != nil {
			return e
		}
		return tx.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{3, recoveryCapability})
	})
	if e != nil {
		t.Fatal(e)
	}
	owner.record = record
	task.Recovery = &TaskRecoveryRef{Version: 3}
	return dataset, task, owner, request
}
func coverageRetryCopy(t *testing.T, value any) []byte {
	t.Helper()
	b, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestCoverageRetryKeepsVersionAndSettledHistory(t *testing.T) {
	for _, name := range []string{"mem", "bolt"} {
		t.Run(name, func(t *testing.T) {
			var store AtomicStorage = NewMemStorage()
			if name == "bolt" {
				store = NewBoltStorage(t.TempDir())
			}
			defer store.Close()
			dataset, task, owner, request := coverageRetryFixture(t, store)
			before := owner.record
			coverage := coverageRetryCopy(t, before.Coverage)
			record, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
			if err != nil || !fresh {
				t.Fatal("v3 explicit Retry rejected/downgraded", err)
			}
			saved, read, e := readRecovery(store, task.ID)
			if e != nil || read.Version != 3 || saved.Recovery.Version != 3 || record.Version != 3 {
				t.Fatal("triplet v3 lost", e)
			}
			var required recoveryRequirement
			store.Get(bucketRecoveryRequired, task.ID, &required)
			if required.Version != 3 {
				t.Fatal("required downgraded")
			}
			if string(coverageRetryCopy(t, read.Coverage)) != string(coverage) || read.SourceHash != before.SourceHash || read.ExecutionDefaultsHash != before.ExecutionDefaultsHash || read.NotBefore != before.NotBefore || saved.Meta.Opts.Path != task.Meta.Opts.Path {
				t.Fatal("Retry lost coverage/source/path/cooldown")
			}
			if lease, e := owner.begin(2, ""); e != nil || lease.Check() != nil || owner.record.TotalUsed != 0 {
				t.Fatal("fresh explicit ACK grant missing", e)
			}
			if _, e = owner.begin(3, ""); e != nil || owner.record.TotalUsed != 1 {
				t.Fatal("ordinary Continue reset budget", e)
			}
			if _, winner, e := owner.commitExplicitRetry(context.Background(), dataset, request, 3); e != nil || winner {
				t.Fatal("replay granted authority", e)
			}
			if string(coverageRetryCopy(t, owner.record.Coverage)) != string(coverage) {
				t.Fatal("begin/replay changed settled baseline")
			}
		})
	}
}
func TestCoverageRetryHistoricalReceiptUsesOriginalTarget(t *testing.T) {
	for _, name := range []string{"mem", "bolt"} {
		t.Run(name, func(t *testing.T) {
			var store AtomicStorage = NewMemStorage()
			if name == "bolt" {
				store = NewBoltStorage(t.TempDir())
			}
			defer store.Close()
			dataset, task, owner, request := coverageRetryFixture(t, store)
			record := owner.record
			record.Episode = "after-earned-settlement"
			record.SourceRevision++
			record.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: request.ActionID, FromEpisode: request.ExpectedEpisode, ToEpisode: "original-user-retry-target", SourceRevision: request.ExpectedSourceRevision}
			if !record.valid(task.ID) {
				t.Fatal("invalid historical fixture")
			}
			if e := store.Put(bucketTaskRecovery, task.ID, &record); e != nil {
				t.Fatal(e)
			}
			before := coverageRetryCopy(t, record)
			d := &Downloader{storage: store}
			query, e := d.QueryRecoveryActionForDataset(dataset, task.ID, request)
			if e != nil {
				t.Fatal(e)
			}
			replay, e := d.RetryRecoveryForDataset(context.Background(), dataset, task.ID, request)
			if e != nil {
				t.Fatal(e)
			}
			for _, s := range []RecoveryTaskState{query, replay} {
				if s.Version != 1 || s.Episode != record.Episode || s.Receipt == nil || s.Receipt.Version != 1 || s.Receipt.NewEpisode != record.LastExplicitRetry.ToEpisode || s.LastExplicitRetry.ToEpisode != s.Receipt.NewEpisode {
					t.Fatalf("historical receipt misreported current episode: %+v", s.Receipt)
				}
			}
			if query.Receipt.State != "accepted" || replay.Receipt.State != "replayed" {
				t.Fatal("changed wire outcomes")
			}
			var after recoveryRecord
			store.Get(bucketTaskRecovery, task.ID, &after)
			if string(before) != string(coverageRetryCopy(t, after)) || owner.retryAuthorization.Load() != nil {
				t.Fatal("history query/replay wrote or granted authority")
			}
			wrong := request
			wrong.ExpectedSourceRevision++
			if _, e = d.QueryRecoveryActionForDataset(dataset, task.ID, wrong); !errors.Is(e, ErrRecoveryRetryConflict) {
				t.Fatal("same action changed payload", e)
			}
		})
	}
}
func TestCoverageRetryUnknownACKReloadNeverRecreatesFreeAttempt(t *testing.T) {
	for _, name := range []string{"mem", "bolt"} {
		t.Run(name, func(t *testing.T) {
			var store AtomicStorage = NewMemStorage()
			dir := t.TempDir()
			if name == "bolt" {
				store = NewBoltStorage(dir)
			}
			defer func() { store.Close() }()
			dataset, task, owner, request := coverageRetryFixture(t, store)
			coverage := coverageRetryCopy(t, owner.record.Coverage)
			fault := &recoveryAckFault{AtomicStorage: store}
			fault.armed.Store(true)
			owner.store = fault
			record, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
			if err == nil || fresh || !reflect.DeepEqual(record, recoveryRecord{}) || owner.retryAuthorization.Load() != nil {
				t.Fatal("unknown ACK leaked winner", err)
			}
			if name == "bolt" {
				if e := store.Close(); e != nil {
					t.Fatal(e)
				}
				store = NewBoltStorage(dir)
			}
			var saved Task
			if ok, e := store.Get(bucketTask, task.ID, &saved); e != nil || !ok {
				t.Fatal(e)
			}
			restored, e := newTaskRecoveryOwner(store, &saved)
			if e != nil {
				t.Fatal(e)
			}
			if restored.record.Version != 3 || restored.record.Episode == request.ExpectedEpisode || restored.record.LastExplicitRetry == nil {
				t.Fatal("fixture did not actually commit v3 Retry")
			}
			if _, winner, e := restored.commitExplicitRetry(context.Background(), dataset, request, 1); e != nil || winner || restored.retryAuthorization.Load() != nil {
				t.Fatal("query/reload restored grant", e)
			}
			if _, e = restored.begin(2, ""); e != nil || restored.record.TotalUsed != 1 {
				t.Fatal("reload got free attempt", e)
			}
			if string(coverageRetryCopy(t, restored.record.Coverage)) != string(coverage) {
				t.Fatal("unknown ACK reset history")
			}
		})
	}
}

func TestCoverageRetryCannotEraseUnsupportedNotBefore(t *testing.T) {
	for _, name := range []string{"mem", "bolt"} {
		t.Run(name, func(t *testing.T) {
			var store AtomicStorage = NewMemStorage()
			if name == "bolt" {
				store = NewBoltStorage(t.TempDir())
			}
			defer store.Close()
			dataset, task, owner, request := coverageRetryFixture(t, store)
			record := owner.record
			record.NotBefore = "2030-01-01T00:00:00Z"
			if err := store.Put(bucketTaskRecovery, task.ID, &record); err != nil {
				t.Fatal(err)
			}
			before := coverageRetryCopy(t, record)
			if _, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1); !errors.Is(err, transfer.ErrRecoveryHeld) || fresh {
				t.Fatal("unsupported cooldown reset by Retry", err)
			}
			var after recoveryRecord
			if ok, err := store.Get(bucketTaskRecovery, task.ID, &after); err != nil || !ok {
				t.Fatal(err)
			}
			if string(before) != string(coverageRetryCopy(t, after)) {
				t.Fatal("Retry erased/corrupted cooldown or coverage")
			}
		})
	}
}
