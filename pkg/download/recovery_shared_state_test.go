package download

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Reuse only the existing private Mem/Bolt value fixture, never its old test
// matrix or a live Downloader/owner/fetcher. These cases exercise actual writes
// by the new kernel, not hand-labelled "actual runtime" writer evidence.
func d4s12New(t *testing.T, backend string, version int, g0, checkpoint bool) *d4k12Fixture {
	t.Helper()
	f := d4k12New(t, backend, checkpoint)
	f.mutate(t, func(tx StorageTransaction, task *Task, r *recoveryRecord) error {
		r.Version, task.Recovery.Version = version, version
		if version == 2 {
			r.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "original-action", FromEpisode: "before", ToEpisode: r.Episode, SourceRevision: 1}
		}
		if version == 3 {
			sharedTypeCoverage(t, r)
			r.Coverage.Status = "unprovable" // The old consumed baseline must survive, not become a grant.
			r.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "history", FromEpisode: "before", ToEpisode: "historical", SourceRevision: 1}
		}
		if g0 {
			r.Generation, r.TotalUsed, r.TransportUsed, r.NextAction = 0, 0, 0, 0
			r.InitialAttempt = recoveryInitialAttempt{State: "uncertain"}
			r.Actions = []recoveryAction{}
		}
		if err := tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, r); err != nil {
			return err
		}
		return tx.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{version, recoveryCapability})
	})
	return f
}

func d4s12Capture(t *testing.T, f *d4k12Fixture) (*Task, recoverySharedExpected) {
	t.Helper()
	var task *Task
	var expected recoverySharedExpected
	err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
		var err error
		task, expected.Record, expected.Required, err = readSharedRecovery(reader, f.id)
		if err != nil {
			return err
		}
		r := expected.Record
		expected.Token = transfer.RecoveryToken{TaskID: f.id, Episode: r.Episode, Generation: r.Generation, SourceRevision: r.SourceRevision}
		var raw json.RawMessage
		expected.CheckpointPresent, err = reader.Get(bucketSave, f.id, &raw)
		if err == nil && expected.CheckpointPresent {
			expected.CheckpointHash, err = checkpointDigest(raw)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, expected
}

func d4s12Change(t *testing.T, f *d4k12Fixture, change func(*Task, *recoveryRecord, *recoveryRequirement)) {
	t.Helper()
	err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, required, err := readSharedRecovery(tx, f.id)
		if err != nil {
			return err
		}
		change(task, &record, &required)
		if err := tx.Put(bucketTask, f.id, task); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, f.id, &record); err != nil {
			return err
		}
		return tx.Put(bucketRecoveryRequired, f.id, &required)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func d4s12Upgrade(t *testing.T, f *d4k12Fixture) recoveryRecord {
	t.Helper()
	_, expected := d4s12Capture(t, f)
	r, err := upgradeSharedRecovery(f.store, f.dataset, expected)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func d4s12Command(r recoveryRecord, kind recoveryActionKind) recoverySharedAction {
	a := recoverySharedAction{Kind: kind, Seen: r.NextAction, Work: "a", Members: []string{"b", "a"}}
	if kind == recoveryDeferralAction {
		a.Until = "2027-01-01T00:00:00Z"
	}
	return a
}

func d4s12Reserve(t *testing.T, f *d4k12Fixture, kind recoveryActionKind) (recoveryRecord, uint64) {
	t.Helper()
	_, expected := d4s12Capture(t, f)
	r, id, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, expected, d4s12Command(expected.Record, kind))
	if err != nil || !fresh || id != r.NextAction {
		t.Fatal("new reservation failed", err)
	}
	return r, id
}

func TestD4S12UpgradeVariantsG0AndPreservation(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, mode := range []string{"v1", "v2", "v3-unprovable", "g0-present", "g0-absent"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				version := 1
				if mode == "v2" {
					version = 2
				}
				if mode == "v3-unprovable" {
					version = 3
				}
				f := d4s12New(t, backend, version, strings.HasPrefix(mode, "g0"), mode != "g0-absent")
				task, expected := d4s12Capture(t, f)
				original := f.snapshot(t)
				result := d4s12Upgrade(t, f)
				afterTask, after := d4s12Capture(t, f)
				task.Recovery.Version = 4
				want := expected.Record
				want.Version = 4
				if !reflect.DeepEqual(result, want) || !reflect.DeepEqual(after.Record, want) || string(mustD4K12JSON(t, task)) != string(mustD4K12JSON(t, afterTask)) || expected.CheckpointPresent != after.CheckpointPresent || expected.CheckpointHash != after.CheckpointHash {
					t.Fatal("upgrade changed original fields/checkpoint")
				}
				cap := recoverySharedPlainCapability
				if version == 3 {
					cap = recoverySharedCoverageCapability
				}
				if after.Required != (recoveryRequirement{4, cap}) || original == f.snapshot(t) {
					t.Fatal("declarations not upgraded")
				}
				// Idempotence must not issue Put, even to rewrite equal bytes.
				before := f.snapshot(t)
				if _, err := upgradeSharedRecovery(d4k12WriteFailure{AtomicStorage: f.store, failPut: 1}, f.dataset, after); err != nil || before != f.snapshot(t) {
					t.Fatal("upgrade replay wrote state", err)
				}
				if strings.HasPrefix(mode, "g0") {
					if result.Generation != 0 || result.InitialAttempt.State != "uncertain" || after.Token.Generation != 0 {
						t.Fatal("G0 was raised or issued")
					}
					if r, id, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, after, d4s12Command(after.Record, recoveryTransportAction)); err == nil || id != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
						t.Fatal("G0 manufactured execution/debit", err)
					}
				}
				if version == 3 {
					result.Coverage.SettledRanges[0].End = 1
					result.LastExplicitRetry.ActionID = "mutated"
				}
				result.Actions = append(result.Actions, recoveryAction{})
				_, again := d4s12Capture(t, f)
				if !reflect.DeepEqual(again.Record, after.Record) {
					t.Fatal("returned upgrade state aliases DB")
				}
				if backend == "bolt" {
					f.reopen(t)
					_, reopened := d4s12Capture(t, f)
					if !reflect.DeepEqual(reopened.Record, after.Record) {
						t.Fatal("Bolt reopen lost declarations")
					}
				}
			})
		}
	}
}

func TestD4S12ReadMissingMixedAndOwned(t *testing.T) {
	for _, mode := range []string{"missing-task", "missing-record", "missing-required", "missing-ref", "mixed", "cap", "malformed", "source", "all-absent"} {
		t.Run(mode, func(t *testing.T) {
			f := d4s12New(t, "mem", 1, false, true)
			d4s12Upgrade(t, f)
			switch mode {
			case "missing-task":
				_ = f.store.Delete(bucketTask, f.id)
			case "missing-record":
				_ = f.store.Delete(bucketTaskRecovery, f.id)
			case "missing-required":
				_ = f.store.Delete(bucketRecoveryRequired, f.id)
			case "all-absent":
				for _, b := range []string{bucketTaskRecovery, bucketRecoveryRequired} {
					_ = f.store.Delete(b, f.id)
				}
				var task Task
				_, _ = f.store.Get(bucketTask, f.id, &task)
				task.Recovery = nil
				_ = f.store.Put(bucketTask, f.id, &task)
			default:
				d4s12Change(t, f, func(task *Task, r *recoveryRecord, q *recoveryRequirement) {
					switch mode {
					case "missing-ref":
						task.Recovery = nil
					case "mixed":
						q.Version = 3
					case "cap":
						q.Capability = recoverySharedCoverageCapability
					case "malformed":
						r.Generation = 0
					case "source":
						task.Meta.Req.URL += "&changed=1"
					}
				})
			}
			before := f.snapshot(t)
			if err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
				task, r, q, err := readSharedRecovery(reader, f.id)
				if task != nil || !reflect.DeepEqual(r, recoveryRecord{}) || q != (recoveryRequirement{}) || !errors.Is(err, transfer.ErrRecoveryHeld) {
					t.Fatalf("bad triplet not held/zero %v", err)
				}
				return nil
			}); err != nil || before != f.snapshot(t) {
				t.Fatal("read repaired/enrolled", err)
			}
		})
	}
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend+"/owned", func(t *testing.T) {
			f := d4s12New(t, backend, 3, false, true)
			d4s12Upgrade(t, f)
			before := f.snapshot(t)
			task, expected := d4s12Capture(t, f)
			if task.Meta.Opts.Extra.(map[string]any)["exact"] != json.Number("9007199254740993") {
				t.Fatal("Task precision lost")
			}
			task.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] = "changed"
			expected.Record.Coverage.SettledRanges[0].End = 1
			expected.Record.Actions[0].Members[0] = "changed"
			expected.Record.LastExplicitRetry.ToEpisode = "changed"
			if before != f.snapshot(t) {
				t.Fatal("owned read aliases DB")
			}
		})
	}
}

func TestD4S12ThreeKindsReplayAndStrongerUntil(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4s12New(t, backend, 1, false, true)
			d4s12Upgrade(t, f)
			for _, kind := range []recoveryActionKind{recoveryTransportAction, recoveryRefreshAction, recoveryDeferralAction} {
				_, expected := d4s12Capture(t, f)
				input := d4s12Command(expected.Record, kind)
				inputCopy, _ := cloneOperationJSON(input)
				r, id, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, expected, input)
				if err != nil || !fresh || id != expected.Record.NextAction+1 || !reflect.DeepEqual(input, inputCopy) {
					t.Fatal("debit failed or input changed", err)
				}
				if r.Actions[id-1].Members[0] != "a" || r.Actions[id-1].Kind != string(kind) {
					t.Fatal("workset/kind not frozen")
				}
				_, current := d4s12Capture(t, f)
				before := f.snapshot(t)
				if replay, got, won, err := reserveSharedRecoveryAction(d4k12WriteFailure{AtomicStorage: f.store, failPut: 1}, f.dataset, current, input); err != nil || won || got != id || !reflect.DeepEqual(replay, current.Record) || before != f.snapshot(t) {
					t.Fatal("same Seen replay wrote or regranted", err)
				}
				if kind == recoveryDeferralAction {
					input.Until = "2028-01-01T00:00:00Z"
					if r, id, won, err := reserveSharedRecoveryAction(f.store, f.dataset, current, input); !errors.Is(err, transfer.ErrRecoveryWork) || id != 0 || won || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
						t.Fatal("stronger Until silently swallowed", err)
					}
				}
			}
			_, final := d4s12Capture(t, f)
			if final.Record.TotalUsed != 5 || final.Record.TransportUsed != 3 || final.Record.RefreshUsed != 1 || final.Record.DeferralUsed != 1 {
				t.Fatal("kind/total counters wrong")
			}
		})
	}
}

func TestD4S12ExtendOnlyCurrentWaitAndNoDebit(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4s12New(t, backend, 1, false, true)
			d4s12Upgrade(t, f)
			_, id := d4s12Reserve(t, f, recoveryDeferralAction)
			// The same wait may survive a generation change; it must not survive a
			// source change or next-wave's atomic close. No real wave runs here.
			d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) {
				r.Generation++
				r.InitialAttempt.Generation = r.Generation
			})
			_, expected := d4s12Capture(t, f)
			input := recoverySharedAction{Kind: recoveryDeferralAction, Seen: id - 1, Work: "a", Members: []string{"a", "b"}, Until: "2028-01-01T00:00:00Z", ExtendActionID: id}
			r, got, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, expected, input)
			if err != nil || fresh || got != id || r.NotBefore != input.Until || r.TotalUsed != expected.Record.TotalUsed || !reflect.DeepEqual(r.Actions, expected.Record.Actions) {
				t.Fatal("extend changed debit/action", err)
			}
			_, expected = d4s12Capture(t, f)
			before := f.snapshot(t)
			input.Until = "2027-06-01T00:00:00Z"
			if _, got, fresh, err := reserveSharedRecoveryAction(d4k12WriteFailure{AtomicStorage: f.store, failPut: 1}, f.dataset, expected, input); err != nil || fresh || got != id || before != f.snapshot(t) {
				t.Fatal("weaker extend wrote", err)
			}
			d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) {
				a := &r.Actions[id-1]
				a.State = "finished"
				a.Finished = append([]string(nil), a.Members...)
			})
			_, expected = d4s12Capture(t, f)
			before = f.snapshot(t)
			input.Until = "2029-01-01T00:00:00Z"
			if r, got, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, expected, input); !errors.Is(err, transfer.ErrRecoveryStale) || got != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("closed wait extended", err)
			}
			newAction := d4s12Command(expected.Record, recoveryDeferralAction)
			newAction.Until = input.Until
			if r, got, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, expected, newAction); err != nil || !fresh || got != id+1 || r.DeferralUsed != 2 {
				t.Fatal("new wave escaped a new debit", err)
			}
		})
	}
	for _, mode := range []string{"old-cursor", "partial-finished", "old-source", "wrong-members", "wrong-seen", "wrong-kind"} {
		t.Run(mode, func(t *testing.T) {
			f := d4s12New(t, "mem", 1, false, true)
			d4s12Upgrade(t, f)
			_, id := d4s12Reserve(t, f, recoveryDeferralAction)
			if mode == "old-cursor" {
				d4s12Reserve(t, f, recoveryTransportAction)
			}
			if mode == "old-source" {
				d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) { r.Actions[id-1].SourceRevision = 1 })
			}
			_, expected := d4s12Capture(t, f)
			if mode == "partial-finished" {
				d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) { r.Actions[id-1].Finished = []string{"a"} })
			}
			input := recoverySharedAction{Kind: recoveryDeferralAction, Seen: id - 1, Work: "a", Members: []string{"a", "b"}, Until: "2028-01-01T00:00:00Z", ExtendActionID: id}
			if mode == "wrong-members" {
				input.Members = []string{"a"}
			}
			if mode == "wrong-seen" {
				input.Seen = id
			}
			if mode == "wrong-kind" {
				input.Kind = recoveryTransportAction
			}
			before := f.snapshot(t)
			if r, id, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, expected, input); err == nil || id != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("invalid wait window changed state", err)
			}
		})
	}
}

func TestD4S12FullRecordRequirementAndCheckpointCAS(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, mode := range []string{"record", "required", "token", "checkpoint-changed", "checkpoint-deleted", "checkpoint-appeared", "hash", "absence-hash", "dataset"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				f := d4s12New(t, backend, 1, false, mode != "checkpoint-appeared")
				d4s12Upgrade(t, f)
				_, expected := d4s12Capture(t, f)
				input := d4s12Command(expected.Record, recoveryTransportAction)
				dataset := f.dataset
				switch mode {
				case "record":
					d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) {
						r.InitialAttempt.State = "clean-paused"
						r.InitialAttempt.CheckpointHash = strings.Repeat("b", 64)
					})
				case "required":
					expected.Required.Capability = "other"
				case "token":
					expected.Token.SourceRevision++
				case "checkpoint-changed", "checkpoint-appeared":
					if err := f.store.Put(bucketSave, f.id, map[string]any{"periodic": 3}); err != nil {
						t.Fatal(err)
					}
				case "checkpoint-deleted":
					if err := f.store.Delete(bucketSave, f.id); err != nil {
						t.Fatal(err)
					}
				case "hash":
					expected.CheckpointHash = strings.Repeat("c", 64)
				case "absence-hash":
					expected.CheckpointPresent = false
				case "dataset":
					dataset = "wrong-dataset"
				}
				before := f.snapshot(t)
				if r, id, fresh, err := reserveSharedRecoveryAction(f.store, dataset, expected, input); err == nil || id != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
					t.Fatal("independent CAS bypassed", err)
				}
			})
		}
	}
	// The same CAS protects the G0 upgrade, including a present checkpoint.
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend+"/g0-upgrade", func(t *testing.T) {
			f := d4s12New(t, backend, 1, true, true)
			_, e := d4s12Capture(t, f)
			if err := f.store.Put(bucketSave, f.id, map[string]any{"new": 1}); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot(t)
			if r, err := upgradeSharedRecovery(f.store, f.dataset, e); !errors.Is(err, transfer.ErrRecoveryStale) || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("G0 checkpoint CAS bypassed", err)
			}
		})
	}
}

func TestD4S12ConcurrentReservationOneWinner(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := d4s12New(t, backend, 1, false, true)
			d4s12Upgrade(t, f)
			_, e := d4s12Capture(t, f)
			a := d4s12Command(e.Record, recoveryRefreshAction)
			var wg sync.WaitGroup
			gate := make(chan struct{})
			out := make(chan error, 8)
			wins := make(chan bool, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate
					_, _, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, e, a)
					wins <- fresh
					out <- err
				}()
			}
			close(gate)
			wg.Wait()
			close(out)
			close(wins)
			winners, stale := 0, 0
			for v := range wins {
				if v {
					winners++
				}
			}
			for err := range out {
				if errors.Is(err, transfer.ErrRecoveryStale) {
					stale++
				} else if err != nil {
					t.Fatal(err)
				}
			}
			_, after := d4s12Capture(t, f)
			if winners != 1 || stale != 7 || after.Record.TotalUsed != e.Record.TotalUsed+1 || after.Record.RefreshUsed != 1 {
				t.Fatal("multiple debit winners", winners, stale)
			}
		})
	}
}

func TestD4S12RollbackAndUnknownACKZeroResults(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, op := range []string{"upgrade", "reserve", "extend"} {
			failures := []int{1, 0}
			if op == "upgrade" {
				failures = []int{1, 2, 3, 0}
			}
			for _, failure := range failures {
				t.Run(fmt.Sprintf("%s/%s/%d", backend, op, failure), func(t *testing.T) {
					f := d4s12New(t, backend, 1, false, true)
					var id uint64
					if op != "upgrade" {
						d4s12Upgrade(t, f)
					}
					if op == "extend" {
						_, id = d4s12Reserve(t, f, recoveryDeferralAction)
					}
					_, e := d4s12Capture(t, f)
					a := d4s12Command(e.Record, recoveryRefreshAction)
					if op == "extend" {
						a = recoverySharedAction{Kind: recoveryDeferralAction, Seen: id - 1, Work: "a", Members: []string{"a", "b"}, Until: "2028-01-01T00:00:00Z", ExtendActionID: id}
					}
					before := f.snapshot(t)
					store := d4k12WriteFailure{AtomicStorage: f.store, failPut: failure, unknown: failure == 0}
					var r recoveryRecord
					var got uint64
					var fresh bool
					var err error
					if op == "upgrade" {
						r, err = upgradeSharedRecovery(store, f.dataset, e)
					} else {
						r, got, fresh, err = reserveSharedRecoveryAction(store, f.dataset, e, a)
					}
					if !errors.Is(err, transfer.ErrRecoveryUnavailable) || got != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) {
						t.Fatal("failure/unknown ACK leaked result", err)
					}
					if failure != 0 && before != f.snapshot(t) {
						t.Fatal("partial transaction survived rollback")
					}
					if failure == 0 {
						if backend == "bolt" {
							f.reopen(t)
						}
						_, confirmed := d4s12Capture(t, f)
						if confirmed.Record.Version != 4 || op == "reserve" && confirmed.Record.RefreshUsed != 1 || op == "extend" && confirmed.Record.NotBefore != a.Until {
							t.Fatal("commit-before-error fixture not effective")
						}
						if before == f.snapshot(t) {
							t.Fatal("unknown ACK fixture did not commit")
						}
						if op == "reserve" {
							_, _, won, err := reserveSharedRecoveryAction(d4k12WriteFailure{AtomicStorage: f.store, failPut: 1}, f.dataset, confirmed, a)
							if err != nil || won {
								t.Fatal("reconcile/replay regranted", err)
							}
						}
					}
				})
			}
		}
	}
}

func TestD4S12KindAndTotalLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind recoveryActionKind
		n    int
	}{
		{"transport", recoveryTransportAction, 6}, {"refresh", recoveryRefreshAction, 3}, {"deferral", recoveryDeferralAction, 10}, {"total", recoveryDeferralAction, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := d4s12New(t, "mem", 1, false, true)
			d4s12Upgrade(t, f)
			d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) {
				kinds := make([]recoveryActionKind, tc.n)
				for i := range kinds {
					kinds[i] = tc.kind
				}
				if tc.name == "total" {
					for i := 0; i < 6; i++ {
						kinds[i] = recoveryTransportAction
					}
					for i := 6; i < 9; i++ {
						kinds[i] = recoveryRefreshAction
					}
				}
				sharedTypeActions(r, kinds...)
			})
			_, e := d4s12Capture(t, f)
			before := f.snapshot(t)
			if r, id, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, e, d4s12Command(e.Record, tc.kind)); !errors.Is(err, transfer.ErrRecoveryExhausted) || id != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("quota bypassed", err)
			}
		})
	}
	for _, mode := range []string{"kind", "work", "members", "until", "until-empty", "seen", "nondeferral-until"} {
		t.Run(mode, func(t *testing.T) {
			f := d4s12New(t, "mem", 1, false, true)
			d4s12Upgrade(t, f)
			_, e := d4s12Capture(t, f)
			a := d4s12Command(e.Record, recoveryDeferralAction)
			switch mode {
			case "kind":
				a.Kind = "unknown"
			case "work":
				a.Work = "foreign"
			case "members":
				a.Members = []string{"a", "a"}
			case "until":
				a.Until = "2027-01-01T00:00:00+00:00"
			case "until-empty":
				a.Until = ""
			case "seen":
				a.Seen++
			case "nondeferral-until":
				a.Kind = recoveryRefreshAction
			}
			before := f.snapshot(t)
			if r, id, fresh, err := reserveSharedRecoveryAction(f.store, f.dataset, e, a); err == nil || id != 0 || fresh || !reflect.DeepEqual(r, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("bad command mutated state", err)
			}
		})
	}
}
