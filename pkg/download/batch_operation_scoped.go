package download

import (
	"context"
	"errors"
)

// The expected dataset is call context, not part of the operation intent or a
// second ledger. The legacy entry points deliberately remain unscoped.
func (d *Downloader) CreateBatchOperationForDataset(ctx context.Context, expectedID string, request BatchOperationRequest) (BatchOperationResult, error) {
	if !datasetIDPattern.MatchString(expectedID) {
		return BatchOperationResult{}, ErrDatasetInvalid
	}
	return d.createBatchOperationInDataset(ctx, request, expectedID)
}

// QueryBatchOperationForDataset never attaches, restores or starts a task.
func (d *Downloader) QueryBatchOperationForDataset(expectedID, operationID string) (BatchOperationResult, error) {
	if !datasetIDPattern.MatchString(expectedID) {
		return BatchOperationResult{}, ErrDatasetInvalid
	}
	if !operationIDPattern.MatchString(operationID) {
		return BatchOperationResult{}, ErrOperationInvalid
	}
	snapshot, err := d.readScopedOperation(expectedID, operationID)
	return snapshot.result, err
}

func (d *Downloader) CancelBatchOperationForDataset(expectedID, operationID string) (BatchOperationResult, error) {
	if !datasetIDPattern.MatchString(expectedID) {
		return BatchOperationResult{}, ErrDatasetInvalid
	}
	return d.cancelBatchOperationInDataset(operationID, expectedID)
}

func isOperationDatasetError(err error) bool {
	return errors.Is(err, ErrDatasetMismatch) || errors.Is(err, ErrDatasetMissing) ||
		errors.Is(err, ErrDatasetInvalid) || errors.Is(err, ErrDatasetUnsupported) ||
		errors.Is(err, ErrDatasetViewClosed)
}

func guardOperationDataset(reader datasetReader, expectedID string) error {
	if expectedID == "" {
		return nil
	}
	return requireDataset(reader, expectedID)
}

func (d *Downloader) loadOperationInDataset(expectedID, id string) (*batchOperationRecord, error) {
	if expectedID == "" {
		return d.loadOperation(id)
	}
	var record *batchOperationRecord
	err := withDatasetReadView(d.storage, expectedID, func(reader datasetReader) error {
		var err error
		record, err = loadOperationFrom(reader, id)
		return err
	})
	if err != nil {
		return nil, operationReadError(err)
	}
	return record, nil
}

// All values are decoded, owned copies from one read view. No storage handle
// escapes into Q. Only a complete, same-domain creation receipt has a State;
// optional presence failures can retain that receipt without guessing absence.
type scopedOperationSnapshot struct {
	record *batchOperationRecord
	result BatchOperationResult
	tasks  map[string]*Task
}

func (d *Downloader) readScopedOperation(expectedID, id string) (scopedOperationSnapshot, error) {
	var snapshot scopedOperationSnapshot
	err := withDatasetReadView(d.storage, expectedID, func(reader datasetReader) error {
		var err error
		snapshot, err = readOperationSnapshotFrom(reader, id)
		return err
	})
	if err != nil {
		if isOperationDatasetError(err) || snapshot.result.State == "" {
			return scopedOperationSnapshot{}, operationReadError(err)
		}
		return snapshot, operationReadError(err)
	}
	return snapshot, nil
}

func readOperationSnapshotFrom(reader datasetReader, id string) (scopedOperationSnapshot, error) {
	var snapshot scopedOperationSnapshot
	record, err := loadOperationFrom(reader, id)
	if err != nil {
		return snapshot, err
	}
	if record != nil && record.Version == 2 {
		if record.Path == nil {
			return snapshot, ErrOperationConflict
		}
		if err := requireDataset(reader, record.Path.DatasetID); err != nil {
			return snapshot, err
		}
	}
	snapshot.record = record
	snapshot.tasks = make(map[string]*Task)
	snapshot.result, err = operationResultFrom(reader, id, record, snapshot.tasks)
	return snapshot, err
}

func operationReadError(err error) error {
	if err == nil || isOperationDatasetError(err) || errors.Is(err, ErrOperationConflict) {
		return err
	}
	return ErrOperationUnavailable
}

func (d *Downloader) publishOperationMemberInDataset(expectedID, id, hash string, index int, createdNow bool) error {
	if expectedID == "" {
		stored, err := d.loadOperation(id)
		if err == nil && stored != nil && stored.RequestHash == hash && index < len(stored.Members) && stored.Members[index].State == "created" {
			err = d.attachOperationTask(stored.Members[index].TaskID, createdNow)
		}
		return err
	}
	var task *Task
	err := withDatasetReadView(d.storage, expectedID, func(reader datasetReader) error {
		var err error
		task, err = readOperationMemberFrom(reader, id, hash, index)
		return err
	})
	if err != nil {
		return operationReadError(err)
	}
	if task == nil {
		return nil
	}
	d.lock.Lock()
	defer d.lock.Unlock()
	return d.attachOperationSnapshotLocked(task, createdNow)
}

// M->Q needs one member, not a full batch receipt. The manifest and target
// Task/reverse link are still decoded under the same domain-checked view.
func readOperationMemberFrom(reader datasetReader, id, hash string, index int) (*Task, error) {
	record, err := loadOperationFrom(reader, id)
	if err != nil {
		return nil, err
	}
	if record == nil || record.RequestHash != hash || index < 0 || index >= len(record.Members) {
		return nil, ErrOperationConflict
	}
	member := record.Members[index]
	if member.State != "created" {
		return nil, nil
	}
	var task Task
	present, err := reader.Get(bucketTask, member.TaskID, &task)
	if err != nil {
		return nil, ErrOperationUnavailable
	}
	if !present {
		return nil, nil
	} // explicit deletion remains historical
	if err = checkScopedMemberAssociation(reader, &task, record, index); err != nil {
		return nil, err
	}
	return &task, nil
}

// record has already passed valid() in this view. This check is constant work
// per member; it neither rescans nor reloads the complete manifest.
func checkScopedMemberAssociation(reader datasetReader, task *Task, record *batchOperationRecord, index int) error {
	if index < 0 || index >= len(record.Members) {
		return ErrOperationConflict
	}
	member := record.Members[index]
	ref := operationRef(record, index)
	if member.State != "created" || task.ID != member.TaskID || task.CreationOperation == nil || *task.CreationOperation != ref {
		return ErrOperationConflict
	}
	var link operationTaskLink
	present, err := reader.Get(bucketOperationTasks, member.TaskID, &link)
	if err != nil {
		return ErrOperationUnavailable
	}
	if !present || link.TaskID != member.TaskID || link.Ref != ref {
		return ErrOperationConflict
	}
	return nil
}

// operationMu spans this snapshot and Q, coordinating Delete/replay as before.
// The read view is the domain/association linearization point; Q does not open
// a later unscoped read or confer first-execution authority on a readback.
func (d *Downloader) attachScopedOperationMember(snapshot scopedOperationSnapshot, index int, createdNow bool) error {
	member := snapshot.result.Members[index]
	if member.Error == "association_conflict" {
		return ErrOperationConflict
	}
	task := snapshot.tasks[member.TaskID]
	if task == nil { // Historical creation whose Task was explicitly deleted.
		return nil
	}
	d.lock.Lock()
	defer d.lock.Unlock()
	return d.attachOperationSnapshotLocked(task, createdNow)
}

func (d *Downloader) reconcileOperationInDataset(expectedID, id string) (*batchOperationRecord, error) {
	if expectedID == "" {
		stored, err := d.loadOperation(id)
		if err == nil && !d.closed.Load() {
			err = d.reconcileOperationTasks(stored)
		}
		return stored, err
	}
	snapshot, err := d.readScopedOperation(expectedID, id)
	if err != nil || d.closed.Load() {
		return snapshot.record, err
	}
	for i, member := range snapshot.result.Members {
		if member.State == "created" {
			if err := d.attachScopedOperationMember(snapshot, i, false); err != nil {
				return snapshot.record, err
			}
		}
	}
	return snapshot.record, nil
}

func (d *Downloader) readAfterOperationErrorInDataset(expectedID, id string, cause error) (BatchOperationResult, error) {
	if expectedID == "" {
		return d.readAfterOperationError(id, cause)
	}
	if isOperationDatasetError(cause) {
		return BatchOperationResult{}, cause
	}
	result, readErr := d.QueryBatchOperationForDataset(expectedID, id)
	if isOperationDatasetError(readErr) {
		return BatchOperationResult{}, readErr
	}
	if result.State == "" {
		return BatchOperationResult{}, cause
	}
	return result, cause
}
