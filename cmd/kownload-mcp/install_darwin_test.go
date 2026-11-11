package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type installFixture struct {
	home, root, bundle string
	m                  helperManifest
	ops                installOps
}

func newInstallFixture(t *testing.T) installFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	x := installFixture{home: home, root: managedRoot(home), bundle: filepath.Join(home, "Known.app"), ops: defaultInstallOps()}
	for _, p := range []string{x.root, filepath.Join(x.bundle, "Contents", "Helpers"), filepath.Join(x.bundle, "Contents", "Resources")} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	x.ops.verify = func(string, bool) error { return nil }
	x.ops.version = func(string, string) error { return nil }
	x.replacePayload(t, []byte("known-payload-one"), strings.Repeat("1", 40))
	return x
}
func (x *installFixture) replacePayload(t *testing.T, payload []byte, commit string) {
	t.Helper()
	sum := sha256.Sum256(payload)
	x.m = helperManifest{1, helperRelative, "1.0.0+" + commit, commit, 1, "v1.7.0", "arm64", hex.EncodeToString(sum[:])}
	if err := os.WriteFile(filepath.Join(x.bundle, helperRelative), payload, 0700); err != nil {
		t.Fatal(err)
	}
	x.writeManifest(t, x.m)
}
func (x installFixture) writeManifest(t *testing.T, m helperManifest) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"sourceCommit": m.SourceCommit, "releaseVersion": "1.0.0", "buildNumber": 1, "architecture": "arm64", "preview": true, "mcp": m})
	if err := os.WriteFile(filepath.Join(x.bundle, "Contents", "Resources", "build-manifest.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func (x installFixture) target() string { return filepath.Join(x.root, "bin", helperName) }
func (x installFixture) install(t *testing.T) {
	t.Helper()
	if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err != nil {
		t.Fatal(err)
	}
}
func fileStat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var s unix.Stat_t
	if err := unix.Lstat(path, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func checkNoStage(t *testing.T, x installFixture) {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(x.root, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Name() != helperName {
			t.Fatalf("orphan stage %s", e.Name())
		}
	}
}
func setTestRecord(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := unix.Setxattr(path, installXattr, b, 0); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFirstUpgradeStablePath(t *testing.T) {
	x := newInstallFixture(t)
	// A real synthetic client config is external to the install transaction.
	config := filepath.Join(x.home, "client.json")
	configBytes, _ := json.Marshal(map[string]any{"imports": []string{}, "mcpServers": map[string]any{"kownload": map[string]any{"command": x.target(), "args": []string{"--socket", "/private/synthetic/api.sock"}}}})
	os.WriteFile(config, configBytes, 0600)
	x.install(t)
	before := fileStat(t, x.target())
	oldBytes := fileBytes(t, x.target())
	if before.Mode&07777 != 0700 {
		t.Fatal("mode")
	}
	old, err := os.Open(x.target())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	x.replacePayload(t, []byte("known-payload-two"), strings.Repeat("2", 40))
	x.install(t)
	after := fileStat(t, x.target())
	if sameFile(&before, &after) {
		t.Fatal("upgrade reused old inode")
	}
	b := make([]byte, len(oldBytes))
	if _, err = old.Read(b); err != nil || !bytes.Equal(b, oldBytes) {
		t.Fatal("open old inode changed")
	}
	if !bytes.Equal(fileBytes(t, config), configBytes) {
		t.Fatal("client config changed")
	}
	f, err := os.Open(x.target())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	record, err := readAttr(int(f.Fd()), installXattr, maxInstallRecord)
	if err != nil {
		t.Fatal(err)
	}
	var got installRecord
	if decodeStrict(record, &got) != nil || got != x.m.record() {
		t.Fatal("record")
	}
	checkNoStage(t, x)
}

func TestInstallPrepublishFailuresPreserveOld(t *testing.T) {
	for _, failure := range []string{"bundle-signature", "helper-signature", "stage-signature", "version", "xattr-unavailable", "xattr-corrupt", "sync", "rename", "digest", "manifest"} {
		t.Run(failure, func(t *testing.T) {
			x := newInstallFixture(t)
			x.install(t)
			before := fileStat(t, x.target())
			beforeBytes := fileBytes(t, x.target())
			x.replacePayload(t, []byte("candidate-two"), strings.Repeat("2", 40))
			fail := errors.New("synthetic failure")
			switch failure {
			case "bundle-signature":
				x.ops.verify = func(_ string, b bool) error {
					if b {
						return fail
					}
					return nil
				}
			case "helper-signature":
				x.ops.verify = func(p string, _ bool) error {
					if p == filepath.Join(x.bundle, helperRelative) {
						return fail
					}
					return nil
				}
			case "stage-signature":
				x.ops.verify = func(p string, _ bool) error {
					if strings.Contains(p, ".kownload-mcp-") {
						return fail
					}
					return nil
				}
			case "version":
				x.ops.version = func(string, string) error { return fail }
			case "xattr-unavailable":
				x.ops.setRecord = func(int, []byte) error { return unix.ENOTSUP }
			case "xattr-corrupt":
				x.ops.setRecord = func(fd int, _ []byte) error { return unix.Fsetxattr(fd, installXattr, []byte("{}"), 0) }
			case "sync":
				x.ops.syncFile = func(*os.File) error { return fail }
			case "rename":
				x.ops.publish = func(int, string, bool) error { return fail }
			case "digest":
				os.WriteFile(filepath.Join(x.bundle, helperRelative), []byte("tampered"), 0700)
			case "manifest":
				x.m.HelperPath = "Contents/Helpers/other"
				x.writeManifest(t, x.m)
			}
			if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err == nil {
				t.Fatal("accepted")
			}
			after := fileStat(t, x.target())
			if !sameFile(&before, &after) || !bytes.Equal(beforeBytes, fileBytes(t, x.target())) {
				t.Fatal("old entry changed")
			}
			checkNoStage(t, x)
		})
	}
}
func TestInstallRejectsUnmanagedEntriesWithoutExecutingThem(t *testing.T) {
	for _, kind := range []string{"unmarked", "bad-digest", "unknown-version", "unknown-field", "oversize-record", "wrong-type", "link", "hardlink", "directory", "fifo", "mode"} {
		t.Run(kind, func(t *testing.T) {
			x := newInstallFixture(t)
			x.install(t)
			r := x.m.record()
			switch kind {
			case "unmarked":
				unix.Removexattr(x.target(), installXattr)
			case "bad-digest":
				os.WriteFile(x.target(), []byte("altered"), 0700)
			case "unknown-version":
				r.RecordVersion = 2
				b, _ := json.Marshal(r)
				setTestRecord(t, x.target(), b)
			case "unknown-field":
				b, _ := json.Marshal(r)
				setTestRecord(t, x.target(), append(b[:len(b)-1], []byte(`,"unknown":true}`)...))
			case "oversize-record":
				setTestRecord(t, x.target(), bytes.Repeat([]byte("x"), maxInstallRecord+1))
			case "wrong-type":
				setTestRecord(t, x.target(), []byte(`{"recordVersion":"1"}`))
			case "link":
				os.Rename(x.target(), x.target()+"-other")
				os.Symlink(x.target()+"-other", x.target())
			case "hardlink":
				os.Link(x.target(), x.target()+"-other")
			case "directory":
				os.Remove(x.target())
				os.Mkdir(x.target(), 0700)
			case "fifo":
				os.Remove(x.target())
				unix.Mkfifo(x.target(), 0700)
			case "mode":
				os.Chmod(x.target(), 0755)
			}
			before := fileStat(t, x.target())
			called := false
			x.ops.version = func(string, string) error { called = true; return nil }
			if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err == nil {
				t.Fatal("accepted")
			}
			after := fileStat(t, x.target())
			if !sameFile(&before, &after) || called {
				t.Fatal("changed or executed rejected entry")
			}
		})
	}
}
func TestInstallRejectsUnsafeDirectories(t *testing.T) {
	for _, kind := range []string{"other-root", "root-link", "root-writable", "home-link", "bin-link", "bin-public", "bundle-link", "helper-link", "manifest-link"} {
		t.Run(kind, func(t *testing.T) {
			x := newInstallFixture(t)
			switch kind {
			case "other-root":
				x.root = filepath.Join(x.home, "arbitrary")
				os.Mkdir(x.root, 0700)
			case "root-link":
				os.Rename(x.root, x.root+"-real")
				os.Symlink(x.root+"-real", x.root)
			case "root-writable":
				os.Chmod(x.root, 0770)
			case "home-link":
				os.Symlink(x.home, x.home+"-link")
				t.Cleanup(func() { os.Remove(x.home + "-link") })
				x.home += "-link"
				x.root = managedRoot(x.home)
			case "bin-link":
				p := filepath.Join(x.home, "outsider")
				os.Mkdir(p, 0700)
				os.Symlink(p, filepath.Join(x.root, "bin"))
			case "bin-public":
				os.Mkdir(filepath.Join(x.root, "bin"), 0755)
			case "bundle-link":
				os.Rename(x.bundle, x.bundle+"-real")
				os.Symlink(x.bundle+"-real", x.bundle)
			case "helper-link":
				p := filepath.Join(x.bundle, helperRelative)
				os.Rename(p, p+"-real")
				os.Symlink(p+"-real", p)
			case "manifest-link":
				p := filepath.Join(x.bundle, "Contents", "Resources", "build-manifest.json")
				os.Rename(p, p+"-real")
				os.Symlink(p+"-real", p)
			}
			if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Lstat(x.target()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created target")
			}
		})
	}
}
func TestInstallRejectsConcurrentAndReplacement(t *testing.T) {
	for _, kind := range []string{"busy", "target-replaced", "bin-replaced", "root-replaced", "stage-replaced"} {
		t.Run(kind, func(t *testing.T) {
			x := newInstallFixture(t)
			x.install(t)
			switch kind {
			case "busy":
				fd, err := openInstallDir(filepath.Join(x.root, "bin"))
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Close(fd)
				if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			default:
				x.ops.version = func(p, _ string) error {
					switch kind {
					case "target-replaced":
						os.Rename(x.target(), x.target()+".old")
						os.WriteFile(x.target(), []byte("foreign"), 0700)
					case "bin-replaced":
						os.Rename(filepath.Join(x.root, "bin"), filepath.Join(x.root, "old-bin"))
						os.Mkdir(filepath.Join(x.root, "bin"), 0700)
					case "root-replaced":
						os.Rename(x.root, x.root+".old")
						os.MkdirAll(filepath.Join(x.root, "bin"), 0700)
					case "stage-replaced":
						os.Remove(p)
						os.WriteFile(p, []byte("replacement"), 0700)
					}
					return nil
				}
			}
			if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err == nil {
				t.Fatal("accepted")
			}
			if kind == "target-replaced" && string(fileBytes(t, x.target())) != "foreign" {
				t.Fatal("overwrote foreign replacement")
			}
		})
	}
}
func TestInstallPreservesQuarantine(t *testing.T) {
	x := newInstallFixture(t)
	q := []byte("0081;00000000;Synthetic Fixture;")
	if err := unix.Setxattr(filepath.Join(x.bundle, helperRelative), quarantineXattr, q, 0); err != nil {
		t.Fatal(err)
	}
	x.install(t)
	f, err := os.Open(x.target())
	if err != nil {
		t.Fatal(err)
	}
	got, err := optionalQuarantine(int(f.Fd()))
	f.Close()
	if err != nil || !bytes.Equal(q, got) {
		t.Fatal("quarantine lost")
	}
	// A subsequent unquarantined source must not clear the installed quarantine.
	unix.Removexattr(filepath.Join(x.bundle, helperRelative), quarantineXattr)
	x.replacePayload(t, []byte("new"), strings.Repeat("2", 40))
	x.install(t)
	f, _ = os.Open(x.target())
	defer f.Close()
	got, err = optionalQuarantine(int(f.Fd()))
	if err != nil || !bytes.Equal(q, got) {
		t.Fatal("upgrade cleared quarantine")
	}
}
func TestInstallManifestClosedContract(t *testing.T) {
	x := newInstallFixture(t)
	for _, mutate := range []func(*helperManifest){
		func(m *helperManifest) { m.SourceCommit = "not-a-sha" }, func(m *helperManifest) { m.Architecture = "amd64" },
		func(m *helperManifest) { m.SDKVersion = "vNext" }, func(m *helperManifest) { m.ManifestVersion = 2 },
		func(m *helperManifest) { m.ReadAPIVersion = 2 }, func(m *helperManifest) { m.Version = "mismatch" },
		func(m *helperManifest) { m.SHA256 = strings.Repeat("X", 64) },
	} {
		m := x.m
		mutate(&m)
		x.writeManifest(t, m)
		if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err == nil {
			t.Fatal("accepted invalid manifest")
		}
	}
}

func TestInstallForeignOwnerPredicate(t *testing.T) {
	s := unix.Stat_t{Mode: unix.S_IFREG | 0700, Nlink: 1, Size: 12, Uid: uint32(os.Getuid())}
	if !validInstallFile(&s, true, 128) {
		t.Fatal("owned fixture rejected")
	}
	s.Uid++
	if validInstallFile(&s, true, 128) {
		t.Fatal("foreign owner accepted")
	}
}
func TestInstallDuplicateJSONRejected(t *testing.T) {
	for _, b := range []string{`{"recordVersion":1,"recordVersion":1}`, `{"mcp":{"sha256":"a","sha256":"b"}}`} {
		if rejectDuplicateKeys([]byte(b)) == nil {
			t.Fatal("duplicate accepted")
		}
	}
}

func TestInstallDoesNotExecuteReplacedStage(t *testing.T) {
	x := newInstallFixture(t)
	x.install(t)
	original := fileStat(t, x.target())
	set := x.ops.setRecord
	var replaced string
	x.ops.setRecord = func(fd int, b []byte) error {
		if err := set(fd, b); err != nil {
			return err
		}
		entries, err := os.ReadDir(filepath.Join(x.root, "bin"))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".kownload-mcp-") {
				replaced = filepath.Join(x.root, "bin", entry.Name())
				if err = os.Remove(replaced); err != nil {
					return err
				}
				return os.WriteFile(replaced, []byte("foreign-stage"), 0700)
			}
		}
		return errors.New("missing stage")
	}
	called := false
	x.ops.version = func(string, string) error { called = true; return nil }
	if _, err := installHelper(x.bundle, x.root, x.home, x.ops); err == nil || called {
		t.Fatal("replaced stage accepted/executed")
	}
	after := fileStat(t, x.target())
	if !sameFile(&original, &after) {
		t.Fatal("old inode changed")
	}
	if string(fileBytes(t, replaced)) != "foreign-stage" {
		t.Fatal("foreign stage removed/changed")
	}
}
