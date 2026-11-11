package download

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

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
