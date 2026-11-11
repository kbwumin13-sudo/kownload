package download

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type verifiedPostCContext struct {
	context.Context
	committed        atomic.Bool
	gateOnce         sync.Once
	entered, release chan struct{}
}

func (c *verifiedPostCContext) Err() error {
	if c.committed.Load() {
		c.gateOnce.Do(func() { close(c.entered); <-c.release })
	}
	return c.Context.Err()
}

type verifiedLostCAfterCommit struct {
	AtomicStorage
	id  string
	ctx *verifiedPostCContext
}

func (s verifiedLostCAfterCommit) UpdateAtomic(fn func(StorageTransaction) error) error {
	committed := false
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		if err := fn(tx); err != nil {
			return err
		}
		var record transfer.VerifiedCompletionRecord
		found, err := tx.Get(bucketTaskCompletion, s.id, &record)
		committed = found && record.Phase == transfer.CompletionCommitted
		return err
	})
	if err == nil && committed {
		s.ctx.committed.Store(true)
		return errors.New("synthetic C ACK lost after commit")
	}
	return err
}

func TestVerifiedHTTPExplicitContinueReconcilesCWithoutFreshAfterCancelledACK(t *testing.T) {
	baseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &verifiedPostCContext{Context: baseContext, entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(ctx.release) }) }
	defer unblock()
	body := bytes.Repeat([]byte("confirmed C after canceled run\n"), 100)
	d, task, _, lease := verifiedWrittenContextFixture(t, body, ctx)
	task.statusLock.Lock()
	task.runCancel = cancel
	task.statusLock.Unlock()
	events, release, err := d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetA)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var successes atomic.Int32
	d.Listener(func(e *Event) {
		if e.Key == EventKeyDone {
			successes.Add(1)
		}
	})
	store := verifiedLostCAfterCommit{d.storage.(AtomicStorage), task.ID, ctx}
	finished := make(chan error, 1)
	go func() {
		fresh, err := d.completeVerifiedHTTPStored(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease, store)
		if fresh {
			err = errors.New("unknown ACK granted fresh")
		}
		finished <- err
	}()
	select {
	case <-ctx.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("unknown C did not reach unlocked reconcile admission")
	}
	// Pause wins after C's Fence has returned its unknown ACK. It cancels the
	// run before the authoritative read can linearize memory/API done.
	if err = d.Pause(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	task.statusLock.Lock()
	paused := task.pauseDone
	task.statusLock.Unlock()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("Pause did not finish")
	}
	unblock()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled ACK was treated as confirmed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old completion did not return")
	}
	if task.getStatus() != base.DownloadStatusPause || task.verifiedSuccess.Load() != nil {
		t.Fatal("canceled run exposed done/fresh before reconcile")
	}
	owned, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
	if err != nil || owned.Status != base.DownloadStatusDone || record.Phase != transfer.CompletionCommitted {
		t.Fatal("authoritative C lost", err)
	}
	before, err := os.ReadFile(d.storage.(*BoltStorage).db.Path())
	if err != nil {
		t.Fatal(err)
	}
	d.cfg.MaxRunning = 0 // No network admission is available to this sealed continuation.
	if err = d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, task.ID, base.DownloadStatusDone, 3*time.Second)
	after, err := os.ReadFile(d.storage.(*BoltStorage).db.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("C reconcile wrote persistent state", err)
	}
	if task.fetcher != nil || task.verifiedSuccess.Load() != nil || successes.Load() != 0 {
		t.Fatal("C reconcile restored IO or fresh effects")
	}
	select {
	case event := <-events:
		t.Fatal("C reconcile replayed live event", event)
	default:
	}
	actual, err := os.ReadFile(filepath.Join(task.Meta.Opts.Path, record.FinalName))
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("C reconcile changed output", err)
	}
}
