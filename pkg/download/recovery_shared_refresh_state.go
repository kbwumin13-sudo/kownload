package download

import (
	"encoding/json"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// commitSharedRecoveryRefresh commits storage facts only, never RAM/run/IO
// authority. The caller owns the active same-D/E/G/S/action Fence, writer
// quiescence and the HTTP-specific RedirectURL/PublicSource patch. It supplies
// an owned complete Store object, preserving connections and representation
// validators. Raw Task source, paths and all unrelated checkpoint fields must
// not be rewritten by that protocol patch.
//
// The whole CURRENT triplet/token/record/required and independent bucketSave
// existence/hash are CASed in one transaction. Double absence is a legal CAS,
// not evidence of first-IO eligibility. Only checkpoint and record are written.
// Unknown ACK returns a zero record even if committed. A later authoritative
// query can reconcile storage; it cannot reconstruct a continuation wave.
func commitSharedRecoveryRefresh(store AtomicStorage, dataset string, expected recoverySharedExpected, actionID uint64, ownedCheckpoint json.RawMessage) (recoveryRecord, error) {
	if store == nil {
		return recoveryRecord{}, transfer.ErrRecoveryUnavailable
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	checkpoint := append(json.RawMessage(nil), ownedCheckpoint...)
	var object map[string]json.RawMessage
	if json.Unmarshal(checkpoint, &object) != nil || object == nil {
		return recoveryRecord{}, transfer.ErrRecoveryWork
	}
	var result recoveryRecord
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, required, err := sharedRecoveryCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if record.Version != recoverySharedVersion {
			return transfer.ErrRecoveryHeld
		}
		if actionID == 0 || actionID != record.NextAction {
			return transfer.ErrRecoveryStale
		}
		parent := &record.Actions[actionID-1] // the validated triplet has contiguous IDs
		if parent.Kind != string(recoveryRefreshAction) || parent.State != "charged" || len(parent.Finished) != 0 ||
			parent.Generation != record.Generation || parent.SourceRevision != record.SourceRevision {
			return transfer.ErrRecoveryStale
		}
		if record.SourceRevision == ^uint64(0) {
			return transfer.ErrRecoveryHeld
		}
		record.SourceRevision++
		parent.Finished = append([]string(nil), parent.Members...)
		parent.State = "finished"
		if record.Coverage != nil {
			// A changed full URL cannot prove continuous identity. Keep C, B,
			// sequence, identity/size and the OLD coverage checkpoint hash.
			record.Coverage.Status = "unprovable"
		}
		if validateRecoveryTriplet(task, record, required) != nil {
			return transfer.ErrRecoveryHeld
		}
		if err := tx.Put(bucketSave, task.ID, checkpoint); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return recoveryRecord{}, sharedRecoveryStoreError(err)
	}
	return result, nil
}
