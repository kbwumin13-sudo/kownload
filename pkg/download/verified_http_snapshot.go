package download

import (
	"encoding/json"
	"errors"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// These owned values are response-only. The caller must use the Task and its
// optional projection together, without replacing either from a second lookup.
// No internal completion record, filesystem identity or digest is exposed.
type TaskCompletionSnapshot struct {
	Task           *Task
	CompletionView *TaskCompletionView
}

// IDs are the caller's captured Task selection. Task, R/member/reverse and the
// completion triple are read under one actual domain-checked View. An absent
// selected ID is omitted; a malformed Task is an unavailable read, not legacy.
// Subsequent live reads are the C memory/API visibility gate and legacy
// progress snapshots, both outside the View. Until C is API-visible this method returns no partially done Task.
func (d *Downloader) TaskCompletionSnapshotsForDataset(dataset string, ids []string) (map[string]TaskCompletionSnapshot, error) {
	snapshots := make(map[string]TaskCompletionSnapshot, len(ids))
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		for _, id := range ids {
			var owned *Task
			found, err := reader.Get(bucketTask, id, &owned)
			if err != nil {
				return completionStateError(TaskCompletionUnavailable, err)
			}
			if !found {
				continue
			}
			if owned == nil || owned.ID != id {
				return completionStateError(TaskCompletionUnavailable, nil)
			}
			snapshot := TaskCompletionSnapshot{Task: owned}
			var record, required json.RawMessage
			present, err := reader.Get(bucketTaskCompletion, id, &record)
			if err != nil && !errors.Is(err, errTransactionBucketMissing) {
				return completionStateError(TaskCompletionUnavailable, err)
			}
			requiredPresent, err := reader.Get(bucketCompletionRequired, id, &required)
			if err != nil && !errors.Is(err, errTransactionBucketMissing) {
				return completionStateError(TaskCompletionUnavailable, err)
			}
			if owned.Completion != nil || present || requiredPresent {
				view := completionViewFrom(reader, dataset, id)
				snapshot.CompletionView = &view
			}
			snapshots[id] = snapshot
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id, snapshot := range snapshots {
		if snapshot.CompletionView == nil {
			// Proven all-absent legacy retains the existing in-memory progress
			// semantics. Clone only after the View has released storage locks.
			live := d.getTask(id)
			if live == nil {
				delete(snapshots, id)
				continue
			}
			owned := live.clone()
			if owned.Completion != nil {
				return nil, completionStateError(TaskCompletionUnavailable, nil)
			}
			snapshot.Task = owned
			snapshots[id] = snapshot
			continue
		}
		view := *snapshot.CompletionView
		if view.Phase == string(transfer.CompletionCommitted) && !d.gateCompletionVisibility(view).Available {
			return nil, completionStateError(TaskCompletionUnavailable, nil)
		}
	}
	return snapshots, nil
}
