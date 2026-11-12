package download

import (
	"encoding/json"
	"reflect"
	"testing"
)

func exactRecoveryTask(t *testing.T) *Task {
	t.Helper()
	task := recoveryFixtureTask(t.TempDir())
	task.Recovery = &TaskRecoveryRef{Version: 1}
	task.Meta.Req.Extra = map[string]any{"exact": json.Number("9007199254740993"), "negative": json.Number("-9007199254740993")}
	task.Meta.Opts.Extra = map[string]any{"exact": json.Number("9007199254740993"), "nested": map[string]any{"fraction": json.Number("0.123456789012345678901"), "keep": "original"}}
	return task
}
func assertRecoveryTaskExact(t *testing.T, task *Task) {
	t.Helper()
	req := task.Meta.Req.Extra.(map[string]any)
	opts := task.Meta.Opts.Extra.(map[string]any)
	for _, v := range []struct {
		got  any
		want string
	}{{req["exact"], "9007199254740993"}, {req["negative"], "-9007199254740993"}, {opts["exact"], "9007199254740993"}, {opts["nested"].(map[string]any)["fraction"], "0.123456789012345678901"}} {
		number, ok := v.got.(json.Number)
		if !ok || number.String() != v.want {
			t.Fatalf("recovery Task rounded Extra: got %T(%v), want json.Number(%s)", v.got, v.got, v.want)
		}
	}
}
func TestRecoveryTaskJSONExactDeclarationAndLegacyBoundary(t *testing.T) {
	task := exactRecoveryTask(t)
	b, e := json.Marshal(task)
	if e != nil {
		t.Fatal(e)
	}
	var before map[string]any
	if e = decodeStoredJSON(b, &before); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		name   string
		marker any
		exact  bool
	}{{"absent", nil, false}, {"null", nil, false}, {"v1", map[string]any{"version": 1}, true}, {"v2", map[string]any{"version": 2}, true}, {"v3", map[string]any{"version": 3}, true}, {"unknown", map[string]any{"version": 99}, true}, {"empty-declaration", map[string]any{}, true}} {
		t.Run(c.name, func(t *testing.T) {
			var values map[string]any
			if e := decodeStoredJSON(b, &values); e != nil {
				t.Fatal(e)
			}
			if c.name == "absent" {
				delete(values, "recovery")
			} else {
				values["recovery"] = c.marker
			}
			raw, e := json.Marshal(values)
			if e != nil {
				t.Fatal(e)
			}
			var decoded Task
			if e = json.Unmarshal(raw, &decoded); e != nil {
				t.Fatal(e)
			}
			if c.exact {
				assertRecoveryTaskExact(t, &decoded)
				return
			}
			if _, ok := decoded.Meta.Opts.Extra.(map[string]any)["exact"].(float64); !ok {
				t.Fatal("changed undeclared legacy codec")
			}
		})
	}
}
func TestRecoveryTaskStorageExactOwnedRoundTrip(t *testing.T) {
	for _, name := range []string{"mem", "bolt"} {
		t.Run(name, func(t *testing.T) {
			var store AtomicStorage = NewMemStorage()
			dir := t.TempDir()
			if name == "bolt" {
				store = NewBoltStorage(dir)
			}
			defer func() { store.Close() }()
			if e := store.Setup([]string{bucketTask}); e != nil {
				t.Fatal(e)
			}
			if e := InitializeDatasetIdentity(store); e != nil {
				t.Fatal(e)
			}
			var identity datasetRecord
			if ok, e := store.Get(bucketConfig, datasetIdentityKey, &identity); e != nil || !ok {
				t.Fatal(e)
			}
			task := exactRecoveryTask(t)
			if e := store.Put(bucketTask, task.ID, task); e != nil {
				t.Fatal(e)
			}
			var got Task
			if ok, e := store.Get(bucketTask, task.ID, &got); e != nil || !ok {
				t.Fatal(e)
			}
			assertRecoveryTaskExact(t, &got)
			assertRecoveryTaskExact(t, got.clone())
			var listed []*Task
			if e := store.List(bucketTask, &listed); e != nil || len(listed) != 1 {
				t.Fatal(e)
			}
			assertRecoveryTaskExact(t, listed[0])
			rawRead := func() map[string]any {
				var value map[string]any
				e := withDatasetReadView(store, identity.DatasetID, func(reader datasetReader) error {
					ok, e := reader.Get(bucketTask, task.ID, &value)
					if !ok && e == nil {
						t.Fatal("lost Task")
					}
					return e
				})
				if e != nil {
					t.Fatal(e)
				}
				return value
			}
			before := rawRead()
			if e := store.UpdateAtomic(func(tx StorageTransaction) error {
				var owned Task
				ok, e := tx.Get(bucketTask, task.ID, &owned)
				if e != nil {
					return e
				}
				if !ok {
					t.Fatal("lost Task")
				}
				assertRecoveryTaskExact(t, &owned)
				owned.Recovery = &TaskRecoveryRef{Version: 3}
				return tx.Put(bucketTask, task.ID, &owned)
			}); e != nil {
				t.Fatal(e)
			}
			before["recovery"] = map[string]any{"version": json.Number("3")}
			if after := rawRead(); !reflect.DeepEqual(before, after) {
				t.Fatal("owned write changed original Task JSON beyond ref")
			}
			got.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] = "foreign"
			var again Task
			if ok, e := store.Get(bucketTask, task.ID, &again); e != nil || !ok {
				t.Fatal(e)
			}
			assertRecoveryTaskExact(t, &again)
			if again.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] != "original" {
				t.Fatal("owned alias leaked")
			}
			if name == "bolt" {
				if e := store.Close(); e != nil {
					t.Fatal(e)
				}
				store = NewBoltStorage(dir)
				var reopened Task
				if ok, e := store.Get(bucketTask, task.ID, &reopened); e != nil || !ok {
					t.Fatal(e)
				}
				assertRecoveryTaskExact(t, &reopened)
				if !reflect.DeepEqual(before, rawRead()) {
					t.Fatal("reopen changed raw Task")
				}
			}
		})
	}
}
