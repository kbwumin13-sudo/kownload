package download

import (
	"context"
	"errors"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"sync"
	"testing"
	"time"
)

func TestRecoveryObsoletePreparationCleanupCannotCancelNewLease(t *testing.T) {
	store := NewMemStorage()
	d := operationDownloader(t, store, t.TempDir())
	defer d.Close()
	task, owner := recoveryFixtureStore(t, store, t.TempDir())
	old, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	task.recoveryOwner.Store(owner)
	task.Recovery = &TaskRecoveryRef{Version: 1}
	task.runGeneration = 1
	task.Status = base.DownloadStatusRunning
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	defer func() { d.lock.Lock(); d.tasks = nil; d.lock.Unlock() }()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	oldContext, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	go func() {
		d.failTaskPreparation(task, 1, func() { close(entered); <-release; cancelOld() }, errors.New("synthetic obsolete preparation error"))
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old cleanup did not reach cancellation boundary")
	}
	// Same status-lock admission used by Pause/Continue assigns a new run before
	// its begin. The old cleanup stays gated until that exact new lease is active.
	task.statusLock.Lock()
	invalidateTaskRecovery(task)
	task.runGeneration = 2
	task.statusLock.Unlock()
	current, err := owner.begin(2, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = current.Check(); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("old cleanup did not return")
	}
	if oldContext.Err() == nil || !errors.Is(old.Check(), transfer.ErrRecoveryStale) {
		t.Fatal("old run was not cancelled")
	}
	if err = current.Check(); err != nil || task.getStatus() != base.DownloadStatusRunning {
		t.Fatal("old preparation cleanup revoked current run", err)
	}
	if owner.record.TotalUsed != 1 {
		t.Fatal("cleanup reset budget")
	}
}
