package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

const (
	bucketTaskCompletion     = "task_completion_v1"
	bucketCompletionRequired = "task_completion_required_v1"
)

type completionRequirement struct {
	Version    int    `json:"version"`
	Capability string `json:"capability"`
}

type TaskCompletionStateErrorKind string

const (
	TaskCompletionHeld        TaskCompletionStateErrorKind = "completion_state_held"
	TaskCompletionUnavailable TaskCompletionStateErrorKind = "completion_state_unavailable"
	TaskCompletionConflict    TaskCompletionStateErrorKind = "completion_state_conflict"
	TaskCompletionRunChanged  TaskCompletionStateErrorKind = "completion_run_changed"
	TaskCompletionNotRequired TaskCompletionStateErrorKind = "completion_not_required"
)

// Error text never contains a Task ID, request, digest, path or backend message.
// Unwrap is for local errors.Is classification, not public error serialization.
type TaskCompletionStateError struct {
	Kind TaskCompletionStateErrorKind
	Err  error
}

func (e *TaskCompletionStateError) Error() string { return string(e.Kind) }
func (e *TaskCompletionStateError) Unwrap() error { return e.Err }
func completionStateError(kind TaskCompletionStateErrorKind, cause error) error {
	return &TaskCompletionStateError{Kind: kind, Err: cause}
}

func completionStoreError(err error) error {
	if err == nil {
		return nil
	}
	var classified *TaskCompletionStateError
	if errors.As(err, &classified) || errors.Is(err, ErrDatasetMismatch) ||
		errors.Is(err, ErrDatasetMissing) || errors.Is(err, ErrDatasetInvalid) {
		return err
	}
	return completionStateError(TaskCompletionUnavailable, err)
}

func decodeCompletionValue(raw json.RawMessage, out any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return completionStateError(TaskCompletionHeld, nil)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return completionStateError(TaskCompletionHeld, nil)
	}
	return nil
}

// readTaskCompletion requires an already-owned dataset view or write
// transaction. It does not attach a Task to the live map, initialize locks,
// repair references, hash files, or invoke the runtime recovery owner.
//
// The backing Mem store must isolate its stored Tasks from live Task/Meta
// pointers. Its serializer may lock only immutable storage objects; the result
// below is a fresh JSON-decoded snapshot, never the stored/live pointer.
func readTaskCompletion(reader datasetReader, id string) (*Task, transfer.VerifiedCompletionRecord, error) {
	var empty transfer.VerifiedCompletionRecord
	if reader == nil || id == "" {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	var taskRaw, recordRaw, requiredRaw json.RawMessage
	t, err := reader.Get(bucketTask, id, &taskRaw)
	if err != nil {
		return nil, empty, completionStateError(TaskCompletionUnavailable, err)
	}
	r, err := reader.Get(bucketTaskCompletion, id, &recordRaw)
	if err != nil {
		return nil, empty, completionStateError(TaskCompletionUnavailable, err)
	}
	q, err := reader.Get(bucketCompletionRequired, id, &requiredRaw)
	if err != nil {
		return nil, empty, completionStateError(TaskCompletionUnavailable, err)
	}
	if !t {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	var envelope struct {
		ID         string          `json:"id"`
		Completion json.RawMessage `json:"completion"`
	}
	if err := decodeStoredJSON(taskRaw, &envelope); err != nil || envelope.ID != id {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	if envelope.Completion == nil && !r && !q {
		return nil, empty, completionStateError(TaskCompletionNotRequired, nil)
	}
	if envelope.Completion == nil || !r || !q {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	var ref TaskCompletionRef
	var required completionRequirement
	var record transfer.VerifiedCompletionRecord
	if err := decodeCompletionValue(envelope.Completion, &ref); err != nil {
		return nil, empty, err
	}
	if err := decodeCompletionValue(recordRaw, &record); err != nil {
		return nil, empty, err
	}
	if err := decodeCompletionValue(requiredRaw, &required); err != nil {
		return nil, empty, err
	}
	task := new(Task)
	if err := decodeStoredJSON(taskRaw, task); err != nil || task.ID != id ||
		ref.Version != 1 || ref.CompletionID == "" || ref.CompletionID != record.ID ||
		required.Version != 1 || required.Capability != transfer.VerifiedCompletionCapability ||
		!completionHTTPTask(task) || !validCompletionStateRecord(record) {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	if (record.Phase == transfer.CompletionCommitted) != (task.Status == base.DownloadStatusDone) {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	if record.Phase == transfer.CompletionCommitted && (task.Progress == nil || task.Progress.Downloaded != record.Size) {
		return nil, empty, completionStateError(TaskCompletionHeld, nil)
	}
	return task, record, nil
}

func completionHTTPTask(task *Task) bool {
	if task == nil || task.ID == "" || task.Protocol != "http" || task.Meta == nil || task.Meta.Req == nil || task.Meta.Opts == nil {
		return false
	}
	u, err := url.Parse(task.Meta.Req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	switch task.Status {
	case base.DownloadStatusReady, base.DownloadStatusRunning, base.DownloadStatusPause,
		base.DownloadStatusWait, base.DownloadStatusError, base.DownloadStatusDone:
		return true
	}
	return false
}

// prepareTaskCompletion is called inside the existing M transaction, after its
// R/member checks. owned is a private, not-yet-published candidate: this helper
// attaches its reference, never a reference on a live Task. A failed/unknown M
// cannot publish that candidate or grant first IO. Bucket setup belongs to Core
// startup; this helper cannot create buckets or open another transaction.
func prepareTaskCompletion(tx StorageTransaction, dataset string, owned *Task, initial transfer.VerifiedCompletionRecord) error {
	if tx == nil {
		return completionStateError(TaskCompletionUnavailable, nil)
	}
	if err := requireDataset(tx, dataset); err != nil {
		return err
	}
	if !completionHTTPTask(owned) || owned.Status == base.DownloadStatusDone ||
		initial.Phase != transfer.CompletionPrepared || !validCompletionStateRecord(initial) {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if owned.Completion != nil && (owned.Completion.Version != 1 || owned.Completion.CompletionID != initial.ID) {
		return completionStateError(TaskCompletionHeld, nil)
	}
	// Never overwrite an old Task or orphan record under a reused ID. Original
	// R/M idempotency must short-circuit before preparing a candidate again.
	for _, bucket := range []string{bucketTask, bucketTaskCompletion, bucketCompletionRequired} {
		var raw json.RawMessage
		exists, err := tx.Get(bucket, owned.ID, &raw)
		if err != nil {
			return completionStateError(TaskCompletionUnavailable, err)
		}
		if exists {
			return completionStateError(TaskCompletionHeld, nil)
		}
	}
	owned.Completion = &TaskCompletionRef{Version: 1, CompletionID: initial.ID}
	if err := tx.Put(bucketTask, owned.ID, owned); err != nil {
		return completionStateError(TaskCompletionUnavailable, err)
	}
	if err := tx.Put(bucketTaskCompletion, owned.ID, initial); err != nil {
		return completionStateError(TaskCompletionUnavailable, err)
	}
	return completionStoreError(tx.Put(bucketCompletionRequired, owned.ID,
		completionRequirement{Version: 1, Capability: transfer.VerifiedCompletionCapability}))
}

// completionStateRun performs only durable CAS conditions. The caller holds
// the real lifecycle Fence and has checked ctx/membership/active state. Taking
// owner.mu, CheckRun, live Task locks or another Fence here would invert the
// owner -> DB order. readRecovery decodes owned storage values, not live Tasks.
func completionStateRun(tx StorageTransaction, token transfer.RecoveryToken) error {
	if token.TaskID == "" || token.Episode == "" || token.Generation == 0 || token.SourceRevision == 0 {
		return completionStateError(TaskCompletionRunChanged, nil)
	}
	_, recovery, err := readRecovery(tx, token.TaskID)
	if err != nil {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if recovery.Episode != token.Episode || recovery.Generation != token.Generation || recovery.SourceRevision != token.SourceRevision {
		return completionStateError(TaskCompletionRunChanged, nil)
	}
	return nil
}

func saveTaskCompletionState(store AtomicStorage, dataset string, token transfer.RecoveryToken, expected, next transfer.VerifiedCompletionRecord) error {
	if store == nil {
		return completionStateError(TaskCompletionUnavailable, nil)
	}
	err := store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		if err := completionStateRun(tx, token); err != nil {
			return err
		}
		_, current, err := readTaskCompletion(tx, token.TaskID)
		if err != nil {
			return err
		}
		if current != expected {
			return completionStateError(TaskCompletionConflict, nil)
		}
		if err := completionStateTransition(expected, next, false); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskCompletion, token.TaskID, next); err != nil {
			return completionStateError(TaskCompletionUnavailable, err)
		}
		return completionStoreError(tx.Put(bucketCompletionRequired, token.TaskID,
			completionRequirement{Version: 1, Capability: transfer.VerifiedCompletionCapability}))
	})
	return completionStoreError(err)
}

// commitTaskCompletionState writes C using the CURRENT DB Task, not an old
// caller snapshot. Only done, UpdatedAt and direct-HTTP progress are changed.
// It neither publishes API memory nor emits completion: those are the Core
// caller's ACK-confirmed, still-Fenced work. Any UpdateAtomic error, including a
// successful disk commit with lost ACK, returns fresh=false. finalProgress must
// be an immutable caller-owned sample, never an unsynchronized live Task field.
func commitTaskCompletionState(store AtomicStorage, dataset string, token transfer.RecoveryToken, expected, next transfer.VerifiedCompletionRecord, finalProgress *Progress) (bool, error) {
	if store == nil {
		return false, completionStateError(TaskCompletionUnavailable, nil)
	}
	fresh := false
	err := store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		if err := completionStateRun(tx, token); err != nil {
			return err
		}
		currentTask, current, err := readTaskCompletion(tx, token.TaskID)
		if err != nil {
			return err
		}
		if err := completionStateTransition(expected, next, true); err != nil {
			return err
		}
		if current.Phase == transfer.CompletionCommitted && current == next {
			return nil // An authoritative same-C read, never a new success permit.
		}
		if !completionFinalProgress(finalProgress, next.Size) || !completionHTTPProgress(currentTask.Progress) {
			return completionStateError(TaskCompletionHeld, nil)
		}
		if current != expected {
			return completionStateError(TaskCompletionConflict, nil)
		}
		used := finalProgress.Used
		if currentTask.Progress != nil && currentTask.Progress.Used > used {
			used = currentTask.Progress.Used
		}
		currentTask.Status = base.DownloadStatusDone
		currentTask.UpdatedAt = time.Now()
		currentTask.Progress = &Progress{Used: used, Downloaded: next.Size}
		if err := tx.Put(bucketTask, token.TaskID, currentTask); err != nil {
			return completionStateError(TaskCompletionUnavailable, err)
		}
		if err := tx.Put(bucketTaskCompletion, token.TaskID, next); err != nil {
			return completionStateError(TaskCompletionUnavailable, err)
		}
		if err := tx.Put(bucketCompletionRequired, token.TaskID,
			completionRequirement{Version: 1, Capability: transfer.VerifiedCompletionCapability}); err != nil {
			return completionStateError(TaskCompletionUnavailable, err)
		}
		fresh = true
		return nil
	})
	if err != nil {
		return false, completionStoreError(err)
	}
	return fresh, nil
}

func completionFinalProgress(p *Progress, size int64) bool {
	return p != nil && size >= 0 && p.Downloaded == size && p.Speed == 0 && completionHTTPProgress(p)
}

func completionHTTPProgress(p *Progress) bool {
	// An early prepared candidate can legitimately lack a progress snapshot.
	return p == nil || (p.Used >= 0 && p.Speed >= 0 && p.Downloaded >= 0 &&
		p.Uploaded == 0 && p.UploadSpeed == 0 && p.MediaPhase == "" && p.ExtractStatus == ExtractStatusNone &&
		p.ExtractProgress == 0 && p.MultiPartBaseName == "" && p.MultiPartNumber == 0 && !p.MultiPartIsFirst)
}

func completionStateTransition(before, after transfer.VerifiedCompletionRecord, commit bool) error {
	if !validCompletionStateRecord(before) || !validCompletionStateRecord(after) ||
		before.Version != after.Version || before.ID != after.ID || before.ExpectedSHA256 != after.ExpectedSHA256 {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if !completionBoundName(before.StagingName, after.StagingName) || !completionBoundName(before.FinalName, after.FinalName) ||
		!completionBoundIdentity(before.DirectoryIdentity, after.DirectoryIdentity) ||
		!completionBoundIdentity(before.StagingIdentity, after.StagingIdentity) ||
		(before.Size >= 0 && before.Size != after.Size) {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if commit {
		if before.Phase == transfer.CompletionPublished && after.Phase == transfer.CompletionCommitted {
			return nil
		}
		return completionStateError(TaskCompletionHeld, nil)
	}
	if before.Phase == transfer.CompletionCommitted || after.Phase == transfer.CompletionCommitted {
		return completionStateError(TaskCompletionHeld, nil)
	}
	if before == after {
		return nil
	}
	switch before.Phase {
	case transfer.CompletionPrepared:
		if after.Phase == transfer.CompletionPrepared || after.Phase == transfer.CompletionWaiting {
			return nil
		}
	case transfer.CompletionWaiting:
		if after.Phase == transfer.CompletionVerifying {
			return nil
		}
	case transfer.CompletionVerifying:
		if after.Phase == transfer.CompletionVerified {
			return nil
		}
	case transfer.CompletionVerified:
		if after.Phase == transfer.CompletionPublished || after.Phase == transfer.CompletionVerifying {
			return nil
		}
	case transfer.CompletionPublished:
		if after.Phase == transfer.CompletionVerifying {
			return nil
		}
	}
	return completionStateError(TaskCompletionHeld, nil)
}

func completionBoundName(before, after string) bool { return before == "" || before == after }
func completionBoundIdentity(before, after transfer.CompletionFileIdentity) bool {
	if (before.Inode == 0 && before.Device != 0) || (after.Inode == 0 && after.Device != 0) {
		return false
	}
	return before.Inode == 0 || before == after
}

func validCompletionStateRecord(r transfer.VerifiedCompletionRecord) bool {
	if transfer.ValidateVerifiedCompletionRecord(r) != nil ||
		(r.DirectoryIdentity.Inode == 0 && r.DirectoryIdentity.Device != 0) ||
		(r.StagingIdentity.Inode == 0 && r.StagingIdentity.Device != 0) {
		return false
	}
	if r.StagingIdentity.Inode != 0 && (r.StagingName == "" || r.FinalName == "" ||
		r.DirectoryIdentity.Inode == 0 || r.StagingIdentity.Device != r.DirectoryIdentity.Device) {
		return false
	}
	return true
}
