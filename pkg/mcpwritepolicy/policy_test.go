package mcpwritepolicy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

const policyDataset = "0123456789abcdef0123456789abcdef"
const policyOtherDataset = "1123456789abcdef0123456789abcdef"

func policyFixture() map[string]any {
	return map[string]any{
		"version":        1,
		"operations":     map[string]any{"create": true, "pause": true, "resume": true},
		"datasets":       []any{map[string]any{"id": policyDataset, "taskIds": []string{"task_1"}, "outputRoots": []string{"downloads"}}},
		"outputRoots":    map[string]any{"downloads": "/private/tmp/not-created-by-policy"},
		"allowedEffects": []string{},
	}
}
func policyBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func mustPolicy(t *testing.T, v any) *mcpwritepolicy.Policy {
	t.Helper()
	p, e := mcpwritepolicy.Parse(policyBytes(t, v))
	if e != nil {
		t.Fatal("valid policy rejected")
	}
	return p
}
func firstPolicyScope(v map[string]any) map[string]any {
	return v["datasets"].([]any)[0].(map[string]any)
}

func TestWritePolicyPermissionMatrix(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("mask%d", mask), func(t *testing.T) {
			v := policyFixture()
			v["operations"] = map[string]any{"create": mask&1 != 0, "pause": mask&2 != 0, "resume": mask&4 != 0}
			p := mustPolicy(t, v)
			_, create := p.AllowCreate(policyDataset, "downloads")
			if create != (mask&1 != 0) || p.AllowTask("pause", policyDataset, "task_1") != (mask&2 != 0) || p.AllowTask("resume", policyDataset, "task_1") != (mask&4 != 0) {
				t.Fatal("operations not independent")
			}
			if _, ok := p.AllowCreate(policyOtherDataset, "downloads"); ok {
				t.Fatal("cross-dataset create")
			}
			if _, ok := p.AllowCreate(policyDataset, "unknown"); ok {
				t.Fatal("unknown output root")
			}
			if p.AllowTask("pause", policyOtherDataset, "task_1") || p.AllowTask("resume", policyDataset, "task_2") || p.AllowTask("delete", policyDataset, "task_1") || p.AllowTask("create", policyDataset, "task_1") {
				t.Fatal("scope escape")
			}
		})
	}
	for _, kind := range []string{"noTasks", "noDataset", "noRootGrant", "omittedOperations"} {
		t.Run(kind, func(t *testing.T) {
			v := policyFixture()
			switch kind {
			case "noTasks":
				firstPolicyScope(v)["taskIds"] = []string{}
			case "noDataset":
				v["datasets"] = []any{}
			case "noRootGrant":
				firstPolicyScope(v)["outputRoots"] = []string{}
			case "omittedOperations":
				v["operations"] = map[string]any{}
			}
			p := mustPolicy(t, v)
			if kind != "noRootGrant" && p.AllowTask("pause", policyDataset, "task_1") {
				t.Fatal("empty scope implicitly all")
			}
			if kind != "noTasks" {
				if _, ok := p.AllowCreate(policyDataset, "downloads"); ok {
					t.Fatal("empty create scope allowed")
				}
			}
		})
	}
	var p *mcpwritepolicy.Policy
	if _, ok := p.AllowCreate(policyDataset, "downloads"); ok || p.AllowTask("pause", policyDataset, "task_1") {
		t.Fatal("nil policy allowed")
	}
}

func TestWritePolicyEffectsAndOwnedValues(t *testing.T) {
	v := policyFixture()
	delete(v, "allowedEffects")
	p := mustPolicy(t, v)
	grant, ok := p.AllowCreate(policyDataset, "downloads")
	if !ok || len(grant.Effects) != 0 {
		t.Fatal("omitted effects not suppressed")
	}
	v["allowedEffects"] = []string{"script", "auto_open", "webhook", "post_process"}
	data := policyBytes(t, v)
	p, err := mcpwritepolicy.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = ' '
	}
	grant, ok = p.AllowCreate(policyDataset, "downloads")
	if !ok || len(grant.Effects) != 4 || grant.OutputRoot != "/private/tmp/not-created-by-policy" {
		t.Fatal("trusted policy not retained")
	}
	grant.Effects[0] = "SYNTHETIC_MODEL_OVERRIDE"
	again, _ := p.AllowCreate(policyDataset, "downloads")
	if again.Effects[0] != "script" {
		t.Fatal("grant aliases policy")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				g, ok := p.AllowCreate(policyDataset, "downloads")
				if !ok || !p.AllowTask("pause", policyDataset, "task_1") {
					t.Error("immutable decision changed")
				}
				g.Effects[0] = "local-copy"
			}
		}()
	}
	wg.Wait()
}

func TestWritePolicyRejectsInvalidJSON(t *testing.T) {
	changes := map[string]func(map[string]any){
		"version":           func(v map[string]any) { v["version"] = 2 },
		"versionString":     func(v map[string]any) { v["version"] = "1" },
		"versionMissing":    func(v map[string]any) { delete(v, "version") },
		"versionNull":       func(v map[string]any) { v["version"] = nil },
		"confirmed":         func(v map[string]any) { v["confirmed"] = true },
		"caseFold":          func(v map[string]any) { v["Version"] = 1; delete(v, "version") },
		"operationsNull":    func(v map[string]any) { v["operations"] = nil },
		"operationsMissing": func(v map[string]any) { delete(v, "operations") },
		"operationNull":     func(v map[string]any) { v["operations"].(map[string]any)["create"] = nil },
		"operationString":   func(v map[string]any) { v["operations"].(map[string]any)["pause"] = "true" },
		"operationUnknown":  func(v map[string]any) { v["operations"].(map[string]any)["delete"] = true },
		"operationCase":     func(v map[string]any) { v["operations"].(map[string]any)["Create"] = true },
		"datasetNull":       func(v map[string]any) { v["datasets"] = nil },
		"datasetMissing":    func(v map[string]any) { delete(v, "datasets") },
		"datasetObject":     func(v map[string]any) { v["datasets"] = map[string]any{} },
		"datasetWildcard":   func(v map[string]any) { firstPolicyScope(v)["id"] = "*" },
		"datasetUpper":      func(v map[string]any) { firstPolicyScope(v)["id"] = strings.ToUpper(policyDataset) },
		"datasetDuplicate":  func(v map[string]any) { v["datasets"] = append(v["datasets"].([]any), firstPolicyScope(v)) },
		"scopeUnknown":      func(v map[string]any) { firstPolicyScope(v)["confirmed"] = true },
		"taskWildcard":      func(v map[string]any) { firstPolicyScope(v)["taskIds"] = []string{"*"} },
		"taskPath":          func(v map[string]any) { firstPolicyScope(v)["taskIds"] = []string{"../a"} },
		"taskLong":          func(v map[string]any) { firstPolicyScope(v)["taskIds"] = []string{strings.Repeat("a", 65)} },
		"taskNull":          func(v map[string]any) { firstPolicyScope(v)["taskIds"] = nil },
		"taskMissing":       func(v map[string]any) { delete(firstPolicyScope(v), "taskIds") },
		"taskDuplicate":     func(v map[string]any) { firstPolicyScope(v)["taskIds"] = []string{"a", "a"} },
		"taskType":          func(v map[string]any) { firstPolicyScope(v)["taskIds"] = []int{1} },
		"taskMany": func(v map[string]any) {
			a := []string{}
			for i := 0; i < 65; i++ {
				a = append(a, fmt.Sprintf("t%d", i))
			}
			firstPolicyScope(v)["taskIds"] = a
		},
		"unregisteredRoot":   func(v map[string]any) { firstPolicyScope(v)["outputRoots"] = []string{"other"} },
		"rootScopeNull":      func(v map[string]any) { firstPolicyScope(v)["outputRoots"] = nil },
		"rootScopeDuplicate": func(v map[string]any) { firstPolicyScope(v)["outputRoots"] = []string{"downloads", "downloads"} },
		"rootMapNull":        func(v map[string]any) { v["outputRoots"] = nil },
		"rootWildcard":       func(v map[string]any) { v["outputRoots"].(map[string]any)["*"] = "/tmp/other" },
		"rootRelative":       func(v map[string]any) { v["outputRoots"].(map[string]any)["downloads"] = "relative" },
		"rootUnclean":        func(v map[string]any) { v["outputRoots"].(map[string]any)["downloads"] = "/tmp/a/../b" },
		"rootSlash":          func(v map[string]any) { v["outputRoots"].(map[string]any)["downloads"] = "/" },
		"rootControl":        func(v map[string]any) { v["outputRoots"].(map[string]any)["downloads"] = "/tmp/secret\nvalue" },
		"rootNull":           func(v map[string]any) { v["outputRoots"].(map[string]any)["downloads"] = nil },
		"rootMany": func(v map[string]any) {
			for i := 0; i < 16; i++ {
				v["outputRoots"].(map[string]any)[fmt.Sprintf("r%d", i)] = "/tmp/path"
			}
		},
		"effectUnknown":   func(v map[string]any) { v["allowedEffects"] = []string{"shell"} },
		"effectDuplicate": func(v map[string]any) { v["allowedEffects"] = []string{"script", "script"} },
		"effectNull":      func(v map[string]any) { v["allowedEffects"] = nil },
		"effectType":      func(v map[string]any) { v["allowedEffects"] = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			v := policyFixture()
			change(v)
			if p, e := mcpwritepolicy.Parse(policyBytes(t, v)); p != nil || e != mcpwritepolicy.ErrInvalidPolicy {
				t.Fatal("invalid config accepted or error leaked")
			}
		})
	}
	base := policyBytes(t, policyFixture())
	for name, data := range map[string][]byte{
		"empty": nil, "array": []byte("[]"), "null": []byte("null"), "oversize": bytes.Repeat([]byte(" "), mcpwritepolicy.MaxBytes+1),
		"trailing":         append(append([]byte{}, base...), []byte(" {}")...),
		"duplicate":        bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"duplicateEscaped": bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"\u0076ersion":1`), 1),
		"nestedDuplicate":  bytes.Replace(base, []byte(`"create":true`), []byte(`"create":false,"create":true`), 1),
		"invalidUTF8":      append(append([]byte{}, base...), 0xff),
	} {
		t.Run(name, func(t *testing.T) {
			if p, e := mcpwritepolicy.Parse(data); p != nil || e != mcpwritepolicy.ErrInvalidPolicy {
				t.Fatal("ambiguous data accepted")
			}
		})
	}
	exact := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), mcpwritepolicy.MaxBytes-len(base))...)
	if _, e := mcpwritepolicy.Parse(exact); e != nil {
		t.Fatal("bounded maximum rejected")
	}
}

func TestZeroPolicyDenies(t *testing.T) {
	var p mcpwritepolicy.Policy
	if _, ok := p.AllowCreate(policyDataset, "downloads"); ok || p.AllowTask("pause", policyDataset, "task_1") || p.AllowTask("resume", policyDataset, "task_1") {
		t.Fatal("zero policy allowed")
	}
}
