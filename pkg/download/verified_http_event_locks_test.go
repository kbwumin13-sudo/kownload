package download

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type verifiedEventGateFetcher struct {
	fetcher.Fetcher
	meta                   *fetcher.FetcherMeta
	mode                   string
	entered, release, stop chan struct{}
	gate, close            sync.Once
}

func (f *verifiedEventGateFetcher) block(mode string) {
	if f.mode == mode {
		f.gate.Do(func() { close(f.entered); <-f.release })
	}
}
func (f *verifiedEventGateFetcher) Meta() *fetcher.FetcherMeta { return f.meta }
func (f *verifiedEventGateFetcher) Start() error               { f.block("start"); return nil }
func (f *verifiedEventGateFetcher) Progress() fetcher.Progress {
	f.block("progress")
	return fetcher.Progress{0}
}
func (f *verifiedEventGateFetcher) Pause() error { return nil }
func (f *verifiedEventGateFetcher) Close() error {
	f.close.Do(func() { close(f.stop) })
	return nil
}
func (f *verifiedEventGateFetcher) Wait() error { <-f.stop; return context.Canceled }

type verifiedEventGateManager struct{ *httpProtocol.FetcherManager }

func (*verifiedEventGateManager) Store(fetcher.Fetcher) (any, error) { return nil, nil }

// Each child exercises the real Start handler or periodic sampler. A deadlocked
// old implementation is bounded by the child timeout, so it cannot strand the
// parent package or its cleanup. No product hook or lock timing sleep is used.
func TestVerifiedEventCallbacksReleaseTaskLocksBeforeDelete(t *testing.T) {
	if mode := os.Getenv("K15_EVENT_LOCK_PROBE"); mode != "" {
		verifiedEventCallbackLockProbe(t, mode)
		return
	}
	for _, mode := range []string{"start", "progress"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.v", "-test.timeout=4s", "-test.run=^TestVerifiedEventCallbacksReleaseTaskLocksBeforeDelete$")
			cmd.Env = append(os.Environ(), "K15_EVENT_LOCK_PROBE="+mode)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s callback/Fence/Delete child failed: %v\n%s", mode, err, output)
			}
		})
	}
}

func verifiedEventCallbackLockProbe(t *testing.T, mode string) {
	dir := t.TempDir()
	manager := &verifiedEventGateManager{FetcherManager: &httpProtocol.FetcherManager{}}
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: dir, RefreshInterval: 10, FetchManagers: []fetcher.FetcherManager{manager}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task := startupStoredTask("callback-source", dir, base.DownloadStatusPause)
	task.Protocol = "event-lock-probe"
	task.IsCreated = true
	task.Meta.Res = &base.Resource{Size: 1, Files: []*base.FileInfo{{Name: "source.bin", Size: 1}}}
	initTask(task)
	f := &verifiedEventGateFetcher{meta: task.Meta, mode: mode, entered: make(chan struct{}), release: make(chan struct{}), stop: make(chan struct{})}
	task.fetcher, task.fetcherManager = f, manager
	if mode == "progress" {
		task.Status = base.DownloadStatusRunning
		task.runGeneration = 1
	}
	victim := startupStoredTask("callback-other-task", dir, base.DownloadStatusPause)
	initTask(victim)
	for _, current := range []*Task{task, victim} {
		if err := d.storage.Put(bucketTask, current.ID, current.clone()); err != nil {
			t.Fatal(err)
		}
	}
	callbackReturned := make(chan error, 1)
	wanted := EventKey(EventKeyStart)
	if mode == "progress" {
		wanted = EventKeyProgress
	}
	var callback sync.Once
	d.Listener(func(event *Event) {
		if event.Key == wanted && event.Task.ID == task.ID {
			callback.Do(func() { callbackReturned <- d.Delete(&TaskFilter{IDs: []string{victim.ID}}, false) })
		}
	})
	d.lock.Lock()
	d.tasks = append(d.tasks, task, victim)
	d.lock.Unlock()
	if mode == "start" {
		if err := d.startTask(task, false); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("real Start/Progress did not reach the Task-locked gate")
	}
	finished := make(chan error, 1)
	go func() {
		// The legacy probe has no recovery owner: after acquiring the held Task
		// lock the real Fence must reject admission, without publishing anything.
		finished <- d.verifiedFence(context.Background(), scopedDatasetA, task, transfer.RecoveryToken{Generation: 1}, func(*Task, transfer.VerifiedCompletionRecord) error {
			return errors.New("unexpected publication admission")
		})
	}()
	deadline := time.Now().Add(time.Second)
	for d.operationMu.TryLock() {
		d.operationMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("Fence did not take operationMu while Start/Progress held Task lock")
		}
		runtime.Gosched()
	}
	// operationMu is now held by Fence, and only this release permits the real
	// callback to call Delete on a different Task. Old code forms the full cycle.
	close(f.release)
	select {
	case err := <-callbackReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener Delete cannot return while Fence waits for its Task lock")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, transfer.ErrRecoveryHeld) {
			t.Fatalf("probe Fence admission changed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fence did not release operation admission")
	}
	if d.GetTask(victim.ID) != nil || d.GetTask(task.ID) == nil {
		t.Fatal("callback did not delete exactly the selected other Task")
	}
}
