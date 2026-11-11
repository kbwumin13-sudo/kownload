package download

import (
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// All authority below comes from one domain-checked view. No fetcher, owner,
// filesystem check or live Task lock is taken inside that view.
func completionViewFrom(reader datasetReader, dataset, id string) TaskCompletionView {
	view := TaskCompletionView{Version: 1, ScopeVersion: 1, DatasetID: dataset, TaskID: id}
	task, record, err := readTaskCompletion(reader, id)
	if err != nil || checkVerifiedAssociation(reader, task, record) != nil {
		return view
	}
	view.CompletionID = record.ID
	view.Available = true
	view.Phase = string(record.Phase)
	switch record.Phase {
	case transfer.CompletionVerified, transfer.CompletionPublished, transfer.CompletionCommitted:
		matched := true
		view.RecordedMatch = &matched
	}
	return view
}

// A committed disk record alone cannot announce API success after unknown ACK.
// Apply this final memory visibility gate only after releasing the DB view.
func (d *Downloader) gateCompletionVisibility(view TaskCompletionView) TaskCompletionView {
	if view.Phase != string(transfer.CompletionCommitted) {
		return view
	}
	task := d.getTask(view.TaskID)
	visible := false
	if task != nil {
		task.dataLock.RLock()
		visible = task.Status == base.DownloadStatusDone && task.Completion != nil && task.Completion.CompletionID == view.CompletionID
		task.dataLock.RUnlock()
	}
	if !visible {
		view.Available = false
		view.Phase = ""
		view.RecordedMatch = nil
		view.ErrorCategory = ""
	}
	return view
}

// Optional public Task decoration; returned values never include internal
// records, paths, resource digests, credentials or publication anchors.
func (d *Downloader) TaskCompletionViewsForDataset(dataset string, ids []string) (map[string]TaskCompletionView, error) {
	views := make(map[string]TaskCompletionView, len(ids))
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		for _, id := range ids {
			views[id] = completionViewFrom(reader, dataset, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id, view := range views {
		views[id] = d.gateCompletionVisibility(view)
	}
	return views, nil
}

func (d *Downloader) queryVerifiedOperation(dataset, id string) (VerifiedHTTPOperationResult, error) {
	if !operationIDPattern.MatchString(id) {
		return VerifiedHTTPOperationResult{}, ErrOperationInvalid
	}
	var result VerifiedHTTPOperationResult
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		snapshot, err := readOperationSnapshotFrom(reader, id)
		if err != nil {
			return err
		}
		if snapshot.record != nil && (snapshot.record.Integrity == nil || !snapshot.record.Integrity.valid()) {
			return ErrOperationConflict
		}
		result.Operation = snapshot.result
		result.CompletionViews = []TaskCompletionView{}
		for _, taskID := range snapshot.result.TaskIDs {
			result.CompletionViews = append(result.CompletionViews, completionViewFrom(reader, dataset, taskID))
		}
		return nil
	})
	if err != nil {
		return VerifiedHTTPOperationResult{}, operationReadError(err)
	}
	for i, view := range result.CompletionViews {
		result.CompletionViews[i] = d.gateCompletionVisibility(view)
	}
	return result, nil
}
