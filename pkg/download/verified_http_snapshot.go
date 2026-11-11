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
// The only subsequent live read is the C memory/API visibility gate, outside
// the View. Until C is API-visible this method returns no partially done Task.
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
	for _, snapshot := range snapshots {
		if snapshot.CompletionView == nil {
			continue
		}
		view := *snapshot.CompletionView
		if view.Phase == string(transfer.CompletionCommitted) && !d.gateCompletionVisibility(view).Available {
			return nil, completionStateError(TaskCompletionUnavailable, nil)
		}
	}
	return snapshots, nil
}
