package download

import (
	"encoding/json"
	"errors"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"path/filepath"
)

// checkVerifiedAssociation binds the three-part completion state back to the
// same R/member/reverse association. It uses only the caller's owned view/tx.
func checkVerifiedAssociation(reader datasetReader, task *Task, record transfer.VerifiedCompletionRecord) error {
	if task == nil || task.CreationOperation == nil {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if err := checkCreationAssociationFrom(reader, task); err != nil {
		return completionStateError(TaskCompletionHeld, err)
	}
	r, err := loadOperationFrom(reader, task.CreationOperation.OperationID)
	if err != nil || r == nil || r.Integrity == nil || !r.Integrity.valid() || len(r.Members) != 1 || task.CreationOperation.Index != 0 {
		return completionStateError(TaskCompletionHeld, nil)
	}
	member := r.Members[0]
	if member.CompletionID != record.ID || r.Integrity.ExpectedSHA256 != record.ExpectedSHA256 {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if task.Meta == nil || task.Meta.Opts == nil || member.Options == nil || filepath.Clean(task.Meta.Opts.Path) != filepath.Clean(member.Options.Path) {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if record.FinalName != "" && (task.Meta.Res == nil || len(task.Meta.Res.Files) != 1 || task.Meta.Res.Name != "" || filepath.Base(task.Meta.SingleFilepath()) != record.FinalName) {
		return completionStateError(TaskCompletionHeld, nil)
	}
	raw, err := json.Marshal(task.Meta.Opts.Extra)
	if err != nil {
		return completionStateError(TaskCompletionHeld, nil)
	}
	var options struct {
		AutoTorrent *bool `json:"autoTorrent"`
		AutoExtract *bool `json:"autoExtract"`
	}
	if err = json.Unmarshal(raw, &options); err != nil || options.AutoTorrent == nil || options.AutoExtract == nil || *options.AutoTorrent || *options.AutoExtract {
		return completionStateError(TaskCompletionHeld, nil)
	}

	if _, err = verifiedServerTime(member.Options.Extra); err != nil {
		return err
	}
	return nil
}

func verifiedServerTime(extra any) (bool, error) {
	raw, err := json.Marshal(extra)
	if err != nil {
		return false, completionStateError(TaskCompletionHeld, nil)
	}
	var values map[string]any
	if err = decodeStoredJSON(raw, &values); err != nil {
		return false, completionStateError(TaskCompletionHeld, nil)
	}
	value, ok := values["verifiedUseServerCtime"].(bool)
	if !ok {
		return false, completionStateError(TaskCompletionHeld, nil)
	}
	return value, nil
}

func (d *Downloader) readVerifiedTask(dataset, id string) (*Task, transfer.VerifiedCompletionRecord, error) {
	var task *Task
	var record transfer.VerifiedCompletionRecord
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		var err error
		task, record, err = readTaskCompletion(reader, id)
		if err != nil {
			return err
		}
		return checkVerifiedAssociation(reader, task, record)
	})
	if err != nil {
		return nil, transfer.VerifiedCompletionRecord{}, err
	}
	return task, record, nil
}

// Kernel Save/C rechecks R/association in the SAME write transaction, without
// opening another view or taking any live Task/owner lock under storage.
type verifiedAssociatedStore struct {
	AtomicStorage
	dataset, id string
}

func (s verifiedAssociatedStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, s.dataset); err != nil {
			return err
		}
		task, record, err := readTaskCompletion(tx, s.id)
		if err != nil {
			return err
		}
		if err = checkVerifiedAssociation(tx, task, record); err != nil {
			return err
		}
		return fn(tx)
	})
}

// No wire authorization is reconstructed from sealed verification state.
// Legacy stores may predate both buckets; a missing optional bucket is the
// sole absence case, while other storage errors remain fail-closed.
func guardCompletionTransport(reader datasetReader, task *Task) error {
	var record, required json.RawMessage
	r, err := reader.Get(bucketTaskCompletion, task.ID, &record)
	if err != nil && !errors.Is(err, errTransactionBucketMissing) {
		return transfer.ErrRecoveryHeld
	}
	q, err := reader.Get(bucketCompletionRequired, task.ID, &required)
	if err != nil && !errors.Is(err, errTransactionBucketMissing) {
		return transfer.ErrRecoveryHeld
	}
	if task.Completion == nil && !r && !q {
		return nil
	}
	_, state, err := readTaskCompletion(reader, task.ID)
	if err != nil {
		return transfer.ErrRecoveryHeld
	}
	if state.Phase != transfer.CompletionPrepared {
		return transfer.ErrRecoveryWork
	}
	return nil
}
