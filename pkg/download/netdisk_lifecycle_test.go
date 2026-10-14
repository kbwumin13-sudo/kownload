package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	netdiskProtocol "github.com/GopeedLab/gopeed/internal/protocol/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	netdiskModel "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type netdiskLifecycleProvider struct {
	cloud.Provider
	url        string
	resolveErr error
}

func (*netdiskLifecycleProvider) Info() netdiskModel.ProviderInfo {
	return netdiskModel.ProviderInfo{ID: "quark", Hosts: []string{"pan.quark.cn"}}
}

func (p *netdiskLifecycleProvider) Resolve(_ context.Context, _ *cloud.APIClient, _ *cloud.Session, _ netdiskModel.Source, file netdiskModel.File) (*cloud.Download, error) {
	if p.resolveErr != nil {
		return nil, p.resolveErr
	}
	return &cloud.Download{URL: p.url, File: file}, nil
}

type netdiskLifecycleStorage struct {
	*MemStorage
	closed           atomic.Bool
	wroteAfterClose  atomic.Bool
	startSaveOnce    sync.Once
	startSave        chan struct{}
	releaseStartSave chan struct{}
	errorSaveOnce    sync.Once
	errorSave        chan struct{}
	releaseSave      chan struct{}
}

func (s *netdiskLifecycleStorage) Put(bucket, key string, value any) error {
	if bucket == bucketTask && s.startSave != nil {
		if task, ok := value.(*Task); ok && task.Status == base.DownloadStatusRunning {
			s.startSaveOnce.Do(func() { close(s.startSave); <-s.releaseStartSave })
		}
	}
	if bucket == bucketTask && s.errorSave != nil {
		if task, ok := value.(*Task); ok && task.Status == base.DownloadStatusError {
			s.errorSaveOnce.Do(func() {
				close(s.errorSave)
				<-s.releaseSave
			})
		}
	}
	if s.closed.Load() {
		s.wroteAfterClose.Store(true)
	}
	return s.MemStorage.Put(bucket, key, value)
}

func (s *netdiskLifecycleStorage) Close() error {
	s.closed.Store(true)
	return s.MemStorage.Close()
}

type netdiskLifecycleFixture struct {
	d              *Downloader
	storage        *netdiskLifecycleStorage
	req            *base.Request
	opts           *base.Options
	started        chan struct{}
	errorsReported atomic.Int32
}

func newNetdiskLifecycleFixture(t *testing.T, resolveErr error, blockErrorSave bool) *netdiskLifecycleFixture {
	t.Helper()
	payload := "offline netdisk lifecycle fixture"
	releaseBody := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-releaseBody:
			_, _ = io.WriteString(w, payload)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseBody) })

	provider := &netdiskLifecycleProvider{url: server.URL + "/fixture.bin", resolveErr: resolveErr}
	service := cloud.NewService(provider)
	captured, err := service.Capture("", "", netdiskModel.Capture{
		Provider: "quark", SourcePage: "https://pan.quark.cn/s/fixture",
		InstanceID: "fixture-browser", ContextID: "fixture-tab", AccountID: "fixture-account",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := &netdiskProtocol.FetcherManager{Service: service}
	storage := &netdiskLifecycleStorage{MemStorage: NewMemStorage()}
	if blockErrorSave {
		storage.errorSave = make(chan struct{})
		storage.releaseSave = make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-storage.releaseSave:
			default:
				close(storage.releaseSave)
			}
		})
	}
	d := NewDownloader(&DownloaderConfig{
		Storage: storage, StorageDir: t.TempDir(), RefreshInterval: 1000,
		FetchManagers: []fetcher.FetcherManager{manager},
	})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Also release a pre-fix waiter if a regression assertion fails.
		if storage.releaseSave != nil {
			select {
			case <-storage.releaseSave:
			default:
				close(storage.releaseSave)
			}
		}
		_ = d.Close()
		for _, task := range d.taskList() {
			if task.fetcher != nil {
				_ = task.fetcher.Close()
			}
		}
	})
	fixture := &netdiskLifecycleFixture{
		d: d, storage: storage,
		started: make(chan struct{}),
		req: &base.Request{URL: captured.Source.URL, Protocol: "netdisk", Extra: &netdiskModel.ReqExtra{
			Provider: "quark", Source: captured.Source, SessionID: captured.SessionID, AccountID: "fixture-account",
			File: netdiskModel.File{ID: "fixture-file", Name: "fixture.bin", Size: int64(len(payload))},
		}},
		opts: &base.Options{Path: t.TempDir()},
	}
	var startedOnce sync.Once
	d.Listener(func(event *Event) {
		if event.Key == EventKeyStart {
			startedOnce.Do(func() { close(fixture.started) })
		}
		if event.Key == EventKeyError {
			fixture.errorsReported.Add(1)
		}
	})
	return fixture
}

func waitNetdiskLifecycle(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertNetdiskTaskNotStored(t *testing.T, storage Storage, id string) {
	t.Helper()
	var task Task
	if found, err := storage.Get(bucketTask, id, &task); err != nil || found {
		t.Fatalf("deleted task reappeared in storage: found=%v err=%v", found, err)
	}
	var data any
	if found, err := storage.Get(bucketSave, id, &data); err != nil || found {
		t.Fatalf("deleted fetcher state reappeared in storage: found=%v err=%v", found, err)
	}
}

func TestNetdiskDeleteWatchDoesNotResurrectTask(t *testing.T) {
	fixture := newNetdiskLifecycleFixture(t, nil, false)
	id, err := fixture.d.CreateDirect(fixture.req, fixture.opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("netdisk task did not finish start persistence")
	}
	waitNetdiskLifecycle(t, "netdisk watcher did not start", func() bool {
		_, watched := fixture.d.watchedTasks.Load(id)
		return watched
	})
	if err := fixture.d.Delete(&TaskFilter{IDs: []string{id}}, false); err != nil {
		t.Fatal(err)
	}
	waitNetdiskLifecycle(t, "deleted task's watcher did not exit", func() bool {
		_, watched := fixture.d.watchedTasks.Load(id)
		return !watched
	})
	assertNetdiskTaskNotStored(t, fixture.storage, id)
	if fixture.errorsReported.Load() != 0 {
		t.Fatal("closing a deleted task emitted a download error")
	}
}

func TestNetdiskDeleteDuringErrorPersistenceDoesNotResurrectTask(t *testing.T) {
	fixture := newNetdiskLifecycleFixture(t, errors.New("fixture resolve failure"), true)
	id, err := fixture.d.CreateDirect(fixture.req, fixture.opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.storage.errorSave:
	case <-time.After(3 * time.Second):
		t.Fatal("failed task did not reach persistence")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- fixture.d.Delete(&TaskFilter{IDs: []string{id}}, false) }()
	waitNetdiskLifecycle(t, "Delete did not remove task from the task list", func() bool {
		return fixture.d.getTask(id) == nil
	})
	close(fixture.storage.releaseSave)
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Delete did not finish after error persistence was released")
	}
	waitNetdiskLifecycle(t, "failed task's watcher did not exit", func() bool {
		_, watched := fixture.d.watchedTasks.Load(id)
		return !watched
	})
	assertNetdiskTaskNotStored(t, fixture.storage, id)
}

func TestNetdiskDownloaderCloseTerminatesWaitAndPreservesResume(t *testing.T) {
	for _, state := range []string{"running", "paused", "failed"} {
		t.Run(state, func(t *testing.T) {
			var resolveErr error
			if state == "failed" {
				resolveErr = errors.New("fixture resolve failure")
			}
			fixture := newNetdiskLifecycleFixture(t, resolveErr, false)
			id, err := fixture.d.CreateDirect(fixture.req, fixture.opts)
			if err != nil {
				t.Fatal(err)
			}
			if state == "failed" {
				waitNetdiskLifecycle(t, "failed task was not persisted", func() bool {
					return fixture.errorsReported.Load() > 0
				})
			} else {
				select {
				case <-fixture.started:
				case <-time.After(3 * time.Second):
					t.Fatal("netdisk task did not finish start persistence")
				}
			}
			waitNetdiskLifecycle(t, "netdisk watcher did not start", func() bool {
				_, watched := fixture.d.watchedTasks.Load(id)
				return watched
			})
			if state == "paused" {
				if err := fixture.d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.d.Close(); err != nil {
				t.Fatal(err)
			}
			waitNetdiskLifecycle(t, "Downloader.Close left the netdisk watcher blocked in Wait", func() bool {
				_, watched := fixture.d.watchedTasks.Load(id)
				return !watched
			})
			expectedStatus := base.DownloadStatusPause
			if state == "failed" {
				expectedStatus = base.DownloadStatusError
			}
			var task Task
			if found, err := fixture.storage.Get(bucketTask, id, &task); err != nil || !found || task.Status != expectedStatus {
				t.Fatalf("task status was not preserved: found=%v status=%s want=%s err=%v", found, task.Status, expectedStatus, err)
			}
			var resume any
			if found, err := fixture.storage.Get(bucketSave, id, &resume); err != nil || !found {
				t.Fatalf("resume state was not preserved: found=%v err=%v", found, err)
			}
			if fixture.storage.wroteAfterClose.Load() {
				t.Fatal("watcher persisted a task after storage closed")
			}
		})
	}
}

func TestNetdiskDeleteDuringStartPersistenceDoesNotResurrectTask(t *testing.T) {
	fixture := newNetdiskLifecycleFixture(t, nil, false)
	fixture.storage.startSave = make(chan struct{})
	fixture.storage.releaseStartSave = make(chan struct{})
	defer func() {
		select {
		case <-fixture.storage.releaseStartSave:
		default:
			close(fixture.storage.releaseStartSave)
		}
	}()
	id, err := fixture.d.CreateDirect(fixture.req, fixture.opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.storage.startSave:
	case <-time.After(3 * time.Second):
		t.Fatal("start did not reach persistence")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- fixture.d.Delete(&TaskFilter{IDs: []string{id}}, false) }()
	waitNetdiskLifecycle(t, "delete did not remove task", func() bool { return fixture.d.getTask(id) == nil })
	close(fixture.storage.releaseStartSave)
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delete blocked on startup")
	}
	waitNetdiskLifecycle(t, "deleted watcher did not exit", func() bool { _, watched := fixture.d.watchedTasks.Load(id); return !watched })
	assertNetdiskTaskNotStored(t, fixture.storage, id)
}
