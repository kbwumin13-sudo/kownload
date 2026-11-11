package download

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func memOwnedTaskFixture(t *testing.T) (*MemStorage, *Task) {
	t.Helper()
	store := NewMemStorage()
	store.Setup([]string{bucketTask})
	task := recoveryFixtureTask(t.TempDir())
	task.CreationOperation = &CreationOperation{1, batchOperationNamespace, "owned-snapshot", 0}
	task.Completion = &TaskCompletionRef{Version: 1, CompletionID: "owned-completion"}
	task.Meta.Req.Extra = map[string]any{"precise": json.Number("9007199254740993"), "nested": map[string]any{"header": "synthetic-original"}}
	if err := store.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	return store, task
}
func TestMemTaskSnapshotsDetachPutGetAndList(t *testing.T) {
	for _, where := range []string{"put", "get", "list"} {
		t.Run(where, func(t *testing.T) {
			store, input := memOwnedTaskFixture(t)
			oldURL := input.Meta.Req.URL
			var exposed *Task
			switch where {
			case "put":
				exposed = input
			case "get":
				var got Task
				if _, err := store.Get(bucketTask, input.ID, &got); err != nil {
					t.Fatal(err)
				}
				exposed = &got
			case "list":
				var listed []*Task
				if err := store.List(bucketTask, &listed); err != nil {
					t.Fatal(err)
				}
				exposed = listed[0]
			}
			exposed.Meta.Req.URL = "https://changed.invalid/foreign"
			exposed.Completion.CompletionID = "foreign-completion"
			exposed.Meta.Req.Extra.(map[string]any)["nested"].(map[string]any)["header"] = "synthetic-mutated"
			var stored Task
			if _, err := store.Get(bucketTask, input.ID, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Meta.Req.URL != oldURL || stored.Completion.CompletionID != "owned-completion" {
				t.Fatal("MemStorage exposed its Task/metadata to caller")
			}
			number, ok := stored.Meta.Req.Extra.(map[string]any)["precise"].(json.Number)
			if !ok || number.String() != "9007199254740993" {
				t.Fatal("owned decode lost Extra precision", stored.Meta.Req.Extra)
			}
			if stored.Meta.Req.Extra.(map[string]any)["nested"].(map[string]any)["header"] != "synthetic-original" {
				t.Fatal("nested Extra alias escaped")
			}
		})
	}
}

func TestTaskStorageDynamicParityAndConcurrentView(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		name := "mem"
		if bolt {
			name = "bolt"
		}
		t.Run(name, func(t *testing.T) {
			var store Storage = NewMemStorage()
			if bolt {
				store = NewBoltStorage(t.TempDir())
			}
			defer store.Close()
			store.Setup([]string{bucketTask})
			task := recoveryFixtureTask(t.TempDir())
			task.Completion = &TaskCompletionRef{1, "parity-completion"}
			if err := store.Put(bucketTask, task.ID, task); err != nil {
				t.Fatal(err)
			}
			var got Task
			if exists, err := store.Get(bucketTask, task.ID, &got); err != nil || !exists {
				t.Fatal(exists, err)
			}
			var listed []*Task
			if err := store.List(bucketTask, &listed); err != nil || len(listed) != 1 || listed[0].Completion.CompletionID != "parity-completion" {
				t.Fatal("dynamic Task contract changed", err)
			}
			got.Meta.Opts.Path = "foreign-path"
			listed[0].Completion.CompletionID = "foreign-id"
			var again Task
			store.Get(bucketTask, task.ID, &again)
			if again.Meta.Opts.Path != task.Meta.Opts.Path || again.Completion.CompletionID != "parity-completion" {
				t.Fatal("snapshot aliases persistent row")
			}
		})
	}
	store, input := memOwnedTaskFixture(t)
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	var identity datasetRecord
	store.Get(bucketConfig, datasetIdentityKey, &identity)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 25; i++ {
			input.dataLock.Lock()
			input.Progress.Downloaded = int64(i)
			input.dataLock.Unlock()
			if err := store.Put(bucketTask, input.ID, input); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 25; i++ {
			if err := withDatasetReadView(store, identity.DatasetID, func(reader datasetReader) error {
				var task Task
				_, err := reader.Get(bucketTask, input.ID, &task)
				if err == nil {
					task.Meta.Req.URL = "https://view-copy.invalid"
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		}
	}()
	workers.Wait()
	var stored Task
	store.Get(bucketTask, input.ID, &stored)
	if stored.Meta.Req.URL != input.Meta.Req.URL {
		t.Fatal("owned view mutated stored metadata")
	}
}
func TestMemTaskTransactionDoesNotAcquireLiveListTaskLock(t *testing.T) {
	store, input := memOwnedTaskFixture(t)
	var listed []*Task
	if err := store.List(bucketTask, &listed); err != nil {
		t.Fatal(err)
	}
	live := listed[0]
	live.dataLock.Lock()
	released := false
	defer func() {
		if !released {
			live.dataLock.Unlock()
		}
	}()
	finished := make(chan error, 1)
	go func() {
		finished <- store.UpdateAtomic(func(tx StorageTransaction) error {
			var saved Task
			_, err := tx.Get(bucketTask, input.ID, &saved)
			return err
		})
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		live.dataLock.Unlock()
		released = true
		<-finished
		t.Fatal("DB transaction waited on a TaskMap/live dataLock")
	}
	live.dataLock.Unlock()
	released = true
}
