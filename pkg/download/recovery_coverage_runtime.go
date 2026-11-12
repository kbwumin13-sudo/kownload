package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

type coverageSettlementAuthorization struct {
	DatasetID, Episode, SourceHash, DefaultsHash, IdentityHash string
	Sequence                                                   uint64
}

// Observation never grants IO. A known exhausted debit has no ambiguous write,
// so its joined bytes can still be measured; cancellation/unknown ACK cannot.
func (o *taskRecoveryOwner) CheckRecoveryObservation(token transfer.RecoveryToken) error {
	if token.Generation == 0 || o.coverageObservationHeld.Load() || !o.acceptsFailure(token) {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (o *taskRecoveryOwner) coverageRunContext(token transfer.RecoveryToken) (context.Context, error) {
	if err := o.CheckRecoveryObservation(token); err != nil {
		return nil, err
	}
	o.mu.Lock()
	ctx := o.runContext
	o.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return nil, transfer.ErrRecoveryStale
	}
	return ctx, nil
}

// Called under task.lock before Resolve/Start; it performs no protocol IO.
func (d *Downloader) observeTaskRecoveryCoverage(task *Task, lease *transfer.RecoveryLease) error {
	owner := task.recoveryOwner.Load()
	if owner == nil || lease == nil {
		return nil
	}
	owner.mu.Lock()
	record, err := cloneOperationJSON(owner.record)
	owner.mu.Unlock()
	if err != nil {
		return err
	}
	if record.Version != 3 && owner.coverageInstallAuthorization.Load() == nil {
		return nil
	}
	if _, err = d.DatasetIdentity(); err != nil {
		owner.coverageInstallAuthorization.Store(nil)
		if record.Version == 3 {
			return transfer.ErrRecoveryHeld
		}
		return nil
	}
	observer, ok := task.fetcher.(interface {
		ObserveRecoveryCoverage(*transfer.RecoveryLease, *transfer.HTTPCoverageState, int) error
	})
	if !ok {
		owner.coverageInstallAuthorization.Store(nil)
		if record.Version == 3 {
			return transfer.ErrRecoveryHeld
		}
		return nil
	}
	if err = observer.ObserveRecoveryCoverage(lease, record.Coverage, recoveryCoverageRangeLimit); err != nil {
		owner.coverageInstallAuthorization.Store(nil)
		if record.Version == 3 {
			return transfer.ErrRecoveryHeld
		}
	}
	return nil
}

// The lifecycle fence shares Delete/Close admission and the main->status order.
// No writer join, Sync, network IO, callbacks or owner lock spans this fence.
func (d *Downloader) coverageFence(ctx context.Context, task *Task, token transfer.RecoveryToken, status base.Status, fn func(*taskRecoveryOwner) error) error {
	if ctx == nil || ctx.Err() != nil || fn == nil {
		return transfer.ErrRecoveryStale
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
	if !present || task.runGeneration != token.Generation || task.getStatus() != status || ctx.Err() != nil {
		return transfer.ErrRecoveryStale
	}
	owner := task.recoveryOwner.Load()
	if owner == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := owner.CheckRecoveryObservation(token); err != nil {
		return err
	}
	return fn(owner)
}
func (d *Downloader) coverageExpected(dataset, id string, token transfer.RecoveryToken) (*Task, recoveryCoverageExpected, error) {
	var task *Task
	var expected recoveryCoverageExpected
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		var err error
		task, expected.Record, err = readRecoveryCoverageParts(reader, id)
		if err != nil {
			return err
		}
		expected.Token = token
		var raw json.RawMessage
		expected.CheckpointPresent, err = reader.Get(bucketSave, id, &raw)
		if err != nil {
			return transfer.ErrRecoveryUnavailable
		}
		if expected.CheckpointPresent {
			expected.CheckpointHash, err = checkpointDigest(raw)
		}
		return err
	})
	if err != nil {
		return nil, recoveryCoverageExpected{}, err
	}
	return task, expected, nil
}

// Caller releases task.lock before entering. Only the private new-enrollment
// token can attempt this once; its absence and durable issued never mint it.
func (d *Downloader) installTaskRecoveryCoverage(ctx context.Context, task *Task, lease *transfer.RecoveryLease) error {
	owner := task.recoveryOwner.Load()
	if owner == nil || lease == nil {
		return nil
	}
	token := owner.coverageInstallAuthorization.Swap(nil)
	if token == nil {
		return nil
	}
	if *token != lease.Token {
		return transfer.ErrRecoveryStale
	}
	// A missing fixed response (e.g. pre-Task interactive Resolve or weak ETag)
	// leaves the original v1 budget unchanged. The burned slot is not recreated.
	response, ok := task.fetcher.(interface {
		InitialRecoveryCoverage(transfer.RecoveryToken) (transfer.HTTPCoverageState, error)
	})
	if !ok {
		return nil
	}
	initial, err := response.InitialRecoveryCoverage(*token)
	if err != nil {
		return nil
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return nil
	}
	return d.coverageFence(ctx, task, *token, base.DownloadStatusRunning, func(owner *taskRecoveryOwner) error {
		if err := lease.Check(); err != nil {
			return err
		}
		if err := d.saveTask(task); err != nil {
			return err
		} // freeze resolved Meta before recapture
		_, expected, err := d.coverageExpected(identity.DatasetID, task.ID, *token)
		if err != nil {
			return err
		}
		record, err := installRecoveryCoverage(owner.store, identity.DatasetID, expected, initial)
		if err != nil {
			d.reconcileCoverageUnknown(identity.DatasetID, task, owner, expected, err)
			return err
		}
		owner.mu.Lock()
		owner.record = record
		owner.mu.Unlock()
		task.dataLock.Lock()
		task.Recovery = &TaskRecoveryRef{Version: record.Version}
		task.dataLock.Unlock()
		if ctx.Err() != nil || d.closed.Load() {
			owner.invalidate()
			return transfer.ErrRecoveryStale
		}
		return nil
	})
}

// recordRunRecoveryCoverage is invoked only after Wait, before Task completion
// or error publication. The protocol owns join/Sync and cached sealed Store;
// no Task/main/status/owner/persistence lock is held across it.
func (d *Downloader) recordRunRecoveryCoverage(task *Task, f fetcher.Fetcher, lease *transfer.RecoveryLease) *httpProtocol.RecoveryCoverageCheckpoint {
	if lease == nil || task.Protocol != "http" {
		return nil
	}
	owner := task.recoveryOwner.Load()
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	managed := owner.record.Version == 3
	owner.mu.Unlock()
	if !managed {
		return nil
	}
	ctx, err := owner.coverageRunContext(lease.Token)
	if err != nil {
		return nil
	}
	manager, ok := task.fetcherManager.(interface {
		StoreRecoveryCoverage(context.Context, fetcher.Fetcher, transfer.RecoveryToken) (*httpProtocol.RecoveryCoverageCheckpoint, error)
	})
	if !ok {
		return nil
	}
	seal, err := manager.StoreRecoveryCoverage(ctx, f, lease.Token)
	if err != nil {
		if !errors.Is(err, transfer.ErrRecoveryStale) && ctx.Err() == nil {
			d.markCoverageUnprovable(ctx, task, lease.Token)
		}
		return nil
	}
	if err = d.storeRunRecoveryCoverage(ctx, task, lease, seal, false); err != nil {
		return nil
	}
	return seal
}

func (d *Downloader) storeRunRecoveryCoverage(ctx context.Context, task *Task, lease *transfer.RecoveryLease, seal *httpProtocol.RecoveryCoverageCheckpoint, earned bool) error {
	if seal == nil || lease == nil || seal.Token() != lease.Token || seal.SourceHash() != lease.SourceHash {
		return transfer.ErrRecoveryStale
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return err
	}
	status := base.DownloadStatusRunning
	if earned {
		status = base.DownloadStatusError
	}
	return d.coverageFence(ctx, task, lease.Token, status, func(owner *taskRecoveryOwner) error {
		if earned && task.Completion != nil {
			return transfer.ErrRecoveryHeld
		} // keep S4 token through hash/publish/C
		if err := d.saveTask(task); err != nil {
			return err
		}
		snapshot, expected, err := d.coverageExpected(identity.DatasetID, task.ID, lease.Token)
		if err != nil {
			return err
		}
		if expected.Record.Version != 3 || expected.Record.Coverage == nil || expected.Record.SourceHash != seal.SourceHash() || expected.Record.Coverage.IdentityHash != seal.IdentityHash() {
			return transfer.ErrRecoveryHeld
		}
		checkpoint, err := seal.Checkpoint()
		if err != nil {
			return err
		}
		raw, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		digest, err := checkpointDigest(checkpoint)
		if err != nil {
			return err
		}
		plan, err := transfer.PlanHTTPCoverageCheckpoint(*expected.Record.Coverage, seal.CurrentRanges(), digest, recoveryCoverageRangeLimit)
		if err != nil {
			return err
		}
		next := recoveryCoverageUpdate{Snapshot: snapshot, Checkpoint: raw, Plan: plan}
		if earned {
			if plan.Settlement == nil || !plan.ThresholdReached && (!plan.WholeResourceCovered || !seal.Complete()) {
				return nil
			}
			next.NewEpisode, err = gonanoid.New()
			if err != nil {
				return transfer.ErrRecoveryUnavailable
			}
		}
		record, fresh, err := saveRecoveryCoverageCheckpoint(coverageCheckpointStore{owner.store, task.ID, checkpoint}, identity.DatasetID, expected, next)
		if err != nil {
			d.reconcileCoverageUnknown(identity.DatasetID, task, owner, expected, err)
			return err
		}
		// ACK grants RAM authority only while the original domain is still
		// authoritative. Do not hold owner.mu across a storage view.
		domainErr := withDatasetReadView(d.storage, identity.DatasetID, func(datasetReader) error { return nil })
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if owner.token != lease.Token {
			return transfer.ErrRecoveryStale
		}
		owner.record = record
		if fresh {
			owner.active.Store(false)
			if domainErr == nil && ctx.Err() == nil && !d.closed.Load() && task.runGeneration == lease.Token.Generation && record.Coverage != nil {
				owner.coverageSettlementAuthorization.Store(&coverageSettlementAuthorization{DatasetID: identity.DatasetID, Episode: record.Episode, SourceHash: record.SourceHash, DefaultsHash: record.ExecutionDefaultsHash, IdentityHash: record.Coverage.IdentityHash, Sequence: record.Coverage.SettlementSequence})
			}
		}
		return nil
	})
}

func (d *Downloader) markCoverageUnprovable(ctx context.Context, task *Task, token transfer.RecoveryToken) {
	identity, err := d.DatasetIdentity()
	if err != nil {
		return
	}
	_ = d.coverageFence(ctx, task, token, base.DownloadStatusRunning, func(owner *taskRecoveryOwner) error {
		_, expected, err := d.coverageExpected(identity.DatasetID, task.ID, token)
		if err != nil {
			return err
		}
		if expected.Record.Version != 3 || expected.Record.Coverage == nil {
			return transfer.ErrRecoveryHeld
		}
		var committed recoveryRecord
		err = owner.store.UpdateAtomic(func(tx StorageTransaction) error {
			_, record, err := recoveryCoverageCAS(tx, identity.DatasetID, expected)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(record, expected.Record) {
				return transfer.ErrRecoveryStale
			}
			record.Coverage.Status = "unprovable"
			if err = tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
				return err
			}
			committed = record
			return nil
		})
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if err != nil {
			owner.observeUnknownWrite(err)
			return err
		}
		owner.record = committed
		owner.coverageSettlementAuthorization.Store(nil)
		return nil
	})
}

// Preserve MemStorage's original checkpoint dynamic type for protocol Restore.
// The kernel remains protocol-neutral JSON; only the exact opaque sealed value
// can replace this JSON write. Bolt receives identical canonical JSON bytes.
type coverageCheckpointStore struct {
	AtomicStorage
	id         string
	checkpoint any
}
type coverageCheckpointTransaction struct {
	StorageTransaction
	id         string
	checkpoint any
}

func (s coverageCheckpointStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(coverageCheckpointTransaction{tx, s.id, s.checkpoint}) })
}
func (tx coverageCheckpointTransaction) Put(bucket, key string, value any) error {
	if bucket == bucketSave {
		if key != tx.id || tx.checkpoint == nil {
			return transfer.ErrRecoveryHeld
		}
		proposed, err := json.Marshal(value)
		if err != nil {
			return transfer.ErrRecoveryHeld
		}
		sealed, err := json.Marshal(tx.checkpoint)
		if err != nil || !bytes.Equal(proposed, sealed) {
			return transfer.ErrRecoveryHeld
		}
		return tx.StorageTransaction.Put(bucket, key, tx.checkpoint)
	}
	return tx.StorageTransaction.Put(bucket, key, value)
}

// An authoritative read may reconcile an unknown committed version/episode,
// but it never recreates either RAM authorization. The lifecycle fence remains
// held, while the view itself acquires no owner/live Task lock.
func (d *Downloader) reconcileCoverageUnknown(dataset string, task *Task, owner *taskRecoveryOwner, expected recoveryCoverageExpected, cause error) {
	_, current, err := d.coverageExpected(dataset, task.ID, expected.Token)
	owner.mu.Lock()
	owner.observeUnknownWrite(cause)
	if err == nil && current.Record.Generation == expected.Token.Generation && current.Record.SourceRevision == expected.Token.SourceRevision && current.Record.SourceHash == expected.Record.SourceHash && current.Record.ExecutionDefaultsHash == expected.Record.ExecutionDefaultsHash {
		owner.record = current.Record
	}
	version := owner.record.Version
	owner.active.Store(false)
	owner.coverageObservationHeld.Store(true)
	owner.coverageInstallAuthorization.Store(nil)
	owner.coverageSettlementAuthorization.Store(nil)
	owner.mu.Unlock()
	if version == 3 {
		task.dataLock.Lock()
		task.Recovery = &TaskRecoveryRef{Version: 3}
		task.dataLock.Unlock()
	}
}
