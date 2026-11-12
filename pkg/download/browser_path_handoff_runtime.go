package download

import (
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

// The caller holds the original handoff mutex and has checked the authenticated
// browser identity. This reader never attaches a Task or recreates new-M RAM.
func (d *Downloader) readBrowserPathHandoff(r BrowserHandoffRequest) (*Task, *batchOperationRecord, *BrowserPathHandoffProjection, error) {
	identity, err := d.DatasetIdentity()
	if err != nil {
		return nil, nil, nil, err
	}
	if r.DatasetID != "" && r.DatasetID != identity.DatasetID {
		return nil, nil, nil, ErrDatasetMismatch
	}
	projection := &BrowserPathHandoffProjection{Version: 1, DatasetID: identity.DatasetID, Kind: HTTPPathCreateKind, RequiredCapabilities: []string{}}
	var task *Task
	var record *batchOperationRecord
	err = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
		var err error
		record, err = loadOperationFrom(reader, r.OperationID)
		if err != nil {
			return err
		}
		if record == nil {
			return nil
		}
		if record.Version != 2 || record.Kind != HTTPPathCreateKind || record.Path == nil || record.Path.Handoff == nil || len(record.Members) != 1 {
			return ErrHandoffConflict
		}
		binding := record.Path.Handoff
		if binding.ClientID != r.ClientID || binding.Browser != r.Browser || r.TaskID != "" && r.TaskID != record.Members[0].TaskID {
			return ErrHandoffConflict
		}
		projection.RequiredCapabilities = append([]string{}, record.RequiredCapabilities...)
		if record.Members[0].State != "created" {
			return nil
		}
		task, _, err = ReadPathFreeze(reader, record.Members[0].TaskID)
		return err
	})
	return task, record, projection, err
}

func browserPathResult(task *Task, operation string, projection *BrowserPathHandoffProjection) BrowserHandoffResult {
	result := handoffResult(task, operation)
	result.PathCreate = projection
	return result
}

func (d *Downloader) browserPathHandoff(r BrowserHandoffRequest, live *Task) (BrowserHandoffResult, error) {
	unknown := handoffResult(nil, r.OperationID)
	ownedRequest, cloneErr := cloneOperationJSON(r)
	if cloneErr != nil {
		return unknown, ErrHandoffInvalid
	}
	r = ownedRequest
	if live != nil && live.PathFreeze == nil {
		// An existing legacy handoff is queried in its original format. It can
		// never be upgraded or claimed by a new typed prepare/commit.
		if r.Action == "query" {
			return handoffResult(live, r.OperationID), nil
		}
		return unknown, ErrHandoffConflict
	}
	stored, record, projection, err := d.readBrowserPathHandoff(r)
	if err != nil {
		return unknown, err
	}
	result := browserPathResult(stored, r.OperationID, projection)
	if r.Action == "query" {
		return result, nil
	}
	if r.DatasetID == "" {
		return result, ErrHandoffInvalid
	}
	store, atomic := d.storage.(AtomicStorage)
	if !atomic {
		return result, ErrPathFreezeUnavailable
	}
	if r.Action == "prepare" {
		request := r.PathCreate
		if request == nil || request.OperationID != r.OperationID || request.Request == nil || mcpWriteHash(r.Req) != mcpWriteHash(request.Request.Req) || mcpWriteHash(r.Opts) != mcpWriteHash(request.Request.Opts) {
			return result, ErrHandoffInvalid
		}
		handoff := BrowserHandoff{Version: 1, OperationID: r.OperationID, ClientID: r.ClientID, Browser: r.Browser, Download: r.Download, Phase: "prepared", RequestHash: browserPathRequestHash(*request)}
		if !browserPathIntentValid(*request, handoff) {
			return result, ErrHandoffInvalid
		}
		if record != nil {
			if record.RequestHash != pathRequestHash(*request) || !browserPathIdentityEqual(*record.Path.Handoff, handoff) {
				return result, ErrHandoffConflict
			}
			return result, nil
		}
		d.operationMu.Lock()
		defer d.operationMu.Unlock()
		capability, err := d.PathCapabilityForDataset(r.DatasetID, HTTPPathCreateKind, request.RequiredCapabilities)
		if err != nil {
			return result, err
		}
		if !capability.Supported {
			return result, ErrPathFreezeHeld
		}
		snapshot, options, err := d.preparePathReception(*request)
		if err != nil {
			return result, err
		}
		if len(options) != 1 {
			return result, ErrHandoffInvalid
		}
		candidate := NewTask()
		candidate.Protocol = "http"
		candidate.Meta = &fetcher.FetcherMeta{Req: request.Request.Req, Opts: options[0]}
		candidate.Progress = &Progress{}
		candidate.Status = base.DownloadStatusPause
		candidate.Recovery = &TaskRecoveryRef{Version: 4}
		candidate.Handoff = &handoff
		defaults, err := d.capturePathDefaults(candidate)
		if err != nil {
			return result, err
		}
		recordValue, freshR, err := reserveBrowserPathOperation(store, r.DatasetID, *request, snapshot, options[0], handoff)
		if err != nil {
			return result, err
		}
		if !freshR {
			return result, nil
		}
		candidate.ID = recordValue.Members[0].TaskID
		candidate.Meta.Opts = recordValue.Members[0].Options
		episode, err := gonanoid.New()
		if err != nil {
			return result, err
		}
		source, err := recoveryTaskSourceHash(candidate, defaults)
		if err != nil {
			return result, err
		}
		initial := recoveryRecord{Version: 4, TaskID: candidate.ID, Episode: episode, SourceRevision: 1, SourceHash: source, ExecutionDefaultsHash: defaults, InitialAttempt: recoveryInitialAttempt{State: "ungranted"}, Actions: []recoveryAction{}}
		owned, freshM, err := createPathOperationTask(store, r.DatasetID, recordValue, candidate, initial)
		if err != nil || !freshM {
			return result, err
		}
		if err = d.attachPreparedBrowserPath(owned, true); err != nil {
			return result, err
		}
		projection.RequiredCapabilities = append([]string{}, recordValue.RequiredCapabilities...)
		return browserPathResult(owned, r.OperationID, projection), nil
	}
	if stored == nil {
		return result, nil
	}
	if r.Action != "commit" && r.Action != "abort" {
		return result, ErrHandoffInvalid
	}
	phase, reason := "committed", ""
	if r.Action == "commit" {
		if r.TaskID == "" || !r.CancelConfirmed || r.Download != stored.Handoff.Download || r.BrowserState.State != "interrupted" || r.BrowserState.Error != "USER_CANCELED" {
			return result, ErrHandoffInvalid
		}
		if stored.Handoff.Phase == "aborted" {
			return result, ErrHandoffConflict
		}
	} else {
		phase, reason = "aborted", r.Reason
		if reason != "browser_completed" && reason != "user_cancelled" && reason != "browser_retained" {
			return result, ErrHandoffInvalid
		}
		if stored.Handoff.Phase == "committed" {
			return result, ErrHandoffConflict
		}
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	owned, fresh, err := transitionBrowserPathHandoff(store, r.DatasetID, *stored.Handoff, phase, reason)
	if err != nil {
		if live != nil {
			live.initialHTTPExecution.Store(false)
			live.recoveryInitialEligible.Store(false)
			invalidateTaskRecovery(live)
		}
		return result, err
	}
	if !fresh {
		return browserPathResult(owned, r.OperationID, projection), nil
	}
	if live == nil {
		if err = d.attachPreparedBrowserPath(owned, false); err != nil {
			return result, err
		}
		live = d.getTask(owned.ID)
	}
	live.dataLock.Lock()
	live.Handoff = owned.Handoff
	live.dataLock.Unlock()
	if phase == "committed" {
		d.lock.Lock()
		if d.getTaskLocked(live.ID) == live {
			live.updateStatus(base.DownloadStatusWait)
			d.enqueueLocked(live)
		}
		d.lock.Unlock()
		d.notifyRunning()
	}
	return browserPathResult(live, r.OperationID, projection), nil
}

// A prepared browser M is deliberately not Q. Only its actual ACK winner can
// retain the private initial qualification while waiting for browser commit.
func (d *Downloader) attachPreparedBrowserPath(task *Task, fresh bool) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.getTaskLocked(task.ID) != nil {
		return nil
	}
	if err := d.assignFetcherManager(task); err != nil {
		return err
	}
	initTask(task)
	task.initialHTTPExecution.Store(fresh)
	task.Status = base.DownloadStatusPause
	d.tasks = append(d.tasks, task)
	return nil
}
