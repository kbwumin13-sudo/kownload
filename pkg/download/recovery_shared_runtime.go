package download

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// A continuation is a process-local consequence of one confirmed deferral ACK.
// It is never reconstructed from a charged action, a query, or a clock wakeup.
// Until is included so an ambiguous extension cannot use an older grant.
type sharedRecoveryContinuation struct {
	DatasetID, Episode, SourceHash, DefaultsHash string
	Generation, SourceRevision, ActionID         uint64
	Until                                        string
	Members                                      []string
}

// sharedRecoveryExpected captures a single owned view, including the CURRENT
// protocol checkpoint. Coverage.CheckpointHash can describe an older seal and
// is deliberately not used as this CAS coordinate.
func (d *Downloader) sharedRecoveryExpected(dataset, id string) (*Task, recoverySharedExpected, error) {
	return captureSharedRecoveryExpected(d.storage, dataset, id)
}

func captureSharedRecoveryExpected(store Storage, dataset, id string) (*Task, recoverySharedExpected, error) {
	var task *Task
	var expected recoverySharedExpected
	err := withDatasetReadView(store, dataset, func(reader datasetReader) error {
		var err error
		task, expected.Record, expected.Required, err = readSharedRecovery(reader, id)
		if err != nil {
			return err
		}
		r := expected.Record
		expected.Token = transfer.RecoveryToken{TaskID: id, Episode: r.Episode,
			Generation: r.Generation, SourceRevision: r.SourceRevision}
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
		return nil, recoverySharedExpected{}, err
	}
	return task, expected, nil
}

// A legal future Until is a scheduling state, not an incompatible declaration.
// Shape-only Setup/read guards must not cache this as recoveryBlocked. Runtime
// admission, and each actual IO gate, call this separately from shape checks.
func checkSharedRecoveryUntil(record recoveryRecord, now time.Time) error {
	if record.Version != recoverySharedVersion {
		return transfer.ErrRecoveryHeld
	}
	until, err := parseRecoveryUntil(record.NotBefore)
	if err != nil {
		return err
	}
	if until.After(now) {
		return &transfer.DeferredError{Until: until}
	}
	return nil
}

func (grant *sharedRecoveryContinuation) matches(dataset string, record recoveryRecord) bool {
	if grant == nil || dataset == "" || grant.DatasetID != dataset ||
		grant.Episode != record.Episode || grant.SourceHash != record.SourceHash ||
		grant.DefaultsHash != record.ExecutionDefaultsHash || grant.SourceRevision != record.SourceRevision ||
		grant.Generation == 0 || grant.Generation > record.Generation ||
		grant.ActionID == 0 || grant.ActionID != record.NextAction ||
		grant.Until != record.NotBefore || !record.validShared(record.TaskID) {
		return false
	}
	parent := record.Actions[grant.ActionID-1]
	return parent.Kind == string(recoveryDeferralAction) && parent.State == "charged" &&
		len(parent.Finished) == 0 && parent.Generation == grant.Generation &&
		parent.SourceRevision == grant.SourceRevision && sameRecoveryMembers(parent.Members, grant.Members)
}

// Caller mutates only a CURRENT owned record after sharedRecoveryCAS inside
// the EXISTING begin/admission UpdateAtomic callback. The final ledger Put and
// ACK must include this closure, the next generation and any transport debit.
// Closing the parent alone does not issue a continuation or mean task success.
func closeSharedDeferralAtAdmission(record *recoveryRecord, now time.Time) (uint64, error) {
	if record == nil || !record.validShared(record.TaskID) {
		return 0, transfer.ErrRecoveryHeld
	}
	if err := checkSharedRecoveryUntil(*record, now); err != nil {
		return 0, err
	}
	if len(record.Actions) == 0 {
		return 0, nil
	}
	parent := &record.Actions[len(record.Actions)-1]
	if parent.Kind != string(recoveryDeferralAction) || parent.State != "charged" {
		return 0, nil
	}
	// A restarted/unknown-ACK owner can close this old wait only as part of
	// its conservatively charged admission. It receives no free grant here.
	parent.Finished = append([]string{}, parent.Members...)
	parent.State = "finished"
	return parent.ID, nil
}

// bindSharedDataset is called only at Core's inactive lifecycle boundary.
// Capturing/binding a domain does not upgrade the triplet or issue IO.
func (o *taskRecoveryOwner) bindSharedDataset(dataset string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if dataset == "" || o.active.Load() || o.sharedDataset != "" && o.sharedDataset != dataset {
		return transfer.ErrRecoveryHeld
	}
	_, expected, err := captureSharedRecoveryExpected(o.sharedReadStore(), dataset, o.taskID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected.Record, o.record) {
		return transfer.ErrRecoveryStale
	}
	o.sharedDataset = dataset
	return nil
}

func (o *taskRecoveryOwner) sharedCurrentLocked(ctx context.Context, token transfer.RecoveryToken) error {
	if ctx == nil || ctx.Err() != nil || !o.active.Load() || token != o.token ||
		o.record.Version != recoverySharedVersion || o.sharedDataset == "" || o.verificationOnly ||
		o.runContext == nil || o.runContext.Err() != nil || o.closed != nil && o.closed.Load() {
		return transfer.ErrRecoveryStale
	}
	return nil
}

// Called with owner.mu held, outside any DB callback. This is IO/result
// revocation, never an attempt to infer a permit from a committed record.
func (o *taskRecoveryOwner) sharedUnknownLocked() {
	o.active.Store(false)
	o.sharedInitialEligible.Store(false)
	o.sharedContinuation.Store(nil)
	o.sharedActionPermit = nil
	o.retryAuthorization.Store(nil)
	o.coverageNewEnrollment.Store(false)
	o.coverageInstallAuthorization.Store(nil)
	o.coverageSettlementAuthorization.Store(nil)
	o.coverageObservationHeld.Store(true)
}

// Caller holds owner.mu and no Task/live/HTTP lock. The view is acquired only
// after releasing owner.mu, and this helper returns with owner.mu held again.
// A read is not a permit: after it completes, the exact token/action/record and
// request lifetime must still match before an ACK may create or retain RAM.
func (o *taskRecoveryOwner) observeSharedActionLocked(ctx context.Context, token transfer.RecoveryToken, record recoveryRecord) (recoverySharedExpected, error) {
	want, err := cloneOperationJSON(record)
	if err != nil {
		o.sharedUnknownLocked()
		return recoverySharedExpected{}, transfer.ErrRecoveryUnavailable
	}
	store, dataset, taskID := o.sharedReadStore(), o.sharedDataset, o.taskID
	o.mu.Unlock()
	_, actual, readErr := captureSharedRecoveryExpected(store, dataset, taskID)
	o.mu.Lock()
	if err = o.sharedCurrentLocked(ctx, token); err == nil && (!reflect.DeepEqual(o.record, want) || readErr == nil && !reflect.DeepEqual(actual.Record, want)) {
		err = transfer.ErrRecoveryStale
	}
	if err == nil {
		err = readErr
	}
	if err != nil {
		// This older read/ACK must not revoke a subsequently admitted run or
		// a newer action. Only this same token/action loses its candidate.
		if o.token == token && o.record.Episode == want.Episode && o.record.Generation == want.Generation && o.record.SourceRevision == want.SourceRevision && o.record.NextAction == want.NextAction {
			o.sharedUnknownLocked()
		}
		return recoverySharedExpected{}, err
	}
	return actual, nil
}

// This callback is invoked after wave drain (or after Resolve returned and
// released Task locks). It never runs on the synchronous HTTP Start stack.
func (o *taskRecoveryOwner) prepareSharedAction(ctx context.Context, token transfer.RecoveryToken) error {
	o.mu.Lock()
	prepare := o.sharedCheckpoint
	o.mu.Unlock()
	if prepare == nil {
		return transfer.ErrRecoveryHeld
	}
	return prepare(ctx, token)
}

// ReserveRecoveryAction is invoked by the independent HTTP coordinator, never
// under a protocol/Task/main lock. A refresh permit covers only the frozen
// resolver plan; it is distinct from a deferral's future-wave continuation.
func (o *taskRecoveryOwner) ReserveRecoveryAction(ctx context.Context, token transfer.RecoveryToken, seen uint64, kind transfer.RecoveryActionKind, work string, input []string, until time.Time) (*transfer.RecoveryActionPermit, error) {
	members, err := recoveryMembers(input)
	if err != nil || !hasRecoveryMember(members, work) ||
		(kind != transfer.RecoveryActionDeferral && kind != transfer.RecoveryActionRefresh) ||
		(kind == transfer.RecoveryActionDeferral && until.IsZero()) ||
		(kind == transfer.RecoveryActionRefresh && !until.IsZero()) {
		return nil, transfer.ErrRecoveryWork
	}
	if err := o.prepareSharedAction(ctx, token); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.sharedCurrentLocked(ctx, token); err != nil {
		return nil, err
	}
	if kind == transfer.RecoveryActionRefresh {
		if err := checkSharedRecoveryUntil(o.record, time.Now()); err != nil {
			return nil, err
		}
	}
	expected, err := o.observeSharedActionLocked(ctx, token, o.record)
	if err != nil {
		return nil, err
	}
	untilText := ""
	if kind == transfer.RecoveryActionDeferral {
		untilText = until.UTC().Format(time.RFC3339Nano)
	}
	record, id, fresh, err := reserveSharedRecoveryAction(o.store, o.sharedDataset, expected,
		recoverySharedAction{Kind: recoveryActionKind(kind), Seen: seen, Work: work, Members: members, Until: untilText})
	if err != nil {
		o.sharedUnknownLocked()
		return nil, err
	}
	o.record = record
	if _, err := o.observeSharedActionLocked(ctx, token, record); err != nil {
		return nil, err
	}
	if kind == transfer.RecoveryActionRefresh {
		if !fresh {
			permit := o.sharedActionPermit
			if permit == nil || permit.Kind != kind || permit.Token != token || permit.ActionID != id {
				return nil, transfer.ErrRecoveryWork
			}
			return permit, nil
		}
		// Keeping the exact process object is essential: a persisted charged
		// refresh, a query or a reload cannot reconstruct this resolver grant.
		var permit *transfer.RecoveryActionPermit
		permit = transfer.IssueRecoveryActionPermit(token, id, kind, time.Time{}, func(checkCtx context.Context) error {
			o.mu.Lock()
			defer o.mu.Unlock()
			if err := o.sharedCurrentLocked(checkCtx, token); err != nil {
				return err
			}
			if o.sharedActionPermit != permit || !reflect.DeepEqual(o.record, record) {
				return transfer.ErrRecoveryStale
			}
			return checkSharedRecoveryUntil(o.record, time.Now())
		})
		o.sharedContinuation.Store(nil)
		o.sharedActionPermit = permit
		return permit, nil
	}
	if !fresh {
		grant := o.sharedContinuation.Load()
		if !grant.matches(o.sharedDataset, record) || o.sharedActionPermit == nil || o.sharedActionPermit.ActionID != id {
			return nil, transfer.ErrRecoveryWork
		}
		return o.sharedActionPermit, nil // Same process object; Consume remains one-shot.
	}
	grant := &sharedRecoveryContinuation{DatasetID: o.sharedDataset, Episode: record.Episode,
		Generation: token.Generation, SourceRevision: record.SourceRevision, SourceHash: record.SourceHash,
		DefaultsHash: record.ExecutionDefaultsHash, ActionID: id, Until: record.NotBefore,
		Members: append([]string{}, members...)}
	o.sharedContinuation.Store(grant)
	deadline, err := parseRecoveryUntil(record.NotBefore)
	if err != nil {
		o.sharedUnknownLocked()
		return nil, err
	}
	permit := transfer.IssueRecoveryActionPermit(token, id, kind, deadline, func(checkCtx context.Context) error {
		o.mu.Lock()
		defer o.mu.Unlock()
		if err := o.sharedCurrentLocked(checkCtx, token); err != nil {
			return err
		}
		current := o.sharedContinuation.Load()
		if current != grant || !grant.matches(o.sharedDataset, o.record) {
			return transfer.ErrRecoveryWork
		}
		return nil
	})
	o.sharedActionPermit = permit
	return permit, nil
}

// Extension is an explicit command referring to the still-waiting parent.
// A confirmed extension preserves an existing RAM grant, never creates one.
// An unknown extension discards it so the earlier deadline cannot send IO.
func (o *taskRecoveryOwner) ExtendRecoveryDeferral(ctx context.Context, token transfer.RecoveryToken, actionID uint64, work string, input []string, until time.Time) (time.Time, error) {
	members, err := recoveryMembers(input)
	if err != nil || !hasRecoveryMember(members, work) || actionID == 0 || until.IsZero() {
		return time.Time{}, transfer.ErrRecoveryWork
	}
	if err := o.prepareSharedAction(ctx, token); err != nil {
		return time.Time{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.sharedCurrentLocked(ctx, token); err != nil {
		return time.Time{}, err
	}
	expected, err := o.observeSharedActionLocked(ctx, token, o.record)
	if err != nil {
		return time.Time{}, err
	}
	previous := o.sharedContinuation.Load()
	preserve := previous.matches(o.sharedDataset, o.record)
	record, _, fresh, err := reserveSharedRecoveryAction(o.store, o.sharedDataset, expected,
		recoverySharedAction{Kind: recoveryDeferralAction, Seen: actionID - 1, Work: work, Members: members,
			Until: until.UTC().Format(time.RFC3339Nano), ExtendActionID: actionID})
	if err != nil || fresh {
		o.sharedUnknownLocked()
		if err == nil {
			err = transfer.ErrRecoveryWork
		}
		return time.Time{}, err
	}
	o.record = record
	if _, err := o.observeSharedActionLocked(ctx, token, record); err != nil {
		return time.Time{}, err
	}
	if preserve && o.sharedContinuation.Load() == previous {
		copy := *previous
		copy.Members = append([]string{}, previous.Members...)
		copy.Until = record.NotBefore
		o.sharedContinuation.Store(&copy)
	}
	return parseRecoveryUntil(record.NotBefore)
}

// sharedLeaseLocked returns only the already admitted owner's RAM state. The
// action version/workset are internal execution markers, not new wire/schema.
func (o *taskRecoveryOwner) sharedLeaseLocked() *transfer.RecoveryLease {
	return &transfer.RecoveryLease{Token: o.token, Owner: o, SourceHash: o.record.SourceHash,
		Round: o.record.NextAction, ActionVersion: recoverySharedVersion, Context: o.runContext, ParentContext: o.parentContext,
		InitialMembers: append([]string(nil), o.sharedInitialMembers...)}
}

// Core calls only after old workers joined and under the inactive lifecycle
// guard. Upgrade itself preserves every budget/proof field and grants nothing.
func (o *taskRecoveryOwner) upgradeShared(ctx context.Context, dataset string) (recoveryRecord, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx == nil || ctx.Err() != nil || o.active.Load() || dataset == "" ||
		o.sharedDataset != "" && o.sharedDataset != dataset || o.closed != nil && o.closed.Load() {
		return recoveryRecord{}, transfer.ErrRecoveryStale
	}
	_, expected, err := captureSharedRecoveryExpected(o.sharedReadStore(), dataset, o.taskID)
	if err != nil || !reflect.DeepEqual(expected.Record, o.record) {
		o.sharedUnknownLocked()
		return recoveryRecord{}, transfer.ErrRecoveryStale
	}
	record, err := upgradeSharedRecovery(o.store, dataset, expected)
	if err != nil {
		o.sharedUnknownLocked()
		return recoveryRecord{}, err
	}
	o.record, o.sharedDataset = record, dataset
	if ctx.Err() != nil || o.closed != nil && o.closed.Load() {
		o.sharedUnknownLocked()
		return recoveryRecord{}, transfer.ErrRecoveryStale
	}
	return record, nil
}

// beginShared performs parent closure and new-run admission in ONE owner
// transaction. Runtime begin dispatches here only for a strict v4 record.
func (o *taskRecoveryOwner) beginShared(ctx context.Context, generation uint64, cleanCheckpoint string) (*transfer.RecoveryLease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx == nil || ctx.Err() != nil || o.record.Version != recoverySharedVersion || !o.record.validShared(o.taskID) ||
		o.sharedDataset == "" || o.closed != nil && o.closed.Load() {
		return nil, transfer.ErrRecoveryHeld
	}
	if err := checkSharedRecoveryUntil(o.record, time.Now()); err != nil {
		return nil, err // No actual admission: retain an existing RAM grant.
	}
	o.active.Store(false)
	o.runContext = nil
	initial := o.sharedInitialEligible.Swap(false)
	initialCoverage := o.coverageNewEnrollment.Swap(false)
	explicit := o.retryAuthorization.Swap(nil)
	earned := o.coverageSettlementAuthorization.Swap(nil)
	continuation := o.sharedContinuation.Swap(nil)
	o.coverageInstallAuthorization.Store(nil)
	o.sharedActionPermit = nil
	_, expected, err := captureSharedRecoveryExpected(o.sharedReadStore(), o.sharedDataset, o.taskID)
	if err != nil || !reflect.DeepEqual(expected.Record, o.record) {
		o.sharedUnknownLocked()
		return nil, transfer.ErrRecoveryStale
	}
	dataset, taskID := o.sharedDataset, o.taskID
	var committed recoveryRecord
	var initialMembers []string
	err = o.store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, required, err := sharedRecoveryCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || generation <= record.Generation {
			return transfer.ErrRecoveryStale
		}
		if err := guardCompletionTransport(tx, task); err != nil {
			return err
		}
		if err := checkCreationAssociationFrom(tx, task); err != nil {
			return err
		}
		if err := checkSharedRecoveryUntil(record, time.Now()); err != nil {
			return err
		}
		clean := record.InitialAttempt.State == "clean-paused" && expected.CheckpointPresent &&
			expected.CheckpointHash == record.InitialAttempt.CheckpointHash &&
			(cleanCheckpoint == "" || cleanCheckpoint == record.InitialAttempt.CheckpointHash)
		freshRetry := false
		if explicit != nil && record.LastExplicitRetry != nil && explicit.Episode == record.Episode &&
			explicit.ActionID == record.LastExplicitRetry.ActionID {
			bound, err := recoveryTaskSourceHash(task, explicit.DefaultsHash)
			freshRetry = err == nil && bound == explicit.SourceHash
		}
		if earned != nil && earned.DatasetID != dataset {
			return transfer.ErrRecoveryHeld
		}
		freshEarned := earned != nil && earned.DatasetID == dataset && record.Coverage != nil &&
			record.Coverage.Status == "tracking" && earned.Episode == record.Episode &&
			earned.SourceHash == record.SourceHash && earned.DefaultsHash == record.ExecutionDefaultsHash &&
			earned.IdentityHash == record.Coverage.IdentityHash && earned.Sequence == record.Coverage.SettlementSequence
		freshContinuation := continuation.matches(dataset, record)
		if freshContinuation {
			initialMembers = append([]string{}, continuation.Members...)
		}
		if _, err := closeSharedDeferralAtAdmission(&record, time.Now()); err != nil {
			return err
		}
		freshInitial := initial && record.InitialAttempt.State == "ungranted" && record.Generation == 0
		if !freshInitial && !clean && !freshRetry && !freshEarned && !freshContinuation {
			if err := appendRecoveryAction(&record, generation, []string{"start"}); err != nil {
				return err
			}
		}
		record.Generation = generation
		record.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: generation}
		if err := validateRecoveryTriplet(task, record, required); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, taskID, &record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		o.sharedUnknownLocked()
		return nil, sharedRecoveryStoreError(err)
	}
	o.record = committed
	if ctx.Err() != nil || o.closed != nil && o.closed.Load() {
		o.sharedUnknownLocked()
		return nil, transfer.ErrRecoveryStale
	}
	if err := withDatasetReadView(o.sharedReadStore(), dataset, func(datasetReader) error { return nil }); err != nil {
		o.sharedUnknownLocked()
		return nil, err
	}
	o.runContext = ctx
	o.token = transfer.RecoveryToken{TaskID: o.taskID, Episode: committed.Episode,
		Generation: committed.Generation, SourceRevision: committed.SourceRevision}
	o.sharedInitialMembers = initialMembers
	o.permits = map[uint64]map[string]*transfer.RecoveryPermit{}
	o.verificationOnly = false
	o.coverageObservationHeld.Store(false)
	if initial && initialCoverage && committed.Coverage == nil {
		token := o.token
		o.coverageInstallAuthorization.Store(&token)
	}
	if o.parentContext != nil {
		o.parentRun = &parentRecoveryRun{token:o.token,epoch:generation,protocol:o.parentContext.Protocol(),active:map[string]int{},children:map[string]string{}}
	}
	o.active.Store(true)
	return o.sharedLeaseLocked(), nil
}

type sharedWaveAdmission func(context.Context, *httpProtocol.RecoveryWaveSeal, uint64) (map[string]*transfer.RecoveryPermit, error)

// Drop owner.mu before entering Core's lifecycle guard. The caller must hold
// no protocol/barrier mutex while waiting for this callback.
func (o *taskRecoveryOwner) AdmitRecoveryWave(ctx context.Context, seal *httpProtocol.RecoveryWaveSeal, id uint64) (map[string]*transfer.RecoveryPermit, error) {
	o.mu.Lock()
	admit := o.sharedAdmission
	o.mu.Unlock()
	if admit == nil {
		return nil, transfer.ErrRecoveryHeld
	}
	return admit(ctx, seal, id)
}

// No join, IO or external callback spans this guard. Seal methods take the
// barrier mutex only OUTSIDE storage transaction callbacks.
func (d *Downloader) admitSharedRecoveryWave(ctx context.Context, dataset string, task *Task, owner *taskRecoveryOwner, seal *httpProtocol.RecoveryWaveSeal, id uint64) (map[string]*transfer.RecoveryPermit, error) {
	if ctx == nil || ctx.Err() != nil || seal == nil || owner == nil {
		return nil, transfer.ErrRecoveryStale
	}
	token := seal.Token() // immutable; no HTTP lock
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	task.lock.Lock()
	defer task.lock.Unlock()
	persist := d.errorPersistenceLock(task)
	persist.Lock()
	defer persist.Unlock()
	d.lock.Lock()
	task.statusLock.Lock()
	present := !d.closed.Load() && d.getTaskLocked(task.ID) == task
	d.lock.Unlock()
	defer task.statusLock.Unlock()
	if !present || ctx.Err() != nil || task.runGeneration != token.Generation ||
		task.getStatus() != base.DownloadStatusRunning || task.recoveryOwner.Load() != owner {
		return nil, transfer.ErrRecoveryStale
	}
	snapshot, err := seal.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	permits, err := owner.admitSharedWave(ctx, dataset, snapshot, id)
	if err != nil {
		return nil, err
	}
	// Recheck lifetime/epoch/domain AFTER ACK, outside the DB callback.
	err = seal.Check(ctx)
	if err == nil {
		err = withDatasetReadView(d.storage, dataset, func(datasetReader) error { return nil })
	}
	if err == nil && (d.closed.Load() || ctx.Err() != nil || task.recoveryOwner.Load() != owner) {
		err = transfer.ErrRecoveryStale
	}
	if err != nil {
		owner.invalidateToken(token)
		return nil, err
	}
	return permits, nil
}

// The caller supplies an owned pre-transaction seal snapshot under Core's
// lifecycle guard. No HTTP seal method is called under owner.mu/DB locks.
func (o *taskRecoveryOwner) admitSharedWave(ctx context.Context, dataset string, snapshot httpProtocol.RecoveryWaveSnapshot, id uint64) (map[string]*transfer.RecoveryPermit, error) {
	members, err := recoveryMembers(snapshot.Members)
	if err != nil || id == 0 || snapshot.Epoch == 0 {
		return nil, transfer.ErrRecoveryWork
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.sharedCurrentLocked(ctx, snapshot.Token); err != nil {
		return nil, err
	}
	if snapshot.SourceHash != o.record.SourceHash && o.parentRun != nil && o.parentRun.token == snapshot.Token && o.parentRun.children[snapshot.SourceHash] != "" {
		snapshot.SourceHash = o.record.SourceHash
	}
	if dataset == "" || o.sharedDataset != dataset || snapshot.SourceHash != o.record.SourceHash {
		return nil, transfer.ErrRecoveryStale
	}
	if err := checkSharedRecoveryUntil(o.record, time.Now()); err != nil {
		return nil, err
	}
	_, expected, err := captureSharedRecoveryExpected(o.sharedReadStore(), dataset, o.taskID)
	if err != nil || !reflect.DeepEqual(expected.Record, o.record) {
		o.sharedUnknownLocked()
		return nil, transfer.ErrRecoveryStale
	}
	continuation := o.sharedContinuation.Swap(nil)
	o.sharedActionPermit = nil
	var committed recoveryRecord
	err = o.store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, required, err := sharedRecoveryCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if record.Version != recoverySharedVersion || snapshot.Token != expected.Token ||
			snapshot.SourceHash != record.SourceHash || id != record.NextAction || len(record.Actions) == 0 {
			return transfer.ErrRecoveryStale
		}
		parent := record.Actions[id-1]
		if parent.Kind != string(recoveryDeferralAction) || parent.State != "charged" || len(parent.Finished) != 0 ||
			parent.Generation > record.Generation || parent.SourceRevision != record.SourceRevision ||
			!sameRecoveryMembers(members, parent.Members) {
			return transfer.ErrRecoveryWork
		}
		if err := guardCompletionTransport(tx, task); err != nil {
			return err
		}
		if err := checkCreationAssociationFrom(tx, task); err != nil {
			return err
		}
		funded := continuation.matches(dataset, record)
		closed, err := closeSharedDeferralAtAdmission(&record, time.Now())
		if err != nil || closed != id {
			if err == nil {
				err = transfer.ErrRecoveryStale
			}
			return err
		}
		if !funded {
			if err := appendRecoveryAction(&record, snapshot.Token.Generation, members); err != nil {
				return err
			}
		}
		if err := validateRecoveryTriplet(task, record, required); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		o.sharedUnknownLocked()
		return nil, sharedRecoveryStoreError(err)
	}
	o.record = committed
	if err := o.sharedCurrentLocked(ctx, snapshot.Token); err != nil {
		o.sharedUnknownLocked()
		return nil, err
	}
	lease := o.sharedLeaseLocked()
	permits := make(map[string]*transfer.RecoveryPermit, len(members))
	for _, work := range members {
		permits[work] = transfer.IssueRecoveryPermit(lease, committed.NextAction, work)
	}
	o.permits[committed.NextAction] = permits
	return permits, nil
}

func (d *Downloader) prepareSharedRecovery(ctx context.Context, task *Task, owner *taskRecoveryOwner, generation uint64) error {
	// Caller released task.lock; old writers and prefetch must really exit
	// before upgrade. Captured immutable channels are joined without Core locks.
	if source, ok := task.fetcher.(interface {
		CaptureRunQuiescence() func(context.Context) error
	}); ok {
		if err := source.CaptureRunQuiescence()(ctx); err != nil {
			return err
		}
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return err
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	task.lock.Lock()
	defer task.lock.Unlock()
	persist := d.errorPersistenceLock(task)
	persist.Lock()
	defer persist.Unlock()
	d.lock.Lock()
	task.statusLock.Lock()
	present := !d.closed.Load() && d.getTaskLocked(task.ID) == task
	d.lock.Unlock()
	defer task.statusLock.Unlock()
	if !present || ctx.Err() != nil || task.runGeneration != generation || task.getStatus() != base.DownloadStatusRunning || task.recoveryOwner.Load() != owner {
		return transfer.ErrRecoveryStale
	}
	record, err := owner.upgradeShared(ctx, identity.DatasetID)
	if err != nil {
		return err
	}
	owner.mu.Lock()
	owner.sharedCheckpoint = func(c context.Context, token transfer.RecoveryToken) error {
		return d.prepareSharedActionCheckpoint(c, task, owner, token)
	}
	owner.sharedAdmission = func(c context.Context, seal *httpProtocol.RecoveryWaveSeal, id uint64) (map[string]*transfer.RecoveryPermit, error) {
		return d.admitSharedRecoveryWave(c, identity.DatasetID, task, owner, seal, id)
	}
	owner.mu.Unlock()
	task.dataLock.Lock()
	task.Recovery = &TaskRecoveryRef{Version: record.Version}
	task.dataLock.Unlock()
	return nil
}

// Joining this Task guard drains both startup's post-Start Store and any
// sampler that already captured progress: each keeps task.lock through its
// synchronous save. Wave IO scopes are closed/idle before this call. Updating
// the actual quiescent frontier leaves the still-running sampler no new bytes
// to save after this guard is released. No run join/Sync or listener runs here.
func (d *Downloader) prepareSharedActionCheckpoint(ctx context.Context, task *Task, owner *taskRecoveryOwner, token transfer.RecoveryToken) error {
	if ctx == nil || ctx.Err() != nil {
		return transfer.ErrRecoveryStale
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	task.lock.Lock()
	defer task.lock.Unlock()
	persist := d.errorPersistenceLock(task)
	persist.Lock()
	defer persist.Unlock()
	d.lock.Lock()
	task.statusLock.Lock()
	present := !d.closed.Load() && d.getTaskLocked(task.ID) == task
	d.lock.Unlock()
	defer task.statusLock.Unlock()
	if !present || ctx.Err() != nil || task.runGeneration != token.Generation || task.getStatus() != base.DownloadStatusRunning || task.recoveryOwner.Load() != owner || task.fetcher == nil {
		return transfer.ErrRecoveryStale
	}
	owner.mu.Lock()
	err := owner.sharedCurrentLocked(ctx, token)
	owner.mu.Unlock()
	if err != nil {
		return err
	}
	current := task.fetcher.Progress().TotalDownloaded()
	if current < 0 {
		return transfer.ErrRecoveryHeld
	}
	task.dataLock.Lock()
	if task.Progress == nil {
		task.dataLock.Unlock()
		return transfer.ErrRecoveryHeld
	}
	task.Progress.Downloaded = current
	task.dataLock.Unlock()
	// Existing Store path preserves Task/source/record/checkpoint together.
	// Once it ACKs, all Core locks are released before the authoritative view.
	return d.saveTask(task)
}

func (d *Downloader) taskSharedUntil(task *Task) (time.Time, error) {
	task.dataLock.RLock()
	shared := task.Recovery != nil && task.Recovery.Version == recoverySharedVersion
	task.dataLock.RUnlock()
	if !shared {
		return time.Time{}, nil
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return time.Time{}, err
	}
	var until time.Time
	err = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
		_, record, _, err := readSharedRecovery(reader, task.ID)
		if err != nil {
			return err
		}
		until, err = parseRecoveryUntil(record.NotBefore)
		return err
	})
	return until, err
}

// Same existing queue/timer mechanism; this helper is called by Continue
// outside d.lock, never by the DB callback. Waiting does not fund the next run.
func (d *Downloader) queueSharedUntil(task *Task, until time.Time) {
	d.lock.Lock()
	task.statusLock.Lock()
	if !d.closed.Load() && d.getTaskLocked(task.ID) == task && task.getStatus() != base.DownloadStatusDone && task.getStatus() != base.DownloadStatusRunning {
		task.dataLock.Lock()
		task.updateStatusLocked(base.DownloadStatusWait)
		task.notBefore = until
		task.clearReadPublishedLocked()
		task.dataLock.Unlock()
		d.enqueueLocked(task)
	}
	task.statusLock.Unlock()
	d.lock.Unlock()
	time.AfterFunc(time.Until(until)+10*time.Millisecond, func() { d.signalQueue(); d.notifyRunning() })
}

// Preparation has returned and released task.lock; Resolve's response was
// closed before returning DeferredError. There are no started body writers.
func (d *Downloader) deferSharedPreparation(task *Task, generation uint64, ctx context.Context, deferred *transfer.DeferredError) error {
	owner := task.recoveryOwner.Load()
	if owner == nil {
		return transfer.ErrRecoveryHeld
	}
	owner.mu.Lock()
	token := owner.token
	seen := owner.record.NextAction
	shared := owner.record.Version == recoverySharedVersion
	owner.mu.Unlock()
	if !shared {
		return transfer.ErrRecoveryHeld
	}
	permit, err := owner.ReserveRecoveryAction(ctx, token, seen, transfer.RecoveryActionDeferral, "start", []string{"start"}, deferred.Until)
	if err != nil {
		return err
	}
	if err = permit.Consume(ctx); err != nil {
		return err
	}
	deferred = &transfer.DeferredError{Until: permit.Until, Status: deferred.Status}
	// The existing generation/status/persistence guard accepts only this run.
	d.deferTask(task, deferred, generation)
	return nil
}

func ownerTokenForRun(owner *taskRecoveryOwner, generation uint64) transfer.RecoveryToken {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.token.Generation != generation {
		return transfer.RecoveryToken{}
	}
	return owner.token
}

func (o *taskRecoveryOwner) sharedReadStore() Storage {
	if o.ownedStore != nil {
		return o.ownedStore
	}
	return o.store
}
