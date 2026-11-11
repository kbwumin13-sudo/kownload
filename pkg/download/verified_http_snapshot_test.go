package download

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type waitingCompletionACK struct {
	AtomicStorage
	id               string
	entered, release chan struct{}
	once             sync.Once
}

func (s *waitingCompletionACK) UpdateAtomic(fn func(StorageTransaction) error) error {
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
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return err
}

func TestVerifiedHTTPSnapshotDoesNotExposeCBeforeAPIVisibility(t *testing.T) {
	d, task, _, lease := verifiedWrittenFixture(t, bytes.Repeat([]byte("API visibility\n"), 100))
	store := &waitingCompletionACK{AtomicStorage: d.storage.(AtomicStorage), id: task.ID, entered: make(chan struct{}), release: make(chan struct{})}
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(store.release) }) }
	defer release()
	finished := make(chan error, 1)
	go func() {
		_, err := d.completeVerifiedHTTPStored(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease, store)
		finished <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("C did not reach ACK barrier")
	}
	// The real store contains C/done, but the live Task is still running. A
	// response must neither leak stored done nor guess a non-committed phase.
	if task.getStatus() != base.DownloadStatusRunning {
		t.Fatal("done preceded C ACK")
	}
	if result, err := d.TaskCompletionSnapshotsForDataset(scopedDatasetA, []string{task.ID}); err == nil || result != nil {
		t.Fatal("unconfirmed API done exposed", result, err)
	}
	if result, err := d.TaskCompletionSnapshotsForDataset(scopedDatasetB, []string{task.ID}); !errors.Is(err, ErrDatasetMismatch) || result != nil {
		t.Fatal("wrong domain leaked task", result, err)
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("C did not confirm")
	}
	result, err := d.TaskCompletionSnapshotsForDataset(scopedDatasetA, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := result[task.ID]
	if snapshot.Task.Status != base.DownloadStatusDone || snapshot.CompletionView == nil || !snapshot.CompletionView.Available || snapshot.CompletionView.Phase != string(transfer.CompletionCommitted) || snapshot.CompletionView.RecordedMatch == nil || !*snapshot.CompletionView.RecordedMatch {
		t.Fatal("confirmed owned projection missing")
	}
	// Both Task and projection are caller-owned, not a live/cache alias.
	snapshot.Task.Meta.Req.URL = "https://fixture.invalid/edited-copy"
	snapshot.Task.Progress.Downloaded = 0
	snapshot.CompletionView.Available = false
	again, err := d.TaskCompletionSnapshotsForDataset(scopedDatasetA, []string{task.ID})
	if err != nil || again[task.ID].Task.Meta.Req.URL == snapshot.Task.Meta.Req.URL || again[task.ID].Task.Progress.Downloaded == 0 || !again[task.ID].CompletionView.Available {
		t.Fatal("response snapshot shared mutable values", err)
	}
}

func TestVerifiedHTTPSnapshotKeepsAllAbsentLegacyLiveProgress(t *testing.T) {
	d, store, _ := scopedOperationFixture(t, false)
	task := NewTask()
	task.Protocol = "http"
	task.Progress = &Progress{}
	initTask(task)
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/legacy"}, Opts: &base.Options{Path: t.TempDir(), Name: "legacy.bin"}}
	if err := store.Put(bucketTask, task.ID, task.clone()); err != nil {
		t.Fatal(err)
	}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	// This lifecycle-free sample has no protocol object; exclude the actual
	// progress ticker while publishing its synthetic in-memory observation.
	task.lock.Lock()
	defer task.lock.Unlock()
	task.dataLock.Lock()
	task.Status = base.DownloadStatusRunning
	task.Progress.Downloaded = 12345
	task.Progress.Speed = 1024
	task.dataLock.Unlock()
	// Persisted status/progress deliberately lag the current legacy sample.
	snapshots, err := d.TaskCompletionSnapshotsForDataset(scopedDatasetA, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := snapshots[task.ID]
	if snapshot.CompletionView != nil || snapshot.Task.Status != base.DownloadStatusRunning || snapshot.Task.Progress.Downloaded != 12345 || snapshot.Task.Progress.Speed != 1024 {
		t.Fatal("legacy sample regressed to DB snapshot")
	}
	snapshot.Task.Progress.Downloaded = 0
	snapshot.Task.Meta.Req.URL = "https://fixture.invalid/copy-only"
	if task.Progress.Downloaded != 12345 || task.Meta.Req.URL == snapshot.Task.Meta.Req.URL {
		t.Fatal("legacy response borrowed live metadata")
	}
	task.dataLock.Lock()
	task.Completion = &TaskCompletionRef{Version: 1, CompletionID: "unexpected-live-ref"}
	task.dataLock.Unlock()
	if got, err := d.TaskCompletionSnapshotsForDataset(scopedDatasetA, []string{task.ID}); err == nil || got != nil {
		t.Fatal("legacy snapshot hid new live completion marker", got, err)
	}
	// The synthetic partial marker is not saved or executed by this getter.
	task.dataLock.Lock()
	task.Completion = nil
	task.Status = base.DownloadStatusPause
	task.dataLock.Unlock()
}
