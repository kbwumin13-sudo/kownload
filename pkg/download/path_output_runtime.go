package download

import (
	"context"
	"errors"
	"os"
	"sync/atomic"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type pathOutputRuntime struct {
	root      *transfer.VerifiedPolicyRoot
	lease     *transfer.VerifiedRootLease
	storage   *transfer.VerifiedLayoutStorage
	workspace *transfer.VerifiedPathWorkspace
	ctx       context.Context
	active    atomic.Bool
}

func (r *pathOutputRuntime) check() error {
	if !r.active.Load() || r.ctx.Err() != nil {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (r *pathOutputRuntime) close() error {
	r.active.Store(false)
	var err error
	if r.storage != nil {
		err = errors.Join(err, r.storage.Close())
	}
	if r.workspace != nil {
		err = errors.Join(err, r.workspace.Close())
	}
	if r.lease != nil {
		err = errors.Join(err, r.lease.Close())
	}
	if r.root != nil {
		err = errors.Join(err, r.root.Close())
	}
	return err
}
func (d *Downloader) releasePathOutput(task *Task) error {
	if f, ok := task.fetcher.(*httpProtocol.Fetcher); ok {
		f.ReleaseVerifiedOutput()
	}
	value, ok := d.pathOutputs.LoadAndDelete(task.ID)
	if !ok {
		return nil
	}
	return value.(*pathOutputRuntime).close()
}
func (d *Downloader) pathOutputRoot(ctx context.Context, task *Task) (*pathOutputRuntime, error) {
	directory, err := transfer.CreateCompletionDirectory(task.PathFreeze.Layout.ParentRoot)
	if err != nil {
		return nil, err
	}
	if err = directory.Close(); err != nil {
		return nil, err
	}
	root, err := transfer.OpenVerifiedPolicyRoot(task.ID, task.PathFreeze.Layout.ParentRoot)
	if err != nil {
		return nil, err
	}
	lease, err := root.Borrow()
	if err != nil {
		root.Close()
		return nil, err
	}
	runtime := &pathOutputRuntime{root: root, lease: lease, ctx: ctx}
	runtime.active.Store(true)
	return runtime, nil
}

// The caller releases Task.lock for root validation, exclusive open and native
// hashing/join. operationMu retains the namespace/lifecycle fence through ACK.
func (d *Downloader) bindTaskPathOutput(ctx context.Context, task *Task, generation uint64) error {
	if task.PathFreeze == nil || task.Completion != nil {
		return nil
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if ctx.Err() != nil || d.closed.Load() || d.getTask(task.ID) != task {
		return transfer.ErrRecoveryStale
	}
	if _, exists := d.pathOutputs.Load(task.ID); exists {
		return ErrPathFreezeHeld
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return err
	}
	task.lock.Lock()
	owned, e, err := d.readPathExpected(identity.DatasetID, task.ID)
	task.lock.Unlock()
	if err != nil {
		return err
	}
	if owned.PathFreeze == nil || owned.PathFreeze.Phase != PathFreezeFrozen {
		return ErrPathFreezeHeld
	}
	runtime, err := d.pathOutputRoot(ctx, owned)
	if err != nil {
		return err
	}
	retain := false
	defer func() {
		if !retain {
			runtime.close()
		}
	}()
	s := *owned.PathFreeze
	if s.Output != nil {
		f, ok := task.fetcher.(*httpProtocol.Fetcher)
		if !ok {
			return ErrPathFreezeHeld
		}
		var file *os.File
		switch s.Output.Phase {
		case MCPWriteOutputPlanned:
			var directory transfer.CompletionFileIdentity
			err = runtime.lease.WithDirectory(func(d *transfer.CompletionDirectory) error { directory = d.Identity(); return nil })
			if err != nil {
				return err
			}
			var binding transfer.MCPOutputBinding
			file, binding, err = transfer.CreateVerifiedMCPOutput(runtime.lease, task.ID, directory, s.Output.StagingName, s.Layout.Members[0].RelativePath)
			if err != nil {
				return err
			}
			next := *s.Output
			next.Phase = MCPWriteOutputStaging
			next.Binding = &binding
			task.lock.Lock()
			owned, _, err = SavePathOutput(d.storage.(AtomicStorage), identity.DatasetID, e.Token, s, next, nil)
			if err == nil {
				task.dataLock.Lock()
				task.PathFreeze = owned.PathFreeze
				task.dataLock.Unlock()
			}
			task.lock.Unlock()
		case MCPWriteOutputStaging:
			file, err = transfer.OpenVerifiedMCPOutputWriter(runtime.lease, *s.Output.Binding)
		default:
			return ErrPathFreezeHeld
		}
		if file != nil {
			defer func() {
				if !retain {
					file.Close()
				}
			}()
		}
		if err != nil {
			return err
		}
		if err = runtime.check(); err != nil {
			return err
		}
		if err = f.SetVerifiedOutput(file); err != nil {
			return err
		}
		if err = f.BindPathFreezeGuard(task.ID, runtime.check); err != nil {
			f.ReleaseVerifiedOutput()
			return err
		}
	} else if task.Protocol == "media" {
		err = d.bindMediaPathOutput(ctx, task, runtime, identity.DatasetID, s)
		if err != nil {
			return err
		}
	} else {
		adapter, ok := task.fetcher.(interface {
			PathFreezeLayout() (pathpolicy.LayoutV1, []int64, error)
			BindPathFreeze(context.Context, pathpolicy.LayoutV1, *transfer.VerifiedLayoutStorage) error
		})
		if !ok {
			return ErrPathFreezeHeld
		}
		layout, sizes, err := adapter.PathFreezeLayout()
		if err != nil {
			return err
		}
		hash, err := pathpolicy.LayoutFingerprint(layout)
		if err != nil || hash != s.LayoutHash {
			return ErrPathFreezeConflict
		}
		var binding transfer.VerifiedLayoutBinding
		if s.LayoutOutput == nil {
			runtime.storage, binding, err = transfer.CreateVerifiedLayoutStorage(runtime.lease, *s.Layout, sizes, runtime.check)
		} else {
			binding = pathNativeBinding(*s.LayoutOutput)
			switch s.LayoutOutput.Phase {
			case MCPWriteOutputStaging:
				runtime.storage, binding, err = transfer.OpenVerifiedLayoutStorage(runtime.lease, *s.Layout, sizes, binding, runtime.check)
			case MCPWriteOutputPublished:
				runtime.storage, binding, err = transfer.OpenVerifiedLayoutReader(runtime.lease, *s.Layout, sizes, binding, runtime.check)
			default:
				return ErrPathFreezeHeld
			}
		}
		if err != nil {
			return err
		}
		if s.LayoutOutput == nil {
			next := pathLayoutBinding(binding)
			next.Phase = MCPWriteOutputStaging
			task.lock.Lock()
			_, e, err = d.readPathExpected(identity.DatasetID, task.ID)
			if err == nil {
				owned, _, err = SavePathLayoutOutput(d.storage.(AtomicStorage), identity.DatasetID, e, next)
			}
			if err == nil {
				task.dataLock.Lock()
				task.PathFreeze = owned.PathFreeze
				task.dataLock.Unlock()
			}
			task.lock.Unlock()
			if err != nil {
				return err
			}
		}
		if err = runtime.check(); err != nil {
			return err
		}
		if err = runtime.storage.Activate(binding); err != nil {
			return err
		}
		if err = adapter.BindPathFreeze(ctx, *s.Layout, runtime.storage); err != nil {
			return err
		}
	}
	if err = runtime.check(); err != nil {
		return err
	}
	task.statusLock.Lock()
	current := task.runGeneration == generation && task.getStatus() == base.DownloadStatusRunning
	task.statusLock.Unlock()
	if !current {
		return transfer.ErrRecoveryStale
	}
	d.pathOutputs.Store(task.ID, runtime)
	retain = true
	return nil
}
func pathNativeBinding(s PathLayoutOutputState) transfer.VerifiedLayoutBinding {
	b := transfer.VerifiedLayoutBinding{Version: 1, DirectoryIdentity: s.DirectoryIdentity, Files: make([]transfer.VerifiedLayoutFileIdentity, len(s.Files))}
	for i, f := range s.Files {
		b.Files[i] = transfer.VerifiedLayoutFileIdentity{Index: f.Index, RelativePath: f.RelativePath, Identity: f.Identity}
	}
	return b
}
func pathLayoutBinding(b transfer.VerifiedLayoutBinding) PathLayoutOutputState {
	s := PathLayoutOutputState{Version: 1, DirectoryIdentity: b.DirectoryIdentity, Files: make([]PathFileIdentity, len(b.Files))}
	for i, f := range b.Files {
		s.Files[i] = PathFileIdentity{Index: f.Index, RelativePath: f.RelativePath, Identity: f.Identity}
	}
	return s
}
