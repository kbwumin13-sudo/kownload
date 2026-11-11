package download

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type recoveryLateFetcher struct {
	fetcher.Fetcher
	meta    *fetcher.FetcherMeta
	entered chan int
	release [2]chan error
	calls   atomic.Int32
}

func (f *recoveryLateFetcher) Meta() *fetcher.FetcherMeta { return f.meta }
func (f *recoveryLateFetcher) Progress() fetcher.Progress { return fetcher.Progress{1} }
func (f *recoveryLateFetcher) Start() error               { return nil }
func (f *recoveryLateFetcher) Pause() error               { return nil }
func (f *recoveryLateFetcher) Close() error               { return nil }
func (f *recoveryLateFetcher) Wait() error {
	i := int(f.calls.Add(1)) - 1
	f.entered <- i
	if i >= len(f.release) {
		return errors.New("unexpected third waiter")
	}
	return <-f.release[i]
}

// Each binding captures its own result channel before launch, exactly the
// capability required by Core's direct-HTTP path.
func (f *recoveryLateFetcher) BindRunWait(ctx context.Context) func() error {
	i := int(f.calls.Add(1)) - 1
	f.entered <- i
	return func() error {
		select {
		case err := <-f.release[i]:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type recoveryLateManager struct{ fetcher.FetcherManager }

func (m *recoveryLateManager) Store(fetcher.Fetcher) (any, error) { return nil, nil }
func (m *recoveryLateManager) ParseName(string) string            { return "synthetic.bin" }

func TestRecoveryLateWaitCannotConsumeNewRun(t *testing.T) {
	for _, lateError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "error"}[lateError], func(t *testing.T) {
			dir := t.TempDir()
			d := operationDownloader(t, NewMemStorage(), dir)
			defer d.Close()
			task := startupStoredTask("late-wait", dir, base.DownloadStatusRunning)
			task.IsCreated = true
			task.Meta.Res = &base.Resource{Size: 1, Files: []*base.FileInfo{{Name: "late.bin", Size: 1}}}
			f := &recoveryLateFetcher{meta: task.Meta, entered: make(chan int, 3), release: [2]chan error{make(chan error, 1), make(chan error, 1)}}
			defer func() {
				select {
				case f.release[0] <- errors.New("fixture cleanup"):
				default:
				}
				select {
				case f.release[1] <- errors.New("fixture cleanup"):
				default:
				}
			}()
			task.fetcher = f
			task.fetcherManager = &recoveryLateManager{}
			initTask(task)
			task.runGeneration = 1
			if err := d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
				t.Fatal(err)
			}
			d.lock.Lock()
			d.tasks = append(d.tasks, task)
			d.lock.Unlock()
			var successes atomic.Int32
			d.Listener(func(e *Event) {
				if e.Key == EventKeyDone {
					successes.Add(1)
				}
			})
			oldReturned := make(chan struct{})
			go func() { d.watch(task); close(oldReturned) }()
			select {
			case i := <-f.entered:
				if i != 0 {
					t.Fatal(i)
				}
			case <-time.After(time.Second):
				t.Fatal("old Wait not entered")
			}
			if err := d.doPause(task); err != nil {
				t.Fatal(err)
			}
			task.statusLock.Lock()
			paused := task.pauseDone
			task.statusLock.Unlock()
			select {
			case <-paused:
			case <-time.After(time.Second):
				t.Fatal("controlled pause did not persist")
			}
			if err := d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
				t.Fatal(err)
			}
			newEntered := false
			select {
			case i := <-f.entered:
				if i != 1 {
					t.Fatal(i)
				}
				newEntered = true
			case <-time.After(time.Second):
				t.Error("new generation did not acquire its own Wait")
			}
			var newRegistration any
			registrationDeadline := time.Now().Add(time.Second)
			for time.Now().Before(registrationDeadline) {
				value, found := d.watchedTasks.Load(task.ID)
				if current, ok := value.(*taskRunWatch); found && ok && current.bound && current.generation == task.runGeneration {
					newRegistration = value
					break
				}
				runtime.Gosched()
			}
			if newRegistration == nil {
				t.Fatal("new bound watcher not registered")
			}
			if lateError {
				f.release[0] <- errors.New("synthetic old run failure")
			} else {
				f.release[0] <- nil
			}
			select {
			case <-oldReturned:
			case <-time.After(time.Second):
				t.Fatal("old watcher did not return")
			}
			registration, exists := d.watchedTasks.Load(task.ID)
			if !exists || registration != newRegistration || task.getStatus() != base.DownloadStatusRunning || successes.Load() != 0 {
				t.Fatal("late result changed new generation or erased its watcher")
			}
			var saved Task
			if _, err := d.storage.Get(bucketTask, task.ID, &saved); err != nil || saved.Status != base.DownloadStatusRunning {
				t.Fatalf("old Wait wrote durable terminal state: %v %v", saved.Status, err)
			}
			if !newEntered {
				return
			}
			f.release[1] <- nil
			waitStatus(t, d, task.ID, base.DownloadStatusDone, 2*time.Second)
			deadline := time.Now().Add(time.Second)
			for successes.Load() != 1 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if successes.Load() != 1 {
				t.Fatal("current run did not publish exactly one completion")
			}
		})
	}
}

type recoveryBlockedProgress struct {
	recoveryLateFetcher
	progressEntered chan struct{}
	progressRelease chan struct{}
	once            atomic.Bool
}

func (f *recoveryBlockedProgress) Progress() fetcher.Progress {
	if f.once.CompareAndSwap(false, true) {
		close(f.progressEntered)
		<-f.progressRelease
	}
	return fetcher.Progress{999999}
}
func TestRecoveryLateProgressAfterDeleteCannotPublishOrResurrect(t *testing.T) {
	dir := t.TempDir()
	store := NewMemStorage()
	d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: dir, RefreshInterval: 1, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, owner := recoveryFixtureStore(t, store, dir)
	if _, err := owner.begin(1, ""); err != nil {
		t.Fatal(err)
	}
	owner.closed = &d.closed
	if _, err := owner.currentLease(context.Background()); err != nil {
		t.Fatal(err)
	}
	task.Recovery = &TaskRecoveryRef{Version: 1}
	task.recoveryOwner.Store(owner)
	task.runGeneration = 1
	task.Status = base.DownloadStatusRunning
	f := &recoveryBlockedProgress{progressEntered: make(chan struct{}), progressRelease: make(chan struct{})}
	task.fetcher = f
	task.fetcherManager = &recoveryLateManager{}
	var events atomic.Int32
	d.Listener(func(event *Event) {
		if event.Key == EventKeyProgress {
			events.Add(1)
		}
	})
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	select {
	case <-f.progressEntered:
	case <-time.After(time.Second):
		t.Fatal("progress not sampled")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- d.Delete(&TaskFilter{IDs: []string{task.ID}}, false) }()
	deadline := time.Now().Add(time.Second)
	for owner.active.Load() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	invalidated := !owner.active.Load()
	close(f.progressRelease)
	if !invalidated {
		t.Fatal("delete waited for obsolete callback before invalidating it")
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete did not finish")
	}
	var saved Task
	if found, err := store.Get(bucketTask, task.ID, &saved); err != nil || found || d.GetTask(task.ID) != nil || events.Load() != 0 || task.Progress.Downloaded != 0 {
		t.Fatal("late progress published or restored deleted task", err)
	}
}

func TestRecoveryOldSourceWaitCleanupCannotInvalidateCurrentToken(t *testing.T) {
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	task, owner := recoveryFixtureStore(t, d.storage.(AtomicStorage), dir)
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	task.Recovery = &TaskRecoveryRef{Version: 1}
	task.recoveryOwner.Store(owner)
	task.runGeneration = 1
	task.Status = base.DownloadStatusRunning
	task.Meta.Res = &base.Resource{Size: 1, Files: []*base.FileInfo{{Name: "token.bin", Size: 1}}}
	f := &recoveryLateFetcher{meta: task.Meta}
	task.fetcher = f
	task.fetcherManager = &recoveryLateManager{}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	oldResult := make(chan error, 1)
	oldDone := make(chan struct{})
	go func() { d.watchRun(task, f, 1, func() error { return <-oldResult }, lease); close(oldDone) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.watchedTasks.Load(task.ID); ok {
			break
		}
		runtime.Gosched()
	}
	snapshot := task.clone()
	snapshot.Meta.Req.URL = "https://new-source.invalid/file"
	if err = owner.persistSnapshot(snapshot, nil, false); err != nil {
		t.Fatal(err)
	}
	current, err := owner.currentLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldResult <- nil
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("old source waiter did not finish")
	}
	if current.Check() != nil || task.getStatus() != base.DownloadStatusRunning {
		t.Fatal("old source result/cleanup consumed current token")
	}
}
