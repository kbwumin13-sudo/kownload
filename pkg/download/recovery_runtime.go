package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Caller owns task.lock. No protocol callback acquires this lock in reverse.
func (d *Downloader) ensureTaskRecovery(task *Task, defaultsHash string) (*taskRecoveryOwner, error) {
	if owner := task.recoveryOwner.Load(); owner != nil {
		return owner, nil
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return nil, transfer.ErrRecoveryUnavailable
	}
	lock := d.errorPersistenceLock(task)
	lock.Lock()
	defer lock.Unlock()
	if d.closed.Load() || d.getTask(task.ID) != task {
		return nil, transfer.ErrRecoveryStale
	}
	owner, err := newTaskRecoveryOwner(store, task, defaultsHash)
	if err != nil {
		return nil, err
	}
	task.dataLock.Lock()
	task.Recovery = &TaskRecoveryRef{Version: owner.record.Version}
	task.dataLock.Unlock()
	owner.closed = &d.closed
	task.recoveryOwner.Store(owner)
	return owner, nil
}

func invalidateTaskRecovery(task *Task) {
	if owner := task.recoveryOwner.Load(); owner != nil {
		owner.invalidate()
	}
}

// persistSnapshot is used after protocol Store has produced an owned snapshot.
// Request/source revision, Task and checkpoint are committed together. None of
// these writes authorizes network IO or replenishes an episode.
func (o *taskRecoveryOwner) persistSnapshot(snapshot *Task, checkpoint any, writeCheckpoint bool, defaults ...string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var committed recoveryRecord
	err := o.store.UpdateAtomic(func(tx StorageTransaction) error {
		_, record, err := readRecovery(tx, o.taskID)
		if err != nil {
			return err
		}
		if record.Episode != o.record.Episode || record.Generation != o.record.Generation || record.SourceRevision != o.record.SourceRevision || record.SourceHash != o.record.SourceHash {
			return transfer.ErrRecoveryStale
		}
		if snapshot.ID != o.taskID || snapshot.Protocol != "http" || snapshot.Recovery == nil || snapshot.Recovery.Version != record.Version || snapshot.Meta == nil || snapshot.Meta.Req == nil {
			return transfer.ErrRecoveryHeld
		}
		if err := guardCompletionSnapshot(tx, snapshot); err != nil {
			return err
		}
		digest := record.ExecutionDefaultsHash
		if len(defaults) > 0 {
			digest = defaults[0]
		}
		hash, err := recoverySourceHash(snapshot.Meta.Req, digest)
		if err != nil {
			return err
		}
		if hash != record.SourceHash {
			record.SourceRevision++
			record.SourceHash = hash
			record.ExecutionDefaultsHash = digest
			if record.InitialAttempt.State == "clean-paused" {
				record.InitialAttempt.State = "issued"
				record.InitialAttempt.CheckpointHash = ""
			}
		}
		if writeCheckpoint {
			// HTTP Store always supplies a checkpoint. Do not silently replace
			// an atomic checkpoint deletion with a second non-atomic operation.
			if checkpoint == nil {
				return transfer.ErrRecoveryHeld
			}
			if err = tx.Put(bucketSave, o.taskID, checkpoint); err != nil {
				return err
			}
		}
		if err = tx.Put(bucketTask, o.taskID, snapshot); err != nil {
			return err
		}
		if err = tx.Put(bucketTaskRecovery, o.taskID, &record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		o.observeUnknownWrite()
		return recoveryStorageError(err)
	}
	if committed.SourceRevision != o.record.SourceRevision {
		o.token.SourceRevision = committed.SourceRevision
		o.permits = map[uint64]map[string]*transfer.RecoveryPermit{}
	}
	o.record = committed
	return nil
}

func (o *taskRecoveryOwner) currentLease(ctx context.Context) (*transfer.RecoveryLease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !o.active.Load() {
		return nil, transfer.ErrRecoveryStale
	}
	o.runContext = ctx
	return &transfer.RecoveryLease{Token: o.token, Round: o.record.NextAction, Owner: o, SourceHash: o.record.SourceHash}, nil
}

func (d *Downloader) recoveryGenerationBase(task *Task) uint64 {
	var record recoveryRecord
	if ok, err := d.storage.Get(bucketTaskRecovery, task.ID, &record); err == nil && ok && record.Generation > task.runGeneration {
		return record.Generation
	}
	return task.runGeneration
}

func checkpointDigest(value any) (string, error) {
	if value == nil {
		return "", transfer.ErrRecoveryHeld
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", transfer.ErrRecoveryUnavailable
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func (o *taskRecoveryOwner) markCleanPaused() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var committed recoveryRecord
	err := o.store.UpdateAtomic(func(tx StorageTransaction) error {
		_, record, err := readRecovery(tx, o.taskID)
		if err != nil {
			return err
		}
		if record.Generation != o.token.Generation || record.SourceRevision != o.token.SourceRevision || record.InitialAttempt.State != "issued" {
			return transfer.ErrRecoveryStale
		}
		for i := range record.Actions {
			action := &record.Actions[i]
			if action.Generation != record.Generation {
				continue
			}
			// The paid startup attempt is settled only by the caller's joined,
			// identity-checked clean-pause proof. Historical uncertain attempts
			// remain charged; they never regain a permit.
			if len(action.Members) == 1 && action.Members[0] == "start" {
				action.Finished = []string{"start"}
				action.State = "finished"
			} else if action.State != "finished" {
				return nil
			}
		}
		var checkpoint json.RawMessage
		found, err := tx.Get(bucketSave, o.taskID, &checkpoint)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		hash, err := checkpointDigest(checkpoint)
		if err != nil {
			return err
		}
		record.InitialAttempt.State = "clean-paused"
		record.InitialAttempt.CheckpointHash = hash
		if err = tx.Put(bucketTaskRecovery, o.taskID, &record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		o.observeUnknownWrite()
		return recoveryStorageError(err)
	}
	if committed.Version != 0 {
		o.record = committed
	}
	return nil
}

// Called while removing membership. No status/task lock is taken: completion
// persistence may already own those locks until Delete removes membership.
func invalidateDeletedRun(task *Task) {
	invalidateTaskRecovery(task)
	if task.Protocol == "http" {
		if cancel := task.recoveryRunCancel.Load(); cancel != nil {
			(*cancel)()
		}
	}
}
