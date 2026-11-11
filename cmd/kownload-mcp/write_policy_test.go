//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
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
func mustPolicy(t *testing.T, v any) *writePolicy {
	t.Helper()
	p, e := parseWritePolicy(policyBytes(t, v))
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
			_, create := p.allowCreate(policyDataset, "downloads")
			if create != (mask&1 != 0) || p.allowTask("pause", policyDataset, "task_1") != (mask&2 != 0) || p.allowTask("resume", policyDataset, "task_1") != (mask&4 != 0) {
				t.Fatal("operations not independent")
			}
			if _, ok := p.allowCreate(policyOtherDataset, "downloads"); ok {
				t.Fatal("cross-dataset create")
			}
			if _, ok := p.allowCreate(policyDataset, "unknown"); ok {
				t.Fatal("unknown output root")
			}
			if p.allowTask("pause", policyOtherDataset, "task_1") || p.allowTask("resume", policyDataset, "task_2") || p.allowTask("delete", policyDataset, "task_1") || p.allowTask("create", policyDataset, "task_1") {
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
			if kind != "noRootGrant" && p.allowTask("pause", policyDataset, "task_1") {
				t.Fatal("empty scope implicitly all")
			}
			if kind != "noTasks" {
				if _, ok := p.allowCreate(policyDataset, "downloads"); ok {
					t.Fatal("empty create scope allowed")
				}
			}
		})
	}
	var p *writePolicy
	if _, ok := p.allowCreate(policyDataset, "downloads"); ok || p.allowTask("pause", policyDataset, "task_1") {
		t.Fatal("nil policy allowed")
	}
}

func TestWritePolicyEffectsAndOwnedValues(t *testing.T) {
	v := policyFixture()
	delete(v, "allowedEffects")
	p := mustPolicy(t, v)
	grant, ok := p.allowCreate(policyDataset, "downloads")
	if !ok || len(grant.effects) != 0 {
		t.Fatal("omitted effects not suppressed")
	}
	v["allowedEffects"] = []string{"script", "auto_open", "webhook", "post_process"}
	data := policyBytes(t, v)
	p, err := parseWritePolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = ' '
	}
	grant, ok = p.allowCreate(policyDataset, "downloads")
	if !ok || len(grant.effects) != 4 || grant.outputRoot != "/private/tmp/not-created-by-policy" {
		t.Fatal("trusted policy not retained")
	}
	grant.effects[0] = "SYNTHETIC_MODEL_OVERRIDE"
	again, _ := p.allowCreate(policyDataset, "downloads")
	if again.effects[0] != "script" {
		t.Fatal("grant aliases policy")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				g, ok := p.allowCreate(policyDataset, "downloads")
				if !ok || !p.allowTask("pause", policyDataset, "task_1") {
					t.Error("immutable decision changed")
				}
				g.effects[0] = "local-copy"
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
			if p, e := parseWritePolicy(policyBytes(t, v)); p != nil || e != errWritePolicy {
				t.Fatal("invalid config accepted or error leaked")
			}
		})
	}
	base := policyBytes(t, policyFixture())
	for name, data := range map[string][]byte{
		"empty": nil, "array": []byte("[]"), "null": []byte("null"), "oversize": bytes.Repeat([]byte(" "), maxTokenFile+1),
		"trailing":         append(append([]byte{}, base...), []byte(" {}")...),
		"duplicate":        bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"duplicateEscaped": bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"\u0076ersion":1`), 1),
		"nestedDuplicate":  bytes.Replace(base, []byte(`"create":true`), []byte(`"create":false,"create":true`), 1),
		"invalidUTF8":      append(append([]byte{}, base...), 0xff),
	} {
		t.Run(name, func(t *testing.T) {
			if p, e := parseWritePolicy(data); p != nil || e != errWritePolicy {
				t.Fatal("ambiguous data accepted")
			}
		})
	}
	exact := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), maxTokenFile-len(base))...)
	if _, e := parseWritePolicy(exact); e != nil {
		t.Fatal("bounded maximum rejected")
	}
}

func TestWritePolicyPrivateFile(t *testing.T) {
	for _, mode := range []os.FileMode{0400, 0600} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			root := privateTokenDir(t)
			path := filepath.Join(root, "policy.json")
			data := policyBytes(t, policyFixture())
			if err := os.WriteFile(path, data, mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			p, err := readWritePolicyFile(path)
			if err != nil || !p.allowTask("pause", policyDataset, "task_1") {
				t.Fatal("private file rejected")
			}
			after, err := os.Stat(path)
			got, e := os.ReadFile(path)
			if err != nil || e != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(data, got) {
				t.Fatal("loader modified file")
			}
			replacement := filepath.Join(root, "next")
			v := policyFixture()
			v["operations"] = map[string]any{}
			if e := os.WriteFile(replacement, policyBytes(t, v), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.Rename(replacement, path); e != nil {
				t.Fatal(e)
			}
			if !p.allowTask("pause", policyDataset, "task_1") {
				t.Fatal("loaded value changed after rotation")
			}
			next, e := readWritePolicyFile(path)
			if e != nil || next.allowTask("pause", policyDataset, "task_1") {
				t.Fatal("explicit next load failed")
			}
		})
	}
	for _, kind := range []string{"missing", "fileLink", "ancestorLink", "hardlink", "directory", "fifo", "publicFile", "executable", "publicParent", "writableAncestor", "relative", "unclean", "oversize", "invalidJSON"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTokenDir(t)
			path := filepath.Join(root, "policy.json")
			data := policyBytes(t, policyFixture())
			if e := os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
			var e error
			switch kind {
			case "missing":
				path += "-missing"
			case "fileLink":
				e = os.Symlink(path, path+"-link")
				path += "-link"
			case "ancestorLink":
				e = os.Symlink(root, filepath.Join(root, "alias"))
				path = filepath.Join(root, "alias", "policy.json")
			case "hardlink":
				e = os.Link(path, path+"-link")
			case "directory":
				path = root
			case "fifo":
				path += "-fifo"
				e = unix.Mkfifo(path, 0600)
			case "publicFile":
				e = os.Chmod(path, 0644)
			case "executable":
				e = os.Chmod(path, 0700)
			case "publicParent":
				e = os.Chmod(root, 0755)
			case "writableAncestor":
				child := filepath.Join(root, "child")
				e = os.Mkdir(child, 0700)
				if e == nil {
					path = filepath.Join(child, "policy.json")
					e = os.WriteFile(path, data, 0600)
				}
				if e == nil {
					e = os.Chmod(root, 0777)
				}
			case "relative":
				path = "policy.json"
			case "unclean":
				path = root + "/./policy.json"
			case "oversize":
				e = os.WriteFile(path, bytes.Repeat([]byte("x"), maxTokenFile+1), 0600)
			case "invalidJSON":
				e = os.WriteFile(path, []byte(`{"SYNTHETIC_SECRET":"must not leak"}`), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			if p, e := readWritePolicyFile(path); p != nil || e != errWritePolicy {
				t.Fatal("unsafe file accepted or details leaked")
			}
			// No repair of public permissions, no creation of a missing target.
			if kind == "missing" {
				if _, e = os.Lstat(path); !os.IsNotExist(e) {
					t.Fatal("loader created missing path")
				}
			}
			if kind == "publicParent" {
				s, e := os.Stat(root)
				if e != nil || s.Mode().Perm() != 0755 {
					t.Fatal("loader repaired parent")
				}
			}
		})
	}
}
