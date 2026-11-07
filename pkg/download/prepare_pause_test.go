package download

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type restorePrepareFetcher struct {
	fetcher.Fetcher
	meta          *fetcher.FetcherMeta
	entered       chan error
	paused, abort chan struct{}
	done          chan error
	pauseOnce     sync.Once
	started       atomic.Bool
}

func (f *restorePrepareFetcher) Setup(*controller.Controller) {}
func (f *restorePrepareFetcher) Meta() *fetcher.FetcherMeta   { return f.meta }
func (f *restorePrepareFetcher) Progress() fetcher.Progress   { return fetcher.Progress{0} }
func (f *restorePrepareFetcher) PrepareContext(ctx context.Context, _ *base.Request, _ *base.Options) error {
	f.entered <- ctx.Err()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.abort:
		return context.Canceled
	}
}

// Deliberately no sticky cancel flag: cancellation must be carried by the
// context of this run, even when Pause precedes entry into PrepareContext.
func (f *restorePrepareFetcher) CancelResolve() {}
func (f *restorePrepareFetcher) Start() error   { f.started.Store(true); return nil }
func (f *restorePrepareFetcher) Pause() error {
	f.pauseOnce.Do(func() { f.done <- context.Canceled; close(f.paused) })
	return nil
}
func (f *restorePrepareFetcher) Wait() error { return <-f.done }

type restorePrepareManager struct {
	fetcher.FetcherManager
	f *restorePrepareFetcher
}

func (m *restorePrepareManager) Name() string                       { return "bt" }
func (m *restorePrepareManager) DefaultConfig() any                 { return map[string]any{} }
func (m *restorePrepareManager) AutoRename() bool                   { return false }
func (m *restorePrepareManager) Close() error                       { return nil }
func (m *restorePrepareManager) Store(fetcher.Fetcher) (any, error) { return nil, nil }
func (m *restorePrepareManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	return nil, func(meta *fetcher.FetcherMeta, _ any) fetcher.Fetcher { m.f.meta = meta; return m.f }
}

func restoredPreparationFixture(t *testing.T) (*Downloader, *Task, *restorePrepareFetcher) {
	t.Helper()
	f := &restorePrepareFetcher{entered: make(chan error, 1), paused: make(chan struct{}), abort: make(chan struct{}), done: make(chan error, 1)}
	manager := &restorePrepareManager{f: f}
	d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{manager}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(f.abort); _ = d.Close() })
	task := snapshotTestTask()
	task.Protocol = "bt"
	task.Meta.Req = &base.Request{URL: "http://localhost/restored", Protocol: "bt"}
	task.fetcherManager = manager
	task.fetcher = nil
	task.updateStatus(base.DownloadStatusPause)
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	return d, task, f
}

func pauseRestoredPreparation(t *testing.T, d *Downloader, task *Task) {
	t.Helper()
	paused := make(chan error, 1)
	go func() { paused <- d.Pause(&TaskFilter{IDs: []string{task.ID}}) }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pause blocked on metadata preparation")
	}
}
func assertPreparationPaused(t *testing.T, d *Downloader, task *Task, f *restorePrepareFetcher) {
	t.Helper()
	select {
	case <-f.paused:
	case <-time.After(time.Second):
		t.Fatal("Pause did not cancel preparation and finish its protocol pause")
	}
	task.statusLock.Lock()
	pauseDone := task.pauseDone
	task.statusLock.Unlock()
	select {
	case <-pauseDone:
	case <-time.After(time.Second):
		t.Fatal("pause persistence did not complete")
	}
	if f.started.Load() || d.GetTask(task.ID).Status != base.DownloadStatusPause {
		t.Fatal("canceled preparation started or failed the restored task")
	}
}

func TestRestoredMetadataPreparationCanBePaused(t *testing.T) {
	d, task, f := restoredPreparationFixture(t)
	if err := d.ContinueBatch(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.entered:
		if err != nil {
			t.Fatalf("new run was already canceled: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restored task skipped metadata preparation")
	}
	pauseRestoredPreparation(t, d, task)
	assertPreparationPaused(t, d, task, f)
}

func TestRestoredMetadataPauseBeforePreparation(t *testing.T) {
	d, task, f := restoredPreparationFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	// Release a failed assertion's gate before the fixture closes the downloader.
	t.Cleanup(gate.Close)
	t.Cleanup(unblock)
	dir := t.TempDir()
	manifest := `{"name":"prepare-gate","title":"gate","version":"1","scripts":[{"event":"onStart","match":{"urls":["*://localhost/*"]},"entry":"index.js"}]}`
	gateURL, _ := json.Marshal(gate.URL)
	script := `gopeed.events.onStart(function(ctx) { var xhr = new XMLHttpRequest(); xhr.open("GET", ` + string(gateURL) + `); xhr.send(); });`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InstallExtensionByFolder(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := d.ContinueBatch(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("onStart gate did not start")
	}
	pauseRestoredPreparation(t, d, task)
	unblock()
	select {
	case err := <-f.entered:
		if err != context.Canceled {
			t.Fatalf("PrepareContext started with %v after Pause, want canceled context", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("preparation did not run after releasing onStart")
	}
	assertPreparationPaused(t, d, task, f)
}
