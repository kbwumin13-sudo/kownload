//go:build darwin

package mcpauthority_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/mcpauthority"
	"github.com/GopeedLab/gopeed/pkg/mcpauthority/policyfile"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

func TestLoadPolicyFileForwarding(t *testing.T) {
	if mcpauthority.ErrInvalidPolicyFile != policyfile.ErrInvalidPolicyFile || mcpauthority.ErrInvalidPolicyFile != mcpwritepolicy.ErrInvalidPolicy {
		t.Fatal("forwarder error identity differs")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "policy.json")
	data, err := json.Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := mcpauthority.LoadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := policyfile.LoadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	as, ae := a.SnapshotV1()
	bs, be := b.SnapshotV1()
	if ae != nil || be != nil || !reflect.DeepEqual(as, bs) {
		t.Fatal("forwarder changed policy")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if p, err := mcpauthority.LoadPolicyFile(path); p != nil || err != mcpauthority.ErrInvalidPolicyFile {
		t.Fatal("forwarder changed missing-file refusal")
	}
}
