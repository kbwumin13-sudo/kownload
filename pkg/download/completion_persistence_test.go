package download

import (
	"bytes"
	"context"
	"errors"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type completionPutGate struct {
	AtomicStorage
	entered  chan struct{}
	release  chan struct{}
	returned chan struct{}
	fired    atomic.Bool
	once     sync.Once
	mode     string
}

func (s *completionPutGate) unblock() { s.once.Do(func() { close(s.release) }) }
func (s *completionPutGate) Put(bucket, key string, value any) error {
	if task, ok := value.(*Task); ok && bucket == bucketTask && task.Status == "done" && s.fired.CompareAndSwap(false, true) {
		if s.returned != nil {
			defer close(s.returned)
		}
		close(s.entered)
		<-s.release
		if s.mode != "" {
			if s.mode == "committed-ack-lost" {
				if err := s.AtomicStorage.Put(bucket, key, value); err != nil {
					return err
				}
			}
			return errors.New("synthetic completion persistence acknowledgement failure")
		}
	}
	return s.AtomicStorage.Put(bucket, key, value)
}
func TestCompletionPutCannotResurrectDeletedTask(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "db")
	store := &completionPutGate{AtomicStorage: NewBoltStorage(dbDir), entered: make(chan struct{}), release: make(chan struct{})}
	url, _ := localOperationSource(t)
	d := operationDownloader(t, store, dir)
	defer func() { store.unblock(); _ = d.Close() }()
	var success atomic.Int32
	d.Listener(func(event *Event) {
		if event.Key == EventKeyDone {
			success.Add(1)
		}
	})
	r := operationRequest(url, dir)
	result := requireOperation(t, d, r)
	id := result.TaskIDs[0]
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("completion Put was not reached")
	}
	published := d.GetTask(id).getStatus() == "done"
	deleted := make(chan error, 1)
	go func() { deleted <- d.Delete(&TaskFilter{IDs: []string{id}}, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for d.GetTask(id) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.GetTask(id) != nil {
		store.unblock()
		t.Fatal("Delete did not remove live task")
	}
	store.unblock()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Delete blocked after completion Put released")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.watchedTasks.Load(id); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var durable Task
	present, err := store.Get(bucketTask, id, &durable)
	if err != nil || present {
		t.Errorf("late completion Put resurrected deleted Task: present=%t err=%v", present, err)
	}
	receipt, err := d.QueryBatchOperation(r.OperationID)
	if err != nil || len(receipt.TaskIDs) != 1 || receipt.Members[0].TaskPresent == nil || *receipt.Members[0].TaskPresent {
		t.Errorf("historical association changed: %+v %v", receipt, err)
	}
	if published {
		t.Error("done was visible before storage acknowledgement")
	}
	if success.Load() != 0 {
		t.Error("deleted/uncertain completion emitted success")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := NewBoltStorage(dbDir)
	defer reopened.Close()
	present, err = reopened.Get(bucketTask, id, &durable)
	if err != nil || present {
		t.Errorf("deleted Task survived real Bolt reopen: present=%t err=%v", present, err)
	}
}

func TestCompletionAcknowledgementGatesSuccess(t *testing.T) {
	for _, mode := range []string{"success", "failed-before-commit", "committed-ack-lost"} {
		t.Run(mode, func(t *testing.T) {
			store := &completionPutGate{AtomicStorage: NewMemStorage(), entered: make(chan struct{}), release: make(chan struct{})}
			if mode != "success" {
				store.mode = mode
			}
			url, _ := localOperationSource(t)
			dir := t.TempDir()
			d := operationDownloader(t, store, dir)
			defer func() { store.unblock(); _ = d.Close() }()
			var successes atomic.Int32
			finished := make(chan *Event, 1)
			d.Listener(func(event *Event) {
				if event.Key == EventKeyDone {
					successes.Add(1)
				}
				if event.Key == EventKeyFinally {
					select {
					case finished <- event:
					default:
					}
				}
			})
			r := operationRequest(url, dir)
			result := requireOperation(t, d, r)
			id := result.TaskIDs[0]
			select {
			case <-store.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("no completion storage attempt")
			}
			if d.GetTask(id).getStatus() == base.DownloadStatusDone || successes.Load() != 0 {
				store.unblock()
				t.Fatal("success escaped before storage ACK")
			}
			store.unblock()
			select {
			case event := <-finished:
				if mode == "success" {
					if event.Err != nil || event.Task.getStatus() != base.DownloadStatusDone || successes.Load() != 1 {
						t.Fatal("acknowledged completion did not publish once")
					}
				} else if event.Err == nil || event.Task.getStatus() != base.DownloadStatusError || successes.Load() != 0 {
					t.Fatal("unknown/failed completion published success")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("completion did not settle")
			}
			data, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
			if err != nil || !bytes.Equal(data, []byte("durable-batch-payload")) {
				t.Fatal("completion failure removed/changed complete file", err)
			}
			receipt, err := d.QueryBatchOperation(r.OperationID)
			if err != nil || len(receipt.TaskIDs) != 1 || receipt.TaskIDs[0] != id || receipt.Members[0].TaskPresent == nil || !*receipt.Members[0].TaskPresent {
				t.Fatal("completion changed operation association", err)
			}
		})
	}
}

func TestCompletionListenerCanReenterClose(t *testing.T) {
	url, _ := localOperationSource(t)
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	result := make(chan error, 1)
	d.Listener(func(event *Event) {
		if event.Key == EventKeyDone {
			result <- d.Close()
		}
	})
	requireOperation(t, d, operationRequest(url, dir))
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("completion callback deadlocked in Close")
	}
}

type completionScheduleFetcher struct {
	fetcher.Fetcher
	meta *fetcher.FetcherMeta
	wait chan error
	stop chan struct{}
	once sync.Once
}

func (f *completionScheduleFetcher) Meta() *fetcher.FetcherMeta { return f.meta }
func (f *completionScheduleFetcher) Start() error               { return errors.New("synthetic scheduler stop") }
func (f *completionScheduleFetcher) Pause() error               { return nil }
func (f *completionScheduleFetcher) Close() error               { f.once.Do(func() { close(f.stop) }); return nil }
func (f *completionScheduleFetcher) Wait() error {
	select {
	case err := <-f.wait:
		return err
	case <-f.stop:
		return context.Canceled
	}
}
func (f *completionScheduleFetcher) Progress() fetcher.Progress { return fetcher.Progress{0} }

type completionScheduleManager struct{ *httpProtocol.FetcherManager }

func (m *completionScheduleManager) Store(fetcher.Fetcher) (any, error) { return nil, nil }

func TestCompletionAfterPauseRequeueDoesNotInvertSchedulerLocks(t *testing.T) {
	dir := t.TempDir()
	manager := &completionScheduleManager{FetcherManager: &httpProtocol.FetcherManager{}}
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: dir, FetchManagers: []fetcher.FetcherManager{manager}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	task := NewTask()
	task.Protocol = "http"
	task.Status = base.DownloadStatusRunning
	task.IsCreated = true
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/one"}, Opts: &base.Options{Path: dir}, Res: &base.Resource{Size: 1, Files: []*base.FileInfo{{Name: "one.bin", Size: 1}}}}
	task.Progress = &Progress{}
	initTask(task)
	f := &completionScheduleFetcher{meta: task.Meta, wait: make(chan error, 1), stop: make(chan struct{})}
	task.fetcher = f
	task.fetcherManager = manager
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	if err := d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
		t.Fatal(err)
	}
	// Real pause preparation/persistence completes before the controlled queue
	// state. This is a new lock-order probe, not a timing change to PauseAll's test.
	if err := d.doPause(task); err != nil {
		t.Fatal(err)
	}
	select {
	case <-task.pauseDone:
	case <-time.After(3 * time.Second):
		t.Fatal("pause did not finish")
	}
	d.lock.Lock()
	var unlockOnce sync.Once
	unlockMain := func() { unlockOnce.Do(func() { d.lock.Unlock() }) }
	defer func() { unlockMain(); _ = f.Close(); _ = d.Close() }()
	task.updateStatus(base.DownloadStatusWait)
	d.enqueueLocked(task)
	type completionResult struct {
		committed bool
		err       error
	}
	completed := make(chan completionResult, 1)
	waitReturned := make(chan struct{})
	go func() {
		err := f.Wait()
		close(waitReturned)
		if err != nil {
			completed <- completionResult{err: err}
			return
		}
		committed, err := d.commitTaskCompletion(task, f)
		completed <- completionResult{committed, err}
	}()
	f.wait <- nil
	<-waitReturned
	var early *completionResult
	// Main is held while the old completion attempts to acquire it. Either the
	// fixed path rejects queued state, or the old path exposes status->main.
	deadline := time.Now().Add(3 * time.Second)
	reached := false
	for time.Now().Before(deadline) {
		select {
		case value := <-completed:
			early = &value
			reached = true
		default:
		}
		if reached {
			break
		}
		if !task.statusLock.TryLock() {
			reached = true
			break
		}
		task.statusLock.Unlock()
		time.Sleep(time.Millisecond)
	}
	if !reached {
		t.Fatal("completion did not reach the controlled critical section")
	}
	scheduled := make(chan error, 1)
	// Transfer the held main lock to this goroutine, executing notifyRunning's
	// exact locked dequeue->startTask path with no racing lock acquisition.
	go func() { d.dequeueLocked(0); err := d.startTask(task, true); unlockMain(); scheduled <- err }()
	select {
	case err := <-scheduled:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Error("completion and scheduler deadlocked in status->main / main->status")
		unlockMain() // Break only the test-owned edge, so old-red cleanup can finish.
		select {
		case <-scheduled:
		case <-time.After(3 * time.Second):
			t.Fatal("scheduler failed to settle after test edge released")
		}
	}
	if early == nil {
		select {
		case value := <-completed:
			early = &value
		case <-time.After(3 * time.Second):
			t.Fatal("completion failed to settle")
		}
	}
	if early.err != nil || early.committed {
		t.Error("paused/queued completion was committed", early.err)
	}
}

func TestCompletionPostAcknowledgementRecheckUsesSchedulerLockOrder(t *testing.T) {
	store := &completionPutGate{AtomicStorage: NewMemStorage(), entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	url, _ := localOperationSource(t)
	dir := t.TempDir()
	d := operationDownloader(t, store, dir)
	defer func() { store.unblock(); _ = d.Close() }()
	done := make(chan struct{}, 1)
	d.Listener(func(event *Event) {
		if event.Key == EventKeyDone {
			done <- struct{}{}
		}
	})
	result := requireOperation(t, d, operationRequest(url, dir))
	task := d.getTask(result.TaskIDs[0])
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("completion did not reach Put")
	}
	d.lock.Lock()
	var once sync.Once
	unlockMain := func() { once.Do(func() { d.lock.Unlock() }) }
	defer unlockMain()
	store.unblock()
	select {
	case <-store.returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Put did not return")
	}
	scheduled := make(chan error, 1)
	// A queue selection can become stale before startTask's state recheck. This
	// is that exact notifyRunning main->status critical section, deterministically
	// overlapping completion's post-ACK membership check after a real HTTP Wait.
	go func() { err := d.startTask(task, true); unlockMain(); scheduled <- err }()
	select {
	case err := <-scheduled:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Error("post-ACK completion recheck deadlocked with scheduler")
		unlockMain()
		select {
		case <-scheduled:
		case <-time.After(3 * time.Second):
			t.Fatal("scheduler did not settle after test edge released")
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("acknowledged completion did not settle")
	}
}

type completionFinalSnapshot struct {
	fetcher.Fetcher
	meta *fetcher.FetcherMeta
}

func (f *completionFinalSnapshot) Meta() *fetcher.FetcherMeta { return f.meta }
func (f *completionFinalSnapshot) Progress() fetcher.Progress { return fetcher.Progress{7} }
func TestCompletionUsesFinalSnapshotForUnknownSize(t *testing.T) {
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	task := NewTask()
	task.Protocol = "media"
	task.Status = base.DownloadStatusRunning
	task.IsCreated = true
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/media"}, Opts: &base.Options{Path: dir}, Res: &base.Resource{Size: 10, Files: []*base.FileInfo{{Name: "final.bin", Size: 10}}}}
	task.Progress = &Progress{}
	initTask(task)
	final := &completionFinalSnapshot{meta: &fetcher.FetcherMeta{Req: task.Meta.Req, Opts: task.Meta.Opts, Res: &base.Resource{Files: []*base.FileInfo{{Name: "final.bin"}}}}}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	if err := d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
		t.Fatal(err)
	}
	committed, err := d.commitTaskCompletion(task, final)
	if err != nil || !committed || task.Meta.Res.Size != 7 || task.Progress.Downloaded != 7 {
		t.Fatalf("final zero-size snapshot lost progress fallback: committed=%t err=%v", committed, err)
	}
}
