//go:build darwin && cgo

package mcpauthority_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpauthority"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
	"golang.org/x/sys/unix"
)

func verifiedAuthorityFixture(t *testing.T) (map[string]any, string, string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(parent, &fs); err != nil {
		t.Fatal(err)
	}
	var name []byte
	for _, b := range fs.Fstypename {
		if b == 0 {
			break
		}
		name = append(name, byte(b))
	}
	if string(name) != "apfs" {
		t.Fatal("fixture requires real APFS")
	}
	a, z := filepath.Join(parent, "a"), filepath.Join(parent, "z")
	for _, path := range []string{a, z} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	v := fixture()
	v["outputRoots"] = map[string]any{"a": a, "z": z}
	v["datasets"] = []any{
		map[string]any{"id": dataset, "taskIds": []string{"task_a"}, "outputRoots": []string{"a"}},
		map[string]any{"id": otherDataset, "taskIds": []string{"task_z"}, "outputRoots": []string{"z"}},
	}
	return v, a, z
}

// Count only descriptors referring to this test's known root objects. No file
// contents or other process descriptors are read, and unrelated runtime FDs do
// not make the leak assertion depend on Go's lazy initialization.
func verifiedAuthorityFDs(t *testing.T, paths ...string) int {
	t.Helper()
	identities := map[[2]uint64]bool{}
	for _, path := range paths {
		var s unix.Stat_t
		if err := unix.Stat(path, &s); err != nil {
			t.Fatal(err)
		}
		identities[[2]uint64{uint64(s.Dev), s.Ino}] = true
	}
	fdDir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	// ReadDir may lstat entries with unknown d_type; fdescfs can list a
	// descriptor that is already closed, making that lstat fail with EBADF.
	// Only names are needed: Fstat below checks currently open descriptors.
	names, readErr := fdDir.Readdirnames(-1)
	if err := errors.Join(readErr, fdDir.Close()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var s unix.Stat_t
		if unix.Fstat(fd, &s) == nil && identities[[2]uint64{uint64(s.Dev), s.Ino}] {
			count++
		}
	}
	return count
}

func TestVerifiedAuthorityFDAccounting(t *testing.T) {
	_, a, _ := verifiedAuthorityFixture(t)
	check := func(want int) {
		t.Helper()
		if got := verifiedAuthorityFDs(t, a); got != want {
			t.Fatalf("root descriptors = %d, want %d", got, want)
		}
	}
	check(0)
	root, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	check(1)
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	dup := os.NewFile(uintptr(fd), "fixture-root-dup")
	defer dup.Close()
	check(2)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err := dup.Close(); err != nil {
		t.Fatal(err)
	}
	check(0)
}

func openVerifiedAuthority(t *testing.T, p *mcpwritepolicy.Policy) *mcpauthority.VerifiedAuthority {
	t.Helper()
	v, err := mcpauthority.OpenVerifiedAuthority(p, dataset, networkID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	return v
}

func TestVerifiedAuthorityScopeAndIdentity(t *testing.T) {
	input, a, z := verifiedAuthorityFixture(t)
	p := parse(t, input)
	v := openVerifiedAuthority(t, p)
	id := v.PolicyID()
	var bs []mcpauthority.VerifiedRootBinding
	for _, pair := range [][2]string{{"a", a}, {"z", z}} {
		var s unix.Stat_t
		if err := unix.Stat(pair[1], &s); err != nil {
			t.Fatal(err)
		}
		bs = append(bs, mcpauthority.VerifiedRootBinding{RootID: pair[0], Path: pair[1], Identity: transfer.CompletionFileIdentity{Device: uint64(s.Dev), Inode: s.Ino}})
	}
	pure := mustNew(t, p, dataset, networkID, bs)
	if pure.PolicyID() != id {
		t.Fatal("composition changed original P")
	}
	if verifiedAuthorityFDs(t, a, z) != 2 {
		t.Fatal("all explicit roots must be pinned")
	}
	for _, test := range []struct{ name, p, d, root string }{
		{"wrong_P", "wrong", dataset, "a"}, {"wrong_D", id, otherDataset, "a"},
		{"other_scope", id, dataset, "z"}, {"unknown_root", id, dataset, "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, lease, err := v.BorrowCreate(test.p, test.d, test.root)
			if !errors.Is(err, mcpauthority.ErrVerifiedAuthorityDenied) || lease != nil || g.OutputRoot != "" || len(g.Effects) != 0 {
				t.Fatal("denial returned grant/lease")
			}
			if verifiedAuthorityFDs(t, a, z) != 2 {
				t.Fatal("denied borrow leaked fd")
			}
		})
	}
	if !v.AllowTask(id, dataset, "pause", "task_a") || !v.AllowTask(id, dataset, "resume", "task_a") || v.AllowTask(id, dataset, "pause", "task_z") || v.AllowTask(id, otherDataset, "pause", "task_a") || v.AllowTask("wrong", dataset, "pause", "task_a") || v.AllowTask(id, dataset, "create", "task_a") {
		t.Fatal("task scope changed")
	}
	// Owned caller snapshots/input maps and returned grants cannot alter authority.
	snap, err := p.SnapshotV1()
	if err != nil {
		t.Fatal(err)
	}
	snap.Roots[0].Path = "untrusted"
	snap.Effects[0] = "untrusted"
	input["operations"] = map[string]any{}
	g, lease, err := v.BorrowCreate(id, dataset, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if g.OutputRoot != a || len(g.Effects) != 2 {
		t.Fatal("wrong owned grant")
	}
	g.Effects[0] = "untrusted"
	g2, l2, err := v.BorrowCreate(id, dataset, "a")
	if err != nil {
		t.Fatal(err)
	}
	if g2.Effects[0] == "untrusted" || v.PolicyID() != id {
		t.Fatal("grant aliases authority")
	}
	if err = l2.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lease.Check(); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = lease.WithDirectory(func(d *transfer.CompletionDirectory) error {
		called = true
		if d.Identity() != bs[0].Identity {
			t.Error("lease changed object")
		}
		return nil
	}); err != nil || !called {
		t.Fatal("checked callback failed", err)
	}
}

func TestVerifiedAuthorityDisabledScopes(t *testing.T) {
	for _, kind := range []string{"create_disabled", "empty_roots", "empty_tasks"} {
		t.Run(kind, func(t *testing.T) {
			input, a, z := verifiedAuthorityFixture(t)
			scope := input["datasets"].([]any)[0].(map[string]any)
			switch kind {
			case "create_disabled":
				input["operations"].(map[string]any)["create"] = false
			case "empty_roots":
				scope["outputRoots"] = []string{}
			case "empty_tasks":
				scope["taskIds"] = []string{}
			}
			v := openVerifiedAuthority(t, parse(t, input))
			if kind == "empty_tasks" {
				if v.AllowTask(v.PolicyID(), dataset, "pause", "task_a") || v.AllowTask(v.PolicyID(), dataset, "resume", "task_a") {
					t.Fatal("empty task scope expanded")
				}
			} else {
				_, l, err := v.BorrowCreate(v.PolicyID(), dataset, "a")
				if l != nil {
					_ = l.Close()
				}
				if !errors.Is(err, mcpauthority.ErrVerifiedAuthorityDenied) || l != nil {
					t.Fatal("disabled create yielded lease")
				}
			}
			if verifiedAuthorityFDs(t, a, z) != 2 {
				t.Fatal("registry pin or denied-borrow accounting changed")
			}
		})
	}
}

func TestVerifiedAuthorityConstructorCleanup(t *testing.T) {
	for _, kind := range []string{"missing_ungranted_root", "unsafe_ungranted_root", "linked_ungranted_root", "invalid_dataset", "invalid_network", "nil_policy"} {
		t.Run(kind, func(t *testing.T) {
			input, a, z := verifiedAuthorityFixture(t)
			d, n := dataset, networkID
			switch kind {
			case "missing_ungranted_root":
				input["outputRoots"].(map[string]any)["z"] = z + "-missing"
			case "unsafe_ungranted_root":
				if err := os.Chmod(z, 0770); err != nil {
					t.Fatal(err)
				}
			case "linked_ungranted_root":
				link := z + "-link"
				if err := os.Symlink(z, link); err != nil {
					t.Fatal(err)
				}
				input["outputRoots"].(map[string]any)["z"] = link
			case "invalid_dataset":
				d = "wrong"
			case "invalid_network":
				n = "wrong"
			}
			p := parse(t, input)
			if kind == "nil_policy" {
				p = nil
			}
			v, err := mcpauthority.OpenVerifiedAuthority(p, d, n)
			if err == nil || v != nil {
				if v != nil {
					_ = v.Close()
				}
				t.Fatal("invalid construction accepted")
			}
			if verifiedAuthorityFDs(t, a, z) != 0 {
				t.Fatal("constructor leaked previously opened root")
			}
		})
	}
}

func TestVerifiedAuthorityRootRevalidation(t *testing.T) {
	for _, kind := range []string{"mode", "replacement", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			input, a, _ := verifiedAuthorityFixture(t)
			v := openVerifiedAuthority(t, parse(t, input))
			id := v.PolicyID()
			switch kind {
			case "mode":
				if err := os.Chmod(a, 0770); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Rename(a, a+"-pinned"); err != nil {
					t.Fatal(err)
				}
				if kind == "replacement" {
					if err := os.Mkdir(a, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(a+"-pinned", a); err != nil {
						t.Fatal(err)
					}
				}
			}
			g, lease, err := v.BorrowCreate(id, dataset, "a")
			if err == nil || lease != nil || g.OutputRoot != "" {
				if lease != nil {
					_ = lease.Close()
				}
				t.Fatal("unsafe/changed root yielded grant")
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifiedAuthorityCloseLifecycle(t *testing.T) {
	var nilValue *mcpauthority.VerifiedAuthority
	zero := &mcpauthority.VerifiedAuthority{}
	for _, v := range []*mcpauthority.VerifiedAuthority{nilValue, zero} {
		if v.PolicyID() != "" || v.AllowTask("", dataset, "pause", "task_a") {
			t.Fatal("zero grants")
		}
		if _, l, err := v.BorrowCreate("", dataset, "a"); err == nil || l != nil {
			t.Fatal("zero lease")
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
	}
	input, a, z := verifiedAuthorityFixture(t)
	v := openVerifiedAuthority(t, parse(t, input))
	id := v.PolicyID()
	_, lease, err := v.BorrowCreate(id, dataset, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = v.Close(); !errors.Is(err, transfer.ErrVerifiedRootBusy) {
		t.Fatal("live lease must report busy", err)
	}
	if v.AllowTask(id, dataset, "pause", "task_a") {
		t.Fatal("closing still grants")
	}
	if _, l, err := v.BorrowCreate(id, dataset, "a"); !errors.Is(err, transfer.ErrVerifiedRootClosed) || l != nil {
		t.Fatal("closing still borrows")
	}
	if err = lease.Check(); err != nil {
		t.Fatal("busy close invalidated existing lease", err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	if v.PolicyID() != id || verifiedAuthorityFDs(t, a, z) != 0 {
		t.Fatal("close changed metadata or leaked pins")
	}
}

func TestVerifiedAuthorityBorrowCloseRace(t *testing.T) {
	input, a, z := verifiedAuthorityFixture(t)
	p := parse(t, input)
	for round := 0; round < 12; round++ {
		v := openVerifiedAuthority(t, p)
		id := v.PolicyID()
		start := make(chan struct{})
		errs := make(chan error, 8)
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, l, err := v.BorrowCreate(id, dataset, "a")
				if err != nil {
					if !errors.Is(err, transfer.ErrVerifiedRootClosed) {
						errs <- err
					}
					return
				}
				if err = l.Close(); err != nil {
					errs <- err
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := v.Close(); err != nil && !errors.Is(err, transfer.ErrVerifiedRootBusy) {
				errs <- err
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if v.AllowTask(id, dataset, "resume", "task_a") || verifiedAuthorityFDs(t, a, z) != 0 {
			t.Fatal("race leaked fd or grant")
		}
	}
}
