package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/util"
)

const (
	bucketTaskOperations    = "task_operations_v1"
	bucketOperationTasks    = "operation_tasks_v1"
	batchOperationNamespace = "task-batch.create/v1"
)

var (
	ErrOperationInvalid     = errors.New("invalid_operation_request")
	ErrOperationConflict    = errors.New("operation_conflict")
	ErrOperationUnavailable = errors.New("operation_unavailable_query_same_id")
	ErrOperationUnsupported = errors.New("atomic_operation_storage_unsupported")
	operationIDPattern      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
)

// CreationOperation is an immutable link, not a new task lifecycle or fetcher.
type CreationOperation struct {
	Version     int    `json:"version"`
	Namespace   string `json:"namespace"`
	OperationID string `json:"operationId"`
	Index       int    `json:"index"`
}
type operationTaskLink struct {
	TaskID string            `json:"taskId"`
	Ref    CreationOperation `json:"ref"`
}
type BatchOperationRequest struct {
	Version     int                          `json:"version"`
	OperationID string                       `json:"operationId"`
	Reqs        []*base.CreateTaskBatchItem  `json:"reqs"`
	Opts        *base.Options                `json:"opts,omitempty"`
	integrity   *VerifiedHTTPIntegrityIntent // dedicated verified receiver only, never legacy wire Extra
}
type operationMember struct {
	Index        int           `json:"index"`
	TaskID       string        `json:"taskId"`
	State        string        `json:"state"`
	Error        string        `json:"error,omitempty"`
	Protocol     string        `json:"protocol,omitempty"`
	Options      *base.Options `json:"options,omitempty"`
	CompletionID string        `json:"completionId,omitempty"`
}
type batchOperationRecord struct {
	Version     int                          `json:"version"`
	Namespace   string                       `json:"namespace"`
	OperationID string                       `json:"operationId"`
	RequestHash string                       `json:"requestHash"`
	CreatedAt   time.Time                    `json:"createdAt"`
	UpdatedAt   time.Time                    `json:"updatedAt"`
	Members     []operationMember            `json:"members"`
	Integrity   *VerifiedHTTPIntegrityIntent `json:"integrity,omitempty"`
}

func (r *batchOperationRecord) UnmarshalJSON(b []byte) error {
	type record batchOperationRecord
	return decodeStoredJSON(b, (*record)(r))
}

type BatchOperationMemberResult struct {
	Index       int    `json:"index"`
	TaskID      string `json:"taskId"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	TaskPresent *bool  `json:"taskPresent,omitempty"`
}
type BatchOperationResult struct {
	Version      int                          `json:"version"`
	OperationID  string                       `json:"operationId"`
	State        string                       `json:"state"`
	Members      []BatchOperationMemberResult `json:"members"`
	TaskIDs      []string                     `json:"taskIds"`
	CreatedCount int                          `json:"createdCount"`
	FailedCount  int                          `json:"failedCount"`
	PendingCount int                          `json:"pendingCount"`
}

func cloneOperationJSON[T any](v T) (T, error) {
	var copied T
	raw, err := json.Marshal(v)
	if err != nil {
		return copied, err
	}
	err = decodeStoredJSON(raw, &copied)
	return copied, err
}

// initOperationOptions freezes Extra without passing opaque numbers through
// the legacy Options.Clone decoder. Defaults and path validation stay shared.
func (d *Downloader) initOperationOptions(input *base.Options) (*base.Options, error) {
	copied, err := cloneOperationJSON(input)
	if err != nil {
		return nil, err
	}
	var extra any
	if copied != nil {
		extra, copied.Extra = copied.Extra, nil
	}
	opts, err := d.initOptions(copied)
	if err != nil {
		return nil, err
	}
	opts.Extra = extra
	return opts, nil
}

// Caller owns the task's metadata lock when it may change concurrently.
func (t *Task) cloneExecutionMeta() (*fetcher.FetcherMeta, error) {
	if t.CreationOperation != nil || t.Recovery != nil {
		return cloneOperationJSON(t.Meta)
	}
	return util.DeepClone(t.Meta), nil
}

func canonicalBatch(r BatchOperationRequest) ([]*base.CreateTaskBatchItem, string, error) {
	if r.integrity != nil && (!r.integrity.valid() || len(r.Reqs) != 1) {
		return nil, "", ErrVerifiedHTTPInvalid
	}
	if r.Version != 1 || !operationIDPattern.MatchString(r.OperationID) || len(r.Reqs) == 0 || len(r.Reqs) > 10000 {
		return nil, "", ErrOperationInvalid
	}
	items := make([]*base.CreateTaskBatchItem, len(r.Reqs))
	for i, item := range r.Reqs {
		if item == nil {
			continue
		}
		copied, err := cloneOperationJSON(item)
		if err != nil {
			return nil, "", ErrOperationInvalid
		}
		if copied.Opts == nil {
			copied.Opts, err = cloneOperationJSON(r.Opts)
			if err != nil {
				return nil, "", ErrOperationInvalid
			}
		}
		if copied.Opts == nil {
			copied.Opts = &base.Options{}
		}
		if copied.Opts.SelectFiles == nil {
			copied.Opts.SelectFiles = []int{}
		}
		if copied.Req != nil && copied.Req.Labels == nil {
			copied.Req.Labels = map[string]string{}
		}
		items[i] = copied
	}
	raw, err := json.Marshal(struct {
		Version   int                          `json:"version"`
		Namespace string                       `json:"namespace"`
		Items     []*base.CreateTaskBatchItem  `json:"items"`
		Integrity *VerifiedHTTPIntegrityIntent `json:"integrity,omitempty"`
	}{1, batchOperationNamespace, items, r.integrity})
	if err != nil {
		return nil, "", ErrOperationInvalid
	}
	sum := sha256.Sum256(raw)
	return items, hex.EncodeToString(sum[:]), nil
}
func (r *batchOperationRecord) valid() bool {
	if r.Version != 1 || r.Namespace != batchOperationNamespace || !operationIDPattern.MatchString(r.OperationID) || len(r.Members) == 0 || len(r.Members) > 10000 {
		return false
	}
	if r.Integrity != nil && (!r.Integrity.valid() || len(r.Members) != 1) {
		return false
	}
	hash, err := hex.DecodeString(r.RequestHash)
	if err != nil || len(hash) != sha256.Size {
		return false
	}
	ids := map[string]bool{}
	for i, m := range r.Members {
		if m.Index != i || m.TaskID == "" || ids[m.TaskID] {
			return false
		}
		ids[m.TaskID] = true
		if r.Integrity != nil {
			if m.CompletionID == "" || m.Protocol != "http" {
				return false
			}
		} else if m.CompletionID != "" {
			return false
		}
		if m.State != "pending" && m.State != "created" && m.State != "failed" {
			return false
		}
		if m.State == "failed" {
			switch m.Error {
			case "invalid_request", "unsupported_protocol", "invalid_options", "cancelled":
			default:
				return false
			}
		} else if m.Error != "" {
			return false
		}
		if m.State != "failed" && (m.Options == nil || m.Protocol == "") {
			return false
		}
	}
	return true
}
func operationKey(id string) string { return batchOperationNamespace + ":" + id }
func operationRef(r *batchOperationRecord, i int) CreationOperation {
	return CreationOperation{1, batchOperationNamespace, r.OperationID, i}
}

func (d *Downloader) loadOperation(id string) (*batchOperationRecord, error) {
	return loadOperationFrom(d.storage, id)
}
func loadOperationFrom(reader datasetReader, id string) (*batchOperationRecord, error) {
	var r batchOperationRecord
	ok, err := reader.Get(bucketTaskOperations, operationKey(id), &r)
	if err != nil {
		return nil, ErrOperationUnavailable
	}
	if !ok {
		return nil, nil
	}
	if !r.valid() || r.OperationID != id {
		return nil, ErrOperationConflict
	}
	return &r, nil
}
func (d *Downloader) checkCreationAssociation(t *Task) error {
	return checkCreationAssociationFrom(d.storage, t)
}
func checkCreationAssociationFrom(reader datasetReader, t *Task) error {
	var link operationTaskLink
	exists, err := reader.Get(bucketOperationTasks, t.ID, &link)
	if err != nil {
		return ErrOperationUnavailable
	}
	if !exists && t.CreationOperation == nil && t.MCPWrite == nil {
		return nil
	}
	if !exists || t.CreationOperation == nil || link.TaskID != t.ID || link.Ref != *t.CreationOperation {
		return ErrOperationConflict
	}
	ref := link.Ref
	if ref.Version == 2 && ref.Namespace == MCPWriteNamespace {
		_, _, err := readMCPWriteTask(reader, t.ID)
		return err
	}
	if ref.Version != 1 || ref.Namespace != batchOperationNamespace {
		return ErrOperationConflict
	}
	r, err := loadOperationFrom(reader, ref.OperationID)
	if err != nil {
		return err
	}
	if r == nil || ref.Index < 0 || ref.Index >= len(r.Members) {
		return ErrOperationConflict
	}
	m := r.Members[ref.Index]
	if m.State != "created" || m.TaskID != t.ID {
		return ErrOperationConflict
	}
	return nil
}
func (t *Task) creationHeld() bool {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	if t.creationBlocked {
		return true
	}
	if t.MCPWrite != nil {
		return t.CreationOperation == nil || t.CreationOperation.Version != 2 || t.CreationOperation.Namespace != MCPWriteNamespace || !mcpWriteStateValid(*t.MCPWrite)
	}
	return t.CreationOperation != nil && (t.CreationOperation.Version != 1 || t.CreationOperation.Namespace != batchOperationNamespace)
}
func creationEligible(tasks []*Task) []*Task {
	out := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		if !t.creationHeld() {
			out = append(out, t)
		}
	}
	return out
}
func (d *Downloader) operationResult(id string, r *batchOperationRecord) (BatchOperationResult, error) {
	return operationResultFrom(d.storage, id, r, nil)
}
func operationResultFrom(reader datasetReader, id string, r *batchOperationRecord, tasks map[string]*Task) (BatchOperationResult, error) {
	result := BatchOperationResult{Version: 1, OperationID: id, State: "unknown", Members: []BatchOperationMemberResult{}, TaskIDs: []string{}}
	if r == nil {
		return result, nil
	}
	result.State = "settled"
	for _, m := range r.Members {
		item := BatchOperationMemberResult{Index: m.Index, TaskID: m.TaskID, State: m.State, Error: m.Error}
		switch m.State {
		case "created":
			result.CreatedCount++
			result.TaskIDs = append(result.TaskIDs, m.TaskID)
		case "failed":
			result.FailedCount++
		case "pending":
			result.PendingCount++
			result.State = "pending"
		}
		result.Members = append(result.Members, item)
	}
	// The complete creation receipt is already authoritative. A failed optional
	// task-presence lookup must not truncate members or turn pending into settled.
	for i := range result.Members {
		item := &result.Members[i]
		if item.State != "created" {
			continue
		}
		var task Task
		present, err := reader.Get(bucketTask, item.TaskID, &task)
		if err != nil {
			return result, ErrOperationUnavailable
		}
		item.TaskPresent = &present
		if present {
			var associationErr error
			if tasks != nil {
				// This manifest was already validated in the same view. Do not
				// reload/decode it once for every created member.
				associationErr = checkScopedMemberAssociation(reader, &task, r, i)
			} else {
				associationErr = checkCreationAssociationFrom(reader, &task)
			}
			if err := associationErr; err != nil {
				if errors.Is(err, ErrOperationUnavailable) {
					return result, err
				}
				item.Error = "association_conflict"
			} else if tasks != nil {
				tasks[task.ID] = &task
			}
		}
	}
	return result, nil
}

// QueryBatchOperation reads only durable metadata. It never attaches or restores tasks.
func (d *Downloader) QueryBatchOperation(id string) (BatchOperationResult, error) {
	if !operationIDPattern.MatchString(id) {
		return BatchOperationResult{}, ErrOperationInvalid
	}
	r, err := d.loadOperation(id)
	if err != nil {
		return BatchOperationResult{}, err
	}
	return d.operationResult(id, r)
}
func (d *Downloader) reserveOperation(atomicStore AtomicStorage, r BatchOperationRequest, items []*base.CreateTaskBatchItem, hash string) error {
	return d.reserveOperationInDataset(atomicStore, r, items, hash, "")
}
func (d *Downloader) reserveOperationInDataset(atomicStore AtomicStorage, r BatchOperationRequest, items []*base.CreateTaskBatchItem, hash, dataset string) error {
	current, err := d.loadOperationInDataset(dataset, r.OperationID)
	if err != nil {
		return err
	}
	if current != nil {
		if current.RequestHash != hash {
			return ErrOperationConflict
		}
		return nil
	}
	now := time.Now()
	record := &batchOperationRecord{Version: 1, Namespace: batchOperationNamespace, OperationID: r.OperationID, RequestHash: hash, CreatedAt: now, UpdatedAt: now, Members: make([]operationMember, len(items))}
	if r.integrity != nil {
		copied := *r.integrity
		record.Integrity = &copied
	}
	for i, item := range items {
		m := operationMember{Index: i, TaskID: NewTask().ID, State: "pending"}
		if item == nil || item.Req == nil {
			m.State = "failed"
			m.Error = "invalid_request"
		} else {
			fm, e := d.parseRequestFm(item.Req)
			if e != nil {
				m.State = "failed"
				m.Error = "unsupported_protocol"
			} else {
				m.Protocol = fm.Name()
				m.Options, e = d.initOperationOptions(item.Opts)
				if e != nil {
					m.State = "failed"
					m.Error = "invalid_options"
				}
			}
		}
		if record.Integrity != nil {
			if m.State != "pending" || m.Protocol != "http" {
				return ErrVerifiedHTTPInvalid
			}
			if err := d.checkVerifiedHTTPContext(item.Req); err != nil {
				return err
			}
			m.CompletionID = NewTask().ID
			m.Options, err = d.freezeVerifiedOptions(m.Options)
			if err != nil {
				return err
			}
		}
		record.Members[i] = m
	}
	err = atomicStore.UpdateAtomic(func(tx StorageTransaction) error {
		if err := guardOperationDataset(tx, dataset); err != nil {
			return err
		}
		var stored batchOperationRecord
		ok, e := tx.Get(bucketTaskOperations, operationKey(r.OperationID), &stored)
		if e != nil {
			return e
		}
		if ok {
			if !stored.valid() || stored.OperationID != r.OperationID || stored.RequestHash != hash {
				return ErrOperationConflict
			}
			return nil
		}
		return tx.Put(bucketTaskOperations, operationKey(r.OperationID), record)
	})
	if errors.Is(err, ErrOperationConflict) || isOperationDatasetError(err) {
		return err
	}
	if err != nil {
		return ErrOperationUnavailable
	}
	return nil
}
func (d *Downloader) createOperationMember(store AtomicStorage, op, hash string, i int, input *base.CreateTaskBatchItem) (bool, error) {
	return d.createOperationMemberInDataset(store, op, hash, i, input, "")
}
func (d *Downloader) createOperationMemberInDataset(store AtomicStorage, op, hash string, i int, input *base.CreateTaskBatchItem, dataset string) (bool, error) {
	created := false
	err := store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := guardOperationDataset(tx, dataset); err != nil {
			return err
		}
		var r batchOperationRecord
		ok, e := tx.Get(bucketTaskOperations, operationKey(op), &r)
		if e != nil {
			return e
		}
		if !ok || !r.valid() || r.OperationID != op || r.RequestHash != hash || i < 0 || i >= len(r.Members) {
			return ErrOperationConflict
		}
		m := &r.Members[i]
		if m.State != "pending" {
			return nil
		}
		var existing Task
		occupied, e := tx.Get(bucketTask, m.TaskID, &existing)
		if e != nil {
			return e
		}
		if occupied {
			return ErrOperationConflict
		}
		var oldLink operationTaskLink
		occupied, e = tx.Get(bucketOperationTasks, m.TaskID, &oldLink)
		if e != nil {
			return e
		}
		if occupied {
			return ErrOperationConflict
		}
		if input == nil || input.Req == nil {
			return ErrOperationConflict
		}
		req, e := cloneOperationJSON(input.Req)
		if e != nil {
			return e
		}
		req.Protocol = m.Protocol
		task := NewTask()
		task.ID = m.TaskID
		task.Protocol = m.Protocol
		task.Meta = &fetcher.FetcherMeta{Req: req, Opts: m.Options}
		task.Status = base.DownloadStatusPause
		task.Progress = &Progress{}
		ref := operationRef(&r, i)
		task.CreationOperation = &ref
		// M: a callback error rolls back task, reverse link and member receipt together.
		if r.Integrity != nil {
			initial := transfer.VerifiedCompletionRecord{Version: 1, ID: m.CompletionID, Phase: transfer.CompletionPrepared, Size: -1, ExpectedSHA256: r.Integrity.ExpectedSHA256}
			if e = prepareTaskCompletion(tx, dataset, task, initial); e != nil {
				return e
			}
		} else if e = tx.Put(bucketTask, task.ID, task); e != nil {
			return e
		}
		if e = tx.Put(bucketOperationTasks, task.ID, &operationTaskLink{task.ID, ref}); e != nil {
			return e
		}
		m.State = "created"
		r.UpdatedAt = time.Now()
		created = true
		return tx.Put(bucketTaskOperations, operationKey(op), &r)
	})
	if errors.Is(err, ErrOperationConflict) || isOperationDatasetError(err) {
		return false, err
	}
	if err != nil {
		// A committed-but-unacknowledged M is not first-execution authority.
		return false, ErrOperationUnavailable
	}
	return created, nil
}

// attachOperationTask is Q: operationMu coordinates attach with deletion and
// repeated POSTs. Only a newly attached task is queued, never an existing one.
func (d *Downloader) attachOperationTask(id string, createdNow bool) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	var task Task
	exists, err := d.storage.Get(bucketTask, id, &task)
	if err != nil {
		return ErrOperationUnavailable
	}
	if !exists {
		return nil
	}
	if err = d.checkCreationAssociation(&task); err != nil {
		return err
	}
	return d.attachOperationSnapshotLocked(&task, createdNow)
}

// Caller holds d.lock; task is an owned, association-checked storage snapshot.
func (d *Downloader) attachOperationSnapshotLocked(task *Task, createdNow bool) error {
	if d.getTaskLocked(task.ID) != nil {
		return nil
	}
	if err := d.assignFetcherManager(task); err != nil {
		return ErrOperationConflict
	}
	initTask(task)
	if task.Completion != nil && !createdNow {
		// Unknown M ACK/replay may expose an already stored Task, never its
		// lost first-IO authority. An explicit later user Continue is separate.
		task.updateStatus(base.DownloadStatusPause)
		d.tasks = append(d.tasks, task)
		return nil
	}
	// Only this process's successful M authorizes the first unresolved HTTP
	// request. Q readback/replay and startup do not recover that authority.
	task.initialHTTPExecution.Store(createdNow && task.Protocol == "http" && task.Meta.Res == nil)
	task.updateStatus(base.DownloadStatusWait)
	d.tasks = append(d.tasks, task)
	d.enqueueLocked(task)
	d.notifyRunning()
	return nil
}
func (d *Downloader) reconcileOperationTasks(r *batchOperationRecord) error {
	if r == nil {
		return nil
	}
	for _, m := range r.Members {
		if m.State == "created" {
			if err := d.attachOperationTask(m.TaskID, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// CreateBatchOperation resumes pending creation only for the exact same intent.
func (d *Downloader) CreateBatchOperation(ctx context.Context, r BatchOperationRequest) (BatchOperationResult, error) {
	return d.createBatchOperationInDataset(ctx, r, "")
}
func (d *Downloader) createBatchOperationInDataset(ctx context.Context, r BatchOperationRequest, dataset string) (BatchOperationResult, error) {
	items, hash, err := canonicalBatch(r)
	if err != nil {
		return BatchOperationResult{}, err
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return BatchOperationResult{Version: 1, OperationID: r.OperationID}, ErrOperationUnsupported
	}
	d.operationMu.Lock()
	if d.closed.Load() {
		d.operationMu.Unlock()
		return BatchOperationResult{Version: 1, OperationID: r.OperationID}, ErrOperationUnavailable
	}
	err = d.reserveOperationInDataset(store, r, items, hash, dataset)
	d.operationMu.Unlock()
	if err != nil {
		return d.readAfterOperationErrorInDataset(dataset, r.OperationID, err)
	}
	for i, item := range items {
		if ctx.Err() != nil {
			break
		}
		d.operationMu.Lock()
		if d.closed.Load() {
			d.operationMu.Unlock()
			return d.readAfterOperationErrorInDataset(dataset, r.OperationID, ErrOperationUnavailable)
		}
		var createdNow bool
		createdNow, err = d.createOperationMemberInDataset(store, r.OperationID, hash, i, item, dataset)
		// A failed commit can have an uncertain acknowledgement. Read authority before
		// deciding whether to publish; never manufacture a new ID or empty success.
		readErr := d.publishOperationMemberInDataset(dataset, r.OperationID, hash, i, createdNow)
		d.operationMu.Unlock()
		if err != nil {
			return d.readAfterOperationErrorInDataset(dataset, r.OperationID, err)
		}
		if readErr != nil {
			return d.readAfterOperationErrorInDataset(dataset, r.OperationID, readErr)
		}
	}
	// Also reconcile M->Q gaps on an explicit replay, including a cancelled request.
	d.operationMu.Lock()
	stored, err := d.reconcileOperationInDataset(dataset, r.OperationID)
	d.operationMu.Unlock()
	if err != nil {
		return d.readAfterOperationErrorInDataset(dataset, r.OperationID, err)
	}
	if dataset != "" {
		return d.QueryBatchOperationForDataset(dataset, r.OperationID)
	}
	return d.operationResult(r.OperationID, stored)
}
func (d *Downloader) readAfterOperationError(id string, cause error) (BatchOperationResult, error) {
	result, err := d.QueryBatchOperation(id)
	if err != nil {
		return BatchOperationResult{Version: 1, OperationID: id}, cause
	}
	return result, cause
}
func (d *Downloader) CancelBatchOperation(id string) (BatchOperationResult, error) {
	return d.cancelBatchOperationInDataset(id, "")
}
func (d *Downloader) cancelBatchOperationInDataset(id, dataset string, requireIntegrity ...bool) (BatchOperationResult, error) {
	if !operationIDPattern.MatchString(id) {
		return BatchOperationResult{}, ErrOperationInvalid
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return BatchOperationResult{Version: 1, OperationID: id}, ErrOperationUnsupported
	}
	d.operationMu.Lock()
	if d.closed.Load() {
		d.operationMu.Unlock()
		return BatchOperationResult{Version: 1, OperationID: id}, ErrOperationUnavailable
	}
	err := store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := guardOperationDataset(tx, dataset); err != nil {
			return err
		}
		var r batchOperationRecord
		exists, e := tx.Get(bucketTaskOperations, operationKey(id), &r)
		if e != nil {
			return e
		}
		if !exists {
			return nil
		}
		if !r.valid() || r.OperationID != id {
			return ErrOperationConflict
		}
		if len(requireIntegrity) > 0 && requireIntegrity[0] && (r.Integrity == nil || !r.Integrity.valid()) {
			return ErrOperationConflict
		}
		for i := range r.Members {
			if r.Members[i].State == "pending" {
				r.Members[i].State = "failed"
				r.Members[i].Error = "cancelled"
			}
		}
		r.UpdatedAt = time.Now()
		return tx.Put(bucketTaskOperations, operationKey(id), &r)
	})
	d.operationMu.Unlock()
	if errors.Is(err, ErrOperationConflict) || isOperationDatasetError(err) {
		return d.readAfterOperationErrorInDataset(dataset, id, err)
	}
	if err != nil {
		return d.readAfterOperationErrorInDataset(dataset, id, ErrOperationUnavailable)
	}
	if dataset != "" {
		return d.QueryBatchOperationForDataset(dataset, id)
	}
	return d.QueryBatchOperation(id)
}

// UnmarshalJSON preserves opaque Extra numbers when a durable operation or
// recovery declaration requires exact owned snapshots. Undeclared legacy tasks
// retain their original decoder behavior; precision never grants execution.
func (t *Task) UnmarshalJSON(raw []byte) error {
	type taskJSON Task
	var marker struct {
		CreationOperation json.RawMessage `json:"creationOperation"`
		Recovery          json.RawMessage `json:"recovery"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return err
	}
	if (len(marker.CreationOperation) > 0 && !bytes.Equal(marker.CreationOperation, []byte("null"))) ||
		(len(marker.Recovery) > 0 && !bytes.Equal(marker.Recovery, []byte("null"))) {
		return decodeStoredJSON(raw, (*taskJSON)(t))
	}
	return json.Unmarshal(raw, (*taskJSON)(t))
}
