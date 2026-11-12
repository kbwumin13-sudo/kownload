package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type mcpWriteMember struct {
	Index     int          `json:"index"`
	TaskID    string       `json:"taskId"`
	State     string       `json:"state"`
	TaskState MCPTaskState `json:"taskState"`
}

// R2 shares the original operation bucket and unified MCP namespace. Actions
// have no member or creation ref. BindingHash is server execution intent; it is
// deliberately separate from the caller command's RawHash.
type mcpWriteRecord struct {
	Version              int                  `json:"version"`
	Namespace            string               `json:"namespace"`
	RequiredCapability   string               `json:"requiredCapability"`
	Kind                 MCPWriteKind         `json:"kind"`
	Command              MCPWriteRequest      `json:"command"`
	RawHash              string               `json:"rawHash"`
	ExecutionBindingHash string               `json:"executionBindingHash"`
	Binding              MCPExecutionBinding  `json:"binding"`
	Member               *mcpWriteMember      `json:"member"`
	State                MCPWriteReceiptState `json:"state"`
	Result               *MCPWriteResult      `json:"result"`
	CreatedAt            time.Time            `json:"createdAt"`
	UpdatedAt            time.Time            `json:"updatedAt"`
}

func mcpWriteDecode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return &MCPWriteError{MCPWriteInvalidRequest}
	}
	return nil
}
func (r *mcpWriteRecord) UnmarshalJSON(raw []byte) error {
	type value mcpWriteRecord
	return mcpWriteDecode(raw, (*value)(r))
}
func (s *MCPTaskState) UnmarshalJSON(raw []byte) error {
	type value MCPTaskState
	return mcpWriteDecode(raw, (*value)(s))
}
func mcpWriteErr(code MCPWriteErrorCode) error { return &MCPWriteError{code} }
func mcpWriteStoreError(err error) error {
	if err == nil {
		return nil
	}
	var fixed *MCPWriteError
	if errors.As(err, &fixed) && fixed.Code.Valid() {
		return fixed
	}
	if errors.Is(err, ErrDatasetMismatch) || errors.Is(err, ErrDatasetMissing) || errors.Is(err, ErrDatasetInvalid) {
		return mcpWriteErr(MCPWriteDatasetMismatch)
	}
	return mcpWriteErr(MCPWriteStorageUnavailable)
}

var mcpTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var mcpEpisodePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

var mcpPolicyPattern = regexp.MustCompile(`^mcp-authority-v1:[0-9a-f]{64}$`)
var mcpNetworkPattern = regexp.MustCompile(`^mcp-network/v1:[0-9a-f]{64}$`)

func mcpWriteName(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 255 && !strings.ContainsAny(s, "/\\\x00") && filepath.Base(s) == s
}
func mcpWriteTargetValid(t MCPWriteTarget) bool {
	return mcpEpisodePattern.MatchString(t.Episode) && t.Generation > 0 && t.Generation <= 9007199254740991 && t.SourceRevision > 0 && t.SourceRevision <= 9007199254740991
}
func mcpWriteCommandValid(c MCPWriteRequest) bool {
	if c.Version != 1 || !datasetIDPattern.MatchString(c.DatasetID) || !mcpPolicyPattern.MatchString(c.PolicyID) || !operationIDPattern.MatchString(c.OperationID) {
		return false
	}
	switch c.Kind {
	case MCPWriteCreate:
		if c.Create == nil || c.Action != nil || !operationIDPattern.MatchString(c.Create.OutputRootID) || !mcpWriteName(c.Create.FileName) {
			return false
		}
		u, e := url.Parse(c.Create.URL)
		return e == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil
	case MCPWritePause, MCPWriteResume:
		return c.Create == nil && c.Action != nil && mcpTaskIDPattern.MatchString(c.Action.TaskID) && mcpWriteTargetValid(c.Action.Expected)
	}
	return false
}
func mcpWriteBindingValid(b MCPExecutionBinding) bool {
	if b.Version != 1 || b.RequiredCapability != MCPWriteNamespace || !datasetIDPattern.MatchString(b.DatasetID) || !mcpPolicyPattern.MatchString(b.PolicyID) || !mcpNetworkPattern.MatchString(b.NetworkPolicyID) || b.EffectsVersion != "effects/v1" || b.Effects == nil {
		return false
	}
	last := ""
	for _, v := range b.Effects {
		if v <= last {
			return false
		}
		switch v {
		case "script", "auto_open", "webhook", "post_process":
		default:
			return false
		}
		last = v
	}
	if b.PreserveGUI {
		return b.Root == nil && b.FileName == "" && b.StagingName == "" && len(b.Effects) == 0
	}
	return b.Root != nil && operationIDPattern.MatchString(b.Root.RootID) && filepath.IsAbs(b.Root.Path) && filepath.Clean(b.Root.Path) == b.Root.Path && b.Root.Identity.Inode != 0 && mcpWriteName(b.FileName) && mcpWriteName(b.StagingName) && !strings.EqualFold(b.FileName, b.StagingName)
}
func mcpWriteHash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func mcpWriteKey(op string) string { return MCPWriteNamespace + ":" + op }
func mcpWriteSameOutput(a, b MCPExecutionBinding) bool {
	return !a.PreserveGUI && !b.PreserveGUI && reflect.DeepEqual(a.Root, b.Root) && a.FileName == b.FileName && a.StagingName == b.StagingName
}
func mcpWriteEffectsSubset(a, b []string) bool {
	for _, v := range a {
		found := false
		for _, p := range b {
			if v == p {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func mcpWriteStateValid(s MCPTaskState) bool {
	if s.Version != 2 || s.RequiredCapability != MCPWriteNamespace || !operationIDPattern.MatchString(s.OperationID) || !mcpWriteBindingValid(s.CreationBinding) || !mcpWriteBindingValid(s.CurrentExecutionBinding) || !mcpWriteSameOutput(s.CreationBinding, s.CurrentExecutionBinding) || s.CreationBinding.DatasetID != s.CurrentExecutionBinding.DatasetID || !mcpWriteEffectsSubset(s.CurrentExecutionBinding.Effects, s.CreationBinding.Effects) {
		return false
	}
	b := s.Output.Binding
	switch s.Output.Phase {
	case MCPWriteOutputPlanned:
		return b == nil
	case MCPWriteOutputStaging, MCPWriteOutputPublishing, MCPWriteOutputPublished, MCPWriteOutputUncertain:
		return b != nil && b.OwnerID == s.OperationID && b.DirectoryIdentity == s.CreationBinding.Root.Identity && b.StagingIdentity.Inode != 0 && b.StagingIdentity.Device == b.DirectoryIdentity.Device && b.StagingName == s.CreationBinding.StagingName && b.FinalName == s.CreationBinding.FileName
	}
	return false
}
func (r *mcpWriteRecord) valid() bool {
	if r == nil || r.Version != 2 || r.Namespace != MCPWriteNamespace || r.RequiredCapability != MCPWriteNamespace || r.Kind != r.Command.Kind || !mcpWriteCommandValid(r.Command) || !mcpWriteBindingValid(r.Binding) || r.Binding.DatasetID != r.Command.DatasetID || r.Binding.PolicyID != r.Command.PolicyID || r.RawHash != mcpWriteHash(r.Command) || r.ExecutionBindingHash != mcpWriteHash(r.Binding) || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return false
	}
	if r.Kind == MCPWriteCreate {
		if r.Binding.PreserveGUI || r.Member == nil || r.Member.Index != 0 || !mcpTaskIDPattern.MatchString(r.Member.TaskID) || !mcpWriteStateValid(r.Member.TaskState) || r.Member.TaskState.OperationID != r.Command.OperationID || !reflect.DeepEqual(r.Member.TaskState.CreationBinding, r.Binding) || r.Binding.Root.RootID != r.Command.Create.OutputRootID || r.Binding.FileName != r.Command.Create.FileName {
			return false
		}
		if r.Member.State != "pending" && r.Member.State != "created" {
			return false
		}
	} else if r.Member != nil {
		return false
	}
	switch r.State {
	case MCPWritePending:
		return r.Result == nil && (r.Member == nil || r.Member.State == "pending")
	case MCPWriteSettled:
		result := r.Result
		if result == nil {
			return false
		}
		if result.Outcome == MCPWriteRejected {
			return result.ErrorCode != nil && result.ErrorCode.Valid() && (result.TaskID == nil || mcpTaskIDPattern.MatchString(*result.TaskID)) && (r.Member == nil || r.Member.State == "pending")
		}
		if result.TaskID == nil || !mcpTaskIDPattern.MatchString(*result.TaskID) || result.ErrorCode != nil {
			return false
		}
		if r.Kind == MCPWriteCreate {
			return result.Outcome == MCPWriteCreated && r.Member.State == "created" && *result.TaskID == r.Member.TaskID && result.Expected == nil && result.Accepted == nil
		}
		return result.Outcome == MCPWriteApplied && *result.TaskID == r.Command.Action.TaskID && result.Expected != nil && *result.Expected == r.Command.Action.Expected && result.Accepted != nil && mcpWriteTargetValid(*result.Accepted)
	}
	return false
}
func (r mcpWriteRecord) receipt() MCPWriteReceipt {
	kind := r.Kind
	return MCPWriteReceipt{Version: 1, DatasetID: r.Command.DatasetID, PolicyID: r.Command.PolicyID, OperationID: r.Command.OperationID, Kind: &kind, State: r.State, Result: r.Result, QueryRequired: r.State != MCPWriteSettled}
}
func readMCPWriteRecord(reader datasetReader, dataset, op string) (*mcpWriteRecord, error) {
	if err := requireDataset(reader, dataset); err != nil {
		return nil, mcpWriteStoreError(err)
	}
	var raw json.RawMessage
	exists, err := reader.Get(bucketTaskOperations, mcpWriteKey(op), &raw)
	if err != nil {
		return nil, mcpWriteErr(MCPWriteStorageUnavailable)
	}
	if !exists {
		return nil, nil
	}
	var r mcpWriteRecord
	if mcpWriteDecode(raw, &r) != nil || !r.valid() || r.Command.DatasetID != dataset || r.Command.OperationID != op {
		return nil, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	return &r, nil
}

// readMCPWriteTask requires the caller's single dataset view. All three durable
// pieces must agree. All-absent legacy is returned unchanged, never enrolled.
func readMCPWriteTask(reader datasetReader, id string) (*Task, mcpWriteRecord, error) {
	task := new(Task)
	present, err := reader.Get(bucketTask, id, task)
	if err != nil {
		return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteStorageUnavailable)
	}
	var link operationTaskLink
	linked, err := reader.Get(bucketOperationTasks, id, &link)
	if err != nil {
		return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteStorageUnavailable)
	}
	if !present || task.ID != id {
		return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	mcpLink := linked && link.Ref.Namespace == MCPWriteNamespace
	mcpRef := task.CreationOperation != nil && task.CreationOperation.Namespace == MCPWriteNamespace
	if task.MCPWrite == nil && !mcpLink && !mcpRef {
		if err := checkCreationAssociationFrom(reader, task); err != nil {
			return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteTaskUnavailable)
		}
		return task, mcpWriteRecord{}, nil
	}
	if task.MCPWrite == nil || !linked || !mcpRef || task.CreationOperation == nil || link.TaskID != id || link.Ref != *task.CreationOperation || link.Ref.Version != 2 || link.Ref.Namespace != MCPWriteNamespace || link.Ref.Index != 0 || !mcpWriteStateValid(*task.MCPWrite) {
		return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	s := task.MCPWrite
	r, err := readMCPWriteRecord(reader, s.CreationBinding.DatasetID, s.OperationID)
	if err != nil {
		return nil, mcpWriteRecord{}, err
	}
	if r == nil || r.Kind != MCPWriteCreate || r.Member == nil || r.Member.State != "created" || r.Member.TaskID != id || link.Ref.OperationID != s.OperationID || !reflect.DeepEqual(r.Member.TaskState, *s) || task.Protocol != "http" || task.Meta == nil || task.Meta.Req == nil || task.Meta.Opts == nil || task.Meta.Req.URL != r.Command.Create.URL || task.Meta.Opts.Path != s.CreationBinding.Root.Path || task.Meta.Opts.Name != s.CreationBinding.FileName {
		return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	if (task.Status == base.DownloadStatusDone) != (s.Output.Phase == MCPWriteOutputPublished) {
		return nil, mcpWriteRecord{}, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	return task, *r, nil
}

// reserveMCPWrite freezes R once. fresh describes the acknowledged reservation
// winner, not admission to RAM, a body writer, an action or external effects.
func reserveMCPWrite(store AtomicStorage, dataset string, command MCPWriteRequest, binding MCPExecutionBinding) (mcpWriteRecord, bool, error) {
	if store == nil || !mcpWriteCommandValid(command) || command.DatasetID != dataset || !mcpWriteBindingValid(binding) || binding.DatasetID != dataset || binding.PolicyID != command.PolicyID {
		return mcpWriteRecord{}, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	command, err := cloneOperationJSON(command)
	if err != nil {
		return mcpWriteRecord{}, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	binding, err = cloneOperationJSON(binding)
	if err != nil {
		return mcpWriteRecord{}, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	now := time.Now()
	candidate := mcpWriteRecord{Version: 2, Namespace: MCPWriteNamespace, RequiredCapability: MCPWriteNamespace, Kind: command.Kind, Command: command, RawHash: mcpWriteHash(command), ExecutionBindingHash: mcpWriteHash(binding), Binding: binding, State: MCPWritePending, CreatedAt: now, UpdatedAt: now}
	if command.Kind == MCPWriteCreate {
		candidate.Member = &mcpWriteMember{Index: 0, TaskID: NewTask().ID, State: "pending", TaskState: MCPTaskState{Version: 2, RequiredCapability: MCPWriteNamespace, OperationID: command.OperationID, CreationBinding: binding, CurrentExecutionBinding: binding, Output: MCPWriteOutputState{Phase: MCPWriteOutputPlanned}}}
	}
	if !candidate.valid() {
		return mcpWriteRecord{}, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	var result mcpWriteRecord
	fresh := false
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		existing, e := readMCPWriteRecord(tx, dataset, command.OperationID)
		if e != nil {
			return e
		}
		if existing != nil {
			if existing.RawHash != candidate.RawHash {
				return mcpWriteErr(MCPWriteOperationConflict)
			}
			result = *existing
			return nil
		}
		if e = tx.Put(bucketTaskOperations, mcpWriteKey(command.OperationID), &candidate); e != nil {
			return e
		}
		result = candidate
		fresh = true
		return nil
	})
	if err != nil {
		return mcpWriteRecord{}, false, mcpWriteStoreError(err)
	}
	return result, fresh, nil
}
func mcpWriteRecordCAS(tx StorageTransaction, dataset string, expected mcpWriteRecord) (*mcpWriteRecord, error) {
	current, err := readMCPWriteRecord(tx, dataset, expected.Command.OperationID)
	if err != nil {
		return nil, err
	}
	if current == nil || !reflect.DeepEqual(*current, expected) {
		return nil, mcpWriteErr(MCPWriteStaleTarget)
	}
	return current, nil
}

// M uses only a private candidate and caller-captured initial recovery state.
// The callback never takes live Task/owner locks or performs filesystem IO.
func createMCPWriteTask(store AtomicStorage, dataset string, expected mcpWriteRecord, owned *Task, initial recoveryRecord) (*Task, bool, error) {
	if store == nil || owned == nil || !expected.valid() || expected.Kind != MCPWriteCreate {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	candidate, err := cloneOperationJSON(owned)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	expected, err = cloneOperationJSON(expected)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	initial, err = cloneOperationJSON(initial)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	var result *Task
	fresh := false
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		r, e := mcpWriteRecordCAS(tx, dataset, expected)
		if e != nil {
			return e
		}
		if r.State != MCPWritePending {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		m := r.Member
		if candidate.ID != m.TaskID || candidate.Protocol != "http" || candidate.Meta == nil || candidate.Meta.Req == nil || candidate.Meta.Opts == nil || candidate.Meta.Req.URL != r.Command.Create.URL || candidate.Meta.Opts.Path != r.Binding.Root.Path || candidate.Meta.Opts.Name != r.Binding.FileName || candidate.Completion != nil || candidate.MCPWrite != nil || candidate.CreationOperation != nil || candidate.Handoff != nil || candidate.Status == base.DownloadStatusDone {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		for _, bucket := range []string{bucketTask, bucketOperationTasks, bucketTaskRecovery, bucketRecoveryRequired, bucketSave, bucketTaskCompletion, bucketCompletionRequired} {
			var raw json.RawMessage
			exists, e := tx.Get(bucket, candidate.ID, &raw)
			if e != nil {
				return e
			}
			if exists {
				return mcpWriteErr(MCPWriteOperationConflict)
			}
		}
		candidate.MCPWrite = &m.TaskState
		ref := CreationOperation{Version: 2, Namespace: MCPWriteNamespace, OperationID: r.Command.OperationID, Index: 0}
		candidate.CreationOperation = &ref
		if initial.Version != 4 || initial.Generation != 0 || initial.SourceRevision != 1 || initial.InitialAttempt.State != "ungranted" || initial.TotalUsed != 0 || initial.TransportUsed != 0 || initial.RefreshUsed != 0 || initial.DeferralUsed != 0 || initial.NextAction != 0 || initial.Actions == nil || len(initial.Actions) != 0 || initial.NotBefore != "" || initial.Coverage != nil || initial.LastExplicitRetry != nil {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		required, e := recoveryRequirementFor(initial)
		if e != nil || validateRecoveryTriplet(candidate, initial, required) != nil {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		hash, e := recoverySourceHash(candidate.Meta.Req, initial.ExecutionDefaultsHash)
		if e != nil || hash != initial.SourceHash {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		candidate.Status = base.DownloadStatusPause
		candidate.Progress = &Progress{}
		m.State = "created"
		r.State = MCPWriteSettled
		id := candidate.ID
		r.Result = &MCPWriteResult{TaskID: &id, Outcome: MCPWriteCreated}
		r.UpdatedAt = time.Now()
		if !r.valid() {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		for _, put := range []struct {
			bucket, key string
			value       any
		}{{bucketTask, id, candidate}, {bucketOperationTasks, id, &operationTaskLink{id, ref}}, {bucketTaskRecovery, id, &initial}, {bucketRecoveryRequired, id, &required}, {bucketTaskOperations, mcpWriteKey(r.Command.OperationID), r}} {
			if e = tx.Put(put.bucket, put.key, put.value); e != nil {
				return e
			}
		}
		result = candidate
		fresh = true
		return nil
	})
	if err != nil {
		return nil, false, mcpWriteStoreError(err)
	}
	return result, fresh, nil
}

func mcpWriteRun(reader datasetReader, token transfer.RecoveryToken) (*Task, error) {
	task, r, _, err := readSharedRecovery(reader, token.TaskID)
	if err != nil {
		return nil, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	if token.Episode != r.Episode || token.Generation != r.Generation || token.SourceRevision != r.SourceRevision || !mcpWriteTargetValid(MCPWriteTarget{r.Episode, r.Generation, r.SourceRevision}) {
		return nil, mcpWriteErr(MCPWriteStaleTarget)
	}
	hash, err := recoverySourceHash(task.Meta.Req, r.ExecutionDefaultsHash)
	if err != nil || hash != r.SourceHash {
		return nil, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	return task, nil
}

// saveMCPWriteOutput is a short, caller-Fenced CAS. No filesystem operation is
// performed here. A published transition must follow confirmed no-replace FS
// success; its ACK is not by itself permission for an external effect.
func saveMCPWriteOutput(store AtomicStorage, dataset string, token transfer.RecoveryToken, expected MCPTaskState, next MCPWriteOutputState, finalProgress *Progress) (*Task, bool, error) {
	if store == nil || !mcpWriteStateValid(expected) {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	next, err = cloneOperationJSON(next)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	progress, err := cloneOperationJSON(finalProgress)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	candidate := expected
	candidate.Output = next
	if !mcpWriteStateValid(candidate) {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	before := expected.Output.Phase
	transition := (before == MCPWriteOutputPlanned && next.Phase == MCPWriteOutputStaging) || (before == MCPWriteOutputStaging && next.Phase == MCPWriteOutputPublishing) || (before == MCPWriteOutputPublishing && (next.Phase == MCPWriteOutputPublished || next.Phase == MCPWriteOutputUncertain))
	if !transition || (expected.Output.Binding != nil && !reflect.DeepEqual(expected.Output.Binding, next.Binding)) {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	if next.Phase == MCPWriteOutputPublished {
		if progress == nil || !completionFinalProgress(progress, progress.Downloaded) {
			return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
		}
	} else if progress != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	var result *Task
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		if _, err := mcpWriteRun(tx, token); err != nil {
			return err
		}
		task, r, err := readMCPWriteTask(tx, token.TaskID)
		if err != nil {
			return err
		}
		if task.MCPWrite == nil || !reflect.DeepEqual(*task.MCPWrite, expected) || task.Status == base.DownloadStatusDone {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		if next.Phase == MCPWriteOutputPublished {
			if !completionHTTPProgress(task.Progress) {
				return mcpWriteErr(MCPWriteTaskUnavailable)
			}
			used := progress.Used
			if task.Progress != nil && task.Progress.Used > used {
				used = task.Progress.Used
			}
			task.Status = base.DownloadStatusDone
			task.Progress = &Progress{Used: used, Downloaded: progress.Downloaded}
		}
		task.MCPWrite = &candidate
		task.UpdatedAt = time.Now()
		r.Member.TaskState = candidate
		r.UpdatedAt = task.UpdatedAt
		if !r.valid() {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		if err = tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		if err = tx.Put(bucketTaskOperations, mcpWriteKey(r.Command.OperationID), &r); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return nil, false, mcpWriteStoreError(err)
	}
	return result, true, nil
}

// commitMCPWriteAction records a same-target action once. Core must stop/join
// the old writer and hold its membership/run Fence before calling. Resume RAM
// admission remains outside this transaction and cannot be recreated by GET.
func commitMCPWriteAction(store AtomicStorage, dataset string, expected mcpWriteRecord, accepted MCPWriteTarget) (*Task, bool, error) {
	if store == nil || !expected.valid() || expected.Kind == MCPWriteCreate || !mcpWriteTargetValid(accepted) {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, mcpWriteErr(MCPWriteInvalidRequest)
	}
	var result *Task
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		r, err := mcpWriteRecordCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if r.State != MCPWritePending {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		a := r.Command.Action
		token := transfer.RecoveryToken{TaskID: a.TaskID, Episode: a.Expected.Episode, Generation: a.Expected.Generation, SourceRevision: a.Expected.SourceRevision}
		if accepted != a.Expected {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		if _, err = mcpWriteRun(tx, token); err != nil {
			return err
		}
		task, creation, err := readMCPWriteTask(tx, a.TaskID)
		if err != nil {
			return err
		}
		if task.Status == base.DownloadStatusDone {
			return mcpWriteErr(MCPWriteTaskUnavailable)
		}
		if task.MCPWrite != nil {
			if r.Binding.PreserveGUI || r.Binding.DatasetID != task.MCPWrite.CreationBinding.DatasetID {
				return mcpWriteErr(MCPWritePermissionDenied)
			}
			if r.Kind == MCPWriteResume {
				s := task.MCPWrite
				if s.Output.Phase != MCPWriteOutputStaging || !mcpWriteSameOutput(s.CreationBinding, r.Binding) || !mcpWriteEffectsSubset(r.Binding.Effects, s.CreationBinding.Effects) {
					return mcpWriteErr(MCPWriteOutputConflict)
				}
				s.CurrentExecutionBinding = r.Binding
				if !mcpWriteStateValid(*s) {
					return mcpWriteErr(MCPWritePermissionDenied)
				}
				creation.Member.TaskState = *s
				creation.UpdatedAt = time.Now()
				if !creation.valid() {
					return mcpWriteErr(MCPWriteTaskUnavailable)
				}
				if err = tx.Put(bucketTaskOperations, mcpWriteKey(creation.Command.OperationID), &creation); err != nil {
					return err
				}
			}
		} else if !r.Binding.PreserveGUI {
			return mcpWriteErr(MCPWritePermissionDenied)
		}
		// Both receipts represent a stopped, durably prepared target. The caller's
		// acknowledged resume winner performs the subsequent run admission itself.
		task.Status = base.DownloadStatusPause
		task.UpdatedAt = time.Now()
		if err = tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		target := a.Expected
		id := task.ID
		r.State = MCPWriteSettled
		r.Result = &MCPWriteResult{TaskID: &id, Outcome: MCPWriteApplied, Expected: &target, Accepted: &accepted}
		r.UpdatedAt = task.UpdatedAt
		if !r.valid() {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		if err = tx.Put(bucketTaskOperations, mcpWriteKey(r.Command.OperationID), r); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return nil, false, mcpWriteStoreError(err)
	}
	return result, true, nil
}
func rejectMCPWrite(store AtomicStorage, dataset string, expected mcpWriteRecord, code MCPWriteErrorCode) (MCPWriteReceipt, error) {
	if store == nil || !expected.valid() || !code.Valid() {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteInvalidRequest)
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteInvalidRequest)
	}
	var receipt MCPWriteReceipt
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		r, err := mcpWriteRecordCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if r.State != MCPWritePending {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		r.State = MCPWriteSettled
		r.Result = &MCPWriteResult{Outcome: MCPWriteRejected, ErrorCode: &code}
		if r.Command.Action != nil {
			id := r.Command.Action.TaskID
			target := r.Command.Action.Expected
			r.Result.TaskID = &id
			r.Result.Expected = &target
		}
		r.UpdatedAt = time.Now()
		if !r.valid() {
			return mcpWriteErr(MCPWriteInvalidRequest)
		}
		if err = tx.Put(bucketTaskOperations, mcpWriteKey(r.Command.OperationID), r); err != nil {
			return err
		}
		receipt = r.receipt()
		return nil
	})
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteStoreError(err)
	}
	return receipt, nil
}

// Query is read-only, including confirmed absence. Historical P remains the
// key's original policy; a new current policy never rewrites old receipts.
func queryMCPWrite(reader datasetReader, q MCPWriteQuery) (MCPWriteReceipt, error) {
	empty := MCPWriteReceipt{Version: 1, DatasetID: q.DatasetID, PolicyID: q.PolicyID, OperationID: q.OperationID, State: MCPWriteUnknown, QueryRequired: true}
	if q.Version != 1 || !datasetIDPattern.MatchString(q.DatasetID) || !mcpPolicyPattern.MatchString(q.PolicyID) || !operationIDPattern.MatchString(q.OperationID) {
		return empty, mcpWriteErr(MCPWriteInvalidRequest)
	}
	r, err := readMCPWriteRecord(reader, q.DatasetID, q.OperationID)
	if err != nil {
		return empty, err
	}
	if r == nil {
		empty.State = MCPWriteNotRecorded
		empty.QueryRequired = false
		return empty, nil
	}
	if r.Command.PolicyID != q.PolicyID {
		return empty, mcpWriteErr(MCPWritePolicyMismatch)
	}
	return r.receipt(), nil
}
