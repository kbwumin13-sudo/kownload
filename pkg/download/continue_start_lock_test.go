package download

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type continueLockFetcher struct {
	fetcher.Fetcher
	meta         *fetcher.FetcherMeta
	done         chan error
	started      chan struct{}
	canceled     chan struct{}
	cancelOnce   sync.Once
	closeOnce    sync.Once
	storeGate    atomic.Bool
	storeEntered chan struct{}
	storeRelease chan struct{}
	pauseEntered chan struct{}
	pauseRelease chan struct{}
	pauseOnce    sync.Once
}

func (f *continueLockFetcher) Meta() *fetcher.FetcherMeta { return f.meta }
func (f *continueLockFetcher) Progress() fetcher.Progress { return fetcher.Progress{0} }
func (f *continueLockFetcher) Patch(_ *base.Request, opts *base.Options) error {
	f.meta.Opts = opts.Clone()
	return nil
}
func (f *continueLockFetcher) Start() error {
	f.started <- struct{}{}
	return nil
}
func (f *continueLockFetcher) Wait() error { return <-f.done }
func (f *continueLockFetcher) CancelResolve() {
	f.cancelOnce.Do(func() { close(f.canceled) })
}
func (f *continueLockFetcher) Pause() error {
	f.pauseOnce.Do(func() {
		if f.pauseEntered != nil {
			close(f.pauseEntered)
			<-f.pauseRelease
		}
	})
	return nil
}
func (f *continueLockFetcher) Close() error {
	f.closeOnce.Do(func() { f.done <- context.Canceled })
	return nil
}

type continueLockManager struct{ fetcher.FetcherManager }

func (*continueLockManager) Name() string     { return "netdisk" }
func (*continueLockManager) AutoRename() bool { return false }
func (*continueLockManager) Close() error     { return nil }
func (*continueLockManager) Store(item fetcher.Fetcher) (any, error) {
	f := item.(*continueLockFetcher)
	if f.storeGate.CompareAndSwap(true, false) {
		close(f.storeEntered)
		<-f.storeRelease
	}
	return nil, nil
}

func newContinueLockFixture(t *testing.T) (*Downloader, *Task, *Task) {
	t.Helper()
	manager := &continueLockManager{}
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{manager}})
	d.cfg.DownloaderStoreConfig = &base.DownloaderStoreConfig{MaxRunning: 1}
	d.cfg.DownloaderStoreConfig.Init()
	// Initialize only storage: this test controls startup/pause scheduling itself.
	if err := d.storage.Setup([]string{bucketTask, bucketSave}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		task := snapshotTestTask()
		task.Protocol = "netdisk"
		task.updateStatus(base.DownloadStatusPause)
		task.fetcherManager = manager
		task.fetcher = &continueLockFetcher{
			meta: task.Meta, done: make(chan error, 1), started: make(chan struct{}, 4),
			canceled: make(chan struct{}), storeEntered: make(chan struct{}), storeRelease: make(chan struct{}),
		}
		d.tasks = append(d.tasks, task)
	}
	t.Cleanup(func() {
		// A red lock-order regression must fail without hanging cleanup as well.
		if t.Failed() {
			d.closed.Store(true)
			return
		}
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d, d.tasks[0], d.tasks[1]
}

func awaitContinueLockSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(label)
	}
}

func TestContinuePreemptsNetdiskStartupAfterPathPatch(t *testing.T) {
	d, first, second := newContinueLockFixture(t)
	opts := first.Meta.Opts.Clone()
	opts.Path = t.TempDir()
	if err := d.Patch(first.ID, nil, opts); err != nil {
		t.Fatal(err)
	}
	f := first.fetcher.(*continueLockFetcher)
	f.storeGate.Store(true)
	if err := d.Continue(&TaskFilter{IDs: []string{first.ID}}); err != nil {
		t.Fatal(err)
	}
	awaitContinueLockSignal(t, f.storeEntered, "startup did not reach persistence")
	continued := make(chan error, 1)
	go func() { continued <- d.Continue(&TaskFilter{IDs: []string{second.ID}}) }()
	awaitContinueLockSignal(t, f.canceled, "Continue did not preempt the first task")
	close(f.storeRelease)
	select {
	case err := <-continued:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Continue holds the downloader lock while startup holds the task lock")
	}
	if got := d.GetTask(first.ID); got.Status != base.DownloadStatusWait || got.Meta.Opts.Path != opts.Path {
		t.Fatalf("preempted task lost its queue state or patched path: %+v", got)
	}
	if d.GetTask(second.ID).Status != base.DownloadStatusRunning {
		t.Fatal("requested task did not start")
	}
}

func TestContinueRechecksMembershipAfterPreemption(t *testing.T) {
	for _, removeTarget := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			name := "victim"
			if removeTarget {
				name = "target"
			}
			if replace {
				name += "-replaced"
			}
			t.Run(name, func(t *testing.T) {
				d, first, second := newContinueLockFixture(t)
				f := first.fetcher.(*continueLockFetcher)
				f.pauseEntered, f.pauseRelease = make(chan struct{}), make(chan struct{})
				if err := d.Continue(&TaskFilter{IDs: []string{first.ID}}); err != nil {
					t.Fatal(err)
				}
				awaitContinueLockSignal(t, f.started, "first task did not start")
				first.lock.Lock()
				first.lock.Unlock()
				continued := make(chan error, 1)
				go func() { continued <- d.Continue(&TaskFilter{IDs: []string{second.ID}}) }()
				awaitContinueLockSignal(t, f.pauseEntered, "preemption did not reach Pause")
				removed := first
				if removeTarget {
					removed = second
				}
				deleted := make(chan error, 1)
				go func() { deleted <- d.Delete(&TaskFilter{IDs: []string{removed.ID}}, false) }()
				select {
				case err := <-deleted:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Delete blocked behind a synchronous preemption")
				}
				var replacement *Task
				if replace {
					replacement = snapshotTestTask()
					replacement.ID = removed.ID
					replacement.Protocol = "netdisk"
					replacement.Meta.Opts.Name = "replacement"
					replacement.updateStatus(base.DownloadStatusPause)
					d.lock.Lock()
					d.tasks = append(d.tasks, replacement)
					d.lock.Unlock()
					if err := d.storage.Put(bucketTask, replacement.ID, replacement.clone()); err != nil {
						t.Fatal(err)
					}
				}
				close(f.pauseRelease)
				select {
				case err := <-continued:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Continue did not finish after deletion")
				}
				// Late startup/poll/error handlers must retain pointer identity, not
				// merely find another task with the same ID.
				if err := d.saveActiveTask(removed); err != nil {
					t.Fatal(err)
				}
				d.doOnError(removed, errors.New("late old handler"))
				var stored Task
				exists, err := d.storage.Get(bucketTask, removed.ID, &stored)
				if err != nil {
					t.Fatal(err)
				}
				if replace {
					if d.getTask(removed.ID) != replacement || !exists || stored.Meta.Opts.Name != "replacement" || replacement.getStatus() != base.DownloadStatusPause {
						t.Fatal("old preemption or handler overwrote the replacement task")
					}
				} else if exists || d.getTask(removed.ID) != nil {
					t.Fatal("pause persistence resurrected a deleted task")
				}
				d.lock.Lock()
				defer d.lock.Unlock()
				for _, task := range d.waitTasks {
					if task == removed || task == replacement {
						t.Fatal("stale preemption queued a removed or replacement task")
					}
				}
			})
		}
	}
}
