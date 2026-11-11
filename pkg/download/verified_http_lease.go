package download

import (
	"context"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

// beginVerificationRun advances only a sealed artifact's lifecycle generation.
// It grants NO transport work/initial permit, is never installed on HTTP, and
// cannot reopen a writer. The hooks expose only Check/Fence to the verifier.
// Existing episode, issued evidence, charges, checkpoint and notBefore survive.
func (o *taskRecoveryOwner) beginVerificationRun(ctx context.Context, dataset string, generation uint64) (*transfer.RecoveryLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active.Store(false)
	var committed recoveryRecord
	err := o.store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		task, record, err := readRecovery(tx, o.taskID)
		if err != nil {
			return err
		}
		if record.InitialAttempt.State != "issued" && record.InitialAttempt.State != "clean-paused" {
			return transfer.ErrRecoveryHeld
		}
		_, completion, err := readTaskCompletion(tx, o.taskID)
		if err != nil {
			return err
		}
		if err = checkVerifiedAssociation(tx, &task, completion); err != nil {
			return err
		}
		if completion.Phase == transfer.CompletionPrepared || completion.Phase == transfer.CompletionCommitted || generation <= record.Generation {
			return transfer.ErrRecoveryHeld
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		record.Generation = generation
		record.InitialAttempt.Generation = generation
		if !record.valid(o.taskID) {
			return transfer.ErrRecoveryHeld
		}
		if err = tx.Put(bucketTaskRecovery, o.taskID, &record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		o.observeUnknownWrite()
		return nil, recoveryStorageError(err)
	}
	o.verificationOnly = true
	o.record = committed
	o.token = transfer.RecoveryToken{TaskID: o.taskID, Episode: committed.Episode, Generation: committed.Generation, SourceRevision: committed.SourceRevision}
	o.runContext = ctx
	o.permits = map[uint64]map[string]*transfer.RecoveryPermit{}
	o.active.Store(true)
	return &transfer.RecoveryLease{SourceHash: committed.SourceHash, Token: o.token, Round: committed.NextAction, Owner: o}, nil
}

func (o *taskRecoveryOwner) markVerificationOnly(token transfer.RecoveryToken) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active.Load() || o.token != token {
		return transfer.ErrRecoveryStale
	}
	o.verificationOnly = true
	o.permits = map[uint64]map[string]*transfer.RecoveryPermit{}
	return nil
}

// Selection only: sealed verification and confirmed-C reconciliation need no
// transport slot and must not pause another download to obtain one. No owner,
// permit, run or write is created by this same-domain phase read.
func (d *Downloader) verificationOnlySelection(tasks []*Task) (map[*Task]bool, int, error) {
	result := make(map[*Task]bool)
	networkCount := 0
	var dataset string
	for _, task := range tasks {
		task.dataLock.RLock()
		present := task.Completion != nil
		task.dataLock.RUnlock()
		if !present {
			networkCount++
			continue
		}
		if dataset == "" {
			identity, err := d.DatasetIdentity()
			if err != nil {
				return nil, 0, err
			}
			dataset = identity.DatasetID
		}
		_, record, err := d.readVerifiedTask(dataset, task.ID)
		if err != nil {
			return nil, 0, err
		}
		if record.Phase != transfer.CompletionPrepared {
			result[task] = true
		} else {
			networkCount++
		}
	}
	return result, networkCount, nil
}
