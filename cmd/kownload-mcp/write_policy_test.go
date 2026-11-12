//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
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

func TestWritePolicyCompatibility(t *testing.T) {
	if maxTokenFile != mcpwritepolicy.MaxBytes || errWritePolicy != mcpwritepolicy.ErrInvalidPolicy {
		t.Fatal("input/error contract differs")
	}
	for _, p := range []*writePolicy{nil, {}} {
		if _, ok := p.allowCreate(policyDataset, "downloads"); ok || p.allowTask("pause", policyDataset, "task_1") {
			t.Fatal("empty wrapper allowed")
		}
	}
	v := policyFixture()
	v["allowedEffects"] = []string{"script"}
	p, err := parseWritePolicy(policyBytes(t, v))
	if err != nil {
		t.Fatal(err)
	}
	grant, ok := p.allowCreate(policyDataset, "downloads")
	if !ok || grant.outputRoot != "/private/tmp/not-created-by-policy" || len(grant.effects) != 1 || !p.allowTask("resume", policyDataset, "task_1") {
		t.Fatal("wrapper changed decision")
	}
	grant.effects[0] = "local-change"
	again, _ := p.allowCreate(policyDataset, "downloads")
	if again.effects[0] != "script" || p.allowTask("delete", policyDataset, "task_1") {
		t.Fatal("wrapper changed ownership/scope")
	}
	if p, err := parseWritePolicy([]byte(`{"version":1,"version":1}`)); p != nil || err != errWritePolicy {
		t.Fatal("wrapper changed invalid input")
	}
}
