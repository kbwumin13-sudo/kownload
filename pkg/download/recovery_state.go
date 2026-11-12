package download

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

const (
	bucketTaskRecovery         = "task_recovery_v1"
	bucketRecoveryRequired     = "task_recovery_required_v1"
	recoveryCapability         = "http-transport-recovery/v1"
	recoveryMaxTotal           = 16
	recoveryMaxTransport       = 6
	recoveryMaxWork            = 1024
	recoveryCoverageRangeLimit = 4096
)

type TaskRecoveryRef struct {
	Version int `json:"version"`
}
type recoveryRequirement struct {
	Version    int    `json:"version"`
	Capability string `json:"capability"`
}
type recoveryInitialAttempt struct {
	State          string `json:"state"` // ungranted, uncertain, issued, clean-paused
	Generation     uint64 `json:"generation"`
	CheckpointHash string `json:"checkpointHash,omitempty"`
}
type recoveryAction struct {
	ID             uint64   `json:"id"`
	Kind           string   `json:"kind"`
	Generation     uint64   `json:"generation"`
	SourceRevision uint64   `json:"sourceRevision"`
	Members        []string `json:"members"`
	Finished       []string `json:"finished"`
	State          string   `json:"state"`
}
type recoveryExplicitRetry struct {
	Version        int    `json:"version"`
	ActionID       string `json:"actionId"`
	FromEpisode    string `json:"fromEpisode"`
	ToEpisode      string `json:"toEpisode"`
	SourceRevision uint64 `json:"sourceRevision"`
}
type recoveryRecord struct {
	Version               int                         `json:"version"`
	TaskID                string                      `json:"taskId"`
	Episode               string                      `json:"episode"`
	Generation            uint64                      `json:"generation"`
	SourceRevision        uint64                      `json:"sourceRevision"`
	SourceHash            string                      `json:"sourceHash"`
	ExecutionDefaultsHash string                      `json:"executionDefaultsHash"`
	InitialAttempt        recoveryInitialAttempt      `json:"initialAttempt"`
	TotalUsed             int                         `json:"totalUsed"`
	TransportUsed         int                         `json:"transportUsed"`
	RefreshUsed           int                         `json:"refreshUsed"`
	DeferralUsed          int                         `json:"deferralUsed"`
	NotBefore             string                      `json:"notBefore,omitempty"`
	NextAction            uint64                      `json:"nextAction"`
	Actions               []recoveryAction            `json:"actions"`
	LastExplicitRetry     *recoveryExplicitRetry      `json:"lastExplicitRetry,omitempty"`
	Coverage              *transfer.HTTPCoverageState `json:"coverage,omitempty"`
}

func supportedRecoveryVersion(version int) bool {
	return version == 1 || version == 2 || version == 3 || version == recoverySharedVersion
}

// The projection is shared with HTTP's capture/send checks. Pure owner tests
// use a synthetic direct/empty-UA context; Core always supplies a captured D.
func recoverySourceHash(req *base.Request, defaults ...string) (string, error) {
	digest := ""
	if len(defaults) > 0 {
		digest = defaults[0]
	} else {
		var err error
		digest, err = transfer.HTTPExecutionDefaultsHash(req, "", "direct")
		if err != nil {
			return "", err
		}
	}
	return transfer.HTTPExecutionSourceHash(req, digest)
}
func validRecoveryHash(hash string) bool { return transfer.ValidExecutionHash(hash) }
func (r *recoveryRecord) valid(id string) bool {
	if r.Version == recoverySharedVersion {
		return r.validShared(id)
	}

	if !supportedRecoveryVersion(r.Version) || r.TaskID != id || r.NotBefore != "" || r.Episode == "" || r.SourceRevision == 0 || !validRecoveryHash(r.SourceHash) || !validRecoveryHash(r.ExecutionDefaultsHash) || r.TotalUsed < 0 || r.TotalUsed > recoveryMaxTotal || r.TransportUsed < 0 || r.TransportUsed > recoveryMaxTransport || r.RefreshUsed != 0 || r.DeferralUsed != 0 || len(r.Actions) > recoveryMaxTotal {
		return false
	}
	if r.Version < 3 && r.Coverage != nil || r.Version == 1 && r.LastExplicitRetry != nil || r.Version == 2 && r.LastExplicitRetry == nil {
		return false
	}
	if r.Version >= 2 && (r.Generation == 0 || r.InitialAttempt.State == "ungranted" || r.InitialAttempt.State == "uncertain") {
		return false
	}
	if r.Version == 3 && (r.Coverage == nil || transfer.ValidateHTTPCoverageState(*r.Coverage, recoveryCoverageRangeLimit) != nil) {
		return false
	}
	if last := r.LastExplicitRetry; last != nil {
		// The receipt is historical. Later legitimate source/default CAS may
		// advance SourceRevision without rewriting the original user action.
		if last.Version != 1 || !operationIDPattern.MatchString(last.ActionID) || last.FromEpisode == "" || last.ToEpisode == "" || (r.Version < 3 && last.ToEpisode != r.Episode) || last.FromEpisode == last.ToEpisode || last.SourceRevision == 0 || last.SourceRevision > r.SourceRevision {
			return false
		}
	}
	switch r.InitialAttempt.State {
	case "ungranted", "uncertain":
		if r.Generation != 0 || r.InitialAttempt.Generation != 0 || r.InitialAttempt.CheckpointHash != "" {
			return false
		}
	case "issued":
		if r.Generation == 0 || r.InitialAttempt.Generation != r.Generation || r.InitialAttempt.CheckpointHash != "" {
			return false
		}
	case "clean-paused":
		if r.Generation == 0 || r.InitialAttempt.Generation != r.Generation {
			return false
		}
	default:
		return false
	}
	if r.InitialAttempt.State == "clean-paused" && !validRecoveryHash(r.InitialAttempt.CheckpointHash) {
		return false
	}
	if len(r.Actions) != r.TotalUsed || r.TransportUsed != r.TotalUsed || r.NextAction != uint64(len(r.Actions)) {
		return false
	}
	for i, a := range r.Actions {
		if a.ID != uint64(i+1) || a.Kind != "transport" || a.Generation == 0 || a.Generation > r.Generation || a.SourceRevision == 0 || a.SourceRevision > r.SourceRevision || (a.State != "charged" && a.State != "finished") {
			return false
		}
		members, e := recoveryMembers(a.Members)
		if e != nil || !sameRecoveryMembers(members, a.Members) {
			return false
		}
		done := map[string]bool{}
		for _, w := range a.Finished {
			if done[w] || !hasRecoveryMember(a.Members, w) {
				return false
			}
			done[w] = true
		}
		if a.State == "finished" && len(done) != len(a.Members) {
			return false
		}
	}
	return true
}
func recoveryMembers(in []string) ([]string, error) {
	if len(in) == 0 || len(in) > recoveryMaxWork {
		return nil, transfer.ErrRecoveryWork
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	for i, w := range out {
		if w == "" || len(w) > 96 || strings.IndexFunc(w, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '/' || r == '-' || r == '_')
		}) >= 0 || i > 0 && out[i-1] == w {
			return nil, transfer.ErrRecoveryWork
		}
	}
	return out, nil
}
func sameRecoveryMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func hasRecoveryMember(m []string, w string) bool {
	for _, v := range m {
		if v == w {
			return true
		}
	}
	return false
}

// This owner never takes Downloader/task locks from a protocol callback. Core
// invalidates the atomic run fence before Pause/Delete/Close waits on protocol IO.
type taskRecoveryOwner struct {
	mu                              sync.Mutex
	store                           AtomicStorage
	ownedStore                      Storage // immutable same-domain read source; write decorators do not replace snapshot capability
	taskID                          string
	record                          recoveryRecord
	active                          atomic.Bool
	closed                          *atomic.Bool
	token                           transfer.RecoveryToken
	runContext                      context.Context // guarded by mu; runtime lease is tied to preparation cancellation
	permits                         map[uint64]map[string]*transfer.RecoveryPermit
	retryAuthorization              atomic.Pointer[recoveryRetryAuthorization]
	verificationOnly                bool // mu; verifier tokens cannot authorize transport work
	coverageNewEnrollment           atomic.Bool
	coverageInstallAuthorization    atomic.Pointer[transfer.RecoveryToken]
	coverageSettlementAuthorization atomic.Pointer[coverageSettlementAuthorization]
	coverageObservationHeld         atomic.Bool
	sharedDataset                   string // mu; immutable dataset binding after inactive preparation
	sharedInitialEligible           atomic.Bool
	sharedContinuation              atomic.Pointer[sharedRecoveryContinuation]
	sharedActionPermit              *transfer.RecoveryActionPermit // mu; one waiting transition per confirmed debit
	sharedInitialMembers            []string                       // mu; frozen continuation workset, not durable authority
	sharedAdmission                 sharedWaveAdmission            // mu; bound once to the owning Core lifecycle
}

type recoveryReader interface {
	Get(string, string, any) (bool, error)
}

func readRecovery(tx recoveryReader, id string) (Task, recoveryRecord, error) {
	var task Task
	var record recoveryRecord
	var required recoveryRequirement
	t, e := tx.Get(bucketTask, id, &task)
	if e != nil {
		return task, record, transfer.ErrRecoveryUnavailable
	}
	r, e := tx.Get(bucketTaskRecovery, id, &record)
	if e != nil {
		return task, record, transfer.ErrRecoveryUnavailable
	}
	q, e := tx.Get(bucketRecoveryRequired, id, &required)
	if e != nil {
		return task, record, transfer.ErrRecoveryUnavailable
	}
	if !t || !r || !q || task.Meta == nil || task.Meta.Req == nil || task.Protocol != "http" || task.Recovery == nil || !supportedRecoveryVersion(task.Recovery.Version) || task.Recovery.Version != record.Version || required.Version != record.Version || !record.valid(id) {
		return task, record, transfer.ErrRecoveryHeld
	}
	if validateRecoveryTriplet(&task, record, required) != nil {
		return task, record, transfer.ErrRecoveryHeld
	}
	hash, e := recoverySourceHash(task.Meta.Req, record.ExecutionDefaultsHash)
	if e != nil || hash != record.SourceHash {
		return task, record, transfer.ErrRecoveryHeld
	}
	return task, record, nil
}
func newTaskRecoveryOwner(store AtomicStorage, task *Task, defaults ...string) (*taskRecoveryOwner, error) {
	digest := ""
	if len(defaults) > 0 {
		digest = defaults[0]
	} else {
		var err error
		digest, err = transfer.HTTPExecutionDefaultsHash(task.Meta.Req, "", "direct")
		if err != nil {
			return nil, err
		}
	}
	hash, e := recoverySourceHash(task.Meta.Req, digest)
	if e != nil {
		return nil, e
	}
	episode, e := gonanoid.New()
	if e != nil {
		return nil, transfer.ErrRecoveryUnavailable
	}
	var record recoveryRecord
	freshInitial := task.recoveryInitialEligible.Swap(false)
	freshEnrollment := false
	e = store.UpdateAtomic(func(tx StorageTransaction) error {
		var required recoveryRequirement
		var existing recoveryRecord
		var saved Task
		r, err := tx.Get(bucketTaskRecovery, task.ID, &existing)
		if err != nil {
			return err
		}
		q, err := tx.Get(bucketRecoveryRequired, task.ID, &required)
		if err != nil {
			return err
		}
		present, err := tx.Get(bucketTask, task.ID, &saved)
		if err != nil {
			return err
		}
		if !present {
			return transfer.ErrRecoveryHeld
		}
		if r || q || saved.Recovery != nil {
			_, record, err = readRecovery(tx, task.ID)
			return err
		}
		if saved.Meta == nil || saved.Meta.Req == nil {
			return transfer.ErrRecoveryHeld
		}
		savedHash, err := recoverySourceHash(saved.Meta.Req, digest)
		if err != nil || savedHash != hash {
			return transfer.ErrRecoveryHeld
		}
		saved.Recovery = &TaskRecoveryRef{Version: 1}
		freshEnrollment = freshInitial
		initialState := "uncertain"
		if freshInitial {
			initialState = "ungranted"
		}
		record = recoveryRecord{Version: 1, TaskID: task.ID, Episode: episode, SourceRevision: 1, SourceHash: hash, ExecutionDefaultsHash: digest, InitialAttempt: recoveryInitialAttempt{State: initialState}, Actions: []recoveryAction{}}
		if err = tx.Put(bucketTask, task.ID, &saved); err != nil {
			return err
		}
		if err = tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		return tx.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{1, recoveryCapability})
	})
	if e != nil {
		return nil, recoveryStorageError(e)
	}
	owner := &taskRecoveryOwner{store: store, ownedStore: store, taskID: task.ID, record: record, permits: map[uint64]map[string]*transfer.RecoveryPermit{}}
	owner.coverageNewEnrollment.Store(freshEnrollment)
	owner.sharedInitialEligible.Store(freshEnrollment)
	return owner, nil
}
func (o *taskRecoveryOwner) invalidate() {
	o.active.Store(false)
	o.sharedInitialEligible.Store(false)
	o.sharedContinuation.Store(nil)
	o.retryAuthorization.Store(nil)
	o.coverageNewEnrollment.Store(false)
	o.coverageInstallAuthorization.Store(nil)
	o.coverageSettlementAuthorization.Store(nil)
	o.coverageObservationHeld.Store(true)
}
func (o *taskRecoveryOwner) invalidateToken(token transfer.RecoveryToken) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token == token {
		o.active.Store(false)
	}
}

// Result ownership is separate from permission to start new IO. A failed
// debit ACK revokes IO but its own failure must still end the current run.
func (o *taskRecoveryOwner) acceptsFailure(token transfer.RecoveryToken) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return token == o.token && token.Episode == o.record.Episode && token.Generation == o.record.Generation && token.SourceRevision == o.record.SourceRevision && (o.runContext == nil || o.runContext.Err() == nil) && (o.closed == nil || !o.closed.Load())
}

func (o *taskRecoveryOwner) CheckRecovery(token transfer.RecoveryToken) error {
	if !o.active.Load() || (o.closed != nil && o.closed.Load()) {
		return transfer.ErrRecoveryStale
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active.Load() || token != o.token || (o.runContext != nil && o.runContext.Err() != nil) || (o.closed != nil && o.closed.Load()) {
		return transfer.ErrRecoveryStale
	}
	if o.record.Version == recoverySharedVersion && !o.verificationOnly {
		return checkSharedRecoveryUntil(o.record, time.Now())
	}
	return nil
}
func appendRecoveryAction(r *recoveryRecord, generation uint64, members []string) error {
	if r.TotalUsed >= recoveryMaxTotal || r.TransportUsed >= recoveryMaxTransport {
		return transfer.ErrRecoveryExhausted
	}
	r.TotalUsed++
	r.TransportUsed++
	r.NextAction++
	r.Actions = append(r.Actions, recoveryAction{ID: r.NextAction, Kind: "transport", Generation: generation, SourceRevision: r.SourceRevision, Members: members, Finished: []string{}, State: "charged"})
	return nil
}
func (o *taskRecoveryOwner) begin(generation uint64, cleanCheckpoint string, contexts ...context.Context) (*transfer.RecoveryLease, error) {
	o.mu.Lock()
	if o.record.Version == recoverySharedVersion {
		o.mu.Unlock()
		if len(contexts) != 1 || contexts[0] == nil {
			return nil, transfer.ErrRecoveryHeld
		}
		return o.beginShared(contexts[0], generation, cleanCheckpoint)
	}
	o.sharedInitialEligible.Store(false)
	defer o.mu.Unlock()
	o.active.Store(false)
	o.runContext = nil
	// An acknowledged explicit user action may fund exactly this one attempt.
	// Neither the durable receipt nor a reloaded owner can reconstruct it.
	explicit := o.retryAuthorization.Swap(nil)
	initialCoverage := o.coverageNewEnrollment.Swap(false)
	o.coverageInstallAuthorization.Store(nil)
	earned := o.coverageSettlementAuthorization.Swap(nil)
	var committed recoveryRecord
	err := o.store.UpdateAtomic(func(tx StorageTransaction) error {
		task, r, e := readRecovery(tx, o.taskID)
		if e != nil {
			return e
		}
		if e = guardCompletionTransport(tx, &task); e != nil {
			return e
		}
		if r.Episode != o.record.Episode || r.Generation != o.record.Generation || r.SourceRevision != o.record.SourceRevision || generation <= r.Generation {
			return transfer.ErrRecoveryHeld
		}
		clean := r.InitialAttempt.State == "clean-paused" && (cleanCheckpoint == "" || cleanCheckpoint == r.InitialAttempt.CheckpointHash)
		if clean {
			var checkpoint json.RawMessage
			exists, readErr := tx.Get(bucketSave, o.taskID, &checkpoint)
			if readErr != nil {
				return transfer.ErrRecoveryUnavailable
			}
			digest, hashErr := checkpointDigest(checkpoint)
			clean = exists && hashErr == nil && digest == r.InitialAttempt.CheckpointHash
		}
		freshRetry := false
		if explicit != nil && r.LastExplicitRetry != nil && explicit.Episode == r.Episode && explicit.ActionID == r.LastExplicitRetry.ActionID {
			// Defaults may legitimately advance while the raw interpreted
			// request must still be the one explicitly authorized by the user.
			bound, err := recoverySourceHash(task.Meta.Req, explicit.DefaultsHash)
			freshRetry = err == nil && bound == explicit.SourceHash
		}
		if earned != nil {
			// A fresh ACK is bound to its dataset, including the admission of
			// its next run. A changed/unknown domain cannot consume the grant.
			if e = requireDataset(tx, earned.DatasetID); e != nil {
				return transfer.ErrRecoveryHeld
			}
		}
		freshCoverage := earned != nil && r.Version == 3 && r.Coverage != nil && r.Coverage.Status == "tracking" &&
			earned.Episode == r.Episode && earned.SourceHash == r.SourceHash && earned.DefaultsHash == r.ExecutionDefaultsHash &&
			earned.IdentityHash == r.Coverage.IdentityHash && earned.Sequence == r.Coverage.SettlementSequence
		if r.InitialAttempt.State != "ungranted" && !clean && !freshRetry && !freshCoverage {
			if e = appendRecoveryAction(&r, generation, []string{"start"}); e != nil {
				return e
			}
		}
		r.Generation = generation
		r.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: generation}
		if e = tx.Put(bucketTaskRecovery, o.taskID, &r); e != nil {
			return e
		}
		committed = r
		return nil
	})
	if err != nil {
		o.observeUnknownWrite(err)
		return nil, recoveryStorageError(err)
	}
	o.verificationOnly = false
	o.record = committed
	o.token = transfer.RecoveryToken{TaskID: o.taskID, Episode: committed.Episode, Generation: generation, SourceRevision: committed.SourceRevision}
	o.permits = map[uint64]map[string]*transfer.RecoveryPermit{}
	o.coverageObservationHeld.Store(false)
	o.active.Store(true)
	if initialCoverage && committed.Version == 1 {
		token := o.token
		o.coverageInstallAuthorization.Store(&token)
	}
	return &transfer.RecoveryLease{Token: o.token, Round: committed.NextAction, Owner: o, SourceHash: committed.SourceHash}, nil
}
func recoveryStorageError(err error) error {
	for _, fixed := range []error{transfer.ErrRecoveryHeld, transfer.ErrRecoveryExhausted, transfer.ErrRecoveryWork, transfer.ErrRecoveryStale} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	return transfer.ErrRecoveryUnavailable
}

// An unknown write result never reconstructs an IO capability. Inspect only
// the original record for later query/restart; even a visible charged action
// stays consumed and this generation remains invalidated.
func (o *taskRecoveryOwner) observeUnknownWrite(causes ...error) {
	o.sharedInitialEligible.Store(false)
	o.sharedContinuation.Store(nil)
	o.sharedActionPermit = nil

	o.coverageNewEnrollment.Store(false)
	o.coverageInstallAuthorization.Store(nil)
	o.coverageSettlementAuthorization.Store(nil)
	// Only a known exhaustion rollback can retain result observation. Unknown
	// ACK/context failures cannot restore IO or coverage settlement authority.
	if len(causes) == 0 || !errors.Is(causes[0], transfer.ErrRecoveryExhausted) {
		o.coverageObservationHeld.Store(true)
	}
	o.active.Store(false)
	if _, record, err := readRecovery(o.store, o.taskID); err == nil && record.Episode == o.record.Episode && record.SourceRevision == o.record.SourceRevision && record.SourceHash == o.record.SourceHash && record.ExecutionDefaultsHash == o.record.ExecutionDefaultsHash {
		o.record = record
	}
}
func (o *taskRecoveryOwner) RetryRecovery(ctx context.Context, token transfer.RecoveryToken, seen uint64, work string, input []string) (*transfer.RecoveryPermit, error) {
	members, err := recoveryMembers(input)
	if err != nil || !hasRecoveryMember(members, work) {
		return nil, transfer.ErrRecoveryWork
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !o.active.Load() || token != o.token || (o.runContext != nil && o.runContext.Err() != nil) || (o.closed != nil && o.closed.Load()) {
		return nil, transfer.ErrRecoveryStale
	}
	if o.verificationOnly {
		return nil, transfer.ErrRecoveryWork
	}
	// A wave existing in this process can serve each frozen member exactly once.
	if seen < o.record.NextAction {
		permits := o.permits[o.record.NextAction]
		if len(permits) != len(members) {
			return nil, transfer.ErrRecoveryWork
		}
		for _, w := range members {
			if permits[w] == nil {
				return nil, transfer.ErrRecoveryWork
			}
		}
		if p := permits[work]; p != nil {
			return p, nil
		}
		return nil, transfer.ErrRecoveryWork
	}
	if seen != o.record.NextAction {
		return nil, transfer.ErrRecoveryWork
	}
	var sharedExpected recoverySharedExpected
	dataset := o.sharedDataset
	if o.record.Version == recoverySharedVersion {
		if err := checkSharedRecoveryUntil(o.record, time.Now()); err != nil {
			return nil, err
		}
		_, captured, captureErr := captureSharedRecoveryExpected(o.sharedReadStore(), dataset, o.taskID)
		if captureErr != nil {
			return nil, captureErr
		}
		sharedExpected = captured
	}
	var committed recoveryRecord
	err = o.store.UpdateAtomic(func(tx StorageTransaction) error {
		if sharedExpected.Record.Version == recoverySharedVersion {
			if _, _, _, err := sharedRecoveryCAS(tx, dataset, sharedExpected); err != nil {
				return err
			}
		}
		task, r, e := readRecovery(tx, o.taskID)
		if e != nil {
			return e
		}
		if e = guardCompletionTransport(tx, &task); e != nil {
			return e
		}
		if r.Episode != token.Episode || r.Generation != token.Generation || r.SourceRevision != token.SourceRevision || r.NextAction != seen {
			return transfer.ErrRecoveryStale
		}
		if e = appendRecoveryAction(&r, token.Generation, members); e != nil {
			return e
		}
		if e = tx.Put(bucketTaskRecovery, o.taskID, &r); e != nil {
			return e
		}
		committed = r
		return nil
	})
	if err != nil {
		o.observeUnknownWrite(err)
		return nil, recoveryStorageError(err)
	}
	o.record = committed
	if !o.active.Load() || ctx.Err() != nil {
		return nil, transfer.ErrRecoveryStale
	}
	lease := &transfer.RecoveryLease{Token: token, Round: committed.NextAction, Owner: o, SourceHash: committed.SourceHash}
	if committed.Version == recoverySharedVersion {
		lease.ActionVersion = recoverySharedVersion
		lease.Context = o.runContext
	}

	permits := map[string]*transfer.RecoveryPermit{}
	for _, w := range members {
		permits[w] = transfer.IssueRecoveryPermit(lease, committed.NextAction, w)
	}
	o.permits[committed.NextAction] = permits
	return permits[work], nil
}
func (o *taskRecoveryOwner) FinishRecovery(ctx context.Context, token transfer.RecoveryToken, round uint64, work string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !o.active.Load() || token != o.token || (o.runContext != nil && o.runContext.Err() != nil) || (o.closed != nil && o.closed.Load()) {
		return transfer.ErrRecoveryStale
	}
	var sharedExpected recoverySharedExpected
	dataset := o.sharedDataset
	if o.record.Version == recoverySharedVersion {
		_, captured, captureErr := captureSharedRecoveryExpected(o.sharedReadStore(), dataset, o.taskID)
		if captureErr != nil {
			return captureErr
		}
		sharedExpected = captured
	}
	var committed recoveryRecord
	err := o.store.UpdateAtomic(func(tx StorageTransaction) error {
		if sharedExpected.Record.Version == recoverySharedVersion {
			if _, _, _, err := sharedRecoveryCAS(tx, dataset, sharedExpected); err != nil {
				return err
			}
		}
		_, r, e := readRecovery(tx, o.taskID)
		if e != nil {
			return e
		}
		if r.Episode != token.Episode || r.Generation != token.Generation || r.SourceRevision != token.SourceRevision || round == 0 || round > r.NextAction {
			return transfer.ErrRecoveryStale
		}
		action := &r.Actions[round-1]
		if r.Version == recoverySharedVersion && action.Kind == string(recoveryDeferralAction) && action.State == "charged" {
			return transfer.ErrRecoveryWork
		}

		if action.Generation != token.Generation || !hasRecoveryMember(action.Members, work) {
			return transfer.ErrRecoveryWork
		}
		if !hasRecoveryMember(action.Finished, work) {
			action.Finished = append(action.Finished, work)
			sort.Strings(action.Finished)
		}
		if len(action.Finished) == len(action.Members) {
			action.State = "finished"
		}
		if e = tx.Put(bucketTaskRecovery, o.taskID, &r); e != nil {
			return e
		}
		committed = r
		return nil
	})
	if err != nil {
		o.observeUnknownWrite(err)
		return recoveryStorageError(err)
	}
	o.record = committed
	return nil
}
