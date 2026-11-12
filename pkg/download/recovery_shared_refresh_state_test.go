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

// Component fixtures reuse the existing private Mem/Bolt helpers, not their
// old test matrix, any live owner, real HTTP Store, or execution permission.
func sharedRefreshK12New(t *testing.T, backend, coverage string, present bool) (*d4k12Fixture, recoverySharedExpected, uint64) {
	t.Helper()
	f := d4s12New(t, backend, 1, false, present)
	if present {
		var old map[string]json.RawMessage
		if err := json.Unmarshal(sharedRefreshK12Checkpoint(), &old); err != nil {
			t.Fatal(err)
		}
		old["RedirectURL"] = json.RawMessage(`"https://cdn.fixture.invalid/old"`)
		old["PublicSource"] = json.RawMessage(`{"URL":"https://share.fixture.invalid/old"}`)
		raw, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Put(bucketSave, f.id, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	d4s12Upgrade(t, f)
	d4s12Change(t, f, func(task *Task, record *recoveryRecord, required *recoveryRequirement) {
		record.NotBefore = "2027-01-01T00:00:00Z"
		record.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "history-action", FromEpisode: "before", ToEpisode: "historic-episode", SourceRevision: 1}
		if coverage != "none" {
			sharedTypeCoverage(t, record)
			record.Coverage.Status = coverage
			required.Capability = recoverySharedCoverageCapability
		}
	})
	_, id := d4s12Reserve(t, f, recoveryRefreshAction)
	_, expected := d4s12Capture(t, f)
	return f, expected, id
}

func sharedRefreshK12Checkpoint() json.RawMessage {
	// Synthetic opaque Store-shaped value. It is NOT proof that the HTTP
	// protocol patch, its network authorization, or its writer has run.
	return json.RawMessage(`{"RedirectURL":"https://cdn.fixture.invalid/refreshed","PublicSource":{"URL":"https://share.fixture.invalid/original"},"Connections":[{"offset":9007199254740993,"negative":-9007199254740993,"fraction":0.1234567890123456789}],"Representation":{"validator":"preserved"},"RangeResumeValidator":{"value":"original"}}`)
}

func sharedRefreshK12Read(t *testing.T, f *d4k12Fixture, bucket string) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error { _, err := reader.Get(bucket, f.id, &raw); return err })
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sharedRefreshK12Want(t *testing.T, expected recoverySharedExpected) recoveryRecord {
	t.Helper()
	want, err := cloneOperationJSON(expected.Record)
	if err != nil {
		t.Fatal(err)
	}
	want.SourceRevision++
	last := &want.Actions[len(want.Actions)-1]
	last.State, last.Finished = "finished", append([]string(nil), last.Members...)
	if want.Coverage != nil {
		want.Coverage.Status = "unprovable"
	}
	return want
}

func sharedRefreshK12AssertCheckpoint(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if string(object["Connections"]) != `[{"fraction":0.1234567890123456789,"negative":-9007199254740993,"offset":9007199254740993}]` {
		// Compare owned UseNumber JSON, not object-key order or float64 values.
		var got, want any
		if decodeStoredJSON(object["Connections"], &got) != nil || decodeStoredJSON(json.RawMessage(`[{"offset":9007199254740993,"negative":-9007199254740993,"fraction":0.1234567890123456789}]`), &want) != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("opaque checkpoint number precision/fields lost")
		}
	}
	for _, field := range []string{"RedirectURL", "PublicSource", "Representation", "RangeResumeValidator"} {
		var original map[string]json.RawMessage
		_ = json.Unmarshal(sharedRefreshK12Checkpoint(), &original)
		var got, want any
		if decodeStoredJSON(object[field], &got) != nil || decodeStoredJSON(original[field], &want) != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("opaque Store field changed", field)
		}
	}
}

func TestSharedRefreshK12CommitPreservation(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, coverage := range []string{"none", "tracking", "unprovable"} {
			for _, present := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/present-%v", backend, coverage, present), func(t *testing.T) {
					f, expected, id := sharedRefreshK12New(t, backend, coverage, present)
					taskBefore, requiredBefore := sharedRefreshK12Read(t, f, bucketTask), sharedRefreshK12Read(t, f, bucketRecoveryRequired)
					want := sharedRefreshK12Want(t, expected)
					inputBefore, cloneErr := cloneOperationJSON(expected)
					if cloneErr != nil {
						t.Fatal(cloneErr)
					}
					payload := sharedRefreshK12Checkpoint()
					got, err := commitSharedRecoveryRefresh(f.store, f.dataset, expected, id, payload)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatal("refresh result changed fields outside S/parent/status", err)
					}
					if !reflect.DeepEqual(expected, inputBefore) {
						t.Fatal("kernel mutated input expected")
					}
					if string(taskBefore) != string(sharedRefreshK12Read(t, f, bucketTask)) || string(requiredBefore) != string(sharedRefreshK12Read(t, f, bucketRecoveryRequired)) {
						t.Fatal("refresh rewrote Task/raw source/path or required")
					}
					_, current := d4s12Capture(t, f)
					if !reflect.DeepEqual(current.Record, want) || !current.CheckpointPresent {
						t.Fatal("atomic refresh state not stored")
					}
					sharedRefreshK12AssertCheckpoint(t, sharedRefreshK12Read(t, f, bucketSave))
					if got.Coverage != nil && (got.Coverage.CheckpointHash != expected.Record.Coverage.CheckpointHash || got.Coverage.CheckpointHash == current.CheckpointHash) {
						t.Fatal("coverage seal mistaken for current checkpoint")
					}
					// Both returned and input values remain owned; no alias reaches DB.
					got.Actions[id-1].Finished[0] = "caller-mutated"
					if got.Coverage != nil {
						got.Coverage.DurableRanges[0].End = 1
					}
					payload[2] = 'X'
					_, again := d4s12Capture(t, f)
					if !reflect.DeepEqual(again.Record, want) {
						t.Fatal("returned record aliases stored state")
					}
					sharedRefreshK12AssertCheckpoint(t, sharedRefreshK12Read(t, f, bucketSave))
					before := f.snapshot(t)
					late, err := commitSharedRecoveryRefresh(f.store, f.dataset, expected, id, sharedRefreshK12Checkpoint())
					if !errors.Is(err, transfer.ErrRecoveryStale) || !reflect.DeepEqual(late, recoveryRecord{}) || f.snapshot(t) != before {
						t.Fatal("old source late callback changed state", err)
					}
					if backend == "bolt" {
						f.reopen(t)
						_, reopened := d4s12Capture(t, f)
						if !reflect.DeepEqual(reopened.Record, want) {
							t.Fatal("Bolt reopen changed refresh")
						}
						sharedRefreshK12AssertCheckpoint(t, sharedRefreshK12Read(t, f, bucketSave))
					}
				})
			}
		}
	}
}

func TestSharedRefreshK12CASAndActionRejection(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, mode := range []string{"checkpoint-changed", "checkpoint-deleted", "checkpoint-appeared", "token-G", "token-S", "record", "required", "missing-task", "raw-source", "frozen-W", "wrong-action", "wrong-kind", "old-parent-G", "old-parent-S", "finished", "partial-finished", "overflow"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				f, expected, id := sharedRefreshK12New(t, backend, "tracking", mode != "checkpoint-appeared")
				switch mode {
				case "checkpoint-changed", "checkpoint-appeared":
					if err := f.store.Put(bucketSave, f.id, json.RawMessage(`{"periodic":9007199254740995}`)); err != nil {
						t.Fatal(err)
					}
				case "checkpoint-deleted":
					if err := f.store.Delete(bucketSave, f.id); err != nil {
						t.Fatal(err)
					}
				case "token-G":
					expected.Token.Generation++
				case "token-S":
					expected.Token.SourceRevision++
				case "record":
					d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) { r.NotBefore = "2028-01-01T00:00:00Z" })
				case "required":
					if err := f.store.Put(bucketRecoveryRequired, f.id, &recoveryRequirement{4, recoverySharedPlainCapability}); err != nil {
						t.Fatal(err)
					}
				case "missing-task":
					if err := f.store.Delete(bucketTask, f.id); err != nil {
						t.Fatal(err)
					}
				case "raw-source":
					d4s12Change(t, f, func(task *Task, _ *recoveryRecord, _ *recoveryRequirement) { task.Meta.Req.URL += "&changed=1" })
				case "frozen-W":
					expected.Record.Actions[id-1].Members[0] = "caller-changed-work"
				case "wrong-action":
					id--
				default:
					d4s12Change(t, f, func(_ *Task, r *recoveryRecord, _ *recoveryRequirement) {
						last := &r.Actions[id-1]
						switch mode {
						case "wrong-kind":
							last.Kind = "transport"
							r.RefreshUsed--
							r.TransportUsed++
						case "old-parent-G":
							last.Generation--
						case "old-parent-S":
							last.SourceRevision--
						case "finished":
							last.State = "finished"
							last.Finished = append([]string(nil), last.Members...)
						case "partial-finished":
							last.Finished = []string{last.Members[0]}
						case "overflow":
							r.SourceRevision = ^uint64(0)
							last.SourceRevision = r.SourceRevision
						}
					})
					_, expected = d4s12Capture(t, f) // coherent CURRENT, so only action guard rejects
				}
				before := f.snapshot(t)
				got, err := commitSharedRecoveryRefresh(f.store, f.dataset, expected, id, sharedRefreshK12Checkpoint())
				if err == nil || !reflect.DeepEqual(got, recoveryRecord{}) || before != f.snapshot(t) {
					t.Fatal("CAS/action rejection changed state or leaked result", err)
				}
			})
		}
		t.Run(backend+"/wrong-dataset", func(t *testing.T) {
			f, expected, id := sharedRefreshK12New(t, backend, "none", true)
			before := f.snapshot(t)
			got, err := commitSharedRecoveryRefresh(f.store, strings.Repeat("f", 32), expected, id, sharedRefreshK12Checkpoint())
			if !errors.Is(err, ErrDatasetMismatch) || !reflect.DeepEqual(got, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("wrong domain accepted", err)
			}
		})
	}
}

func TestSharedRefreshK12InvalidCheckpoint(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend+"/legacy-v1-not-enrolled", func(t *testing.T) {
			legacy := d4s12New(t, backend, 1, false, true)
			_, expected := d4s12Capture(t, legacy)
			before := legacy.snapshot(t)
			got, err := commitSharedRecoveryRefresh(legacy.store, legacy.dataset, expected, expected.Record.NextAction, sharedRefreshK12Checkpoint())
			if !errors.Is(err, transfer.ErrRecoveryHeld) || !reflect.DeepEqual(got, recoveryRecord{}) || before != legacy.snapshot(t) {
				t.Fatal("legacy recovery implicitly enrolled", err)
			}
		})
		f, expected, id := sharedRefreshK12New(t, backend, "none", true)
		for _, input := range []string{"", "null", "[]", "1", `{"a":1}{"b":2}`, `{"bad":`} {
			t.Run(backend+"/"+fmt.Sprint(len(input))+"-"+fmt.Sprint(input == "null"), func(t *testing.T) {
				before := f.snapshot(t)
				got, err := commitSharedRecoveryRefresh(f.store, f.dataset, expected, id, json.RawMessage(input))
				if !errors.Is(err, transfer.ErrRecoveryWork) || !reflect.DeepEqual(got, recoveryRecord{}) || before != f.snapshot(t) {
					t.Fatal("nonobject/malformed input changed state", err)
				}
			})
		}
	}
}

func TestSharedRefreshK12RollbackAndUnknownACK(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, failure := range []int{1, 2, 0} {
			t.Run(fmt.Sprintf("%s/put-%d", backend, failure), func(t *testing.T) {
				f, expected, id := sharedRefreshK12New(t, backend, "tracking", true)
				before := f.snapshot(t)
				// Existing fault seam delegates to this concrete backend's real
				// transaction; all authoritative readback uses f.store, not wrapper.
				store := d4k12WriteFailure{AtomicStorage: f.store, failPut: failure, unknown: failure == 0}
				got, err := commitSharedRecoveryRefresh(store, f.dataset, expected, id, sharedRefreshK12Checkpoint())
				if !errors.Is(err, transfer.ErrRecoveryUnavailable) || !reflect.DeepEqual(got, recoveryRecord{}) {
					t.Fatal("failed/unknown ACK leaked result", err)
				}
				if failure != 0 {
					if before != f.snapshot(t) {
						t.Fatal("transaction rollback changed checkpoint/record")
					}
					return
				}
				_, current := d4s12Capture(t, f)
				if !reflect.DeepEqual(current.Record, sharedRefreshK12Want(t, expected)) {
					t.Fatal("unknown ACK did not actually commit")
				}
				sharedRefreshK12AssertCheckpoint(t, sharedRefreshK12Read(t, f, bucketSave))
				if backend == "bolt" {
					f.reopen(t)
					_, current = d4s12Capture(t, f)
				}
				state := f.snapshot(t)
				// Neither retrying the old expected nor querying the same committed
				// record can turn this finished parent into a new continuation.
				for _, attempt := range []recoverySharedExpected{expected, current} {
					got, err := commitSharedRecoveryRefresh(f.store, f.dataset, attempt, id, sharedRefreshK12Checkpoint())
					if err == nil || !reflect.DeepEqual(got, recoveryRecord{}) || state != f.snapshot(t) {
						t.Fatal("query/unknown retry reconstructed result", err)
					}
				}
			})
		}
	}
}

// This wrapper changes only when the real transaction is entered. It does not
// implement a fake reader, initialize identity, or bypass trusted storage views.
type sharedRefreshK12BeforeStore struct {
	AtomicStorage
	before func()
}

func (s sharedRefreshK12BeforeStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	s.before()
	return s.AtomicStorage.UpdateAtomic(fn)
}

// A read failure in the real transaction must not become an absent checkpoint.
type sharedRefreshK12ReadFailure struct{ AtomicStorage }
type sharedRefreshK12ReadFailureTx struct{ StorageTransaction }

func (tx sharedRefreshK12ReadFailureTx) Get(bucket, key string, out any) (bool, error) {
	if bucket == bucketSave {
		return false, errors.New("synthetic read failure")
	}
	return tx.StorageTransaction.Get(bucket, key, out)
}
func (s sharedRefreshK12ReadFailure) UpdateAtomic(fn func(StorageTransaction) error) error {
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(sharedRefreshK12ReadFailureTx{tx}) })
}

func TestSharedRefreshK12OwnedAndReadFailure(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend+"/owned-input", func(t *testing.T) {
			f, expected, id := sharedRefreshK12New(t, backend, "tracking", true)
			want := sharedRefreshK12Want(t, expected)
			payload := sharedRefreshK12Checkpoint()
			store := sharedRefreshK12BeforeStore{AtomicStorage: f.store, before: func() {
				expected.Record.Actions[id-1].Members[0] = "mutated-after-copy"
				expected.Record.Coverage.DurableRanges[0].End = 1
				payload[2] = 'X'
			}}
			got, err := commitSharedRecoveryRefresh(store, f.dataset, expected, id, payload)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("caller values were not owned before transaction", err)
			}
			sharedRefreshK12AssertCheckpoint(t, sharedRefreshK12Read(t, f, bucketSave))
		})
		t.Run(backend+"/read-error-is-not-absence", func(t *testing.T) {
			f, expected, id := sharedRefreshK12New(t, backend, "none", false)
			before := f.snapshot(t)
			got, err := commitSharedRecoveryRefresh(sharedRefreshK12ReadFailure{f.store}, f.dataset, expected, id, sharedRefreshK12Checkpoint())
			if !errors.Is(err, transfer.ErrRecoveryUnavailable) || !reflect.DeepEqual(got, recoveryRecord{}) || before != f.snapshot(t) {
				t.Fatal("read error became double-absence enrollment", err)
			}
		})
	}
}

func TestSharedRefreshK12ConcurrentWinner(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f, expected, id := sharedRefreshK12New(t, backend, "tracking", true)
			start := make(chan struct{})
			results := make(chan struct {
				record recoveryRecord
				err    error
			}, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					r, err := commitSharedRecoveryRefresh(f.store, f.dataset, expected, id, sharedRefreshK12Checkpoint())
					results <- struct {
						record recoveryRecord
						err    error
					}{r, err}
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			wins := 0
			for result := range results {
				if result.err == nil {
					wins++
					if !reflect.DeepEqual(result.record, sharedRefreshK12Want(t, expected)) {
						t.Fatal("winner field loss")
					}
				} else if !errors.Is(result.err, transfer.ErrRecoveryStale) || !reflect.DeepEqual(result.record, recoveryRecord{}) {
					t.Fatal("loser leaked record", result.err)
				}
			}
			if wins != 1 {
				t.Fatal("expected exactly one ACK winner", wins)
			}
		})
	}
}
