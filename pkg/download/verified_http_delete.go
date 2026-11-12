package download

import (
	"encoding/json"
	"errors"
)

var errCompletionDeleteClear = errors.New("completion_force_delete_preflight_clear")

// Temporary v1 limitation: owned artifact removal is not implemented. Reject
// the entire force selection before touching membership, queues, records or
// files. Task-only deletion remains supported. The existing atomic write API
// is used as a rollback-only snapshot: no Put/Delete, and BOTH outcomes abort
// the transaction, including the all-absent legacy case. Thus even Bolt's
// metadata is unchanged by this preflight. Never take a live Task/owner lock
// from inside the callback; stored Tasks are immutable owned decodes.
func (d *Downloader) checkCompletionForceDelete(selected []*Task) error {
	if len(selected) == 0 {
		return nil
	}
	// Capture live marker presence before entering storage. The caller holds
	// operationMu, which serializes every completion enrollment/publication.
	livePresence := false
	for _, task := range selected {
		task.dataLock.RLock()
		present := task.Completion != nil || mcpTaskPresence(task)
		task.dataLock.RUnlock()
		livePresence = livePresence || present
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return ErrVerifiedHTTPUnsupportedOption
	}
	err := store.UpdateAtomic(func(tx StorageTransaction) error {
		if livePresence {
			return ErrVerifiedHTTPUnsupportedOption
		}
		for _, task := range selected {
			var owned *Task
			found, err := tx.Get(bucketTask, task.ID, &owned)
			if err != nil {
				return ErrVerifiedHTTPUnsupportedOption
			}
			if found && (owned == nil || owned.Completion != nil || mcpTaskPresence(owned)) {
				return ErrVerifiedHTTPUnsupportedOption
			}
			var link operationTaskLink
			linked, err := tx.Get(bucketOperationTasks, task.ID, &link)
			if err != nil || linked && (link.Ref.Version == 2 || link.Ref.Namespace == MCPWriteNamespace) {
				return ErrVerifiedHTTPUnsupportedOption
			}
			for _, bucket := range []string{bucketTaskCompletion, bucketCompletionRequired} {
				var raw json.RawMessage
				present, err := tx.Get(bucket, task.ID, &raw)
				if err != nil && !errors.Is(err, errTransactionBucketMissing) {
					return ErrVerifiedHTTPUnsupportedOption
				}
				if present {
					return ErrVerifiedHTTPUnsupportedOption
				}
			}
		}
		return errCompletionDeleteClear
	})
	if errors.Is(err, errCompletionDeleteClear) {
		return nil
	}
	return ErrVerifiedHTTPUnsupportedOption
}
