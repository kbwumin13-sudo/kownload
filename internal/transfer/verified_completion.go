package transfer

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

// VerifiedCompletionCapability applies only to new, single-artifact direct HTTP
// tasks. A caller must negotiate a versioned endpoint, not send an ignorable
// option to a legacy create endpoint. Provider and media digests are unrelated.
const VerifiedCompletionCapability = "http-sha256-completion/v1"

type VerifiedCompletionPhase string

const (
	CompletionPrepared  VerifiedCompletionPhase = "prepared"
	CompletionWaiting   VerifiedCompletionPhase = "waiting_verify"
	CompletionVerifying VerifiedCompletionPhase = "verifying"
	CompletionVerified  VerifiedCompletionPhase = "verified"
	CompletionPublished VerifiedCompletionPhase = "published_pending_commit"
	CompletionCommitted VerifiedCompletionPhase = "committed"
)

// CompletionFileIdentity is a local filesystem ownership anchor, not proof of
// remote resource identity or of absence of writers. Reuse the Core run token
// and source revision; never use this identity as another execution generation.
type CompletionFileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// VerifiedCompletionRecord lives in the existing Task store. The Core must save
// its intent/required-state before creating staging, then pin the created file
// identity before the first download write. The initial phase is prepared,
// with size -1, no staging inode, and possibly unresolved basenames. Persist
// the final names/root before creating staging, then the file anchor before any
// download write; Complete rejects prepared.
// Size must be sealed to a known size after joining all writers. Paths are
// single basenames in one pinned directory, never arbitrary relative trees.
// ActualSHA256 describes the bytes at completion, not later edits by scripts.
type VerifiedCompletionRecord struct {
	Version           int                     `json:"version"`
	ID                string                  `json:"completionId"`
	Phase             VerifiedCompletionPhase `json:"phase"`
	DirectoryIdentity CompletionFileIdentity  `json:"directoryIdentity"`
	StagingIdentity   CompletionFileIdentity  `json:"stagingIdentity"`
	StagingName       string                  `json:"stagingName"`
	FinalName         string                  `json:"finalName"`
	Size              int64                   `json:"size"`
	ExpectedSHA256    string                  `json:"expectedSHA256"`
	ActualSHA256      string                  `json:"actualSHA256,omitempty"`
}

type VerifiedCompletionErrorKind string

const (
	CompletionInvalid     VerifiedCompletionErrorKind = "invalid_completion"
	CompletionCanceled    VerifiedCompletionErrorKind = "completion_canceled"
	CompletionStale       VerifiedCompletionErrorKind = "stale_completion"
	CompletionIdentity    VerifiedCompletionErrorKind = "completion_identity_changed"
	CompletionIO          VerifiedCompletionErrorKind = "completion_io_failed"
	CompletionConflict    VerifiedCompletionErrorKind = "publish_conflict"
	CompletionUnsupported VerifiedCompletionErrorKind = "publish_unsupported"
	CompletionStorage     VerifiedCompletionErrorKind = "completion_storage_unconfirmed"
)

// Errors omit paths and digests. Unwrap is for local classification, not an API
// error message; the Core/REST owners must whitelist public failure fields.
type VerifiedCompletionError struct {
	Kind VerifiedCompletionErrorKind
	Err  error
}

func (e *VerifiedCompletionError) Error() string { return "completion: " + string(e.Kind) }
func (e *VerifiedCompletionError) Unwrap() error { return e.Err }

func completionError(kind VerifiedCompletionErrorKind, err error) error {
	return &VerifiedCompletionError{Kind: kind, Err: err}
}

// NormalizeExpectedSHA256 allows an absent requirement at the create boundary.
// VerifyHTTPCompletion and Complete themselves require a nonempty expectation.
// Invalid input is rejected before any file operation; whitespace is not hex.
func NormalizeExpectedSHA256(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) != 64 {
		return "", completionError(CompletionInvalid, nil)
	}
	bytes, err := hex.DecodeString(value)
	if err != nil {
		return "", completionError(CompletionInvalid, err)
	}
	return hex.EncodeToString(bytes), nil
}

func checkCompletion(ctx context.Context, checkRun func() error) error {
	if ctx == nil || checkRun == nil {
		return completionError(CompletionInvalid, nil)
	}
	if err := ctx.Err(); err != nil {
		return completionError(CompletionCanceled, err)
	}
	if err := checkRun(); err != nil {
		return completionError(CompletionStale, err)
	}
	return nil
}

// VerifyHTTPCompletion borrows the same O_RDONLY regular file handle throughout
// the bounded streaming hash. The caller must have joined the exact HTTP run's
// writers, and keep them stopped until publication/commit. CheckRun consumes
// the existing immutable run token. Its context must be canceled on invalidation;
// CheckRun alone is not an atomic admission fence for publication or commit.
// No Core/Task/owner/budget lock may be held while calling this function.
func VerifyHTTPCompletion(ctx context.Context, file *os.File, size int64, expectedSHA256 string, checkRun func() error) (IntegrityResult, error) {
	expected, err := NormalizeExpectedSHA256(expectedSHA256)
	if err != nil || expected == "" {
		return IntegrityResult{}, completionError(CompletionInvalid, err)
	}
	if err := checkCompletion(ctx, checkRun); err != nil {
		return IntegrityResult{}, err
	}
	if err := completionReadOnly(file); err != nil {
		return IntegrityResult{}, err
	}
	result, hashErr := VerifyIntegrityFile(ctx, file, size, []IntegrityCheck{{Algorithm: IntegritySHA256, Expected: expected}})
	if err := checkCompletion(ctx, checkRun); err != nil {
		return IntegrityResult{}, err // A late result cannot masquerade as verified.
	}
	return result, hashErr
}

// VerifiedCompletionHooks adapt the existing Core/Store, not a second ledger.
// CheckRun must include the Task's required-state and immutable run/source guard.
// Fence serializes invalidation (pause/delete/source change) and the callback
// using the same lifecycle admission mechanism. Checking then unlocking before
// callback execution is NOT a valid Fence. Do not acquire d.lock while waiting
// for it, or reenter Task/errorPersistence locks from these callbacks.
//
// Save atomically persists a copy with the same completion ID and required-state;
// it must reject missing/deleted Tasks, mismatched intent, or regressing a done
// Task. Commit atomically saves committed+required+Task.done, then linearizes the
// memory/API snapshot. It returns true ONLY for the acknowledged fresh winner;
// false means an authoritative read/transaction proved this same ID committed.
// Errors, including unknown ACK, grant no success permission. Resolve them by
// reading the same Task/ID, never by constructing another Task or blindly retrying.
type VerifiedCompletionHooks struct {
	CheckRun func() error
	Fence    func(context.Context, func() error) error
	Save     func(VerifiedCompletionRecord) error
	Commit   func(VerifiedCompletionRecord) (bool, error)
}

// Fresh is admission to the existing Core success path, not proof that an
// external webhook/script ran exactly once. The Core must guard each still
// pending asynchronous success action and apply its trusted side-effect policy.
// Reopening an already committed Task never replays external success actions.
type VerifiedCompletionPermit struct {
	Record VerifiedCompletionRecord
	Fresh  bool
}

// VerifiedCompleter limits only streaming hashes; it is not another task queue
// or background worker. Callers choose an explicit bound, outside network slots.
type VerifiedCompleter struct{ slots chan struct{} }

func NewVerifiedCompleter(limit int) (*VerifiedCompleter, error) {
	if limit < 1 || limit > 64 {
		return nil, completionError(CompletionInvalid, nil)
	}
	return &VerifiedCompleter{slots: make(chan struct{}, limit)}, nil
}

// Complete is verify -> persist verified -> no-replace publish -> persist
// published -> atomic Core commit. Every uncommitted recovery hashes again.
// A preexisting final is accepted ONLY when it is the same regular file as the
// persisted staging anchor. Merely equal size/hash never establishes ownership.
// Filesystem publication and the Store transaction are separate: retain staging
// on all paths, including success. No automatic output/cloud cleanup occurs.
//
// Caller-owned file and directory handles stay open. File reads/hash/Sync happen
// outside Fence; only short metadata/publication and store callbacks run within
// it. The caller owns quiescence and exclusive staging namespace mutation.
func (v *VerifiedCompleter) Complete(ctx context.Context, directory *CompletionDirectory, file *os.File, record VerifiedCompletionRecord, hooks VerifiedCompletionHooks) (VerifiedCompletionPermit, error) {
	var none VerifiedCompletionPermit
	if v == nil || v.slots == nil || hooks.Fence == nil || hooks.Save == nil || hooks.Commit == nil {
		return none, completionError(CompletionInvalid, nil)
	}
	if err := validateCompletionRecord(record); err != nil {
		return none, err
	}
	if err := checkCompletion(ctx, hooks.CheckRun); err != nil {
		return none, err
	}
	// This snapshot must come from the guarded authoritative store, not a client.
	if record.Phase == CompletionCommitted {
		return VerifiedCompletionPermit{Record: record}, nil
	}
	if directory == nil || directory.handle == nil {
		return none, completionError(CompletionInvalid, nil)
	}
	before, err := directory.checkAnchor(file, record)
	if err != nil {
		return none, err
	}
	fenced := func(operation func() error) error {
		return hooks.Fence(ctx, func() error {
			if err := checkCompletion(ctx, hooks.CheckRun); err != nil {
				return err
			}
			return operation()
		})
	}
	save := func(phase VerifiedCompletionPhase) error {
		record.Phase = phase
		return fenced(func() error {
			if err := hooks.Save(record); err != nil {
				return completionError(CompletionStorage, err)
			}
			return nil
		})
	}
	record.ActualSHA256 = "" // A new run never reuses an uncommitted digest.
	if err := save(CompletionVerifying); err != nil {
		return none, err
	}
	select {
	case v.slots <- struct{}{}:
	case <-ctx.Done():
		return none, completionError(CompletionCanceled, ctx.Err())
	}
	result, err := VerifyHTTPCompletion(ctx, file, record.Size, record.ExpectedSHA256, hooks.CheckRun)
	<-v.slots
	if err != nil {
		return none, err
	}
	if result.Outcome != IntegrityMatched {
		return none, completionError(CompletionInvalid, nil)
	}
	// The transfer writer must also have synced/closed its own handle. Sync here
	// confirms this filesystem accepts the durability operation before publishing.
	if err := file.Sync(); err != nil {
		return none, completionError(CompletionIO, err)
	}
	record.ActualSHA256 = result.Actual[IntegritySHA256]
	if err := fenced(func() error {
		if err := directory.checkUnchanged(file, record, before); err != nil {
			return err
		}
		record.Phase = CompletionVerified
		if err := hooks.Save(record); err != nil {
			return completionError(CompletionStorage, err)
		}
		return nil
	}); err != nil {
		return none, err
	}
	if err := fenced(func() error {
		if err := directory.checkUnchanged(file, record, before); err != nil {
			return err
		}
		return directory.publish(file, record)
	}); err != nil {
		return none, err
	}
	if err := save(CompletionPublished); err != nil {
		return none, err
	}
	var fresh bool
	if err := fenced(func() error {
		if err := directory.checkUnchanged(file, record, before); err != nil {
			return err
		}
		if err := directory.checkFinal(file, record); err != nil {
			return err
		}
		record.Phase = CompletionCommitted
		var err error
		fresh, err = hooks.Commit(record)
		if err != nil {
			return completionError(CompletionStorage, err)
		}
		return nil
	}); err != nil {
		return none, err
	}
	return VerifiedCompletionPermit{Record: record, Fresh: fresh}, nil
}

func completionName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00")
}

func validateCompletionRecord(r VerifiedCompletionRecord) error {
	if r.Phase == CompletionPrepared {
		return completionError(CompletionInvalid, nil)
	}
	return ValidateVerifiedCompletionRecord(r)
}

// ValidateVerifiedCompletionRecord checks only this value's schema. It does not
// grant execution, prove a Task/required-state tuple, or replace the Core token.
// A prepared intent can be persisted before opening files; Complete rejects it.
func ValidateVerifiedCompletionRecord(r VerifiedCompletionRecord) error {
	expected, err := NormalizeExpectedSHA256(r.ExpectedSHA256)
	if err != nil || expected == "" || expected != r.ExpectedSHA256 || r.Version != 1 || r.ID == "" || len(r.ID) > 128 {
		return completionError(CompletionInvalid, err)
	}
	if (r.StagingName != "" && !completionName(r.StagingName)) ||
		(r.FinalName != "" && !completionName(r.FinalName)) ||
		(r.StagingName != "" && strings.EqualFold(r.StagingName, r.FinalName)) {
		return completionError(CompletionInvalid, nil)
	}
	if r.Phase == CompletionPrepared {
		if r.Size != -1 || r.ActualSHA256 != "" || (r.StagingIdentity.Inode != 0 && r.DirectoryIdentity.Inode == 0) {
			return completionError(CompletionInvalid, nil)
		}
		return nil
	}
	if !completionName(r.StagingName) || !completionName(r.FinalName) || r.Size < 0 || r.DirectoryIdentity.Inode == 0 || r.StagingIdentity.Inode == 0 {
		return completionError(CompletionInvalid, nil)
	}
	switch r.Phase {
	case CompletionWaiting, CompletionVerifying:
		if r.ActualSHA256 != "" {
			return completionError(CompletionInvalid, nil)
		}
	case CompletionVerified, CompletionPublished, CompletionCommitted:
		if r.ActualSHA256 != expected {
			return completionError(CompletionInvalid, nil)
		}
	default:
		return completionError(CompletionInvalid, nil)
	}
	return nil
}

// CompletionDirectory pins the actual directory object, not a mutable path
// prefix. It is borrowed by Complete; Close belongs to the caller. Concurrent
// namespace changes inside its managed staging area are forbidden by contract.
type CompletionDirectory struct {
	handle   *os.File
	identity CompletionFileIdentity
}

func (d *CompletionDirectory) Identity() CompletionFileIdentity { return d.identity }
func (d *CompletionDirectory) Close() error                     { return d.handle.Close() }

// OpenCompletionDirectory opens an existing absolute directory component by
// component without following symlinks. No directories or files are created.
// The persisted Identity must match when reopening a completion after restart.
func OpenCompletionDirectory(path string) (*CompletionDirectory, error) {
	return openCompletionDirectory(path)
}

func CompletionIdentityOf(file *os.File) (CompletionFileIdentity, error) {
	return completionIdentityOf(file)
}

// OpenStaging never follows a symlink or blocks opening a FIFO. It checks the
// persisted inode before returning the same O_RDONLY handle to the caller.
func (d *CompletionDirectory) OpenStaging(record VerifiedCompletionRecord) (*os.File, error) {
	if d == nil || d.handle == nil || !completionName(record.StagingName) || d.identity != record.DirectoryIdentity {
		return nil, completionError(CompletionIdentity, nil)
	}
	file, err := d.openFile(record.StagingName)
	if err != nil {
		return nil, err
	}
	identity, err := CompletionIdentityOf(file)
	if err != nil || identity != record.StagingIdentity {
		_ = file.Close()
		return nil, completionError(CompletionIdentity, err)
	}
	return file, nil
}

func (d *CompletionDirectory) checkAnchor(file *os.File, record VerifiedCompletionRecord) (os.FileInfo, error) {
	if file == nil || record.DirectoryIdentity != d.identity {
		return nil, completionError(CompletionIdentity, nil)
	}
	if err := completionReadOnly(file); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.Size {
		return nil, completionError(CompletionIdentity, err)
	}
	anchor, err := d.OpenStaging(record)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	other, err := anchor.Stat()
	if err != nil || !os.SameFile(info, other) {
		return nil, completionError(CompletionIdentity, err)
	}
	return info, nil
}

func (d *CompletionDirectory) checkUnchanged(file *os.File, r VerifiedCompletionRecord, before os.FileInfo) error {
	after, err := d.checkAnchor(file, r)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		return completionError(CompletionIdentity, nil)
	}
	return nil
}

func (d *CompletionDirectory) checkFinal(file *os.File, r VerifiedCompletionRecord) error {
	final, err := d.openFile(r.FinalName)
	if err != nil {
		return completionError(CompletionConflict, err)
	}
	defer final.Close()
	actual, err := final.Stat()
	if err != nil {
		return completionError(CompletionIO, err)
	}
	wanted, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(wanted, actual) {
		return completionError(CompletionConflict, err)
	}
	return nil
}

func (d *CompletionDirectory) publish(file *os.File, record VerifiedCompletionRecord) error {
	if err := d.link(record.StagingName, record.FinalName); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := d.checkFinal(file, record); err != nil {
		return err
	}
	if err := d.handle.Sync(); err != nil {
		return completionError(CompletionIO, err)
	}
	return nil
}
