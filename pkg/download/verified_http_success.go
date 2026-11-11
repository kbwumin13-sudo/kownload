package download

import (
	"context"
	"sync/atomic"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// This is neither a persisted receipt nor a recovered run lease. Only a fresh
// C ACK creates it, after API done is visible, under the same lifecycle Fence.
// Its context outlives the network watch, but Delete/Close revoke it. An already
// admitted external action cannot be undone; no read or restart recreates one.
type verifiedSuccessPermit struct {
	delivered           atomic.Bool
	ctx                 context.Context
	cancel              context.CancelFunc
	task                *Task
	dataset, completion string
	token               transfer.RecoveryToken
}

func (d *Downloader) installVerifiedSuccess(task *Task, dataset, completion string, token transfer.RecoveryToken) {
	ctx, cancel := context.WithCancel(context.Background())
	permit := &verifiedSuccessPermit{ctx: ctx, cancel: cancel, task: task, dataset: dataset, completion: completion, token: token}
	if old := task.verifiedSuccess.Swap(permit); old != nil {
		old.cancel()
	}
}

func cancelVerifiedSuccess(task *Task) {
	if permit := task.verifiedSuccess.Swap(nil); permit != nil {
		permit.cancel()
	}
}

func (d *Downloader) verifiedSuccessAllowed(p *verifiedSuccessPermit) bool {
	if p == nil || p.ctx.Err() != nil || d.closed.Load() || p.task.verifiedSuccess.Load() != p {
		return false
	}
	// Membership is always checked main -> status; no live Task lock is held
	// while reading the owned same-domain DB view.
	d.lock.Lock()
	p.task.statusLock.Lock()
	present := d.getTaskLocked(p.task.ID) == p.task && p.task.runGeneration == p.token.Generation && p.task.getStatus() == base.DownloadStatusDone
	p.task.statusLock.Unlock()
	d.lock.Unlock()
	if !present {
		return false
	}
	err := withDatasetReadView(d.storage, p.dataset, func(reader datasetReader) error {
		owned, record, err := readTaskCompletion(reader, p.task.ID)
		if err != nil {
			return err
		}
		if record.Phase != transfer.CompletionCommitted || record.ID != p.completion {
			return transfer.ErrRecoveryHeld
		}
		if err = checkVerifiedAssociation(reader, owned, record); err != nil {
			return err
		}
		_, recovery, err := readRecovery(reader, p.task.ID)
		if err != nil {
			return err
		}
		if recovery.Episode != p.token.Episode || recovery.Generation != p.token.Generation || recovery.SourceRevision != p.token.SourceRevision {
			return transfer.ErrRecoveryStale
		}
		return nil
	})
	return err == nil && p.ctx.Err() == nil && !d.closed.Load() && p.task.verifiedSuccess.Load() == p
}

func (d *Downloader) publishVerifiedHTTPDone(task *Task) {
	p := task.verifiedSuccess.Load()
	if d.verifiedDone != nil && d.verifiedSuccessAllowed(p) {
		d.verifiedDone.publish(VerifiedHTTPDoneEvent{Version: 1, DatasetID: p.dataset, TaskID: task.ID, CompletionID: p.completion})
	}
}

func (d *Downloader) deliverVerifiedSuccess(task *Task) {
	p := task.verifiedSuccess.Load()
	if !d.verifiedSuccessAllowed(p) || !p.delivered.CompareAndSwap(false, true) {
		return
	}
	d.publishVerifiedHTTPDone(task)
	if !d.verifiedSuccessAllowed(p) {
		return
	}
	d.emit(EventKeyDone, task)
	if !d.verifiedSuccessAllowed(p) {
		return
	}
	d.emit(EventKeyFinally, task)
	if !d.verifiedSuccessAllowed(p) {
		return
	}
	d.triggerOnDone(task, p)
	if !d.verifiedSuccessAllowed(p) {
		return
	}
	d.triggerWebhooks(WebhookEventDownloadDone, task, nil, p)
	d.triggerScripts(ScriptEventDownloadDone, task, nil, p)
}
