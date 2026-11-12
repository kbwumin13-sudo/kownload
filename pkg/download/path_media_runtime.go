package download

import (
	"context"
	"sync"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func (d *Downloader) savePathLayoutState(dataset string, task *Task, next PathLayoutOutputState) (*Task, error) {
	task.lock.Lock()
	defer task.lock.Unlock()
	_, e, err := d.readPathExpected(dataset, task.ID)
	if err != nil {
		return nil, err
	}
	owned, _, err := SavePathLayoutOutput(d.storage.(AtomicStorage), dataset, e, next)
	if err != nil {
		return nil, err
	}
	task.dataLock.Lock()
	task.PathFreeze = owned.PathFreeze
	task.dataLock.Unlock()
	return owned, nil
}
func (d *Downloader) bindMediaPathOutput(ctx context.Context, task *Task, runtime *pathOutputRuntime, dataset string, s TaskPathFreeze) error {
	var previous *transfer.CompletionFileIdentity
	if s.LayoutOutput != nil {
		if s.LayoutOutput.Phase != MCPWriteOutputStaging {
			return ErrPathFreezeHeld
		}
		previous = s.LayoutOutput.WorkDirectoryIdentity
	}
	workspace, work, err := transfer.OpenVerifiedPathWorkspace(runtime.lease, *s.Layout, s.WorkDirectory, previous, runtime.check)
	if err != nil {
		return err
	}
	runtime.workspace = workspace
	next := PathLayoutOutputState{Version: 1, Phase: MCPWriteOutputStaging, DirectoryIdentity: runtime.root.Binding().Identity, WorkDirectoryIdentity: &work, Files: []PathFileIdentity{}}
	if s.LayoutOutput != nil {
		next = *s.LayoutOutput
	}
	if _, err = d.savePathLayoutState(dataset, task, next); err != nil {
		return err
	}
	var mu sync.Mutex
	published := make(map[int]bool)
	started := false
	publish := func(index int) error {
		mu.Lock()
		defer mu.Unlock()
		if err := runtime.check(); err != nil {
			return err
		}
		d.operationMu.Lock()
		defer d.operationMu.Unlock()
		if d.getTask(task.ID) != task {
			return transfer.ErrRecoveryStale
		}
		if published[index] {
			return ErrPathFreezeConflict
		}
		if !started {
			binding, err := workspace.Seal()
			if err != nil {
				return err
			}
			next = pathLayoutBinding(binding)
			next.WorkDirectoryIdentity = &work
			next.Phase = MCPWriteOutputPublishing
			if _, err = d.savePathLayoutState(dataset, task, next); err != nil {
				return err
			}
			started = true
		}
		if err := workspace.Publish(index); err != nil {
			next.Phase = MCPWriteOutputUncertain
			_, _ = d.savePathLayoutState(dataset, task, next)
			return err
		}
		published[index] = true
		if len(published) == len(s.Layout.Members) {
			next.Phase = MCPWriteOutputPublished
			_, err := d.savePathLayoutState(dataset, task, next)
			return err
		}
		return nil
	}
	execution, err := transfer.IssuePathFreezeExecution(task.ID, *s.Layout, workspace.WorkPath(), runtime.check, publish)
	if err != nil {
		return err
	}
	binder, ok := task.fetcher.(interface {
		BindPathFreezeExecution(*transfer.PathFreezeExecution) error
	})
	if !ok {
		return ErrPathFreezeHeld
	}
	return binder.BindPathFreezeExecution(execution)
}
