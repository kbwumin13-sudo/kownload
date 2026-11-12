package download

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// These are state transactions, not proof of the caller's process-only new-M
// installation slot, IO, joins/Sync/Complete, or Until admission.
func coverageCompatNew(t *testing.T, backend string, covered bool) *d4k12Fixture {
	t.Helper()
	version := 1
	if covered {
		version = 3
	}
	f := d4s12New(t, backend, version, false, true)
	d4s12Upgrade(t, f)
	d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) {
		if r.Coverage != nil {
			r.Coverage.Status = "tracking"
		}
		r.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "historical", FromEpisode: "from", ToEpisode: "past", SourceRevision: 1}
	})
	d4s12Reserve(t, f, recoveryRefreshAction)
	d4s12Reserve(t, f, recoveryDeferralAction)
	return f
}

func coverageCompatUpdate(t *testing.T, f *d4k12Fixture, earned bool) (recoveryCoverageExpected, recoveryCoverageUpdate) {
	t.Helper()
	task, expected := f.capture(t)
	checkpoint := json.RawMessage(`{"offset":2097152,"exact":9007199254740993}`)
	hash, err := checkpointDigest(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := transfer.PlanHTTPCoverageCheckpoint(*expected.Record.Coverage,
		[]transfer.HTTPCoverageRange{{Begin: 0, End: expected.Record.Coverage.Size}}, hash, recoveryCoverageRangeLimit)
	if err != nil {
		t.Fatal(err)
	}
	task.Progress.Downloaded, task.UpdatedAt = expected.Record.Coverage.Size, task.UpdatedAt.Add(time.Second)
	next := recoveryCoverageUpdate{Snapshot: task, Checkpoint: checkpoint, Plan: plan}
	if earned {
		next.NewEpisode = "compat-earned"
	}
	return expected, next
}

type coverageCompatNoTaskWrite struct{ AtomicStorage }
type coverageCompatTransaction struct{ StorageTransaction }

func (s coverageCompatNoTaskWrite) UpdateAtomic(fn func(StorageTransaction) error) error {
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(coverageCompatTransaction{tx}) })
}
func (tx coverageCompatTransaction) Put(bucket, key string, value any) error {
	if bucket == bucketTask {
		return errors.New("shared installation must preserve the existing Task")
	}
	return tx.StorageTransaction.Put(bucket, key, value)
}

func TestCoverageSharedCompatInstall(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := coverageCompatNew(t, backend, false)
			beforeTask, expected := f.capture(t)
			_, sharedBefore := d4s12Capture(t, f)
			r, err := installRecoveryCoverage(coverageCompatNoTaskWrite{f.store}, f.dataset, expected, f.initial)
			if err != nil {
				t.Fatal(err)
			}
			want := expected.Record
			want.Coverage = &f.initial
			afterTask, after := f.capture(t)
			_, sharedAfter := d4s12Capture(t, f)
			if !reflect.DeepEqual(r, want) || !reflect.DeepEqual(after.Record, want) ||
				string(mustD4K12JSON(t, beforeTask)) != string(mustD4K12JSON(t, afterTask)) ||
				after.CheckpointHash != expected.CheckpointHash || after.CheckpointPresent != expected.CheckpointPresent ||
				sharedBefore.Required.Capability != recoverySharedPlainCapability || sharedAfter.Required != (recoveryRequirement{4, recoverySharedCoverageCapability}) {
				t.Fatal("v4 install changed version/metadata/charges/history/Until/checkpoint")
			}
			if afterTask.Meta.Opts.Extra.(map[string]any)["exact"] != json.Number("9007199254740993") {
				t.Fatal("install rounded original Meta")
			}
			before := f.snapshot(t)
			if r, err := installRecoveryCoverage(f.store, f.dataset, after, f.initial); err == nil || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("existing v4 coverage installed an empty baseline", err)
			}
		})
	}
	for _, mode := range []string{"bad-initial", "g0", "done"} {
		t.Run(mode, func(t *testing.T) {
			f := d4s12New(t, "mem", 1, mode == "g0", true)
			d4s12Upgrade(t, f)
			if mode == "done" {
				d4s12Change(t, f, func(task *Task, _ *recoveryRecord, _ *recoveryRequirement) { task.Status = "done" })
			}
			_, expected := f.capture(t)
			initial := f.initial
			if mode == "bad-initial" {
				initial.DurableRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1}}
			}
			before := f.snapshot(t)
			if r, err := installRecoveryCoverage(f.store, f.dataset, expected, initial); err == nil || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("invalid shared installation changed state", err)
			}
		})
	}
}

func TestCoverageSharedCompatSave(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := coverageCompatNew(t, backend, true)
			_, sharedBefore := d4s12Capture(t, f)
			expected, next := coverageCompatUpdate(t, f, false)
			r, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next)
			want := expected.Record
			want.Coverage = &next.Plan.Snapshot
			if err != nil || fresh || !reflect.DeepEqual(r, want) {
				t.Fatal("snapshot consumed B/seq/charges/Until/history", err)
			}
			expected, next = coverageCompatUpdate(t, f, true)
			r, fresh, err = saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next)
			want = expected.Record
			want.Episode, want.Coverage = next.NewEpisode, next.Plan.Settlement
			want.TotalUsed, want.TransportUsed, want.RefreshUsed, want.DeferralUsed, want.NextAction = 0, 0, 0, 0, 0
			want.Actions = []recoveryAction{}
			want.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: expected.Record.Generation}
			if err != nil || !fresh || !reflect.DeepEqual(r, want) || r.Actions == nil || r.Version != 4 || r.NotBefore != "2027-01-01T00:00:00Z" {
				t.Fatal("earned changed v4 variant/Until/history or lost reset", err)
			}
			task, sharedAfter := d4s12Capture(t, f)
			if sharedAfter.Required != sharedBefore.Required || task.Recovery.Version != 4 || !reflect.DeepEqual(sharedAfter.Record, r) {
				t.Fatal("save downgraded declaration/cap")
			}
			if backend == "bolt" {
				f.reopen(t)
				_, reopened := d4s12Capture(t, f)
				if !reflect.DeepEqual(reopened.Record, r) || reopened.Required != sharedBefore.Required {
					t.Fatal("reopen lost v4 coverage state")
				}
			}
		})
	}
}

func TestCoverageSharedCompatRejectWrongVariant(t *testing.T) {
	for _, mode := range []string{"plain", "wrong-cap", "missing-coverage", "mixed", "unprovable"} {
		t.Run(mode, func(t *testing.T) {
			f := coverageCompatNew(t, "mem", true)
			expected, next := coverageCompatUpdate(t, f, false)
			d4s12Change(t, f, func(_ *Task, r *recoveryRecord, q *recoveryRequirement) {
				switch mode {
				case "plain":
					r.Coverage, q.Capability = nil, recoverySharedPlainCapability
				case "wrong-cap":
					q.Capability = recoverySharedPlainCapability
				case "missing-coverage":
					r.Coverage = nil
				case "mixed":
					q.Version = 3
				case "unprovable":
					r.Coverage.Status = "unprovable"
				}
			})
			if mode == "plain" || mode == "unprovable" {
				_, expected = f.capture(t) // Both are legal declarations, not a new baseline/Plan grant.
			}
			before := f.snapshot(t)
			err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
				task, r, err := readRecoveryCoverage(reader, f.id)
				if mode == "unprovable" {
					if err != nil || task == nil || r.Coverage.Status != "unprovable" {
						t.Fatal("lost read-only history", err)
					}
				} else if !errors.Is(err, transfer.ErrRecoveryHeld) || task != nil || !reflect.DeepEqual(r, recoveryRecord{}) {
					t.Fatal("bad variant became coverage baseline", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if r, fresh, err := saveRecoveryCoverageCheckpoint(f.store, f.dataset, expected, next); err == nil || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("invalid/unprovable save changed state", err)
			}
		})
	}
}

func TestCoverageSharedCompatCASAndUnknownACK(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, op := range []string{"install", "snapshot", "earned"} {
			for _, failure := range []string{"checkpoint", "rollback", "unknown"} {
				t.Run(backend+"/"+op+"/"+failure, func(t *testing.T) {
					f := coverageCompatNew(t, backend, op != "install")
					_, expected := f.capture(t)
					var next recoveryCoverageUpdate
					if op != "install" {
						expected, next = coverageCompatUpdate(t, f, op == "earned")
					}
					if failure == "checkpoint" {
						if err := f.store.Put(bucketSave, f.id, map[string]any{"periodic": 9}); err != nil {
							t.Fatal(err)
						}
					}
					before := f.snapshot(t)
					var store AtomicStorage = f.store
					if failure == "rollback" {
						store = d4k12WriteFailure{AtomicStorage: f.store, failPut: 2}
					}
					if failure == "unknown" {
						store = d4k12WriteFailure{AtomicStorage: f.store, unknown: true}
					}
					var r recoveryRecord
					var err error
					fresh := false
					if op == "install" {
						r, err = installRecoveryCoverage(store, f.dataset, expected, f.initial)
					} else {
						r, fresh, err = saveRecoveryCoverageCheckpoint(store, f.dataset, expected, next)
					}
					if err == nil || fresh || !reflect.DeepEqual(r, recoveryRecord{}) {
						t.Fatal("failed/unknown result leaked", err)
					}
					if failure != "unknown" && before != f.snapshot(t) {
						t.Fatal("CAS/rollback partly persisted")
					}
					if failure == "unknown" {
						if !errors.Is(err, transfer.ErrRecoveryUnavailable) {
							t.Fatal(err)
						}
						if backend == "bolt" {
							f.reopen(t)
						}
						_, actual := d4s12Capture(t, f)
						if actual.Record.Version != 4 || actual.Required.Capability != recoverySharedCoverageCapability || before == f.snapshot(t) {
							t.Fatal("fixture did not commit before losing ACK")
						}
						if op == "earned" && actual.Record.Episode != next.NewEpisode {
							t.Fatal("missing committed settlement")
						}
						if op == "snapshot" && actual.Record.Coverage.SettlementSequence != expected.Record.Coverage.SettlementSequence {
							t.Fatal("snapshot spent B/seq")
						}
					}
				})
			}
		}
	}
}
