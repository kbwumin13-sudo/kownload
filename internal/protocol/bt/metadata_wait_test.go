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

func setTestInfo(t *testing.T, tr *torrent.Torrent) {
	t.Helper()
	mi, err := metainfo.LoadFromFile("testdata/ubuntu-22.04-live-server-amd64.iso.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SetInfoBytes(mi.InfoBytes); err != nil {
		t.Fatal(err)
	}
}

func returnsPromptly(t *testing.T, name string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatalf("%s waited for metadata", name)
		return nil
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInteractiveResolveKeepsDeadline(t *testing.T) {
	UseLoopbackClients(t, 20*time.Millisecond)
	f := buildFetcher().(*Fetcher)
	defer f.Close()
	err := f.ResolveContext(context.Background(), &base.Request{URL: offlineMagnet}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "获取种子信息超时，请检查网络或稍后重试" {
		t.Fatalf("got %v, want the localized resolve deadline", err)
	}
	assertNoTorrentClient(t)
}

// A created task, new or restored, keeps waiting for metadata past the
// interactive deadline, holds on to its torrent meanwhile and leaves the
// operation lock free.
func TestTaskMetadataOutlivesResolveDeadline(t *testing.T) {
	for _, path := range []string{"resolve", "prepare"} {
		t.Run(path, func(t *testing.T) {
			UseLoopbackClients(t, 20*time.Millisecond)
			f := buildFetcher().(*Fetcher)
			defer f.Close()
			persisted := &base.Resource{Size: 1466714112, Files: []*base.FileInfo{{Name: "kept", Size: 1466714112}}}
			opts := &base.Options{Path: t.TempDir()}
			req := &base.Request{URL: offlineMagnet}
			if path == "prepare" {
				f.meta.Res = persisted
				opts.SelectFiles = []int{0}
			}
			done := make(chan error, 1)
			go func() {
				if path == "resolve" {
					done <- f.ResolveTask(context.Background(), req, opts)
				} else {
					done <- f.PrepareContext(context.Background(), req, opts)
				}
			}()
			tr := waitTorrentRefs(t, 1)
			select {
			case err := <-done:
				t.Fatalf("created task stopped waiting for metadata: %v", err)
			case <-time.After(10 * resolveTimeout):
			}
			select {
			case <-tr.Closed():
				t.Fatal("waiting task dropped its torrent and the peers found so far")
			default:
			}
			if err := returnsPromptly(t, "Pause", f.Pause); err != nil {
				t.Fatal(err)
			}
			if err := returnsPromptly(t, "Start", f.Start); !errors.Is(err, errMetadataNotReady) {
				t.Fatalf("Start before metadata got %v, want errMetadataNotReady", err)
			}
			setTestInfo(t, tr)
			if err := waitResolve(t, done); err != nil {
				t.Fatal(err)
			}
			if !f.torrentReady.Load() {
				t.Fatal("metadata arrived but the fetcher is not ready")
			}
			if path == "resolve" && (f.Meta().Res == nil || f.Meta().Res.Hash != "8a55cfbd5ca5d11507364765936c4f9e55b253ed") {
				t.Fatalf("resolved resource = %+v", f.Meta().Res)
			}
			if path == "prepare" && f.Meta().Res != persisted {
				t.Fatal("preparation replaced the persisted resource")
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Seeding a finished magnet task fetches its metadata in the background: the
// downloader calls Upload while holding the task's locks.
func TestUploadWaitsForMetadataInBackground(t *testing.T) {
	for _, end := range []string{"metadata", "delete"} {
		t.Run(end, func(t *testing.T) {
			UseLoopbackClients(t, 20*time.Millisecond)
			f := buildFetcher().(*Fetcher)
			defer f.Close()
			f.meta.Req = &base.Request{URL: offlineMagnet}
			f.meta.Opts = &base.Options{Path: t.TempDir(), SelectFiles: []int{0}}
			if err := returnsPromptly(t, "Upload", f.Upload); err != nil {
				t.Fatal(err)
			}
			tr := waitTorrentRefs(t, 1)
			time.Sleep(10 * resolveTimeout)
			if f.torrentReady.Load() || f.torrentUpload.Load() {
				t.Fatal("seeding started without metadata")
			}
			select {
			case <-tr.Closed():
				t.Fatal("seeding gave up waiting for metadata")
			default:
			}
			if end == "metadata" {
				setTestInfo(t, tr)
				waitUntil(t, "seeding starts", f.torrentUpload.Load)
			}
			if err := returnsPromptly(t, "Close", f.Close); err != nil {
				t.Fatal(err)
			}
			if err := returnsPromptly(t, "WaitUpload", f.WaitUpload); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the torrent is released", func() bool {
				lock.Lock()
				defer lock.Unlock()
				return client == nil && len(torrentRefs) == 0
			})
		})
	}
}
