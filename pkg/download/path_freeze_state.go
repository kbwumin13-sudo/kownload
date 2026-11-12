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
	Version        int                   `json:"version"`
	DatasetID      string                `json:"datasetId"`
	Request        HTTPPathCreateRequest `json:"request"`
	Snapshot       PathPolicySnapshot    `json:"snapshot"`
	PolicyHash     string                `json:"policyHash"`
	TreeDecision   *pathpolicy.Decision  `json:"treeDecision,omitempty"`
	TreeLayout     *pathpolicy.LayoutV1  `json:"treeLayout,omitempty"`
	TreeLayoutHash string                `json:"treeLayoutHash,omitempty"`
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

func pathItems(r HTTPPathCreateRequest) []*base.CreateTaskBatchItem {
	if r.Request != nil {
		return []*base.CreateTaskBatchItem{r.Request}
	}
	return r.Reqs
}
func pathRequestValid(r HTTPPathCreateRequest) bool {
	v, ok := pathVariantForCaps(r.RequiredCapabilities)
	if !ok || r.Version != 1 || !operationIDPattern.MatchString(r.OperationID) || r.PathIntent.Version != 1 {
		return false
	}
	switch r.PathIntent.Origin {
	case pathpolicy.Unknown, pathpolicy.Default, pathpolicy.UserSelected, pathpolicy.ManualCategory, pathpolicy.APIExplicit, pathpolicy.SourceSuggested:
	default:
		return false
	}
	if r.Request != nil {
		if r.Reqs != nil || r.BatchID != "" {
			return false
		}
	} else if v.Protocol != "netdisk" || len(r.Reqs) == 0 || len(r.Reqs) > 10000 || !operationIDPattern.MatchString(r.BatchID) {
		return false
	}
	for _, item := range pathItems(r) {
		if item == nil || item.Req == nil || item.Opts == nil || item.Opts.Path != r.PathIntent.Path {
			return false
		}
		req := item.Req
		if req.URL == "" || (req.Protocol != "" && req.Protocol != v.Protocol) {
			return false
		}
		if v.Protocol == "http" {
			u, e := url.Parse(req.URL)
			if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return false
			}
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
		return s.Decision == nil && s.Layout == nil && s.LayoutHash == "" && s.TreeLayoutHash == "" && s.WorkDirectory == "" && s.LayoutOutput == nil && s.Output == nil
	case PathFreezeFrozen:
		if s.Decision == nil || s.Layout == nil || s.Decision.Version != 1 || s.Decision.Root != s.Layout.ParentRoot {
			return false
		}
		hash, err := pathpolicy.LayoutFingerprint(*s.Layout)
		if err != nil || hash != s.LayoutHash || !validRecoveryHash(s.TreeLayoutHash) {
			return false
		}
		if s.WorkDirectory != "" && (s.RequiredCapability != "media-path-freeze/v1" || !mcpWriteName(s.WorkDirectory)) {
			return false
		}
		if s.LayoutOutput != nil && !pathLayoutOutputValid(s, *s.LayoutOutput) {
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
	if r == nil || r.Version != 2 || r.Namespace != batchOperationNamespace || r.Path == nil || r.Path.Version != 1 || !datasetIDPattern.MatchString(r.Path.DatasetID) || !pathRequestValid(r.Path.Request) || !pathSnapshotValid(r.Path.Snapshot) || r.Path.PolicyHash != mcpWriteHash(r.Path.Snapshot) || r.RequestHash != mcpWriteHash(r.Path.Request) || r.OperationID != r.Path.Request.OperationID || !reflect.DeepEqual(r.RequiredCapabilities, r.Path.Request.RequiredCapabilities) || !reflect.DeepEqual(r.Integrity, r.Path.Request.Integrity) || len(r.Members) != len(pathItems(r.Path.Request)) {
		return false
	}
	v, ok := pathVariantForCaps(r.RequiredCapabilities)
	if !ok || r.Kind != v.Kind {
		return false
	}
	allFrozen := true
	for _, m := range r.Members {
		if m.Protocol != v.Protocol || m.PathFreeze == nil || m.PathFreeze.RequiredCapability != v.Capability || !pathFrozenValid(*m.PathFreeze) || !pathDecisionMatches(*m.PathFreeze, *r.Path) {
			return false
		}
		frozen := m.PathFreeze.Phase == PathFreezeFrozen
		allFrozen = allFrozen && frozen
		if frozen && (r.Path.TreeDecision == nil || !reflect.DeepEqual(*r.Path.TreeDecision, *m.PathFreeze.Decision) || m.PathFreeze.TreeLayoutHash != r.Path.TreeLayoutHash) {
			return false
		}
		if r.Integrity != nil && m.PathFreeze.Output != nil {
			return false
		}
		if v.Protocol == "http" && r.Integrity == nil && frozen && m.PathFreeze.Output == nil {
			return false
		}
		if v.Protocol != "http" && m.PathFreeze.Output != nil {
			return false
		}
	}
	if r.Path.TreeDecision == nil {
		return r.Path.TreeLayout == nil && r.Path.TreeLayoutHash == "" && !allFrozen
	}
	if !allFrozen || r.Path.TreeLayout == nil {
		return false
	}
	layout, err := pathTreeLayout(r.Members)
	if err != nil {
		return false
	}
	hash, err := pathpolicy.LayoutFingerprint(layout)
	return err == nil && hash == r.Path.TreeLayoutHash && reflect.DeepEqual(layout, *r.Path.TreeLayout)
}
func pathTreeLayout(members []operationMember) (pathpolicy.LayoutV1, error) {
	root := ""
	var all []pathpolicy.Member
	for _, m := range members {
		if m.PathFreeze == nil || m.PathFreeze.Layout == nil {
			return pathpolicy.LayoutV1{}, ErrPathFreezeHeld
		}
		l := m.PathFreeze.Layout
		if root == "" {
			root = l.ParentRoot
		}
		if l.ParentRoot != root {
			return pathpolicy.LayoutV1{}, ErrPathFreezeInvalid
		}
		for _, file := range l.Members {
			file.Index = len(all)
			all = append(all, file)
		}
	}
	return pathpolicy.BuildLayoutV1(root, all)
}
func pathReqEqual(a, b *base.Request) bool {
	if a == nil || b == nil {
		return false
	}
	x, y := *a, *b
	if x.Protocol == "" || y.Protocol == "" {
		for _, v := range pathVariants {
			if x.Protocol == v.Protocol {
				x.Protocol = ""
			}
			if y.Protocol == v.Protocol {
				y.Protocol = ""
			}
		}
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
	if ref.Version != 2 || ref.Namespace != batchOperationNamespace || (ref.Index < 0) {
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
	if ref.Index >= len(r.Members) {
		return nil, empty, ErrPathFreezeHeld
	}
	m := r.Members[ref.Index]
	if !linked || reverse.TaskID != id || reverse.Ref != *ref || m.State != "created" || m.TaskID != id || m.Protocol != task.Protocol || !reflect.DeepEqual(m.PathFreeze, task.PathFreeze) || !pathReqEqual(pathMetaReq(task), pathItems(r.Path.Request)[ref.Index].Req) || task.Meta == nil || task.Meta.Opts == nil || !reflect.DeepEqual(task.Meta.Opts, m.Options) {
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
	return reservePathTreeOperation(store, dataset, request, snapshot, []*base.Options{effectiveOptions})
}
func reservePathTreeOperation(store AtomicStorage, dataset string, request HTTPPathCreateRequest, snapshot PathPolicySnapshot, effectiveOptions []*base.Options) (batchOperationRecord, bool, error) {
	if store == nil || !datasetIDPattern.MatchString(dataset) || !pathRequestValid(request) || !pathSnapshotValid(snapshot) || len(effectiveOptions) != len(pathItems(request)) {
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
	members := make([]operationMember, len(opts))
	for i, opt := range opts {
		if opt == nil || opt.Path != snapshot.InputRoot {
			return batchOperationRecord{}, false, ErrPathFreezeInvalid
		}
		members[i] = operationMember{Index: i, TaskID: NewTask().ID, State: "pending", Protocol: v.Protocol, Options: opt, PathFreeze: &TaskPathFreeze{Version: 1, RequiredCapability: v.Capability, Phase: PathFreezePending}}
		if request.Integrity != nil {
			members[i].CompletionID = NewTask().ID
		}
	}
	candidate := batchOperationRecord{Version: 2, Namespace: batchOperationNamespace, Kind: v.Kind, RequiredCapabilities: request.RequiredCapabilities, OperationID: request.OperationID, RequestHash: mcpWriteHash(request), CreatedAt: now, UpdatedAt: now, Integrity: request.Integrity, Path: &pathOperationState{Version: 1, DatasetID: dataset, Request: request, Snapshot: snapshot, PolicyHash: mcpWriteHash(snapshot)}, Members: members}
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
	return createPathOperationMember(store, dataset, expected, 0, owned, initial)
}
func createPathOperationMember(store AtomicStorage, dataset string, expected batchOperationRecord, index int, owned *Task, initial recoveryRecord) (*Task, bool, error) {
	if store == nil || owned == nil || !expected.valid() || expected.Path == nil || index < 0 || index >= len(expected.Members) {
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
		m := &r.Members[index]
		v, _ := pathVariantForCaps(r.RequiredCapabilities)
		if m.State != "pending" || task.ID != m.TaskID || task.Protocol != v.Protocol || !pathReqEqual(pathMetaReq(task), pathItems(r.Path.Request)[index].Req) || task.Meta == nil || task.Meta.Opts == nil || !reflect.DeepEqual(task.Meta.Opts, m.Options) || task.Meta.Res != nil || task.CreationOperation != nil || task.MCPWrite != nil || task.PathFreeze != nil || task.Completion != nil || task.Status == base.DownloadStatusDone {
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
		ref := CreationOperation{Version: 2, Namespace: batchOperationNamespace, OperationID: r.OperationID, Index: index}
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
		for _, bucket := range []string{bucketTaskRecovery, bucketRecoveryRequired} {
			var raw json.RawMessage
			present, err := reader.Get(bucket, task.ID, &raw)
			if err != nil {
				return ErrPathFreezeUnavailable
			}
			if present {
				return ErrPathFreezeHeld
			}
		}
		return nil
	}
	_, record, _, err := readSharedRecovery(reader, task.ID)
	if err != nil {
		return ErrPathFreezeHeld
	}
	if token.Episode != record.Episode || token.Generation != record.Generation || token.SourceRevision != record.SourceRevision || record.SourceHash != expected.SourceHash || record.ExecutionDefaultsHash != expected.ExecutionDefaultsHash {
		return ErrPathFreezeConflict
	}
	return nil
}

// ConfirmPathFreeze is F, not IO admission. It applies only owned metadata,
// never a Task snapshot, and keeps recovery/checkpoint/budgets byte-for-byte.
func ConfirmPathFreeze(store AtomicStorage, dataset string, expected PathFreezeExpected, prepared PathFreezePrepared) (*Task, bool, error) {
	tasks, fresh, err := ConfirmPathFreezeTree(store, dataset, []PathFreezeExpected{expected}, []PathFreezePrepared{prepared})
	if err != nil {
		return nil, false, err
	}
	return tasks[0], fresh, nil
}

// The tree reservation has exactly one logical decision. This F transaction
// commits every member's resolved paths and the joint tree layout together.
func ConfirmPathFreezeTree(store AtomicStorage, dataset string, expected []PathFreezeExpected, prepared []PathFreezePrepared) ([]*Task, bool, error) {
	if store == nil {
		return nil, false, ErrPathFreezeUnavailable
	}
	if len(expected) == 0 || len(expected) != len(prepared) {
		return nil, false, ErrPathFreezeInvalid
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	prepared, err = cloneOperationJSON(prepared)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	var result []*Task
	fresh := false
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		first, _, err := ReadPathFreeze(tx, expected[0].Token.TaskID)
		if err != nil {
			return err
		}
		r, err := loadOperationFrom(tx, first.CreationOperation.OperationID)
		if err != nil || r == nil || len(r.Members) != len(expected) {
			return ErrPathFreezeConflict
		}
		v, _ := pathVariantForCaps(r.RequiredCapabilities)
		tasks := make([]*Task, len(expected))
		already := r.Path.TreeDecision != nil
		for i, e := range expected {
			task, s, err := ReadPathFreeze(tx, e.Token.TaskID)
			if err != nil {
				return err
			}
			if task.CreationOperation.OperationID != r.OperationID || task.CreationOperation.Index != i || r.Members[i].TaskID != task.ID {
				return ErrPathFreezeConflict
			}
			if err = pathRunCAS(tx, task, v, e); err != nil {
				return err
			}
			if err = pathCheckpointCAS(tx, task.ID, e.CheckpointPresent, e.CheckpointHash); err != nil {
				return err
			}
			if already {
				if e.Record.Path == nil || r.RequestHash != e.Record.RequestHash || r.Path.PolicyHash != e.Record.Path.PolicyHash {
					return ErrPathFreezeConflict
				}
				tasks[i] = task
				continue
			}
			if _, err = pathRecordCAS(tx, dataset, e.Record); err != nil {
				return err
			}
			if !reflect.DeepEqual(task.Meta, e.Meta) {
				return ErrPathFreezeConflict
			}
			p := prepared[i]
			if p.ResolvedMeta == nil || !pathReqEqual(p.ResolvedMeta.Req, task.Meta.Req) || p.ResolvedMeta.Opts == nil || !reflect.DeepEqual(p.ResolvedMeta.Opts.Extra, task.Meta.Opts.Extra) || !reflect.DeepEqual(p.ResolvedMeta.Opts.SelectFiles, task.Meta.Opts.SelectFiles) {
				return ErrPathFreezeInvalid
			}
			layout, err := pathResourceLayout(p.ResolvedMeta)
			if err != nil {
				return err
			}
			hash, err := pathpolicy.LayoutFingerprint(layout)
			if err != nil || hash != p.LayoutHash || !reflect.DeepEqual(layout, p.Layout) {
				return ErrPathFreezeInvalid
			}
			if i > 0 && !reflect.DeepEqual(p.Decision, prepared[0].Decision) {
				return ErrPathFreezeInvalid
			}
			s.Phase = PathFreezeFrozen
			s.Decision = &p.Decision
			s.Layout = &p.Layout
			s.LayoutHash = p.LayoutHash
			s.WorkDirectory = p.WorkDirectory
			if v.Protocol == "http" && r.Integrity == nil {
				s.Output = &PathOutputState{Phase: MCPWriteOutputPlanned, StagingName: p.StagingName}
			}
			if !pathDecisionMatches(s, *r.Path) {
				return ErrPathFreezeInvalid
			}
			if r.Integrity != nil {
				_, completion, err := readTaskCompletion(tx, task.ID)
				if err != nil || completion.Phase != transfer.CompletionPrepared || completion.Size != -1 || completion.DirectoryIdentity.Inode != 0 || completion.StagingIdentity.Inode != 0 || completion.StagingName != "" || completion.FinalName != "" {
					return ErrPathFreezeHeld
				}
			}
			task.Meta = p.ResolvedMeta
			task.PathFreeze = &s
			task.UpdatedAt = time.Now()
			tasks[i] = task
			r.Members[i].Options = task.Meta.Opts
			r.Members[i].PathFreeze = &s
		}
		if already {
			result = tasks
			return nil
		}
		layout, err := pathTreeLayout(r.Members)
		if err != nil {
			return ErrPathFreezeInvalid
		}
		hash, err := pathpolicy.LayoutFingerprint(layout)
		if err != nil {
			return ErrPathFreezeInvalid
		}
		decision := prepared[0].Decision
		r.Path.TreeDecision = &decision
		r.Path.TreeLayout = &layout
		r.Path.TreeLayoutHash = hash
		for i, task := range tasks {
			task.PathFreeze.TreeLayoutHash = hash
			r.Members[i].PathFreeze = task.PathFreeze
		}
		if !r.valid() {
			return ErrPathFreezeInvalid
		}
		r.UpdatedAt = time.Now()
		for _, task := range tasks {
			if err = tx.Put(bucketTask, task.ID, task); err != nil {
				return err
			}
		}
		if err = tx.Put(bucketTaskOperations, operationKey(r.OperationID), r); err != nil {
			return err
		}
		result = tasks
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
		r.Members[task.CreationOperation.Index].PathFreeze = &candidate
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

func pathLayoutOutputValid(s TaskPathFreeze, out PathLayoutOutputState) bool {
	if out.Version != 1 || out.DirectoryIdentity.Inode == 0 || out.Files == nil || s.Layout == nil {
		return false
	}
	if out.WorkDirectoryIdentity != nil && (s.WorkDirectory == "" || out.WorkDirectoryIdentity.Inode == 0 || out.WorkDirectoryIdentity.Device != out.DirectoryIdentity.Device) {
		return false
	}
	last := -1
	for _, file := range out.Files {
		if file.Index <= last || file.Index < 0 || file.Index >= len(s.Layout.Members) || file.RelativePath != s.Layout.Members[file.Index].RelativePath || file.Identity.Inode == 0 || file.Identity.Device != out.DirectoryIdentity.Device {
			return false
		}
		last = file.Index
	}
	if s.RequiredCapability == "bt-path-freeze/v1" || s.RequiredCapability == "ed2k-path-freeze/v1" {
		return len(out.Files) == len(s.Layout.Members) && out.WorkDirectoryIdentity == nil
	}
	return len(out.Files) > 0 || out.WorkDirectoryIdentity != nil
}
func pathLayoutOutputExtends(before, next PathLayoutOutputState) bool {
	if before.Version != next.Version || before.DirectoryIdentity != next.DirectoryIdentity {
		return false
	}
	if before.WorkDirectoryIdentity != nil && (next.WorkDirectoryIdentity == nil || *before.WorkDirectoryIdentity != *next.WorkDirectoryIdentity) {
		return false
	}
	for _, old := range before.Files {
		found := false
		for _, file := range next.Files {
			if old.Index == file.Index {
				if old != file {
					return false
				}
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

// SavePathLayoutOutput ACKs actual complete native-file bindings (or an owned
// media workspace) before activation. It performs no open/IO/adoption and
// returns zero fresh on any rollback or commit whose acknowledgement is lost.
func SavePathLayoutOutput(store AtomicStorage, dataset string, expected PathFreezeExpected, next PathLayoutOutputState) (*Task, bool, error) {
	if store == nil {
		return nil, false, ErrPathFreezeUnavailable
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return nil, false, ErrPathFreezeInvalid
	}
	next, err = cloneOperationJSON(next)
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
		if s.Phase != PathFreezeFrozen || s.Output != nil || !pathLayoutOutputValid(s, next) {
			return ErrPathFreezeInvalid
		}
		r, err := pathRecordCAS(tx, dataset, expected.Record)
		if err != nil {
			return err
		}
		v, _ := pathVariantForCaps(r.RequiredCapabilities)
		if err = pathRunCAS(tx, task, v, expected); err != nil {
			return err
		}
		if err = pathCheckpointCAS(tx, task.ID, expected.CheckpointPresent, expected.CheckpointHash); err != nil {
			return err
		}
		if !reflect.DeepEqual(task.Meta, expected.Meta) {
			return ErrPathFreezeConflict
		}
		if s.LayoutOutput != nil {
			if reflect.DeepEqual(*s.LayoutOutput, next) {
				result = task
				return nil
			}
			if !pathLayoutOutputExtends(*s.LayoutOutput, next) {
				return ErrPathFreezeConflict
			}
		}
		s.LayoutOutput = &next
		task.PathFreeze = &s
		task.UpdatedAt = time.Now()
		r.Members[task.CreationOperation.Index].PathFreeze = &s
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
