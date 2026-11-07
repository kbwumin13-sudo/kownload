package bt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const offlineMagnet = "magnet:?xt=urn:btih:8a55cfbd5ca5d11507364765936c4f9e55b253ed"

// No peer discovery or public listener is used by these metadata lifecycle tests.
func offlineClient(t *testing.T) {
	t.Helper()
	lock.Lock()
	defer lock.Unlock()
	_ = closeClientLocked()
	cfg = torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.DisableUTP = true
	cfg.DisableWebtorrent = true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	var err error
	client, err = torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeClient() })
}

func waitTorrentRefs(t *testing.T, count int) *torrent.Torrent {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		lock.Lock()
		for tr, ref := range torrentRefs {
			if ref.count == count {
				lock.Unlock()
				return tr
			}
		}
		lock.Unlock()
		select {
		case <-deadline.C:
			t.Fatalf("torrent reference count never reached %d", count)
		case <-tick.C:
		}
	}
}

func waitResolve(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("metadata resolution did not exit")
		return nil
	}
}

func assertNoTorrentClient(t *testing.T) {
	t.Helper()
	lock.Lock()
	defer lock.Unlock()
	if client != nil || len(torrentRefs) != 0 {
		t.Fatal("failed resolve retained a torrent/client")
	}
}

func TestMagnetResolveDeadlineAndRetry(t *testing.T) {
	offlineClient(t)
	f := buildFetcher().(*Fetcher)
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := f.ResolveContext(ctx, &base.Request{URL: offlineMagnet}, nil); !errors.Is(err, context.DeadlineExceeded) || err.Error() != "获取种子信息超时，请检查网络或稍后重试" {
		t.Fatalf("got %v, want a localized deadline error", err)
	}
	if f.torrentReady.Load() {
		t.Fatal("timed out torrent was marked ready")
	}
	assertNoTorrentClient(t)
	// Reusing this fetcher must work after the failed acquisition was released.
	offlineClient(t)
	if err := f.Resolve(&base.Request{URL: "testdata/ubuntu-22.04-live-server-amd64.iso.torrent"}, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if f.Meta().Res.Hash != "8a55cfbd5ca5d11507364765936c4f9e55b253ed" || !f.torrentReady.Load() {
		t.Fatal("retry lost normal torrent metadata")
	}
	knownTorrent := f.torrent
	if err := f.ResolveContext(ctxForKnownInfo(t), &base.Request{URL: offlineMagnet}, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatalf("repeat resolution lost known metadata: %v", err)
	}
	if f.torrent != knownTorrent {
		t.Fatal("repeat resolution replaced the known torrent")
	}
}

func ctxForKnownInfo(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestMagnetResolveCancellation(t *testing.T) {
	for _, action := range []string{"request", "pause", "fetcher", "manager"} {
		t.Run(action, func(t *testing.T) {
			offlineClient(t)
			f := buildFetcher().(*Fetcher)
			defer f.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- f.ResolveContext(ctx, &base.Request{URL: offlineMagnet}, nil) }()
			waitTorrentRefs(t, 1)
			switch action {
			case "request":
				cancel()
			case "pause":
				f.CancelResolve()
			case "fetcher":
				_ = f.Close()
			case "manager":
				_ = closeClient()
			}
			err := waitResolve(t, done)
			if err == nil {
				t.Fatal("canceled resolve succeeded")
			}
			if action != "manager" && !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want canceled", err)
			}
			if action != "manager" && err.Error() != "已取消获取种子信息" {
				t.Fatalf("missing cancellation message: %v", err)
			}
			_ = f.Close()
			_ = f.Close() // Closing an unsuccessful fetcher must be idempotent and nonblocking.
			assertNoTorrentClient(t)
		})
	}
}

func TestMagnetTimeoutPreservesSharedDownload(t *testing.T) {
	offlineClient(t)
	first := buildFetcher().(*Fetcher)
	second := buildFetcher().(*Fetcher)
	defer first.Close()
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.ResolveContext(ctx, &base.Request{URL: offlineMagnet}, nil) }()
	tr := waitTorrentRefs(t, 1)
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer secondCancel()
	secondDone := make(chan error, 1)
	secondOpts := &base.Options{Path: t.TempDir()}
	go func() { secondDone <- second.ResolveContext(secondCtx, &base.Request{URL: offlineMagnet}, secondOpts) }()
	if got := waitTorrentRefs(t, 2); got != tr {
		t.Fatal("same hash was not shared")
	}
	if err := waitResolve(t, firstDone); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	select {
	case <-tr.Closed():
		t.Fatal("timeout dropped the other resolver's torrent")
	default:
	}
	mi, err := metainfo.LoadFromFile("testdata/ubuntu-22.04-live-server-amd64.iso.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SetInfoBytes(mi.InfoBytes); err != nil {
		t.Fatal(err)
	}
	if err := waitResolve(t, secondDone); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	third := buildFetcher().(*Fetcher)
	if err := third.Resolve(&base.Request{URL: offlineMagnet}, nil); err != nil {
		t.Fatal(err)
	}
	_ = third.Close()
	_ = first.Close()
	select {
	case <-tr.Closed():
		t.Fatal("failed fetcher Close dropped the shared download")
	default:
	}
	if !second.torrentReady.Load() || second.Meta().Res.Size != 1466714112 {
		t.Fatal("shared download lost metadata")
	}
	_ = second.Close()
	select {
	case <-tr.Closed():
	default:
		t.Fatal("last owner did not drop its torrent")
	}
	assertNoTorrentClient(t)
}

func TestMagnetPrepareCancellationPreservesRestoredResource(t *testing.T) {
	offlineClient(t)
	f := buildFetcher().(*Fetcher)
	defer f.Close()
	resource := &base.Resource{Size: 1, Files: []*base.FileInfo{{Name: "kept", Size: 1}}}
	f.meta.Res = resource
	options := &base.Options{Path: t.TempDir(), SelectFiles: []int{0}}
	done := make(chan error, 1)
	go func() { done <- f.PrepareContext(context.Background(), &base.Request{URL: offlineMagnet}, options) }()
	waitTorrentRefs(t, 1)
	f.CancelResolve()
	if err := waitResolve(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled preparation", err)
	}
	if f.Meta().Res != resource || len(f.Meta().Opts.SelectFiles) != 1 || f.Meta().Opts.SelectFiles[0] != 0 {
		t.Fatal("preparation replaced persisted metadata or file selection")
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	assertNoTorrentClient(t)
}
