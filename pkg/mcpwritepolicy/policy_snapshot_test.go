package mcpwritepolicy_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

func TestPolicySnapshotSortedSemantics(t *testing.T) {
	v := policyFixture()
	v["outputRoots"] = map[string]any{"z": "/private/tmp/z", "a": "/private/tmp/a"}
	v["datasets"] = []any{
		map[string]any{"id": policyOtherDataset, "taskIds": []string{"z", "a"}, "outputRoots": []string{"z", "a"}},
		map[string]any{"id": policyDataset, "taskIds": []string{}, "outputRoots": []string{}},
	}
	v["allowedEffects"] = []string{"webhook", "script"}
	p := mustPolicy(t, v)
	first, err := p.SnapshotV1()
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.Datasets[0].ID != policyDataset || first.Roots[0].ID != "a" ||
		!reflect.DeepEqual(first.Datasets[1].TaskIDs, []string{"a", "z"}) ||
		!reflect.DeepEqual(first.Datasets[1].RootIDs, []string{"a", "z"}) ||
		!reflect.DeepEqual(first.Effects, []string{"script", "webhook"}) {
		t.Fatalf("not normalized: %+v", first)
	}
	ds := v["datasets"].([]any)
	ds[0], ds[1] = ds[1], ds[0]
	ds[1].(map[string]any)["taskIds"] = []string{"a", "z"}
	ds[1].(map[string]any)["outputRoots"] = []string{"a", "z"}
	v["allowedEffects"] = []string{"script", "webhook"}
	second, err := mustPolicy(t, v).SnapshotV1()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("set/input order changed semantics")
	}
	encoded, err := json.Marshal(first)
	if err != nil || strings.Contains(string(encoded), "null") {
		t.Fatal("empty sets must be explicit arrays")
	}
}

func TestPolicySnapshotDefaultAndInvalid(t *testing.T) {
	for _, p := range []*mcpwritepolicy.Policy{nil, {}} {
		if _, err := p.SnapshotV1(); !errors.Is(err, mcpwritepolicy.ErrInvalidPolicy) {
			t.Fatal("nil/zero accepted")
		}
	}
	v := policyFixture()
	v["operations"] = map[string]any{}
	v["datasets"] = []any{}
	v["outputRoots"] = map[string]any{}
	delete(v, "allowedEffects")
	a, err := mustPolicy(t, v).SnapshotV1()
	if err != nil {
		t.Fatal(err)
	}
	v["operations"] = map[string]any{"create": false, "pause": false, "resume": false}
	v["allowedEffects"] = []string{}
	b, err := mustPolicy(t, v).SnapshotV1()
	if err != nil || !reflect.DeepEqual(a, b) || a.Operations != (mcpwritepolicy.PolicyOperationsV1{}) || a.Datasets == nil || a.Roots == nil || a.Effects == nil {
		t.Fatal("omitted defaults not canonical")
	}
}

func TestPolicySnapshotOwnedCopies(t *testing.T) {
	v := policyFixture()
	v["allowedEffects"] = []string{"script"}
	p := mustPolicy(t, v)
	before, _ := p.SnapshotV1()
	copy, _ := p.SnapshotV1()
	copy.Operations.Create = false
	copy.Datasets[0].ID = policyOtherDataset
	copy.Datasets[0].TaskIDs[0] = "other_task"
	copy.Datasets[0].RootIDs[0] = "other_root"
	copy.Roots[0].ID, copy.Roots[0].Path = "other_root", "/other"
	copy.Effects[0] = "webhook"
	after, _ := p.SnapshotV1()
	if !reflect.DeepEqual(before, after) || !p.AllowTask("pause", policyDataset, "task_1") {
		t.Fatal("snapshot aliases policy")
	}
	g, ok := p.AllowCreate(policyDataset, "downloads")
	if !ok || g.Effects[0] != "script" {
		t.Fatal("snapshot changed original grants")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				s, e := p.SnapshotV1()
				if e != nil || !reflect.DeepEqual(s, before) {
					t.Error("concurrent snapshot changed")
					return
				}
				s.Datasets[0].TaskIDs[0] = "local"
				s.Effects[0] = "local"
			}
		}()
	}
	wg.Wait()
}
