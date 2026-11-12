package mcpauthority_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpauthority"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

const dataset = "0123456789abcdef0123456789abcdef"
const otherDataset = "1123456789abcdef0123456789abcdef"

var networkID = "mcp-network/v1:" + strings.Repeat("a", 64)

func fixture() map[string]any {
	return map[string]any{
		"version":        1,
		"operations":     map[string]any{"create": true, "pause": true, "resume": true},
		"datasets":       []any{map[string]any{"id": dataset, "taskIds": []string{"task_z", "task_a"}, "outputRoots": []string{"z", "a"}}},
		"outputRoots":    map[string]any{"z": "/private/tmp/never-open-z", "a": "/private/tmp/never-open-a"},
		"allowedEffects": []string{"webhook", "script"},
	}
}
func parse(t *testing.T, v map[string]any) *mcpwritepolicy.Policy {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p, err := mcpwritepolicy.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func bindings() []mcpauthority.VerifiedRootBinding {
	return []mcpauthority.VerifiedRootBinding{
		{RootID: "z", Path: "/private/tmp/never-open-z", Identity: transfer.CompletionFileIdentity{Device: 10, Inode: 21}},
		{RootID: "a", Path: "/private/tmp/never-open-a", Identity: transfer.CompletionFileIdentity{Device: 10, Inode: 20}},
	}
}
func mustNew(t *testing.T, p *mcpwritepolicy.Policy, d, n string, roots []mcpauthority.VerifiedRootBinding) *mcpauthority.Authority {
	t.Helper()
	a, err := mcpauthority.New(p, d, n, roots)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMCPAuthorityStableIdentity(t *testing.T) {
	v := fixture()
	a := mustNew(t, parse(t, v), dataset, networkID, bindings())
	v["allowedEffects"] = []string{"script", "webhook"}
	scope := v["datasets"].([]any)[0].(map[string]any)
	scope["taskIds"], scope["outputRoots"] = []string{"task_a", "task_z"}, []string{"a", "z"}
	r := bindings()
	r[0], r[1] = r[1], r[0]
	b := mustNew(t, parse(t, v), dataset, networkID, r)
	if a.PolicyID() != b.PolicyID() {
		t.Fatal("set/binding order changed P")
	}
	// Golden v1 encoding fixes the domain NUL, field order, explicit arrays and
	// decimal-string filesystem identities independently of constructor code.
	const want = "mcp-authority-v1:86e9a8e89a1c9f62f94ccafd9b996932debcc3e3f2fdd918f9cbb9840ded8ac8"
	if a.PolicyID() != want {
		t.Fatalf("v1 encoding changed: %s", a.PolicyID())
	}
}

func TestMCPAuthorityIdentityChanges(t *testing.T) {
	original := mustNew(t, parse(t, fixture()), dataset, networkID, bindings()).PolicyID()
	for _, name := range []string{"operation", "task_scope", "root_scope", "effects", "dataset", "network", "root_path", "root_device", "root_inode"} {
		t.Run(name, func(t *testing.T) {
			v, r, d, n := fixture(), bindings(), dataset, networkID
			scope := v["datasets"].([]any)[0].(map[string]any)
			switch name {
			case "operation":
				v["operations"].(map[string]any)["pause"] = false
			case "task_scope":
				scope["taskIds"] = []string{"task_a"}
			case "root_scope":
				scope["outputRoots"] = []string{"a"}
			case "effects":
				v["allowedEffects"] = []string{}
			case "dataset":
				d = otherDataset
			case "network":
				n = "mcp-network/v1:" + strings.Repeat("b", 64)
			case "root_path":
				v["outputRoots"].(map[string]any)["z"] = "/private/tmp/changed"
				r[0].Path = "/private/tmp/changed"
			case "root_device":
				r[0].Identity.Device++
			case "root_inode":
				r[0].Identity.Inode++
			}
			if mustNew(t, parse(t, v), d, n, r).PolicyID() == original {
				t.Fatal("changed authority reused P")
			}
		})
	}
}

func TestMCPAuthorityRejectsInvalid(t *testing.T) {
	for _, name := range []string{"nil_policy", "zero_policy", "empty_dataset", "bad_dataset", "empty_network", "bad_network", "missing_root", "extra_root", "duplicate_root", "wrong_root", "wrong_path", "zero_identity"} {
		t.Run(name, func(t *testing.T) {
			p, r, d, n := parse(t, fixture()), bindings(), dataset, networkID
			switch name {
			case "nil_policy":
				p = nil
			case "zero_policy":
				p = &mcpwritepolicy.Policy{}
			case "empty_dataset":
				d = ""
			case "bad_dataset":
				d = "SYNTHETIC_PRIVATE"
			case "empty_network":
				n = ""
			case "bad_network":
				n = "SYNTHETIC_PRIVATE"
			case "missing_root":
				r = r[:1]
			case "extra_root":
				r = append(r, mcpauthority.VerifiedRootBinding{RootID: "extra"})
			case "duplicate_root":
				r[1] = r[0]
			case "wrong_root":
				r[0].RootID = "unknown"
			case "wrong_path":
				r[0].Path = "/SYNTHETIC_PRIVATE"
			case "zero_identity":
				r[0].Identity = transfer.CompletionFileIdentity{}
			}
			a, err := mcpauthority.New(p, d, n, r)
			if a != nil || !errors.Is(err, mcpauthority.ErrInvalidAuthority) || err.Error() != "invalid MCP authority" {
				t.Fatalf("not fixed fail-closed: %v", err)
			}
		})
	}
}

func TestMCPAuthorityScopeAndDefaults(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		v := fixture()
		v["operations"] = map[string]any{"create": mask&1 != 0, "pause": mask&2 != 0, "resume": mask&4 != 0}
		a := mustNew(t, parse(t, v), dataset, networkID, bindings())
		p := a.PolicyID()
		_, create := a.AllowCreate(p, dataset, "a")
		if create != (mask&1 != 0) || a.AllowTask(p, dataset, "pause", "task_a") != (mask&2 != 0) || a.AllowTask(p, dataset, "resume", "task_a") != (mask&4 != 0) {
			t.Fatal("operation scope changed")
		}
		for _, wrong := range []struct{ p, d, root, task string }{
			{"", dataset, "a", "task_a"}, {"wrong", dataset, "a", "task_a"}, {p, otherDataset, "a", "task_a"}, {p, dataset, "unknown", "unknown"},
		} {
			if g, ok := a.AllowCreate(wrong.p, wrong.d, wrong.root); ok || g.OutputRoot != "" || len(g.Effects) != 0 {
				t.Fatal("create scope escape/leak")
			}
			if a.AllowTask(wrong.p, wrong.d, "pause", wrong.task) || a.AllowTask(wrong.p, wrong.d, "resume", wrong.task) {
				t.Fatal("task scope escape")
			}
		}
		for _, op := range []string{"", "create", "delete", "Pause"} {
			if a.AllowTask(p, dataset, op, "task_a") {
				t.Fatal("unknown action allowed")
			}
		}
	}
	for _, a := range []*mcpauthority.Authority{nil, {}} {
		if a.PolicyID() != "" || a.AllowTask("", "", "pause", "") {
			t.Fatal("nil/zero allowed")
		}
		if _, ok := a.AllowCreate("", "", ""); ok {
			t.Fatal("zero create allowed")
		}
	}
	v := fixture()
	v["operations"] = map[string]any{}
	delete(v, "allowedEffects")
	a := mustNew(t, parse(t, v), dataset, networkID, bindings())
	v["operations"] = map[string]any{"create": false, "pause": false, "resume": false}
	v["allowedEffects"] = []string{}
	b := mustNew(t, parse(t, v), dataset, networkID, bindings())
	if a.PolicyID() != b.PolicyID() || a.AllowTask(a.PolicyID(), dataset, "pause", "task_a") {
		t.Fatal("default-false semantics changed")
	}
	if _, ok := a.AllowCreate(a.PolicyID(), dataset, "a"); ok {
		t.Fatal("default create allowed")
	}
	v["operations"] = map[string]any{"create": true, "pause": true, "resume": true}
	v["datasets"] = []any{}
	v["outputRoots"] = map[string]any{}
	empty := mustNew(t, parse(t, v), dataset, networkID, nil)
	if empty.AllowTask(empty.PolicyID(), dataset, "pause", "task_a") {
		t.Fatal("empty dataset means all")
	}
	if _, ok := empty.AllowCreate(empty.PolicyID(), dataset, "a"); ok {
		t.Fatal("empty root means all")
	}
}

func TestMCPAuthorityOwnedValues(t *testing.T) {
	p, r := parse(t, fixture()), bindings()
	a := mustNew(t, p, dataset, networkID, r)
	id := a.PolicyID()
	before, _ := a.AllowCreate(id, dataset, "a")
	r[0].Path = "/changed"
	r[1].Identity.Inode++
	s, _ := p.SnapshotV1()
	s.Effects[0], s.Datasets[0].TaskIDs[0], s.Roots[0].Path = "changed", "changed", "/changed"
	*p = mcpwritepolicy.Policy{}
	g, ok := a.AllowCreate(id, dataset, "a")
	if !ok || !reflect.DeepEqual(before, g) || a.PolicyID() != id || !a.AllowTask(id, dataset, "resume", "task_a") {
		t.Fatal("caller mutation changed authority")
	}
	g.Effects[0] = "local"
	again, _ := a.AllowCreate(id, dataset, "a")
	if !reflect.DeepEqual(before, again) {
		t.Fatal("grant aliases authority")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				g, ok := a.AllowCreate(id, dataset, "a")
				if !ok || !reflect.DeepEqual(g, before) || !a.AllowTask(id, dataset, "pause", "task_a") {
					t.Error("concurrent decision changed")
					return
				}
				g.Effects[0] = "local"
			}
		}()
	}
	wg.Wait()
}

func TestMCPAuthorityLargeIdentityAndZeroDevice(t *testing.T) {
	r := bindings()
	r[0].Identity = transfer.CompletionFileIdentity{Device: 0, Inode: ^uint64(0)}
	a := mustNew(t, parse(t, fixture()), dataset, networkID, r)
	r[0].Identity.Inode--
	b := mustNew(t, parse(t, fixture()), dataset, networkID, r)
	if a.PolicyID() == b.PolicyID() {
		t.Fatal("uint64 identity lost precision")
	}
}
