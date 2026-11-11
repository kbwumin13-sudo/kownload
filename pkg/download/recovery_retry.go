package download

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

var (
	ErrRecoveryRetryInvalid  = errors.New("invalid_explicit_retry")
	ErrRecoveryRetryConflict = errors.New("explicit_retry_conflict")
	ErrRecoveryRetryStale    = errors.New("explicit_retry_stale")
)

// This DTO is accepted only by the dedicated authenticated user-action route.
// Ordinary Continue, automatic resume and MCP never construct this intent.
type RecoveryRetryRequest struct {
	Version                int    `json:"version"`
	ActionID               string `json:"actionId"`
	ExpectedEpisode        string `json:"expectedEpisode"`
	ExpectedSourceRevision uint64 `json:"expectedSourceRevision"`
	Decision               string `json:"decision"`
}

func (r RecoveryRetryRequest) valid() bool {
	return r.Version == 1 && operationIDPattern.MatchString(r.ActionID) && r.ExpectedEpisode != "" && len(r.ExpectedEpisode) <= 128 && r.ExpectedSourceRevision > 0 && r.ExpectedSourceRevision <= 9007199254740991 && r.Decision == "retry"
}

// ValidRecoveryRetryRequest validates shape only; it grants no user intent,
// task access, episode reset or execution permission.
func ValidRecoveryRetryRequest(r RecoveryRetryRequest) bool { return r.valid() }

type recoveryRetryAuthorization struct {
	Episode      string
	ActionID     string
	SourceHash   string
	DefaultsHash string
}

type RecoveryTaskState struct {
	Version           int                    `json:"version"`
	TaskID            string                 `json:"taskId"`
	Status            base.Status            `json:"status"`
	Management        string                 `json:"management"`
	RetryAllowed      bool                   `json:"retryAllowed"`
	Reason            string                 `json:"reason"`
	Episode           string                 `json:"episode,omitempty"`
	Generation        uint64                 `json:"generation"`
	SourceRevision    uint64                 `json:"sourceRevision"`
	TotalUsed         int                    `json:"totalUsed"`
	TransportUsed     int                    `json:"transportUsed"`
	LastExplicitRetry *recoveryExplicitRetry `json:"lastExplicitRetry,omitempty"`
	Receipt           *RecoveryRetryReceipt  `json:"receipt,omitempty"`
}

type RecoveryRetryReceipt struct {
	Version                int    `json:"version"`
	ActionID               string `json:"actionId"`
	ExpectedEpisode        string `json:"expectedEpisode"`
	ExpectedSourceRevision uint64 `json:"expectedSourceRevision"`
	Decision               string `json:"decision"`
	State                  string `json:"state"`
	NewEpisode             string `json:"newEpisode,omitempty"`
	QueryRequired          bool   `json:"queryRequired"`
}

func (s RecoveryTaskState) MarshalJSON() ([]byte, error) {
	if s.Management == "managed" {
		type wire RecoveryTaskState
		return json.Marshal(wire(s))
	}
	// Legacy/held has no trustworthy recovery tuple or counters. In
	// particular, an unknown source revision is not a numeric zero grant.
	return json.Marshal(struct {
		Version      int         `json:"version"`
		TaskID       string      `json:"taskId"`
		Status       base.Status `json:"status"`
		Management   string      `json:"management"`
		RetryAllowed bool        `json:"retryAllowed"`
		Reason       string      `json:"reason"`
	}{s.Version, s.TaskID, s.Status, s.Management, false, s.Reason})
}

func recoveryReceipt(state RecoveryTaskState, request RecoveryRetryRequest, outcome string) RecoveryTaskState {
	receipt := &RecoveryRetryReceipt{Version: 1, ActionID: request.ActionID, ExpectedEpisode: request.ExpectedEpisode, ExpectedSourceRevision: request.ExpectedSourceRevision, Decision: request.Decision, State: outcome}
	if outcome == "accepted" || outcome == "replayed" {
		receipt.NewEpisode = state.Episode
	}
	if outcome == "not_recorded" {
		receipt.QueryRequired = true
	}
	state.Receipt = receipt
	return state
}

func recoveryTaskState(task Task, record recoveryRecord) RecoveryTaskState {
	allowed := task.Status == base.DownloadStatusError && !task.handoffHeld() && !task.creationHeld()
	reason := "not_error"
	if allowed {
		reason = "explicit_retry_available"
	}
	return RecoveryTaskState{Version: 1, TaskID: task.ID, Status: task.Status, Management: "managed", RetryAllowed: allowed, Reason: reason, Episode: record.Episode, Generation: record.Generation, SourceRevision: record.SourceRevision, TotalUsed: record.TotalUsed, TransportUsed: record.TransportUsed, LastExplicitRetry: record.LastExplicitRetry}
}

// QueryRecoveryForDataset is a single real read view. No enrollment, version
// migration, owner construction, protocol restore or scheduling occurs here.
func (d *Downloader) QueryRecoveryForDataset(dataset, id string) (RecoveryTaskState, error) {
	var result RecoveryTaskState
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		var task Task
		present, err := reader.Get(bucketTask, id, &task)
		if err != nil {
			return transfer.ErrRecoveryUnavailable
		}
		if !present {
			return ErrTaskNotFound
		}
		if task.ID != id {
			return transfer.ErrRecoveryHeld
		}
		switch task.Status {
		case base.DownloadStatusReady, base.DownloadStatusWait, base.DownloadStatusRunning, base.DownloadStatusPause, base.DownloadStatusDone, base.DownloadStatusError:
		default:
			return transfer.ErrRecoveryHeld
		}
		result = RecoveryTaskState{Version: 1, TaskID: id, Status: task.Status, Management: "held", Reason: "recovery_held"}
		// This candidate does not execute S4 state. A dedicated Retry must
		// not bypass the same presence guard used by Continue/restore.
		if task.Completion != nil {
			return nil
		}
		for _, bucket := range []string{"task_completion_v1", "task_completion_required_v1"} {
			var value json.RawMessage
			present, err := reader.Get(bucket, id, &value)
			if err != nil {
				return transfer.ErrRecoveryUnavailable
			}
			if present {
				return nil
			}
		}
		if task.handoffHeld() || task.creationHeld() {
			return nil
		}
		if err := checkCreationAssociationFrom(reader, &task); err != nil {
			return err
		}
		var recordPart, requiredPart any
		r, err := reader.Get(bucketTaskRecovery, id, &recordPart)
		if err != nil {
			return transfer.ErrRecoveryUnavailable
		}
		q, err := reader.Get(bucketRecoveryRequired, id, &requiredPart)
		if err != nil {
			return transfer.ErrRecoveryUnavailable
		}
		if !r && !q && task.Recovery == nil {
			result.Management = "legacy"
			result.Reason = "not_managed"
			return nil
		}
		task, record, err := readRecovery(reader, id)
		if err != nil {
			if errors.Is(err, transfer.ErrRecoveryHeld) {
				return nil
			}
			return err
		}
		if record.SourceRevision > 9007199254740991 || record.Generation > 9007199254740991 {
			return nil
		}
		result = recoveryTaskState(task, record)
		return nil
	})
	if err != nil {
		return RecoveryTaskState{}, err
	}
	return result, nil
}

func (d *Downloader) QueryRecoveryActionForDataset(dataset, id string, request RecoveryRetryRequest) (RecoveryTaskState, error) {
	if !request.valid() {
		return RecoveryTaskState{}, ErrRecoveryRetryInvalid
	}
	state, err := d.QueryRecoveryForDataset(dataset, id)
	if err != nil {
		return RecoveryTaskState{}, err
	}
	if state.Management != "managed" {
		return state, nil
	}
	if last := state.LastExplicitRetry; last != nil && last.ActionID == request.ActionID && last.FromEpisode == request.ExpectedEpisode {
		if last.SourceRevision != request.ExpectedSourceRevision {
			return RecoveryTaskState{}, ErrRecoveryRetryConflict
		}
		return recoveryReceipt(state, request, "accepted"), nil
	}
	if state.Episode != request.ExpectedEpisode || state.SourceRevision != request.ExpectedSourceRevision {
		return recoveryReceipt(state, request, "stale"), nil
	}
	return recoveryReceipt(state, request, "not_recorded"), nil
}

// RetryRecoveryForDataset is the dedicated user-intent path. A request body
// cannot turn ordinary Continue or a read into a new episode. The caller must
// not automatically resend after an unknown response.
func (d *Downloader) RetryRecoveryForDataset(ctx context.Context, dataset, id string, request RecoveryRetryRequest) (RecoveryTaskState, error) {
	if !request.valid() {
		return RecoveryTaskState{}, ErrRecoveryRetryInvalid
	}
	state, err := d.QueryRecoveryForDataset(dataset, id)
	if err != nil {
		return RecoveryTaskState{}, err
	}
	if last := state.LastExplicitRetry; last != nil && last.ActionID == request.ActionID && last.FromEpisode == request.ExpectedEpisode {
		if last.SourceRevision != request.ExpectedSourceRevision {
			return RecoveryTaskState{}, ErrRecoveryRetryConflict
		}
		return recoveryReceipt(state, request, "replayed"), nil
	}
	if state.Management != "managed" || !state.RetryAllowed {
		return RecoveryTaskState{}, transfer.ErrRecoveryHeld
	}
	if state.Episode != request.ExpectedEpisode {
		return RecoveryTaskState{}, ErrRecoveryRetryStale
	}
	if state.SourceRevision != request.ExpectedSourceRevision {
		return RecoveryTaskState{}, ErrRecoveryRetryConflict
	}
	task := d.getTask(id)
	if task == nil || d.closed.Load() {
		return RecoveryTaskState{}, transfer.ErrRecoveryHeld
	}
	task.statusLock.Lock()
	if task.Protocol != "http" || task.getStatus() != base.DownloadStatusError {
		task.statusLock.Unlock()
		return RecoveryTaskState{}, transfer.ErrRecoveryHeld
	}
	// The initial view may precede another Retry's commit. Recheck under
	// this lifecycle decision before invalidating anything; a stale request
	// must not clear a newer winner's unconsumed private authorization.
	latest, err := d.QueryRecoveryForDataset(dataset, id)
	if err != nil {
		task.statusLock.Unlock()
		return RecoveryTaskState{}, err
	}
	if latest.Episode != request.ExpectedEpisode {
		task.statusLock.Unlock()
		if last := latest.LastExplicitRetry; last != nil && last.ActionID == request.ActionID && last.FromEpisode == request.ExpectedEpisode && last.SourceRevision == request.ExpectedSourceRevision {
			return recoveryReceipt(latest, request, "replayed"), nil
		}
		return RecoveryTaskState{}, ErrRecoveryRetryStale
	}
	if latest.Management != "managed" || !latest.RetryAllowed || latest.SourceRevision != request.ExpectedSourceRevision {
		task.statusLock.Unlock()
		return RecoveryTaskState{}, ErrRecoveryRetryConflict
	}
	generation := task.runGeneration
	var join func(context.Context) error
	if task.fetcher != nil {
		quiescence, ok := task.fetcher.(interface {
			CaptureRunQuiescence() func(context.Context) error
		})
		if !ok {
			task.statusLock.Unlock()
			return RecoveryTaskState{}, transfer.ErrRecoveryHeld
		}
		join = quiescence.CaptureRunQuiescence()
	}
	invalidateTaskRecovery(task)
	if task.runCancel != nil {
		task.runCancel()
	}
	task.statusLock.Unlock()
	if join != nil {
		if err := join(ctx); err != nil {
			return RecoveryTaskState{}, err
		}
	}

	fresh := false
	err = func() error {
		task.lock.Lock()
		defer task.lock.Unlock()
		lock := d.errorPersistenceLock(task)
		lock.Lock()
		defer lock.Unlock()
		d.lock.Lock()
		task.statusLock.Lock()
		present := !d.closed.Load() && d.getTaskLocked(id) == task
		d.lock.Unlock()
		defer task.statusLock.Unlock()
		if !present || task.getStatus() != base.DownloadStatusError || task.runGeneration != generation {
			return transfer.ErrRecoveryStale
		}
		if err := d.checkTaskRecovery(task); err != nil {
			return err
		}
		if task.fetcher == nil {
			if err := d.restoreFetcher(task); err != nil {
				return err
			}
		}
		capture, ok := task.fetcher.(interface {
			CaptureRecoveryContext(*base.Request) (*transfer.HTTPExecutionContext, bool, error)
		})
		if !ok {
			return transfer.ErrRecoveryHeld
		}
		task.dataLock.RLock()
		requestCopy, cloneErr := cloneOperationJSON(task.Meta.Req)
		task.dataLock.RUnlock()
		if cloneErr != nil {
			return transfer.ErrRecoveryHeld
		}
		_, supported, captureErr := capture.CaptureRecoveryContext(requestCopy)
		if captureErr != nil {
			return captureErr
		}
		if !supported {
			return transfer.ErrRecoveryHeld
		}
		owner := task.recoveryOwner.Load()
		if owner == nil {
			store, ok := d.storage.(AtomicStorage)
			if !ok {
				return transfer.ErrRecoveryUnavailable
			}
			var err error
			owner, err = newTaskRecoveryOwner(store, task)
			if err != nil {
				return err
			}
			owner.closed = &d.closed
			task.recoveryOwner.Store(owner)
		}
		record, accepted, err := owner.commitExplicitRetry(ctx, dataset, request, generation)
		if err != nil {
			return err
		}
		fresh = accepted
		if fresh {
			task.dataLock.Lock()
			task.Recovery = &TaskRecoveryRef{Version: 2}
			task.dataLock.Unlock()
		}
		task.dataLock.RLock()
		snapshot := Task{ID: task.ID, Status: task.Status}
		task.dataLock.RUnlock()
		state = recoveryTaskState(snapshot, record)
		return nil
	}()
	if err != nil {
		return RecoveryTaskState{}, err
	}
	if fresh {
		if err := ctx.Err(); err != nil {
			invalidateTaskRecovery(task)
			return state, err
		}
		if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
			invalidateTaskRecovery(task)
			return state, err
		}
	}
	outcome := "replayed"
	if fresh {
		outcome = "accepted"
	}
	return recoveryReceipt(state, request, outcome), nil
}

// Caller first joins the old writer outside Task/Downloader/owner locks, then
// owns the Task operation and persistence guards plus its status lock. No
// protocol IO, scheduling or callbacks occur in this transaction.
func (o *taskRecoveryOwner) commitExplicitRetry(ctx context.Context, dataset string, request RecoveryRetryRequest, generation uint64) (recoveryRecord, bool, error) {
	if !request.valid() || !datasetIDPattern.MatchString(dataset) {
		return recoveryRecord{}, false, ErrRecoveryRetryInvalid
	}
	if err := ctx.Err(); err != nil {
		return recoveryRecord{}, false, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var result recoveryRecord
	fresh := false
	err := o.store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		task, record, err := readRecovery(tx, o.taskID)
		if err != nil {
			return err
		}
		if last := record.LastExplicitRetry; last != nil && last.ActionID == request.ActionID && last.FromEpisode == request.ExpectedEpisode {
			if last.SourceRevision != request.ExpectedSourceRevision {
				return ErrRecoveryRetryConflict
			}
			result = record
			return nil
		}
		if record.Episode != request.ExpectedEpisode {
			return ErrRecoveryRetryStale
		}
		if record.SourceRevision != request.ExpectedSourceRevision || (record.LastExplicitRetry != nil && record.LastExplicitRetry.ActionID == request.ActionID) {
			return ErrRecoveryRetryConflict
		}
		if task.Status != base.DownloadStatusError || task.handoffHeld() || task.creationHeld() {
			return transfer.ErrRecoveryHeld
		}
		if task.CreationOperation != nil {
			if err := checkCreationAssociationFrom(tx, &task); err != nil {
				return transfer.ErrRecoveryHeld
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		episode, err := gonanoid.New()
		if err != nil {
			return transfer.ErrRecoveryUnavailable
		}
		if generation < record.Generation {
			generation = record.Generation
		}
		if generation == 0 {
			generation = 1
		}
		record.Version = 2
		record.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: request.ActionID, FromEpisode: record.Episode, ToEpisode: episode, SourceRevision: record.SourceRevision}
		record.Episode = episode
		record.Generation = generation
		record.TotalUsed, record.TransportUsed, record.RefreshUsed, record.DeferralUsed = 0, 0, 0, 0
		record.Actions = []recoveryAction{}
		record.NextAction = 0
		record.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: generation}
		task.Recovery = &TaskRecoveryRef{Version: 2}
		if !record.valid(task.ID) {
			return transfer.ErrRecoveryHeld
		}
		if err := tx.Put(bucketTask, task.ID, &task); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		if err := tx.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{2, recoveryCapability}); err != nil {
			return err
		}
		result, fresh = record, true
		return nil
	})
	if err != nil {
		o.observeUnknownWrite()
		return recoveryRecord{}, false, err
	}
	if fresh {
		o.active.Store(false)
		o.token = transfer.RecoveryToken{}
		o.record = result
		o.retryAuthorization.Store(&recoveryRetryAuthorization{Episode: result.Episode, ActionID: request.ActionID, SourceHash: result.SourceHash, DefaultsHash: result.ExecutionDefaultsHash})
	}
	return result, fresh, nil
}
