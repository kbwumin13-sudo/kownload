//go:build darwin

package policyfile

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

func policyFileTestDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

const filePolicyDataset = "0123456789abcdef0123456789abcdef"
const filePolicyOtherDataset = "1123456789abcdef0123456789abcdef"

func filePolicyFixture() map[string]any {
	return map[string]any{
		"version":        1,
		"operations":     map[string]any{"create": true, "pause": true, "resume": true},
		"datasets":       []any{map[string]any{"id": filePolicyDataset, "taskIds": []string{"task_1"}, "outputRoots": []string{"downloads"}}},
		"outputRoots":    map[string]any{"downloads": "/private/tmp/not-created-by-policy"},
		"allowedEffects": []string{},
	}
}
func filePolicyBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestLoadPolicyFilePrivate(t *testing.T) {
	for _, mode := range []os.FileMode{0400, 0600} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			root := policyFileTestDir(t)
			path := filepath.Join(root, "policy.json")
			data := filePolicyBytes(t, filePolicyFixture())
			if err := os.WriteFile(path, data, mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			p, err := LoadPolicyFile(path)
			if err != nil || !p.AllowTask("pause", filePolicyDataset, "task_1") {
				t.Fatal("private file rejected")
			}
			after, err := os.Stat(path)
			got, e := os.ReadFile(path)
			if err != nil || e != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(data, got) {
				t.Fatal("loader modified file")
			}
			replacement := filepath.Join(root, "next")
			v := filePolicyFixture()
			v["operations"] = map[string]any{}
			if e := os.WriteFile(replacement, filePolicyBytes(t, v), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.Rename(replacement, path); e != nil {
				t.Fatal(e)
			}
			if !p.AllowTask("pause", filePolicyDataset, "task_1") {
				t.Fatal("loaded value changed after rotation")
			}
			next, e := LoadPolicyFile(path)
			if e != nil || next.AllowTask("pause", filePolicyDataset, "task_1") {
				t.Fatal("explicit next load failed")
			}
		})
	}
	for _, kind := range []string{"missing", "fileLink", "ancestorLink", "hardlink", "directory", "fifo", "publicFile", "executable", "publicParent", "writableAncestor", "relative", "unclean", "oversize", "invalidJSON"} {
		t.Run(kind, func(t *testing.T) {
			root := policyFileTestDir(t)
			path := filepath.Join(root, "policy.json")
			data := filePolicyBytes(t, filePolicyFixture())
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
				e = os.WriteFile(path, bytes.Repeat([]byte("x"), mcpwritepolicy.MaxBytes+1), 0600)
			case "invalidJSON":
				e = os.WriteFile(path, []byte(`{"SYNTHETIC_SECRET":"must not leak"}`), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			if p, e := LoadPolicyFile(path); p != nil || e != ErrInvalidPolicyFile {
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

func TestLoadPolicyFileStrictParseAndLimit(t *testing.T) {
	valid := filePolicyBytes(t, filePolicyFixture())
	for _, sample := range []struct {
		name string
		data []byte
		good bool
	}{
		{"exact_limit", append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), mcpwritepolicy.MaxBytes-len(valid))...), true},
		{"over_limit", append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), mcpwritepolicy.MaxBytes+1-len(valid))...), false},
		{"duplicate_key", []byte(`{"version":1,"version":1}`), false},
		{"trailing_value", append(append([]byte{}, valid...), []byte(" {}")...), false},
		{"invalid_utf8", append(append([]byte{}, valid...), 0xff), false},
		{"empty", nil, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			path := filepath.Join(policyFileTestDir(t), "policy.json")
			if err := os.WriteFile(path, sample.data, 0600); err != nil {
				t.Fatal(err)
			}
			p, err := LoadPolicyFile(path)
			if sample.good {
				if err != nil || p == nil || !p.AllowTask("pause", filePolicyDataset, "task_1") {
					t.Fatal("valid bounded file rejected")
				}
			} else if p != nil || err != ErrInvalidPolicyFile || err.Error() != "invalid write policy" {
				t.Fatal("parse error contract changed")
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(got, sample.data) {
				t.Fatal("parse check changed contents")
			}
		})
	}
	if ErrInvalidPolicyFile != mcpwritepolicy.ErrInvalidPolicy {
		t.Fatal("error identity changed")
	}
	if p, err := LoadPolicyFile(""); p != nil || err != ErrInvalidPolicyFile {
		t.Fatal("empty path discovered a policy")
	}
}

// Synthetic stat values check otherwise privileged/foreign cases without
// reading a user's files, changing ownership or weakening the real FS tests.
func TestLoadPolicyFileStatRules(t *testing.T) {
	uid := uint32(os.Getuid())
	base := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uid, Nlink: 1, Size: 1, Dev: 1, Ino: 2}
	for _, name := range []string{"valid", "read_only", "foreign", "two_links", "zero_links", "negative", "oversize", "group_read", "executable", "setuid", "directory", "fifo"} {
		t.Run(name, func(t *testing.T) {
			s := base
			switch name {
			case "read_only":
				s.Mode = unix.S_IFREG | 0400
			case "foreign":
				s.Uid++
			case "two_links":
				s.Nlink = 2
			case "zero_links":
				s.Nlink = 0
			case "negative":
				s.Size = -1
			case "oversize":
				s.Size = mcpwritepolicy.MaxBytes + 1
			case "group_read":
				s.Mode = unix.S_IFREG | 0640
			case "executable":
				s.Mode = unix.S_IFREG | 0700
			case "setuid":
				s.Mode |= unix.S_ISUID
			case "directory":
				s.Mode = unix.S_IFDIR | 0600
			case "fifo":
				s.Mode = unix.S_IFIFO | 0600
			}
			if privatePolicyFile(&s) != (name == "valid" || name == "read_only") {
				t.Fatal("file stat rule changed")
			}
		})
	}
	for _, name := range []string{"owned", "root_owned", "foreign", "writable", "sticky", "not_directory"} {
		t.Run("ancestor_"+name, func(t *testing.T) {
			s := unix.Stat_t{Mode: unix.S_IFDIR | 0755, Uid: uid}
			switch name {
			case "root_owned":
				s.Uid = 0
			case "foreign":
				s.Uid++
			case "writable":
				s.Mode = unix.S_IFDIR | 0777
			case "sticky":
				s.Mode = unix.S_IFDIR | unix.S_ISVTX | 0777
			case "not_directory":
				s.Mode = unix.S_IFREG | 0755
			}
			want := name == "owned" || name == "root_owned" || name == "sticky"
			if trustedPolicyDirectory(&s) != want {
				t.Fatal("ancestor rule changed")
			}
		})
	}
	other := base
	if !samePolicyFile(&base, &other) {
		t.Fatal("same fd identity rejected")
	}
	other.Ino++
	if samePolicyFile(&base, &other) {
		t.Fatal("different inode accepted")
	}
	other = base
	other.Dev++
	if samePolicyFile(&base, &other) {
		t.Fatal("different device accepted")
	}
}
