package download

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type d4k12Fixture struct {
	store   AtomicStorage
	dataset string
	id      string
	initial transfer.HTTPCoverageState
}

func d4k12New(t *testing.T, backend string, checkpoint bool) *d4k12Fixture {
	t.Helper()
	var store AtomicStorage = NewMemStorage()
	if backend == "bolt" {
		store = NewBoltStorage(t.TempDir())
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Setup([]string{bucketConfig, bucketTask, bucketSave, bucketTaskRecovery, bucketRecoveryRequired,
		bucketTaskOperations, bucketOperationTasks, bucketTaskCompletion, bucketCompletionRequired}); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	identity, err := (&Downloader{storage: store}).DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	task := NewTask()
	task.ID, task.Protocol, task.Status = "d4-kernel-fixture", "http", base.DownloadStatusPause
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/file?exact=1"},
		Opts: &base.Options{Name: "unchanged.bin", Path: "/synthetic-only/no-output-created",
			Extra: map[string]any{"exact": json.Number("9007199254740993"), "nested": map[string]any{"keep": "original"}}}}
	task.Progress, task.Recovery = &Progress{Used: 7, Downloaded: 11}, &TaskRecoveryRef{Version: 1}
	task.Handoff = &BrowserHandoff{Version: 1, Phase: "committed"}
	defaults, err := transfer.HTTPExecutionDefaultsHash(task.Meta.Req, "", "direct")
	if err != nil {
		t.Fatal(err)
	}
	source, err := recoverySourceHash(task.Meta.Req, defaults)
	if err != nil {
		t.Fatal(err)
	}
	record := recoveryRecord{Version: 1, TaskID: task.ID, Episode: "old-episode", Generation: 4,
		SourceRevision: 2, SourceHash: source, ExecutionDefaultsHash: defaults,
		InitialAttempt: recoveryInitialAttempt{State: "issued", Generation: 4}, TotalUsed: 2, TransportUsed: 2,
		NextAction: 2, Actions: []recoveryAction{
			{ID: 1, Kind: "transport", Generation: 4, SourceRevision: 2, Members: []string{"part-a"}, Finished: []string{"part-a"}, State: "finished"},
			{ID: 2, Kind: "transport", Generation: 4, SourceRevision: 2, Members: []string{"part-b"}, Finished: []string{"part-b"}, State: "finished"},
		}}
	if !record.valid(task.ID) {
		t.Fatal("invalid fixture recovery record")
	}
	for _, part := range []struct {
		bucket string
		value  any
	}{{bucketTask, task}, {bucketTaskRecovery, &record}, {bucketRecoveryRequired, &recoveryRequirement{1, recoveryCapability}}} {
		if err := store.Put(part.bucket, task.ID, part.value); err != nil {
			t.Fatal(err)
		}
	}
	if checkpoint {
		if err := store.Put(bucketSave, task.ID, map[string]any{"offset": json.Number("9007199254740993"), "part": 1}); err != nil {
			t.Fatal(err)
		}
	}
	coverage, err := transfer.NewHTTPCoverageState(strings.Repeat("a", 64), 2*transfer.HTTPCoverageThreshold)
	if err != nil {
		t.Fatal(err)
	}
	return &d4k12Fixture{store: store, dataset: identity.DatasetID, id: task.ID, initial: coverage}
}

func (f *d4k12Fixture) capture(t *testing.T) (*Task, recoveryCoverageExpected) {
	t.Helper()
	var task *Task
	var expected recoveryCoverageExpected
	err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
		var err error
		task, expected.Record, err = readRecoveryCoverageParts(reader, f.id)
		if err != nil {
			return err
		}
		r := expected.Record
		expected.Token = transfer.RecoveryToken{TaskID: f.id, Episode: r.Episode, Generation: r.Generation, SourceRevision: r.SourceRevision}
		var checkpoint json.RawMessage
		expected.CheckpointPresent, err = reader.Get(bucketSave, f.id, &checkpoint)
		if err != nil {
			return err
		}
		if expected.CheckpointPresent {
			expected.CheckpointHash, err = checkpointDigest(checkpoint)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, expected
}

func (f *d4k12Fixture) install(t *testing.T) recoveryRecord {
	t.Helper()
	_, expected := f.capture(t)
	r, err := installRecoveryCoverage(f.store, f.dataset, expected, f.initial)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *d4k12Fixture) update(t *testing.T, earned bool) (recoveryCoverageExpected, recoveryCoverageUpdate) {
	t.Helper()
	task, expected := f.capture(t)
	checkpoint := json.RawMessage(`{"offset":1048576,"exact":9007199254740993}`)
	hash, err := checkpointDigest(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := transfer.PlanHTTPCoverageCheckpoint(*expected.Record.Coverage,
		[]transfer.HTTPCoverageRange{{Begin: 0, End: transfer.HTTPCoverageThreshold}}, hash, recoveryCoverageRangeLimit)
	if err != nil {
		t.Fatal(err)
	}
	task.Progress.Downloaded, task.UpdatedAt = transfer.HTTPCoverageThreshold, task.UpdatedAt.Add(time.Second)
	next := recoveryCoverageUpdate{Snapshot: task, Checkpoint: checkpoint, Plan: plan}
	if earned {
		next.NewEpisode = "new-episode"
	}
	return expected, next
}

func (f *d4k12Fixture) mutate(t *testing.T, change func(StorageTransaction, *Task, *recoveryRecord) error) {
	t.Helper()
	err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, err := readRecoveryCoverageParts(tx, f.id)
		if err != nil {
			return err
		}
		return change(tx, task, &record)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *d4k12Fixture) snapshot(t *testing.T) string {
	t.Helper()
	values := map[string]json.RawMessage{}
	err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
		for _, bucket := range []string{bucketTask, bucketSave, bucketTaskRecovery, bucketRecoveryRequired, bucketTaskCompletion, bucketCompletionRequired} {
			var raw json.RawMessage
			if _, err := reader.Get(bucket, f.id, &raw); err != nil {
				return err
			}
			values[bucket] = raw
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestD4K12InstallAtomicOwnedAndPreservesTask(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			for _, present := range []bool{false, true} {
				t.Run(fmt.Sprint(present), func(t *testing.T) {
					f := d4k12New(t, backend, present)
					before, expected := f.capture(t)
					result, err := installRecoveryCoverage(f.store, f.dataset, expected, f.initial)
					if err != nil || result.Version != 3 || result.Coverage == nil {
						t.Fatal("install failed", err)
					}
					after, current := f.capture(t)
					before.Recovery.Version = 3
					if a, _ := json.Marshal(before); !reflect.DeepEqual(a, mustD4K12JSON(t, after)) {
						t.Fatal("install rewrote original Task fields")
					}
					want := expected.Record
					want.Version, want.Coverage = 3, &f.initial
					if !reflect.DeepEqual(want, current.Record) || current.CheckpointHash != expected.CheckpointHash || current.CheckpointPresent != present {
						t.Fatal("install changed balances/source/checkpoint")
					}
					result.Actions[0].Members[0] = "caller-mutated"
					result.Coverage.DurableRanges = append(result.Coverage.DurableRanges, transfer.HTTPCoverageRange{Begin: 0, End: 1})
					_, again := f.capture(t)
					if !reflect.DeepEqual(current.Record, again.Record) || expected.Record.Version != 1 || len(f.initial.DurableRanges) != 0 {
						t.Fatal("install exposed aliases or mutated input")
					}
					state := f.snapshot(t)
					if r, err := installRecoveryCoverage(f.store, f.dataset, expected, f.initial); err == nil || !reflect.DeepEqual(r, recoveryRecord{}) || state != f.snapshot(t) {
						t.Fatal("install replay changed state or returned a result", err)
					}
				})
			}
		})
	}
}

func mustD4K12JSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestD4K12InstallPreservesRawTaskJSON(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4k12New(t, backend, true)
			read := func() map[string]any {
				var value map[string]any
				if err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
					_, err := reader.Get(bucketTask, f.id, &value)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := read()
			f.install(t)
			before["recovery"] = map[string]any{"version": json.Number("3")}
			after := read()
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("install changed raw Task beyond ref: before exact=%v after exact=%v", before["meta"].(map[string]any)["opts"].(map[string]any)["extra"], after["meta"].(map[string]any)["opts"].(map[string]any)["extra"])
			}
		})
	}
}

func TestD4K12ReadStrictAndOwned(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4k12New(t, backend, true)
			f.install(t)
			err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
				task, record, err := readRecoveryCoverage(reader, f.id)
				if err != nil {
					return err
				}
				task.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] = "changed"
				record.Actions[0].Finished[0] = "changed"
				record.Coverage.SettledRanges = append(record.Coverage.SettledRanges, transfer.HTTPCoverageRange{Begin: 0, End: 1})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			task, expected := f.capture(t)
			if task.Meta.Opts.Extra.(map[string]any)["exact"] != json.Number("9007199254740993") || task.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] != "original" || expected.Record.Actions[0].Finished[0] != "part-a" || len(expected.Record.Coverage.SettledRanges) != 0 {
				t.Fatalf("read aliased/rounded values: exact=%T(%v) nested=%v finished=%v settled=%v", task.Meta.Opts.Extra.(map[string]any)["exact"], task.Meta.Opts.Extra.(map[string]any)["exact"], task.Meta.Opts.Extra.(map[string]any)["nested"], expected.Record.Actions[0].Finished, expected.Record.Coverage.SettledRanges)
			}
		})
	}
	for _, mode := range []string{"legacy", "v1", "missing-ref", "missing-record", "missing-required", "mixed", "null-coverage", "nil-ranges", "unknown", "wrong-id", "wrong-source", "malformed-record", "malformed-ref", "malformed-required"} {
		t.Run(mode, func(t *testing.T) {
			f := d4k12New(t, "mem", true)
			if mode != "v1" && mode != "legacy" {
				f.install(t)
			}
			task, expected := f.capture(t)
			switch mode {
			case "legacy":
				task.Recovery = nil
				_ = f.store.Put(bucketTask, f.id, task)
				_ = f.store.Delete(bucketTaskRecovery, f.id)
				_ = f.store.Delete(bucketRecoveryRequired, f.id)
			case "missing-ref":
				task.Recovery = nil
				_ = f.store.Put(bucketTask, f.id, task)
			case "missing-record":
				_ = f.store.Delete(bucketTaskRecovery, f.id)
			case "missing-required":
				_ = f.store.Delete(bucketRecoveryRequired, f.id)
			case "mixed":
				_ = f.store.Put(bucketRecoveryRequired, f.id, &recoveryRequirement{2, recoveryCapability})
			case "null-coverage":
				expected.Record.Coverage = nil
				_ = f.store.Put(bucketTaskRecovery, f.id, &expected.Record)
			case "nil-ranges":
				expected.Record.Coverage.DurableRanges = nil
				_ = f.store.Put(bucketTaskRecovery, f.id, &expected.Record)
			case "unknown":
				task.Recovery.Version = 99
				_ = f.store.Put(bucketTask, f.id, task)
			case "wrong-id":
				expected.Record.TaskID = "another-task"
				_ = f.store.Put(bucketTaskRecovery, f.id, &expected.Record)
			case "wrong-source":
				task.Meta.Req.URL += "&changed=1"
				_ = f.store.Put(bucketTask, f.id, task)
			case "malformed-record":
				_ = f.store.Put(bucketTaskRecovery, f.id, map[string]any{"version": "bad"})
			case "malformed-ref":
				var raw map[string]any
				if err := decodeStoredJSON(mustD4K12JSON(t, task), &raw); err != nil {
					t.Fatal(err)
				}
				raw["recovery"] = map[string]any{"version": "bad"}
				_ = f.store.Put(bucketTask, f.id, raw)
			case "malformed-required":
				_ = f.store.Put(bucketRecoveryRequired, f.id, map[string]any{"version": false})
			}
			before := f.snapshot(t)
			err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
				task, record, err := readRecoveryCoverage(reader, f.id)
				if task != nil || !reflect.DeepEqual(record, recoveryRecord{}) || !errors.Is(err, transfer.ErrRecoveryHeld) {
					t.Fatalf("invalid variant not held/zero: %v", err)
				}
				return nil
			})
			if err != nil || before != f.snapshot(t) {
				t.Fatal("read repaired state", err)
			}
		})
	}
}

func TestD4K12SnapshotAndSettlement(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4k12New(t, backend, true)
			f.install(t)
			expected, next := f.update(t, false)
			result, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next)
			if err != nil || fresh || result.Episode != expected.Record.Episode || result.TotalUsed != 2 || result.Coverage.SettlementSequence != 0 || len(result.Coverage.SettledRanges) != 0 {
				t.Fatal("snapshot-only consumed baseline or reset balance", err)
			}
			if expected.Record.Coverage.CheckpointHash != "" || next.Plan.Snapshot.SettlementSequence != 0 {
				t.Fatal("save mutated inputs")
			}
			expected, next = f.update(t, true)
			result, fresh, err = saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next)
			if err != nil || !fresh || result.Episode != next.NewEpisode || result.Generation != expected.Token.Generation || result.InitialAttempt.State != "issued" || result.TotalUsed != 0 || result.NextAction != 0 || len(result.Actions) != 0 || result.Coverage.SettlementSequence != 1 || len(result.Coverage.SettledRanges) != 1 {
				t.Fatal("earned settlement lost state", err)
			}
			result.Coverage.DurableRanges[0].End = 1
			next.Plan.Settlement.SettledRanges[0].End = 1
			_, current := f.capture(t)
			if current.Record.Coverage.DurableRanges[0].End != transfer.HTTPCoverageThreshold || current.Record.Coverage.SettledRanges[0].End != transfer.HTTPCoverageThreshold {
				t.Fatal("returned/caller ranges alias DB")
			}
			expected, next = f.update(t, true)
			next.NewEpisode = "third-episode"
			before := f.snapshot(t)
			if r, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err == nil || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("same coverage awarded again", err)
			}
		})
	}
}

func TestD4K12CheckpointCASIndependentOfCoverageSeal(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, mode := range []string{"changed", "deleted", "appeared", "expected-hash", "expected-absence-hash", "record", "token", "dataset"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				f := d4k12New(t, backend, mode != "appeared")
				f.install(t)
				expected, next := f.update(t, true)
				switch mode {
				case "changed", "appeared":
					_ = f.store.Put(bucketSave, f.id, map[string]any{"ordinary-periodic": 2})
				case "deleted":
					_ = f.store.Delete(bucketSave, f.id)
				case "expected-hash":
					expected.CheckpointHash = strings.Repeat("b", 64)
				case "expected-absence-hash":
					expected.CheckpointPresent = false
				case "record":
					f.mutate(t, func(tx StorageTransaction, task *Task, record *recoveryRecord) error {
						if err := appendRecoveryAction(record, record.Generation, []string{"other"}); err != nil {
							return err
						}
						return tx.Put(bucketTaskRecovery, task.ID, record)
					})
				case "token":
					expected.Token.Generation++
				case "dataset":
					f.dataset = "another-valid-dataset"
				}
				// A wrong dataset cannot read even the baseline through that domain.
				dataset := f.dataset
				if mode == "dataset" {
					identity, _ := (&Downloader{storage: f.store}).DatasetIdentity()
					f.dataset = identity.DatasetID
				}
				before := f.snapshot(t)
				if r, fresh, err := saveRecoveryCoverageCheckpoint(f.store, dataset, expected, next); err == nil || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
					t.Fatal("CAS conflict produced mutation/result", err)
				}
			})
		}
	}
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend+"/current-store-not-coverage-seal", func(t *testing.T) {
			f := d4k12New(t, backend, true)
			f.install(t)
			expected, next := f.update(t, false)
			if _, _, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err != nil {
				t.Fatal(err)
			}
			_ = f.store.Put(bucketSave, f.id, map[string]any{"periodic-store": 3})
			expected, next = f.update(t, false)
			if expected.CheckpointHash == expected.Record.Coverage.CheckpointHash {
				t.Fatal("fixture did not advance ordinary checkpoint")
			}
			if _, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err != nil || fresh {
				t.Fatal("valid independent checkpoint rejected", err)
			}
		})
	}
}

func TestD4K12UntrustedPlanAndTaskNeverRewrite(t *testing.T) {
	for _, mode := range []string{"net-new", "threshold", "settlement", "identity", "baseline", "null-checkpoint", "bad-json", "path", "req", "handoff", "done", "same-episode", "v1-save"} {
		t.Run(mode, func(t *testing.T) {
			f := d4k12New(t, "mem", true)
			f.install(t)
			expected, next := f.update(t, true)
			switch mode {
			case "net-new":
				next.Plan.NetNewBytes++
			case "threshold":
				next.Plan.ThresholdReached = false
			case "settlement":
				next.Plan.Settlement.SettlementSequence++
			case "identity":
				next.Plan.Snapshot.IdentityHash = strings.Repeat("b", 64)
			case "baseline":
				next.Plan.Snapshot.SettledRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1}}
			case "null-checkpoint":
				next.Checkpoint = json.RawMessage(" null ")
			case "bad-json":
				next.Checkpoint = json.RawMessage("{")
			case "path":
				next.Snapshot.Meta.Opts.Path += "/different"
			case "req":
				next.Snapshot.Meta.Req.URL += "&different=1"
			case "handoff":
				next.Snapshot.Handoff = nil
			case "done":
				next.Snapshot.Status = base.DownloadStatusDone
			case "same-episode":
				next.NewEpisode = expected.Record.Episode
			case "v1-save":
				f.mutate(t, func(tx StorageTransaction, task *Task, record *recoveryRecord) error {
					task.Recovery.Version, record.Version, record.Coverage = 1, 1, nil
					if err := tx.Put(bucketTask, task.ID, task); err != nil {
						return err
					}
					if err := tx.Put(bucketTaskRecovery, task.ID, record); err != nil {
						return err
					}
					return tx.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{1, recoveryCapability})
				})
				_, expected = f.capture(t)
			}
			before := f.snapshot(t)
			if r, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err == nil || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("untrusted input authorized mutation", err)
			}
		})
	}
}

var d4k12BackendFailure = errors.New("private-backend-detail-do-not-return")

type d4k12WriteFailure struct {
	AtomicStorage
	failPut int
	unknown bool
}

type d4k12TransactionFailure struct {
	StorageTransaction
	remaining int
}

func (tx *d4k12TransactionFailure) Put(bucket, key string, value any) error {
	tx.remaining--
	if tx.remaining == 0 {
		return d4k12BackendFailure
	}
	return tx.StorageTransaction.Put(bucket, key, value)
}

func (s d4k12WriteFailure) UpdateAtomic(fn func(StorageTransaction) error) error {
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		if s.failPut != 0 {
			tx = &d4k12TransactionFailure{tx, s.failPut}
		}
		return fn(tx)
	})
	if err == nil && s.unknown {
		return d4k12BackendFailure
	}
	return err
}

func TestD4K12InstallAndSaveRollbackUnknownACK(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, operation := range []string{"install", "save"} {
			for _, failure := range []int{1, 2, 3, 0} {
				t.Run(fmt.Sprintf("%s/%s/%d", backend, operation, failure), func(t *testing.T) {
					f := d4k12New(t, backend, true)
					if operation == "save" {
						f.install(t)
					}
					before := f.snapshot(t)
					_, expected := f.capture(t)
					store := d4k12WriteFailure{AtomicStorage: f.store, failPut: failure, unknown: failure == 0}
					var record recoveryRecord
					var err error
					fresh := false
					if operation == "install" {
						record, err = installRecoveryCoverage(store, f.dataset, expected, f.initial)
					} else {
						var next recoveryCoverageUpdate
						expected, next = f.update(t, true)
						record, fresh, err = saveRecoveryCoverageCheckpoint(store, f.dataset, expected, next)
					}
					if !errors.Is(err, transfer.ErrRecoveryUnavailable) || err.Error() == d4k12BackendFailure.Error() || fresh || !reflect.DeepEqual(record, recoveryRecord{}) {
						t.Fatal("failed/unknown ACK leaked closure result or backend error", err)
					}
					if failure != 0 && before != f.snapshot(t) {
						t.Fatal("failed transaction partly persisted")
					}
					if failure == 0 {
						if backend == "bolt" {
							f.reopen(t)
						}
						_, current := f.capture(t)
						if current.Record.Version != 3 || operation == "save" && current.Record.Coverage.SettlementSequence != 1 {
							t.Fatal("fixture did not commit before losing ACK")
						}
						// An authoritative read reports history; it cannot recreate the
						// original success result or a runtime one-shot authorization.
						if operation == "install" {
							if _, err := installRecoveryCoverage(f.store, f.dataset, expected, f.initial); err == nil {
								t.Fatal("unknown installation replayed")
							}
						} else {
							_, next := f.update(t, true)
							next.NewEpisode = "another-episode"
							if _, won, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err == nil || won {
								t.Fatal("unknown settlement replayed", err)
							}
						}
					}
				})
			}
		}
	}
}

func (f *d4k12Fixture) reopen(t *testing.T) {
	t.Helper()
	bolt := f.store.(*BoltStorage)
	if err := bolt.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = NewBoltStorage(filepath.Dir(bolt.path))
	t.Cleanup(func() { _ = f.store.Close() })
}

func TestD4K12AssociationsRemainRequired(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4k12New(t, backend, true)
			task, _ := f.capture(t)
			r := &batchOperationRecord{Version: 1, Namespace: batchOperationNamespace, OperationID: "d4-scoped",
				RequestHash: strings.Repeat("a", 64), Members: []operationMember{{Index: 0, TaskID: f.id, State: "created", Protocol: "http", Options: task.Meta.Opts}}}
			ref := operationRef(r, 0)
			task.CreationOperation = &ref
			if err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
				if err := tx.Put(bucketTaskOperations, operationKey(r.OperationID), r); err != nil {
					return err
				}
				if err := tx.Put(bucketOperationTasks, f.id, &operationTaskLink{f.id, ref}); err != nil {
					return err
				}
				return tx.Put(bucketTask, f.id, task)
			}); err != nil {
				t.Fatal(err)
			}
			f.install(t)
			expected, next := f.update(t, false)
			if _, _, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err != nil {
				t.Fatal("valid S2 association blocked", err)
			}
			if backend == "bolt" {
				f.reopen(t)
			}
			task, expected = f.capture(t)
			if task.CreationOperation == nil || *task.CreationOperation != ref || task.Handoff.Phase != "committed" {
				t.Fatal("install/save/reopen lost association")
			}
			expected, next = f.update(t, true)
			if err := f.store.Delete(bucketOperationTasks, f.id); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot(t)
			if _, won, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); !errors.Is(err, transfer.ErrRecoveryHeld) || won || before != f.snapshot(t) {
				t.Fatal("missing reverse association bypassed guard", err)
			}
		})
	}
	for _, mode := range []string{"handoff-pending", "completion-presence"} {
		t.Run(mode, func(t *testing.T) {
			f := d4k12New(t, "mem", true)
			f.install(t)
			f.mutate(t, func(tx StorageTransaction, task *Task, record *recoveryRecord) error {
				if mode == "handoff-pending" {
					task.Handoff.Phase = "prepared"
				} else {
					task.Completion = &TaskCompletionRef{Version: 1, CompletionID: "required"}
				}
				return tx.Put(bucketTask, f.id, task)
			})
			expected, next := f.update(t, true)
			before := f.snapshot(t)
			if _, won, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err == nil || won || before != f.snapshot(t) {
				t.Fatal("other required guard bypassed", err)
			}
		})
	}
}

func TestD4K12ConcurrentSettlementOneACKWinner(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4k12New(t, backend, true)
			f.install(t)
			expected, next := f.update(t, true)
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan bool, 8)
			errorsOut := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					owned := next
					owned.NewEpisode = fmt.Sprintf("winner-%d", i)
					_, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, owned)
					results <- fresh
					errorsOut <- err
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			close(errorsOut)
			winners, stale := 0, 0
			for fresh := range results {
				if fresh {
					winners++
				}
			}
			for err := range errorsOut {
				if errors.Is(err, transfer.ErrRecoveryStale) {
					stale++
				} else if err != nil {
					t.Fatal(err)
				}
			}
			_, current := f.capture(t)
			if winners != 1 || stale != 7 || current.Record.Coverage.SettlementSequence != 1 || current.Record.TotalUsed != 0 {
				t.Fatal("concurrent seal produced multiple winners", winners, stale)
			}
		})
	}
}

func TestD4K12InstallRejectsNonInitialAndOldStates(t *testing.T) {
	for _, mode := range []string{"durable", "settled", "seq", "checkpoint", "unprovable", "nil", "ungranted", "clean-paused", "v2"} {
		t.Run(mode, func(t *testing.T) {
			f := d4k12New(t, "mem", true)
			switch mode {
			case "durable":
				f.initial.DurableRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1}}
			case "settled":
				f.initial.SettledRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1}}
			case "seq":
				f.initial.SettlementSequence = 1
			case "checkpoint":
				f.initial.CheckpointHash = strings.Repeat("c", 64)
			case "unprovable":
				f.initial.Status = "unprovable"
			case "nil":
				f.initial.SettledRanges = nil
			case "ungranted", "clean-paused", "v2":
				f.mutate(t, func(tx StorageTransaction, task *Task, record *recoveryRecord) error {
					if mode == "ungranted" {
						record.Generation, record.InitialAttempt = 0, recoveryInitialAttempt{State: "ungranted"}
						record.TotalUsed, record.TransportUsed, record.NextAction = 0, 0, 0
						record.Actions = []recoveryAction{}
					} else if mode == "clean-paused" {
						record.InitialAttempt.State, record.InitialAttempt.CheckpointHash = "clean-paused", strings.Repeat("c", 64)
					} else {
						task.Recovery.Version, record.Version = 2, 2
						record.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "prior-action", FromEpisode: "prior", ToEpisode: record.Episode, SourceRevision: 2}
					}
					if err := tx.Put(bucketTask, task.ID, task); err != nil {
						return err
					}
					if err := tx.Put(bucketTaskRecovery, task.ID, record); err != nil {
						return err
					}
					return tx.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{record.Version, recoveryCapability})
				})
			}
			_, expected := f.capture(t)
			before := f.snapshot(t)
			if r, err := installRecoveryCoverage(f.store, f.dataset, expected, f.initial); err == nil || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("non-initial/historical state installed", err)
			}
		})
	}
}

func TestD4K12HistoricalLastPreservedAndCurrentCoverageCanShrink(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4k12New(t, backend, true)
			f.install(t)
			f.mutate(t, func(tx StorageTransaction, task *Task, record *recoveryRecord) error {
				record.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "historical-action", FromEpisode: "from-history", ToEpisode: "to-history", SourceRevision: 1}
				return tx.Put(bucketTaskRecovery, task.ID, record)
			})
			expected, next := f.update(t, true)
			if _, won, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err != nil || !won {
				t.Fatal(err)
			}
			expected, next = f.update(t, false)
			next.Checkpoint = json.RawMessage(`{"truncated":true}`)
			hash, _ := checkpointDigest(next.Checkpoint)
			next.Plan, _ = transfer.PlanHTTPCoverageCheckpoint(*expected.Record.Coverage, nil, hash, recoveryCoverageRangeLimit)
			r, won, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next)
			if err != nil || won || len(r.Coverage.DurableRanges) != 0 || len(r.Coverage.SettledRanges) != 1 || r.Coverage.SettlementSequence != 1 || !reflect.DeepEqual(r.LastExplicitRetry, expected.Record.LastExplicitRetry) {
				t.Fatal("truncate cleared consumed baseline/history", err)
			}
			r.LastExplicitRetry.ActionID = "caller-mutated"
			expected.Record.LastExplicitRetry.ToEpisode = "caller-mutated"
			next.Checkpoint[0] = '['
			_, current := f.capture(t)
			if current.Record.LastExplicitRetry.ActionID != "historical-action" || current.Record.LastExplicitRetry.ToEpisode != "to-history" || current.CheckpointHash != r.Coverage.CheckpointHash {
				t.Fatal("Last/raw result aliases escaped into persistence")
			}
		})
	}
}
