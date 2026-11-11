package download

import (
	"context"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// The callback may do only short publication metadata/atomic store work. Hash,
// network IO and joining writers happen before/after this function. Callers do
// not hold Task/owner/main locks and callbacks must not reenter the Fence.
// Existing Delete/Close admission precedes task and persistence locks; scheduler
// membership then uses main->status. No owner lock is held during store work.
func (d *Downloader) verifiedFence(ctx context.Context, dataset string, task *Task, token transfer.RecoveryToken, fn func(*Task, transfer.VerifiedCompletionRecord) error) error {
	if ctx == nil || fn == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	task.lock.Lock()
	defer task.lock.Unlock()
	persistence := d.errorPersistenceLock(task)
	persistence.Lock()
	defer persistence.Unlock()
	d.lock.Lock()
	task.statusLock.Lock()
	present := !d.closed.Load() && d.getTaskLocked(task.ID) == task
	d.lock.Unlock()
	defer task.statusLock.Unlock()
	if !present || task.runGeneration != token.Generation || task.getStatus() != base.DownloadStatusRunning {
		return transfer.ErrRecoveryStale
	}
	owner := task.recoveryOwner.Load()
	if owner == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := owner.CheckRecovery(token); err != nil {
		return err
	}
	owned, record, err := d.readVerifiedTask(dataset, task.ID)
	if err != nil {
		return err
	}
	if task.Completion == nil || owned.Completion == nil || *task.Completion != *owned.Completion {
		return transfer.ErrRecoveryHeld
	}
	return fn(owned, record)
}
