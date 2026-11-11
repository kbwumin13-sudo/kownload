package download

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func (o *taskRecoveryOwner) verificationContext(token transfer.RecoveryToken) (context.Context, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active.Load() || o.token != token || o.runContext == nil || o.runContext.Err() != nil {
		return nil, transfer.ErrRecoveryStale
	}
	return o.runContext, nil
}

// Called only inside verifiedFence after C is confirmed by authoritative read.
// It grants no effects; the caller separately retains the fresh ACK decision.
func (d *Downloader) acceptVerifiedCompletion(task *Task, owned *Task) error {
	if owned.Status != base.DownloadStatusDone || owned.Completion == nil || owned.Progress == nil {
		return transfer.ErrRecoveryHeld
	}
	task.dataLock.Lock()
	task.Meta, task.Progress = owned.Meta, owned.Progress
	task.Status, task.UpdatedAt = owned.Status, owned.UpdatedAt
	task.clearReadPublishedLocked()
	task.timer.Pause()
	task.dataLock.Unlock()
	return nil
}

func (d *Downloader) completeVerifiedHTTP(task *Task, f fetcher.Fetcher, generation uint64, lease *transfer.RecoveryLease) (bool, error) {
	if lease == nil || lease.Token.Generation != generation || d.verifiedCompleter == nil {
		return false, transfer.ErrRecoveryHeld
	}
	owner := task.recoveryOwner.Load()
	if owner == nil {
		return false, transfer.ErrRecoveryHeld
	}
	ctx, err := owner.verificationContext(lease.Token)
	if err != nil {
		return false, err
	}
	if f != nil {
		writer, ok := f.(interface{ JoinVerifiedWriters(context.Context) error })
		if !ok {
			return false, transfer.ErrRecoveryHeld
		}
		if err = writer.JoinVerifiedWriters(ctx); err != nil {
			return false, err
		}
	} else if !task.verificationOnly.Load() {
		return false, transfer.ErrRecoveryHeld
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return false, err
	}
	dataset := identity.DatasetID
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return false, transfer.ErrRecoveryHeld
	}
	store = verifiedAssociatedStore{store, dataset, task.ID}
	var directory *transfer.CompletionDirectory
	var file *os.File
	var current transfer.VerifiedCompletionRecord
	var finalProgress *Progress
	err = d.verifiedFence(ctx, dataset, task, lease.Token, func(owned *Task, record transfer.VerifiedCompletionRecord) error {
		if owned.Meta == nil || owned.Meta.Opts == nil || owned.Meta.Res == nil || len(owned.Meta.Res.Files) != 1 {
			return transfer.ErrRecoveryHeld
		}
		var err error
		directory, err = transfer.OpenCompletionDirectory(filepath.Clean(owned.Meta.Opts.Path))
		if err != nil {
			return err
		}
		file, err = directory.OpenStaging(record)
		if err != nil {
			return err
		}
		if err = file.Sync(); err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if record.Phase == transfer.CompletionPrepared {
			if owned.Meta.Res.Size > 0 && info.Size() != owned.Meta.Res.Size {
				return completionStateError(TaskCompletionHeld, nil)
			}
			// Seal only after this run's network writers are joined. Store the
			// resolved final total before C without exposing a success state.
			if owned.Meta.Res.Size == 0 {
				owned.Meta.Res.Size = info.Size()
				owned.Meta.Res.Files[0].Size = info.Size()
			}
			owned.Progress = &Progress{Downloaded: info.Size(), Used: task.timer.Used()}
			if err = owner.persistSnapshot(owned, nil, false); err != nil {
				return err
			}
			next := record
			next.Size = info.Size()
			next.Phase = transfer.CompletionWaiting
			if err = saveTaskCompletionState(store, dataset, lease.Token, record, next); err != nil {
				return err
			}
			record = next
			task.dataLock.Lock()
			task.Meta = owned.Meta
			task.Progress = owned.Progress
			task.clearReadPublishedLocked()
			task.dataLock.Unlock()
		}
		if record.Phase == transfer.CompletionCommitted {
			return transfer.ErrRecoveryHeld
		}
		if err = owner.markVerificationOnly(lease.Token); err != nil {
			return err
		}
		task.verificationOnly.Store(true)
		current = record
		finalProgress = &Progress{Downloaded: record.Size, Used: task.timer.Used()}
		// The policy is read from R, never the parsed Task Extra or current cfg.
		r, err := loadOperationFrom(d.storage, task.CreationOperation.OperationID)
		if err != nil || r == nil || len(r.Members) != 1 {
			return transfer.ErrRecoveryHeld
		}
		times, err := verifiedServerTime(r.Members[0].Options.Extra)
		if err != nil {
			return err
		}
		if record.Phase == transfer.CompletionWaiting && times && owned.Meta.Res.Files[0].Ctime != nil {
			if err = directory.SetStagingServerTimes(file, record, time.Now(), *owned.Meta.Res.Files[0].Ctime); err != nil {
				return err
			}
		}
		return nil
	})
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if directory != nil {
			_ = directory.Close()
		}
	}()
	if err != nil {
		return false, err
	}
	// No hash slot occupies a network slot, and no second scheduler is used.
	d.notifyRunning()
	permit, err := d.verifiedCompleter.Complete(ctx, directory, file, current, transfer.VerifiedCompletionHooks{
		CheckRun: lease.Check,
		Fence: func(callCtx context.Context, fn func() error) error {
			return d.verifiedFence(callCtx, dataset, task, lease.Token, func(*Task, transfer.VerifiedCompletionRecord) error { return fn() })
		},
		Save: func(next transfer.VerifiedCompletionRecord) error {
			if err := saveTaskCompletionState(store, dataset, lease.Token, current, next); err != nil {
				return err
			}
			current = next
			return nil
		},
		Commit: func(next transfer.VerifiedCompletionRecord) (bool, error) {
			fresh, err := commitTaskCompletionState(store, dataset, lease.Token, current, next, finalProgress)
			if err != nil {
				return false, err
			}
			owned, record, err := d.readVerifiedTask(dataset, task.ID)
			if err != nil || record.Phase != transfer.CompletionCommitted {
				return false, completionStateError(TaskCompletionUnavailable, err)
			}
			if err = d.acceptVerifiedCompletion(task, owned); err != nil {
				return false, err
			}
			current = record
			return fresh, nil
		},
	})
	if err != nil {
		// C may have committed despite a lost ACK. Confirm only the original
		// CID, rebuild API done, and never reconstruct a fresh success permit.
		confirmed := false
		_ = d.verifiedFence(ctx, dataset, task, lease.Token, func(owned *Task, record transfer.VerifiedCompletionRecord) error {
			if record.Phase != transfer.CompletionCommitted {
				return transfer.ErrRecoveryHeld
			}
			if e := d.acceptVerifiedCompletion(task, owned); e != nil {
				return e
			}
			confirmed = true
			return nil
		})
		if confirmed {
			return false, nil
		}
		return false, err
	}
	return permit.Fresh, nil
}

// A live event is emitted only by the caller holding this call's fresh C ACK
// result, after the Fence has published API done. Reads never call this method.
func (d *Downloader) publishVerifiedHTTPDone(task *Task) {
	if d.closed.Load() || d.getTask(task.ID) != task || d.verifiedDone == nil {
		return
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return
	}
	task.dataLock.RLock()
	ref := task.Completion
	status := task.Status
	task.dataLock.RUnlock()
	if ref == nil || status != base.DownloadStatusDone {
		return
	}
	d.verifiedDone.publish(VerifiedHTTPDoneEvent{Version: 1, DatasetID: identity.DatasetID, TaskID: task.ID, CompletionID: ref.CompletionID})
}

// Ordinary saves may update pause/error/progress, but cannot overwrite C or
// replace its frozen path/intent. This check runs in their existing transaction.
func guardCompletionSnapshot(tx StorageTransaction, snapshot *Task) error {
	var a, b any
	r, err := tx.Get(bucketTaskCompletion, snapshot.ID, &a)
	if err != nil && !errors.Is(err, errTransactionBucketMissing) {
		return transfer.ErrRecoveryHeld
	}
	q, err := tx.Get(bucketCompletionRequired, snapshot.ID, &b)
	if err != nil && !errors.Is(err, errTransactionBucketMissing) {
		return transfer.ErrRecoveryHeld
	}
	if snapshot.Completion == nil && !r && !q {
		return nil
	}
	stored, record, err := readTaskCompletion(tx, snapshot.ID)
	if err != nil {
		return err
	}
	if record.Phase == transfer.CompletionCommitted || snapshot.Completion == nil || *snapshot.Completion != *stored.Completion {
		return transfer.ErrRecoveryHeld
	}
	return checkVerifiedAssociation(tx, snapshot, record)
}
