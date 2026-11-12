package download

import (
	"context"
	"errors"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// Wait/body/process joins have completed before this path. Publication never
// sends events itself; the caller retains the original one-shot Done pipeline.
func (d *Downloader) completeTaskPath(task *Task, f fetcher.Fetcher, generation uint64, run *transfer.RecoveryLease) (bool, error) {
	value, ok := d.pathOutputs.Load(task.ID)
	if !ok {
		return false, ErrPathFreezeHeld
	}
	runtime := value.(*pathOutputRuntime)
	if err := runtime.check(); err != nil {
		return false, err
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return false, err
	}
	if task.Protocol == "http" {
		httpFetcher, ok := f.(*httpProtocol.Fetcher)
		if !ok || run == nil {
			return false, ErrPathFreezeHeld
		}
		if err = httpFetcher.JoinVerifiedWriters(run.Context); err != nil {
			return false, err
		}
		if err = httpFetcher.MCPOutputSyncResult(); err != nil {
			return false, err
		}
		d.operationMu.Lock()
		defer d.operationMu.Unlock()
		if d.getTask(task.ID) != task || run.Check() != nil {
			return false, transfer.ErrRecoveryStale
		}
		task.lock.Lock()
		defer task.lock.Unlock()
		if task.runGeneration != generation || task.getStatus() != base.DownloadStatusRunning {
			return false, transfer.ErrRecoveryStale
		}
		if err = d.saveTask(task); err != nil {
			return false, err
		}
		owned, e, err := d.readPathExpected(identity.DatasetID, task.ID)
		if err != nil {
			return false, err
		}
		s := *owned.PathFreeze
		if s.Output == nil || s.Output.Phase != MCPWriteOutputStaging {
			return false, ErrPathFreezeHeld
		}
		next := *s.Output
		next.Phase = MCPWriteOutputPublishing
		owned, _, err = SavePathOutput(d.storage.(AtomicStorage), identity.DatasetID, e.Token, s, next, nil)
		if err != nil {
			return false, err
		}
		task.dataLock.Lock()
		task.PathFreeze = owned.PathFreeze
		task.dataLock.Unlock()
		s = *owned.PathFreeze
		readonly, err := transfer.OpenVerifiedMCPOutput(runtime.lease, *s.Output.Binding)
		if err != nil {
			return false, err
		}
		defer readonly.Close()
		info, err := readonly.Stat()
		if err != nil {
			return false, err
		}
		if owned.Meta.Res == nil || (owned.Meta.Res.Size > 0 && info.Size() != owned.Meta.Res.Size) || f.Progress().TotalDownloaded() != info.Size() {
			return false, ErrPathFreezeHeld
		}
		if err = runtime.check(); err != nil {
			return false, err
		}
		if err = run.Check(); err != nil {
			return false, err
		}
		outcome, err := transfer.PublishVerifiedMCPOutput(runtime.lease, readonly, transfer.MCPOutputSeal{Binding: *s.Output.Binding, SealedSize: info.Size()})
		next = *s.Output
		if err != nil || outcome != transfer.MCPOutputConfirmed {
			next.Phase = MCPWriteOutputUncertain
			uncertain, _, _ := SavePathOutput(d.storage.(AtomicStorage), identity.DatasetID, e.Token, s, next, nil)
			if uncertain != nil {
				task.dataLock.Lock()
				task.PathFreeze = uncertain.PathFreeze
				task.dataLock.Unlock()
			}
			return false, errors.Join(err, ErrPathFreezeHeld)
		}
		if err = runtime.check(); err != nil {
			return false, err
		}
		if err = run.Check(); err != nil {
			return false, err
		}
		next.Phase = MCPWriteOutputPublished
		owned, fresh, err := SavePathOutput(d.storage.(AtomicStorage), identity.DatasetID, e.Token, s, next, &Progress{Used: task.timer.Used(), Downloaded: info.Size()})
		if err != nil || !fresh {
			return false, errors.Join(err, ErrPathFreezeUnavailable)
		}
		task.dataLock.Lock()
		task.PathFreeze, task.Status, task.Progress, task.UpdatedAt = owned.PathFreeze, owned.Status, owned.Progress, owned.UpdatedAt
		task.clearReadPublishedLocked()
		task.dataLock.Unlock()
		return true, nil
	}
	if task.Protocol == "bt" || task.Protocol == "ed2k" {
		if err = d.quiesceTaskPath(task); err != nil {
			return false, err
		}
	} else if runtime.storage != nil {
		if err = runtime.storage.Sync(); err != nil {
			return false, err
		}
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err = runtime.check(); err != nil {
		return false, err
	}
	if d.getTask(task.ID) != task {
		return false, transfer.ErrRecoveryStale
	}
	if task.Protocol != "media" {
		task.dataLock.RLock()
		s, err := cloneOperationJSON(task.PathFreeze)
		task.dataLock.RUnlock()
		if err != nil || s == nil || s.LayoutOutput == nil {
			return false, ErrPathFreezeHeld
		}
		next := *s.LayoutOutput
		next.Phase = MCPWriteOutputPublishing
		if _, err = d.savePathLayoutState(identity.DatasetID, task, next); err != nil {
			return false, err
		}
		next.Phase = MCPWriteOutputPublished
		if _, err = d.savePathLayoutState(identity.DatasetID, task, next); err != nil {
			return false, err
		}
	}
	task.dataLock.RLock()
	published := task.PathFreeze.LayoutOutput != nil && task.PathFreeze.LayoutOutput.Phase == MCPWriteOutputPublished
	task.dataLock.RUnlock()
	if !published {
		return false, ErrPathFreezeHeld
	}
	return d.commitTaskCompletion(task, f, generation)
}

func (d *Downloader) quiesceTaskPath(task *Task) error {
	if task.PathFreeze == nil || task.fetcher == nil {
		return nil
	}
	if quiesce, ok := task.fetcher.(interface{ QuiescePathFreeze(context.Context) error }); ok {
		return quiesce.QuiescePathFreeze(context.Background())
	}
	return nil
}

// A completed BT Task retains its original upload policy, using a new
// read-only binding and lifetime rather than reviving the cancelled writer.
func (d *Downloader) rebindPathUpload(task *Task) error {
	if task.Protocol != "bt" || task.PathFreeze == nil {
		return nil
	}
	if err := d.releasePathOutput(task); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	task.statusLock.Lock()
	task.runCancel = cancel
	task.recoveryRunCancel.Store(&cancel)
	task.statusLock.Unlock()
	identity, err := d.DatasetIdentity()
	if err != nil {
		cancel()
		return err
	}
	runtime, err := d.pathOutputRoot(ctx, task)
	if err != nil {
		cancel()
		return err
	}
	adapter, ok := task.fetcher.(interface {
		PathFreezeLayout() (pathpolicy.LayoutV1, []int64, error)
		BindPathFreeze(context.Context, pathpolicy.LayoutV1, *transfer.VerifiedLayoutStorage) error
	})
	if !ok {
		runtime.close()
		cancel()
		return ErrPathFreezeHeld
	}
	layout, sizes, err := adapter.PathFreezeLayout()
	if err != nil {
		runtime.close()
		cancel()
		return err
	}
	owned, _, err := d.readPathExpected(identity.DatasetID, task.ID)
	if err != nil || owned.PathFreeze.LayoutOutput == nil || owned.PathFreeze.LayoutOutput.Phase != MCPWriteOutputPublished {
		runtime.close()
		cancel()
		return ErrPathFreezeHeld
	}
	binding := pathNativeBinding(*owned.PathFreeze.LayoutOutput)
	runtime.storage, _, err = transfer.OpenVerifiedLayoutReader(runtime.lease, layout, sizes, binding, runtime.check)
	if err == nil {
		err = runtime.storage.Activate(binding)
	}
	if err == nil {
		err = adapter.BindPathFreeze(ctx, layout, runtime.storage)
	}
	if err != nil {
		runtime.close()
		cancel()
		return err
	}
	d.pathOutputs.Store(task.ID, runtime)
	if uploader, ok := task.fetcher.(fetcher.Uploader); ok {
		return uploader.Upload()
	}
	return nil
}
