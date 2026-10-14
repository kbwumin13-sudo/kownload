package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func snapshotTestTask() *Task {
	task := NewTask()
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://localhost/file"}, Opts: &base.Options{Name: "file"}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "file", Size: 1}}, Size: 1}}
	task.Progress = &Progress{}
	task.IsCreated = true
	initTask(task)
	return task
}

func TestTaskSnapshotsDuringStatusUpdates(t *testing.T) {
	task := snapshotTestTask()
	d := &Downloader{lock: &sync.Mutex{}, tasks: []*Task{task}}
	first := d.GetTask(task.ID)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			task.statusLock.Lock()
			task.updateStatus(base.DownloadStatusRunning)
			task.dataLock.Lock()
			task.Progress.Downloaded = int64(i)
			task.dataLock.Unlock()
			// Persistence while holding statusLock must not recursively lock it.
			_, _ = json.Marshal(task)
			task.statusLock.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		snapshot := d.GetTask(task.ID)
		if snapshot.Name() != "file" {
			t.Fatal("lost task name")
		}
		if _, err := json.Marshal(snapshot); err != nil {
			t.Fatal(err)
		}
		_ = d.GetTasksByFilter(&TaskFilter{Statuses: []base.Status{base.DownloadStatusRunning}})
		tasks := d.GetTasks()
		tasks[0].Meta.Opts.Name = "client edit"
	}
	wg.Wait()
	if first.Status != base.DownloadStatusReady || first.Progress.Downloaded != 0 || task.Name() != "file" {
		t.Fatal("public task snapshot aliases live data")
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "protocol", "meta", "status", "uploading", "progress", "isCreated", "createdAt", "updatedAt", "name"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing serialized field %s", key)
		}
	}
	if len(fields) != 10 {
		t.Fatalf("changed task shape: %s", raw)
	}
}

type patchGateFetcher struct {
	fetcher.Fetcher
	meta    *fetcher.FetcherMeta
	entered chan struct{}
	release chan struct{}
	started chan string
	done    chan error
}

func (f *patchGateFetcher) Setup(*controller.Controller) {}
func (f *patchGateFetcher) Meta() *fetcher.FetcherMeta   { return f.meta }
func (f *patchGateFetcher) Patch(req *base.Request, opts *base.Options) error {
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	if req != nil {
		f.meta.Req = req
	}
	if opts != nil {
		f.meta.Opts = opts
	}
	return nil
}
func (f *patchGateFetcher) Start() error               { f.started <- f.meta.Req.URL; return nil }
func (f *patchGateFetcher) Wait() error                { return <-f.done }
func (f *patchGateFetcher) Progress() fetcher.Progress { return fetcher.Progress{0} }
func (f *patchGateFetcher) Pause() error               { return nil }

type patchGateManager struct{ httpProtocol.FetcherManager }

func (*patchGateManager) Name() string                       { return "media" }
func (*patchGateManager) AutoRename() bool                   { return false }
func (*patchGateManager) Store(fetcher.Fetcher) (any, error) { return nil, nil }

func TestMediaPatchLifecycleGuard(t *testing.T) {
	manager := &patchGateManager{}
	d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{manager}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task := snapshotTestTask()
	task.Protocol = "media"
	task.fetcherManager = manager
	f := &patchGateFetcher{meta: task.Meta, started: make(chan string, 1), done: make(chan error, 1)}
	task.fetcher = f
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	for _, status := range []base.Status{base.DownloadStatusReady, base.DownloadStatusWait, base.DownloadStatusRunning, base.DownloadStatusDone} {
		task.updateStatus(status)
		if err := d.Patch(task.ID, &base.Request{URL: "rejected"}, nil); err == nil {
			t.Fatalf("Patch accepted %s", status)
		}
	}
	for _, status := range []base.Status{base.DownloadStatusPause, base.DownloadStatusError} {
		task.updateStatus(status)
		if err := d.Patch(task.ID, &base.Request{URL: "allowed"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	task.updateStatus(base.DownloadStatusPause)
	f.entered, f.release = make(chan struct{}), make(chan struct{})
	patched := make(chan error, 1)
	go func() {
		patched <- d.Patch(task.ID, &base.Request{URL: "patched-before-start"}, &base.Options{Name: "patched"})
	}()
	<-f.entered
	continued := make(chan error, 1)
	go func() { continued <- d.ContinueBatch(&TaskFilter{IDs: []string{task.ID}}) }()
	select {
	case err := <-continued:
		t.Fatalf("Continue passed an unfinished patch: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(f.release)
	if err := <-patched; err != nil {
		t.Fatal(err)
	}
	if err := <-continued; err != nil {
		t.Fatal(err)
	}
	select {
	case url := <-f.started:
		if url != "patched-before-start" {
			t.Fatalf("Start used %q", url)
		}
	case <-time.After(time.Second):
		t.Fatal("Start deadlocked after Patch")
	}
	if err := d.Patch(task.ID, nil, &base.Options{Name: "running"}); err == nil {
		t.Fatal("running options patch accepted")
	}
	if err := d.Pause(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	f.done <- errors.New("test finished")
}

// Exercise Setup's background loops, concurrent appends, live progress and JSON
// consumers through the public API, then verify the downloaded bytes.
func TestDownloaderConcurrentHTTPTaskSnapshots(t *testing.T) {
	payload := bytes.Repeat([]byte("concurrent-download"), 32768)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "source.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	dir := t.TempDir()
	d := NewDownloader(&DownloaderConfig{StorageDir: dir, RefreshInterval: 1, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const count = 8
	results := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.CreateDirect(&base.Request{URL: server.URL + "/source.bin"}, &base.Options{Path: dir, Name: fmt.Sprintf("file-%d.bin", i)})
			results <- err
		}(i)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		tasks := d.GetTasks()
		completed := 0
		for _, task := range tasks {
			if _, err := json.Marshal(task); err != nil {
				t.Fatal(err)
			}
			if task.Status == base.DownloadStatusError {
				t.Fatalf("download failed: %s", task.ID)
			}
			if task.Status == base.DownloadStatusDone {
				completed++
			}
		}
		if completed == count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d downloads completed", completed, count)
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("file-%d.bin", i)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, payload) {
			t.Fatalf("file %d content mismatch", i)
		}
	}
}

type pauseCloseFetcher struct {
	*patchGateFetcher
	entered chan struct{}
	release chan struct{}
}

func (f *pauseCloseFetcher) Pause() error { close(f.entered); <-f.release; return nil }

type pauseCloseStorage struct {
	*MemStorage
	closed          atomic.Bool
	wroteAfterClose atomic.Bool
}

func (s *pauseCloseStorage) Close() error { s.closed.Store(true); return nil }
func (s *pauseCloseStorage) Put(bucket, key string, value any) error {
	if s.closed.Load() {
		s.wroteAfterClose.Store(true)
	}
	return s.MemStorage.Put(bucket, key, value)
}

func TestCloseWaitsForPendingPausePersistence(t *testing.T) {
	storage := &pauseCloseStorage{MemStorage: NewMemStorage()}
	manager := &patchGateManager{}
	d := NewDownloader(&DownloaderConfig{Storage: storage, StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{manager}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	task := snapshotTestTask()
	task.Protocol = "http" // HTTP pauses are asynchronous.
	task.fetcherManager = manager
	f := &pauseCloseFetcher{patchGateFetcher: &patchGateFetcher{meta: task.Meta}, entered: make(chan struct{}), release: make(chan struct{})}
	task.fetcher = f
	task.timer.Start()
	task.updateStatus(base.DownloadStatusRunning)
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	callback := make(chan error, 1)
	d.Listener(func(event *Event) {
		if event.Key == EventKeyPause {
			callback <- d.Close()
		}
	})
	if err := d.Pause(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	<-f.entered
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close finished before Pause persisted: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(f.release)
	for _, done := range []<-chan error{closed, callback} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close or reentrant pause callback deadlocked")
		}
	}
	if storage.wroteAfterClose.Load() {
		t.Fatal("task persisted after storage closed")
	}
}

func TestListenerReplacementAndReentry(t *testing.T) {
	d := &Downloader{}
	task := snapshotTestTask()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			d.Listener(func(*Event) { d.Listener(nil) })
		}
	}()
	for i := 0; i < 1000; i++ {
		d.emit(EventKeyProgress, task)
	}
	wg.Wait()
}
