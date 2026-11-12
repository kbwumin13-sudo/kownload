package download

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

var (
	ErrPathFreezeInvalid     = errors.New("invalid_path_freeze")
	ErrPathFreezeHeld        = errors.New("path_freeze_held")
	ErrPathFreezeConflict    = errors.New("path_freeze_conflict")
	ErrPathFreezeUnavailable = errors.New("path_freeze_unconfirmed")
)

type pathVariant struct {
	Kind, Capability, Protocol string
	Native                     bool
}

// Closed variants describe durable formats, not active producer capabilities.
// Core advertises support only after its real first-write guard is installed.
var pathVariants = []pathVariant{
	{HTTPPathCreateKind, HTTPPathCapability, "http", false},
	{"bt-path-create/v1", "bt-path-freeze/v1", "bt", true},
	{"ed2k-path-create/v1", "ed2k-path-freeze/v1", "ed2k", true},
	{"netdisk-path-create/v1", "netdisk-path-freeze/v1", "netdisk", false},
	{"media-path-create/v1", "media-path-freeze/v1", "media", false},
}

func pathVariantForCaps(caps []string) (pathVariant, bool) {
	for _, v := range pathVariants {
		if len(caps) == 1 && caps[0] == v.Capability {
			return v, true
		}
		if v.Protocol == "http" && len(caps) == 2 && caps[0] == v.Capability && caps[1] == verifiedHTTPCapability {
			return v, true
		}
	}
	return pathVariant{}, false
}

type pathOperationState struct {
	Version    int                   `json:"version"`
	DatasetID  string                `json:"datasetId"`
	Request    HTTPPathCreateRequest `json:"request"`
	Snapshot   PathPolicySnapshot    `json:"snapshot"`
	PolicyHash string                `json:"policyHash"`
}

func pathStoreError(err error) error {
	if err == nil {
		return nil
	}
	for _, fixed := range []error{ErrPathFreezeInvalid, ErrPathFreezeHeld, ErrPathFreezeConflict, ErrDatasetInvalid, ErrDatasetMissing, ErrDatasetMismatch} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	return ErrPathFreezeUnavailable
}
func (s *TaskPathFreeze) UnmarshalJSON(raw []byte) error {
	type value TaskPathFreeze
	return mcpWriteDecode(raw, (*value)(s))
}

func pathRequestValid(r HTTPPathCreateRequest) bool {
	v, ok := pathVariantForCaps(r.RequiredCapabilities)
	if !ok || r.Version != 1 || !operationIDPattern.MatchString(r.OperationID) || r.Request == nil || r.Request.Req == nil || r.Request.Opts == nil || r.PathIntent.Version != 1 || r.PathIntent.Path != r.Request.Opts.Path {
		return false
	}
	switch r.PathIntent.Origin {
	case pathpolicy.Unknown, pathpolicy.Default, pathpolicy.UserSelected, pathpolicy.ManualCategory, pathpolicy.APIExplicit, pathpolicy.SourceSuggested:
	default:
		return false
	}
	req := r.Request.Req
	if req.URL == "" || (req.Protocol != "" && req.Protocol != v.Protocol) {
		return false
	}
	if v.Protocol == "http" {
		u, e := url.Parse(req.URL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
	}
	if len(r.RequiredCapabilities) == 2 {
		return r.Integrity != nil && r.Integrity.valid()
	}
	return r.Integrity == nil
}
func pathSnapshotValid(s PathPolicySnapshot) bool {
	if s.Version != 1 || !filepath.IsAbs(s.EffectiveDefaultRoot) || !filepath.IsAbs(s.InputRoot) || strings.ContainsAny(s.EffectiveDefaultRoot+s.InputRoot, "\x00") {
		return false
	}
	_, err := pathpolicy.Evaluate(pathpolicy.Input{Path: s.InputRoot, Origin: pathpolicy.Default}, s.Config)
	return err == nil
}
func pathFrozenValid(s TaskPathFreeze) bool {
	if s.Version != 1 {
		return false
	}
	_, known := pathVariantForCaps([]string{s.RequiredCapability})
	if !known {
		return false
	}
	switch s.Phase {
	case PathFreezePending:
		return s.Decision == nil && s.Layout == nil && s.LayoutHash == "" && s.Output == nil
	case PathFreezeFrozen:
		if s.Decision == nil || s.Layout == nil || s.Decision.Version != 1 || s.Decision.Root != s.Layout.ParentRoot {
			return false
		}
		hash, err := pathpolicy.LayoutFingerprint(*s.Layout)
		if err != nil || hash != s.LayoutHash {
			return false
		}
		if s.Output != nil {
			if s.RequiredCapability != HTTPPathCapability || len(s.Layout.Members) != 1 || !mcpWriteName(s.Layout.Members[0].RelativePath) || !mcpWriteName(s.Output.StagingName) || strings.EqualFold(s.Output.StagingName, s.Layout.Members[0].RelativePath) {
				return false
			}
			switch s.Output.Phase {
			case MCPWriteOutputPlanned:
				return s.Output.Binding == nil
			case MCPWriteOutputStaging, MCPWriteOutputPublishing, MCPWriteOutputPublished, MCPWriteOutputUncertain:
				b := s.Output.Binding
				return b != nil && b.OwnerID != "" && b.DirectoryIdentity.Inode != 0 && b.StagingIdentity.Inode != 0 && b.DirectoryIdentity.Device == b.StagingIdentity.Device && b.StagingName == s.Output.StagingName && b.FinalName == s.Layout.Members[0].RelativePath
			default:
				return false
			}
		}
		return true
	}
	return false
}
func pathDecisionMatches(s TaskPathFreeze, p pathOperationState) bool {
	if s.Phase == PathFreezePending {
		return true
	}
	d := s.Decision
	if d.Origin == pathpolicy.RuleSelected {
		if !p.Snapshot.Config.Enabled || d.RuleVersion == nil || *d.RuleVersion != p.Snapshot.Config.Version || d.RuleID == "" {
			return false
		}
		for _, rule := range p.Snapshot.Config.Rules {
			if rule.ID == d.RuleID && rule.Root == d.Root {
				return true
			}
		}
		return false
	}
	return d.Origin == p.Request.PathIntent.Origin && d.Root == p.Snapshot.InputRoot && d.RuleID == "" && d.RuleVersion == nil
}
func pathRecordValid(r *batchOperationRecord) bool {
	if r == nil || r.Version != 2 || r.Namespace != batchOperationNamespace || r.Path == nil || r.Path.Version != 1 || !datasetIDPattern.MatchString(r.Path.DatasetID) || !pathRequestValid(r.Path.Request) || !pathSnapshotValid(r.Path.Snapshot) || r.Path.PolicyHash != mcpWriteHash(r.Path.Snapshot) || r.RequestHash != mcpWriteHash(r.Path.Request) || r.OperationID != r.Path.Request.OperationID || !reflect.DeepEqual(r.RequiredCapabilities, r.Path.Request.RequiredCapabilities) || !reflect.DeepEqual(r.Integrity, r.Path.Request.Integrity) || len(r.Members) != 1 {
		return false
	}
	v, ok := pathVariantForCaps(r.RequiredCapabilities)
	if !ok || r.Kind != v.Kind {
		return false
	}
	m := r.Members[0]
	if m.Protocol != v.Protocol || m.PathFreeze == nil || m.PathFreeze.RequiredCapability != v.Capability || !pathFrozenValid(*m.PathFreeze) || !pathDecisionMatches(*m.PathFreeze, *r.Path) {
		return false
	}
	if r.Integrity != nil {
		return m.PathFreeze.Output == nil
	}
	if v.Protocol == "http" && m.PathFreeze.Phase == PathFreezeFrozen {
		return m.PathFreeze.Output != nil
	}
	return m.PathFreeze.Output == nil
}
func pathReqEqual(a, b *base.Request) bool {
	if a == nil || b == nil {
		return false
	}
	x, y := *a, *b
	if x.Protocol == "http" {
		x.Protocol = ""
	}
	if y.Protocol == "http" {
		y.Protocol = ""
	}
	return reflect.DeepEqual(x, y)
}
func pathResourceLayout(meta *fetcher.FetcherMeta) (pathpolicy.LayoutV1, error) {
	if meta == nil || meta.Res == nil || meta.Opts == nil || len(meta.Res.Files) == 0 {
		return pathpolicy.LayoutV1{}, ErrPathFreezeHeld
	}
	members := make([]pathpolicy.Member, len(meta.Res.Files))
	for i, file := range meta.Res.Files {
		if file == nil || file.Name == "" {
			return pathpolicy.LayoutV1{}, ErrPathFreezeHeld
		}
		selected := len(meta.Opts.SelectFiles) == 0
		for _, index := range meta.Opts.SelectFiles {
			if index == i {
				selected = true
			}
		}
		name := file.Name
		folder := meta.Res.Name
		if meta.Opts.Name != "" {
			if folder != "" {
				folder = meta.Opts.Name
			} else if len(meta.Res.Files) == 1 {
				name = meta.Opts.Name
			}
		}
		// Reject traversal/empty components before path.Join could erase them.
		for _, part := range []string{folder, file.Path, name} {
			if part != "" {
				if _, err := pathpolicy.BuildLayoutV1(meta.Opts.Path, []pathpolicy.Member{{Index: 0, RelativePath: part}}); err != nil {
					return pathpolicy.LayoutV1{}, ErrPathFreezeHeld
				}
			}
		}
		members[i] = pathpolicy.Member{Index: i, RelativePath: path.Join(folder, file.Path, name), Selected: selected}
	}
	return pathpolicy.BuildLayoutV1(meta.Opts.Path, members)
}

// ReadPathFreeze is an owned same-view read. Frozen values bind R/Task/source
// and actual layout, never an obsolete execution generation. No implicit F.
func ReadPathFreeze(reader datasetReader, id string) (*Task, TaskPathFreeze, error) {
	var empty TaskPathFreeze
	if reader == nil || id == "" {
		return nil, empty, ErrPathFreezeHeld
	}
	task := new(Task)
	present, err := reader.Get(bucketTask, id, task)
	if err != nil {
		return nil, empty, ErrPathFreezeUnavailable
	}
	if !present || task.ID != id || task.PathFreeze == nil || task.MCPWrite != nil || task.CreationOperation == nil {
		return nil, empty, ErrPathFreezeHeld
	}
	ref := task.CreationOperation
	if ref.Version != 2 || ref.Namespace != batchOperationNamespace || ref.Index != 0 {
		return nil, empty, ErrPathFreezeHeld
	}
	r, err := loadOperationFrom(reader, ref.OperationID)
	if err != nil || r == nil || !r.valid() || r.Path == nil {
		return nil, empty, ErrPathFreezeHeld
	}
	if err = requireDataset(reader, r.Path.DatasetID); err != nil {
		return nil, empty, err
	}
	var reverse operationTaskLink
	linked, err := reader.Get(bucketOperationTasks, id, &reverse)
	if err != nil {
		return nil, empty, ErrPathFreezeUnavailable
	}
	m := r.Members[0]
	if !linked || reverse.TaskID != id || reverse.Ref != *ref || m.State != "created" || m.TaskID != id || m.Protocol != task.Protocol || !reflect.DeepEqual(m.PathFreeze, task.PathFreeze) || !pathReqEqual(pathMetaReq(task), r.Path.Request.Request.Req) || task.Meta == nil || task.Meta.Opts == nil || !reflect.DeepEqual(task.Meta.Opts, m.Options) {
		return nil, empty, ErrPathFreezeHeld
	}
	s := *task.PathFreeze
	if !pathFrozenValid(s) {
		return nil, empty, ErrPathFreezeHeld
	}
	if s.Phase == PathFreezeFrozen {
		layout, err := pathResourceLayout(task.Meta)
		if err != nil {
			return nil, empty, err
		}
		hash, err := pathpolicy.LayoutFingerprint(layout)
		if err != nil || hash != s.LayoutHash {
			return nil, empty, ErrPathFreezeHeld
		}
		if s.Output != nil {
			if s.Output.Binding != nil && s.Output.Binding.OwnerID != task.ID {
				return nil, empty, ErrPathFreezeHeld
			}
			if (task.Status == base.DownloadStatusDone) != (s.Output.Phase == MCPWriteOutputPublished) {
				return nil, empty, ErrPathFreezeHeld
			}
		}
	}
	if r.Integrity != nil {
		ct, completion, err := readTaskCompletion(reader, id)
		if err != nil || ct == nil || completion.ID != m.CompletionID || completion.ExpectedSHA256 != r.Integrity.ExpectedSHA256 {
			return nil, empty, ErrPathFreezeHeld
		}
	} else if task.Completion != nil {
		return nil, empty, ErrPathFreezeHeld
	}
	return task, s, nil
}

// pathMetaReq avoids dereferencing malformed task metadata in strict guards.
func pathMetaReq(t *Task) *base.Request {
	if t == nil || t.Meta == nil {
		return nil
	}
	return t.Meta.Req
}

func reservePathOperation(store AtomicStorage, dataset string, request HTTPPathCreateRequest, snapshot PathPolicySnapshot, effectiveOptions *base.Options) (batchOperationRecord, bool, error) {
	if store == nil || !datasetIDPattern.MatchString(dataset) || !pathRequestValid(request) || !pathSnapshotValid(snapshot) || effectiveOptions == nil || effectiveOptions.Path != snapshot.InputRoot {
		return batchOperationRecord{}, false, ErrPathFreezeInvalid
	}
	request, err := cloneOperationJSON(request)
	if err != nil {
		return batchOperationRecord{}, false, ErrPathFreezeInvalid
	}
	snapshot, err = cloneOperationJSON(snapshot)
	if err != nil {
		return batchOperationRecord{}, false, ErrPathFreezeInvalid
	}
	opts, err := cloneOperationJSON(effectiveOptions)
	if err != nil {
		return batchOperationRecord{}, false, ErrPathFreezeInvalid
	}
	v, _ := pathVariantForCaps(request.RequiredCapabilities)
	now := time.Now()
	pending := &TaskPathFreeze{Version: 1, RequiredCapability: v.Capability, Phase: PathFreezePending}
	m := operationMember{Index: 0, TaskID: NewTask().ID, State: "pending", Protocol: v.Protocol, Options: opts, PathFreeze: pending}
	if request.Integrity != nil {
		m.CompletionID = NewTask().ID
	}
	candidate := batchOperationRecord{Version: 2, Namespace: batchOperationNamespace, Kind: v.Kind, RequiredCapabilities: request.RequiredCapabilities, OperationID: request.OperationID, RequestHash: mcpWriteHash(request), CreatedAt: now, UpdatedAt: now, Integrity: request.Integrity, Path: &pathOperationState{Version: 1, DatasetID: dataset, Request: request, Snapshot: snapshot, PolicyHash: mcpWriteHash(snapshot)}, Members: []operationMember{m}}
	if !candidate.valid() {
		return batchOperationRecord{}, false, ErrPathFreezeInvalid
	}
	var result batchOperationRecord
	fresh := false
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		old, err := loadOperationFrom(tx, request.OperationID)
		if err != nil {
			return err
		}
		if old != nil {
			if old.Version != 2 || old.Kind != v.Kind || old.RequestHash != candidate.RequestHash || old.Path == nil || old.Path.DatasetID != dataset {
				return ErrPathFreezeConflict
			}
			result = *old
			return nil
		}
		if err = tx.Put(bucketTaskOperations, operationKey(request.OperationID), &candidate); err != nil {
			return err
		}
		result = candidate
		fresh = true
		return nil
	})
	if err != nil {
		return batchOperationRecord{}, false, pathStoreError(err)
	}
	return result, fresh, nil
}
func pathRecordCAS(tx StorageTransaction, dataset string, expected batchOperationRecord) (*batchOperationRecord, error) {
	if err := requireDataset(tx, dataset); err != nil {
		return nil, err
	}
	r, err := loadOperationFrom(tx, expected.OperationID)
	if err != nil {
		return nil, ErrPathFreezeHeld
	}
	if r == nil || !pathRecordValid(r) || r.Path.DatasetID != dataset || !reflect.DeepEqual(*r, expected) {
		return nil, ErrPathFreezeConflict
	}
	return r, nil
}
func createPathOperationTask(store AtomicStorage, dataset string, expected batchOperationRecord, owned *Task, initial recoveryRecord) (*Task, bool, error) {
	if store == nil || owned == nil || !expected.valid() || expected.Path == nil {
		return nil, false, ErrPathFreezeInvalid
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	task, err := cloneOperationJSON(owned)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	initial, err = cloneOperationJSON(initial)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	var result *Task
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		r, err := pathRecordCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		m := &r.Members[0]
		v, _ := pathVariantForCaps(r.RequiredCapabilities)
		if m.State != "pending" || task.ID != m.TaskID || task.Protocol != v.Protocol || !pathReqEqual(pathMetaReq(task), r.Path.Request.Request.Req) || task.Meta == nil || task.Meta.Opts == nil || !reflect.DeepEqual(task.Meta.Opts, m.Options) || task.Meta.Res != nil || task.CreationOperation != nil || task.MCPWrite != nil || task.PathFreeze != nil || task.Completion != nil || task.Status == base.DownloadStatusDone {
			return ErrPathFreezeInvalid
		}
		for _, bucket := range []string{bucketTask, bucketOperationTasks, bucketTaskRecovery, bucketRecoveryRequired, bucketSave, bucketTaskCompletion, bucketCompletionRequired} {
			var raw json.RawMessage
			present, e := tx.Get(bucket, task.ID, &raw)
			if e != nil {
				return e
			}
			if present {
				return ErrPathFreezeConflict
			}
		}
		ref := CreationOperation{Version: 2, Namespace: batchOperationNamespace, OperationID: r.OperationID, Index: 0}
		task.CreationOperation = &ref
		task.PathFreeze = m.PathFreeze
		task.Status = base.DownloadStatusPause
		task.Progress = &Progress{}
		if v.Native {
			if task.Recovery != nil || !reflect.DeepEqual(initial, recoveryRecord{}) {
				return ErrPathFreezeInvalid
			}
		} else {
			required, e := recoveryRequirementFor(initial)
			if e != nil || initial.Version != 4 || initial.TaskID != task.ID || initial.Generation != 0 || initial.SourceRevision != 1 || initial.InitialAttempt.State != "ungranted" || initial.TotalUsed != 0 || initial.NextAction != 0 || initial.Actions == nil || len(initial.Actions) != 0 || initial.Coverage != nil || initial.LastExplicitRetry != nil || validateRecoveryTriplet(task, initial, required) != nil {
				return ErrPathFreezeInvalid
			}
			if e = tx.Put(bucketTaskRecovery, task.ID, &initial); e != nil {
				return e
			}
			if e = tx.Put(bucketRecoveryRequired, task.ID, &required); e != nil {
				return e
			}
		}
		if r.Integrity != nil {
			if err = prepareTaskCompletion(tx, dataset, task, transfer.VerifiedCompletionRecord{Version: 1, ID: m.CompletionID, Phase: transfer.CompletionPrepared, Size: -1, ExpectedSHA256: r.Integrity.ExpectedSHA256}); err != nil {
				return err
			}
		} else if err = tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		if err = tx.Put(bucketOperationTasks, task.ID, &operationTaskLink{task.ID, ref}); err != nil {
			return err
		}
		m.State = "created"
		r.UpdatedAt = time.Now()
		if err = tx.Put(bucketTaskOperations, operationKey(r.OperationID), r); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return nil, false, pathStoreError(err)
	}
	return result, true, nil
}
func pathCheckpointCAS(reader datasetReader, id string, present bool, hash string) error {
	if present && !validRecoveryHash(hash) || !present && hash != "" {
		return ErrPathFreezeInvalid
	}
	var raw json.RawMessage
	exists, err := reader.Get(bucketSave, id, &raw)
	if err != nil {
		return ErrPathFreezeUnavailable
	}
	if exists != present {
		return ErrPathFreezeConflict
	}
	if exists {
		actual, err := checkpointDigest(raw)
		if err != nil || actual != hash {
			return ErrPathFreezeConflict
		}
	}
	return nil
}

// pathRunCAS uses persistent recovery only for the variants that have it.
// BT/ED2K retain native recovery: zero E/G/S is not an execution token; their
// real run/cancel/membership Fence belongs to Core, outside this transaction.
func pathRunCAS(reader datasetReader, task *Task, v pathVariant, expected PathFreezeExpected) error {
	token := expected.Token
	if token.TaskID != task.ID {
		return ErrPathFreezeConflict
	}
	if v.Native {
		if token.Episode != "" || token.Generation != 0 || token.SourceRevision != 0 || task.Recovery != nil || expected.ExecutionDefaultsHash != "" || expected.SourceHash != mcpWriteHash(task.Meta.Req) {
			return ErrPathFreezeConflict
		}
		return nil
	}
	_, record, _, err := readSharedRecovery(reader, task.ID)
	if err != nil {
		return ErrPathFreezeHeld
	}
	if token.Episode != record.Episode || token.Generation != record.Generation || token.SourceRevision != record.SourceRevision || record.SourceHash != expected.SourceHash || record.ExecutionDefaultsHash != expected.ExecutionDefaultsHash || token.Generation == 0 {
		return ErrPathFreezeConflict
	}
	return nil
}

// ConfirmPathFreeze is F, not IO admission. It applies only owned metadata,
// never a Task snapshot, and keeps recovery/checkpoint/budgets byte-for-byte.
func ConfirmPathFreeze(store AtomicStorage, dataset string, expected PathFreezeExpected, prepared PathFreezePrepared) (*Task, bool, error) {
	if store == nil {
		return nil, false, ErrPathFreezeUnavailable
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	prepared, err = cloneOperationJSON(prepared)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	var result *Task
	fresh := false
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		task, s, err := ReadPathFreeze(tx, expected.Token.TaskID)
		if err != nil {
			return err
		}
		r, err := loadOperationFrom(tx, task.CreationOperation.OperationID)
		if err != nil || r == nil {
			return ErrPathFreezeHeld
		}
		v, _ := pathVariantForCaps(r.RequiredCapabilities)
		if err = pathRunCAS(tx, task, v, expected); err != nil {
			return err
		}
		if err = pathCheckpointCAS(tx, task.ID, expected.CheckpointPresent, expected.CheckpointHash); err != nil {
			return err
		}
		if s.Phase == PathFreezeFrozen {
			if expected.Record.Path == nil || len(expected.Record.Members) != 1 || r.RequestHash != expected.Record.RequestHash || r.Path.PolicyHash != expected.Record.Path.PolicyHash || r.Members[0].TaskID != expected.Record.Members[0].TaskID {
				return ErrPathFreezeConflict
			}
			result = task
			return nil
		}
		if _, err = pathRecordCAS(tx, dataset, expected.Record); err != nil {
			return err
		}
		if !reflect.DeepEqual(task.Meta, expected.Meta) {
			return ErrPathFreezeConflict
		}
		if prepared.ResolvedMeta == nil || !pathReqEqual(prepared.ResolvedMeta.Req, task.Meta.Req) || prepared.ResolvedMeta.Opts == nil || !reflect.DeepEqual(prepared.ResolvedMeta.Opts.Extra, task.Meta.Opts.Extra) || !reflect.DeepEqual(prepared.ResolvedMeta.Opts.SelectFiles, task.Meta.Opts.SelectFiles) {
			return ErrPathFreezeInvalid
		}
		layout, err := pathResourceLayout(prepared.ResolvedMeta)
		if err != nil {
			return err
		}
		hash, err := pathpolicy.LayoutFingerprint(layout)
		if err != nil || hash != prepared.LayoutHash || !reflect.DeepEqual(layout, prepared.Layout) {
			return ErrPathFreezeInvalid
		}
		s.Phase = PathFreezeFrozen
		s.Decision = &prepared.Decision
		s.Layout = &prepared.Layout
		s.LayoutHash = prepared.LayoutHash
		if v.Protocol == "http" && r.Integrity == nil {
			s.Output = &PathOutputState{Phase: MCPWriteOutputPlanned, StagingName: prepared.StagingName}
		}
		if !pathFrozenValid(s) || !pathDecisionMatches(s, *r.Path) {
			return ErrPathFreezeInvalid
		}
		if r.Integrity != nil {
			_, completion, err := readTaskCompletion(tx, task.ID)
			if err != nil || completion.Phase != transfer.CompletionPrepared || completion.Size != -1 || completion.DirectoryIdentity.Inode != 0 || completion.StagingIdentity.Inode != 0 || completion.StagingName != "" || completion.FinalName != "" {
				return ErrPathFreezeHeld
			}
		}
		task.Meta = prepared.ResolvedMeta
		task.PathFreeze = &s
		task.UpdatedAt = time.Now()
		r.Members[0].Options = task.Meta.Opts
		r.Members[0].PathFreeze = &s
		r.UpdatedAt = task.UpdatedAt
		if !r.valid() {
			return ErrPathFreezeInvalid
		}
		if err = tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		if err = tx.Put(bucketTaskOperations, operationKey(r.OperationID), r); err != nil {
			return err
		}
		result = task
		fresh = true
		return nil
	})
	if err != nil {
		return nil, false, pathStoreError(err)
	}
	return result, fresh, nil
}

// SavePathOutput provides the no-SHA output binding/publication CAS. Caller
// must hold the genuine run Fence, join writers and confirm FS no-replace.
func SavePathOutput(store AtomicStorage, dataset string, token transfer.RecoveryToken, expected TaskPathFreeze, next PathOutputState, finalProgress *Progress) (*Task, bool, error) {
	if store == nil || !pathFrozenValid(expected) || expected.Output == nil {
		return nil, false, ErrPathFreezeInvalid
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	next, err = cloneOperationJSON(next)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	progress, err := cloneOperationJSON(finalProgress)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	candidate := expected
	candidate.Output = &next
	if !pathFrozenValid(candidate) {
		return nil, false, ErrPathFreezeInvalid
	}
	before := expected.Output
	allowed := (before.Phase == MCPWriteOutputPlanned && next.Phase == MCPWriteOutputStaging) || (before.Phase == MCPWriteOutputStaging && next.Phase == MCPWriteOutputPublishing) || (before.Phase == MCPWriteOutputPublishing && (next.Phase == MCPWriteOutputPublished || next.Phase == MCPWriteOutputUncertain))
	if !allowed || before.StagingName != next.StagingName || (before.Binding != nil && !reflect.DeepEqual(before.Binding, next.Binding)) {
		return nil, false, ErrPathFreezeInvalid
	}
	if next.Phase == MCPWriteOutputPublished {
		if progress == nil || !completionFinalProgress(progress, progress.Downloaded) {
			return nil, false, ErrPathFreezeInvalid
		}
	} else if progress != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	var result *Task
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		task, s, err := ReadPathFreeze(tx, token.TaskID)
		if err != nil {
			return err
		}
		if task.Protocol != "http" || task.Completion != nil || !reflect.DeepEqual(s, expected) || next.Binding == nil || next.Binding.OwnerID != task.ID {
			return ErrPathFreezeConflict
		}
		if _, err = mcpWriteRun(tx, token); err != nil {
			return ErrPathFreezeConflict
		}
		r, err := loadOperationFrom(tx, task.CreationOperation.OperationID)
		if err != nil || r == nil {
			return ErrPathFreezeHeld
		}
		task.PathFreeze = &candidate
		task.UpdatedAt = time.Now()
		if next.Phase == MCPWriteOutputPublished {
			if !completionHTTPProgress(task.Progress) {
				return ErrPathFreezeHeld
			}
			used := progress.Used
			if task.Progress != nil && task.Progress.Used > used {
				used = task.Progress.Used
			}
			task.Status = base.DownloadStatusDone
			task.Progress = &Progress{Used: used, Downloaded: progress.Downloaded}
		}
		r.Members[0].PathFreeze = &candidate
		r.UpdatedAt = task.UpdatedAt
		if !r.valid() {
			return ErrPathFreezeInvalid
		}
		if err = tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		if err = tx.Put(bucketTaskOperations, operationKey(r.OperationID), r); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return nil, false, pathStoreError(err)
	}
	return result, true, nil
}
