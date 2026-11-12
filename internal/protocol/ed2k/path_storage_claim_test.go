//go:build darwin && cgo

package ed2k

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/monkeyWie/goed2k"
)

func TestED2KLegacyPathClaimIsAtomicAndQuiesces(t *testing.T) {
	// No Start/bootstrap, sockets, DHT or remote source discovery.
	settings := goed2k.NewSettings()
	settings.EnableDHT, settings.EnableUPnP = false, false
	client := goed2k.NewClient(settings)
	defer client.Close()
	fm := &FetcherManager{client: client}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	req := &base.Request{URL: "ed2k://|file|frozen.bin|4|01010101010101010101010101010101|/"}
	f := &Fetcher{manager: fm, meta: &fetcher.FetcherMeta{}}
	if err := f.RequirePathFreeze("task"); err != nil {
		t.Fatal(err)
	}
	if err := f.Resolve(req, &base.Options{Path: root}); err != nil {
		t.Fatal(err)
	}
	layout, sizes, err := f.PathFreezeLayout()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := transfer.OpenVerifiedPolicyRoot("test-root", root)
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	lease, err := policy.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	output, binding, err := transfer.CreateVerifiedLayoutStorage(lease, layout, sizes, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := output.Activate(binding); err != nil {
		t.Fatal(err)
	}
	if err := f.BindPathFreeze(context.Background(), layout, output); err != nil {
		t.Fatal(err)
	}
	link, err := parseLink(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "legacy.bin")
	atp := goed2k.AddTransferParams{Hash: link.Hash, Size: 4, FilePath: legacyPath, Paused: true}
	checked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	legacyDone := make(chan error, 1)
	go func() {
		_, err := fm.startLegacyTransfer(client, atp, func() error {
			// The actual legacy Start prepares its directory here, after the
			// ownership check and before native Find/Add. Hold that exact gap.
			close(checked)
			<-release
			return os.MkdirAll(filepath.Dir(legacyPath), 0755)
		})
		legacyDone <- err
	}()
	<-checked
	// Deterministic assertion at the held gap, without scheduler sleeps: D6
	// registration cannot pass the legacy check until insertion completes.
	barrierHeld := !fm.mu.TryLock()
	if !barrierHeld {
		fm.mu.Unlock()
	}
	managedDone := make(chan error, 1)
	go func() { managedDone <- f.Start() }()
	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, done := range []<-chan error{legacyDone, managedDone} {
		select {
		case err := <-done:
			if done == legacyDone && err != nil || done == managedDone && !errors.Is(err, transfer.ErrLayoutStorageHeld) {
				t.Fatalf("claim result: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("claim did not finish")
		}
	}
	if !barrierHeld {
		t.Fatal("legacy ownership check/add gap was not protected")
	}
	legacy := client.FindTransfer(link.Hash)
	if !legacy.IsValid() || legacy.IsPaused() || legacy.GetFilePath() != legacyPath {
		t.Fatal("legacy winner was not preserved")
	}
	legacyFD := legacy.GetFile()
	if legacyFD == nil {
		t.Fatal("legacy handler is missing")
	}
	f.pathHandler.mu.Lock()
	submitted := f.pathHandler.submitted
	f.pathHandler.mu.Unlock()
	if submitted || f.handle.IsValid() || fm.managedPathHash(link.Hash) {
		t.Fatal("D6 adopted the legacy handle or its ownership")
	}
	if err := f.QuiescePathFreeze(ctx); err != nil {
		t.Fatalf("unsubmitted D6 Quiesce: %v", err)
	}
	select {
	case <-f.pathHandler.done:
	default:
		t.Fatal("D6 waited for an unrelated native Close")
	}
	if client.FindTransfer(link.Hash) != legacy || legacy.IsPaused() || legacy.GetFile() != legacyFD {
		t.Fatal("D6 Quiesce paused/removed/replaced the legacy winner")
	}
	if _, err := legacyFD.Stat(); err != nil {
		t.Fatalf("D6 Quiesce closed the legacy file: %v", err)
	}
	// Conversely, a registered D6 owner rejects legacy before directory/file IO.
	if err := client.RemoveTransfer(link.Hash, false); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterPathFreeze("task", req); err != nil {
		t.Fatal(err)
	}
	prepared := false
	if _, err := fm.startLegacyTransfer(client, atp, func() error { prepared = true; return nil }); !errors.Is(err, transfer.ErrLayoutStorageHeld) || prepared {
		t.Fatal("registered D6 owner allowed legacy preparation/add")
	}
}
