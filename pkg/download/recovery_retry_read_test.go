package download

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func TestExplicitRecoveryRetryReadOnlyAndPresenceBoundaries(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			var store AtomicStorage = NewMemStorage()
			if bolt {
				store = NewBoltStorage(t.TempDir())
			}
			defer store.Close()
			dataset, task, _, request := explicitRetryFixture(t, store)
			d := &Downloader{storage: store}
			beforeTask, beforeRecord, err := readRecovery(store, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(&beforeTask)
			recordBefore, _ := json.Marshal(beforeRecord)
			for i := 0; i < 3; i++ {
				state, err := d.QueryRecoveryForDataset(dataset, task.ID)
				if err != nil || state.Management != "managed" || !state.RetryAllowed || state.LastExplicitRetry != nil {
					t.Fatal("invalid authoritative state", err, state)
				}
				state, err = d.QueryRecoveryActionForDataset(dataset, task.ID, request)
				if err != nil || state.Receipt == nil || state.Receipt.State != "not_recorded" || !state.Receipt.QueryRequired {
					t.Fatal("GET claimed user action", err, state)
				}
			}
			afterTask, afterRecord, err := readRecovery(store, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(&afterTask)
			recordAfter, _ := json.Marshal(afterRecord)
			if string(before) != string(after) || string(recordBefore) != string(recordAfter) {
				t.Fatal("GET modified persistent state")
			}
			if _, err = d.QueryRecoveryForDataset(strings.Repeat("f", 32), task.ID); !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal("wrong dataset returned state", err)
			}
			if err = store.Setup([]string{"task_completion_v1", "task_completion_required_v1"}); err != nil {
				t.Fatal(err)
			}
			for _, presence := range []string{"ref", "record", "required"} {
				t.Run(presence, func(t *testing.T) {
					if presence == "ref" {
						task.Completion = &TaskCompletionRef{Version: 999, CompletionID: "unknown"}
						if err := store.Put(bucketTask, task.ID, task); err != nil {
							t.Fatal(err)
						}
					} else {
						bucket := "task_completion_v1"
						if presence == "required" {
							bucket = "task_completion_required_v1"
						}
						if err := store.Put(bucket, task.ID, json.RawMessage(`{"version":999}`)); err != nil {
							t.Fatal(err)
						}
					}
					state, err := d.QueryRecoveryForDataset(dataset, task.ID)
					if err != nil || state.Management != "held" || state.RetryAllowed {
						t.Fatal("S4 presence allowed Retry", err, state)
					}
					if _, err = d.RetryRecoveryForDataset(context.Background(), dataset, task.ID, request); !errors.Is(err, transfer.ErrRecoveryHeld) {
						t.Fatal("Retry bypassed unsupported completion", err)
					}
					task.Completion = nil
					store.Put(bucketTask, task.ID, task)
					store.Delete("task_completion_v1", task.ID)
					store.Delete("task_completion_required_v1", task.ID)
				})
			}
			// Only the authoritative all-absent triple is classified legacy.
			task.Recovery = nil
			store.Put(bucketTask, task.ID, task)
			store.Delete(bucketTaskRecovery, task.ID)
			store.Delete(bucketRecoveryRequired, task.ID)
			state, err := d.QueryRecoveryForDataset(dataset, task.ID)
			if err != nil || state.Management != "legacy" || state.RetryAllowed {
				t.Fatal("legacy inference wrong", err, state)
			}
			raw, _ := json.Marshal(state)
			if strings.Contains(string(raw), "episode") || strings.Contains(string(raw), "sourceRevision") {
				t.Fatal("legacy invented managed tuple", string(raw))
			}
		})
	}
}
