package download

import (
	"context"
	"os"
	"reflect"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func (d *Downloader) configureMCPFetcher(task *Task) error {
	holder := d.mcpWriteHolder()
	f, ok := task.fetcher.(*httpProtocol.Fetcher)
	if holder == nil || !ok {
		return transfer.ErrRecoveryHeld
	}
	f.RequireMCPOutput()
	if f.HasMCPHTTPClient(holder.client) {
		return nil
	}
	return f.SetMCPHTTPTransport(holder.client, holder.network)
}

func (d *Downloader) readMCPTask(dataset, id string) (*Task, error) {
	var task *Task
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		var err error
		task, _, err = readMCPWriteTask(reader, id)
		return err
	})
	return task, err
}

// Caller owns task.lock; actual root checks and file opening occur after
// releasing it. Resumed prefix probes therefore read the acknowledged stage,
// never a missing or foreign final pathname.
func (d *Downloader) bindMCPOutputForRun(ctx context.Context, task *Task, lease *transfer.RecoveryLease) error {
	if lease == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := d.saveTask(task); err != nil {
		return err
	}
	task.lock.Unlock()
	file, err := d.prepareMCPOutput(ctx, task, lease)
	task.lock.Lock()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		file.Close()
		return ctx.Err()
	}
	output, ok := task.fetcher.(*httpProtocol.Fetcher)
	if !ok {
		file.Close()
		return transfer.ErrRecoveryHeld
	}
	if err := output.SetVerifiedOutput(file); err != nil {
		file.Close()
		return err
	}
	return nil
}

func (d *Downloader) mcpOutputRuntime(holder *mcpWriteHolder, state MCPTaskState, id string) (*mcpTaskRuntime, error) {
	if entry, ok := holder.tasks.Load(id); ok {
		return entry.(*mcpTaskRuntime), nil
	}
	binding := state.CurrentExecutionBinding
	grant, lease, err := holder.authority.BorrowTaskOutput(holder.authority.PolicyID(), holder.dataset, id, binding.Root.RootID)
	if err != nil {
		return nil, transfer.ErrRecoveryHeld
	}
	if grant.OutputRoot != binding.Root.Path {
		lease.Close()
		return nil, transfer.ErrRecoveryHeld
	}
	if err = lease.WithDirectory(func(directory *transfer.CompletionDirectory) error {
		if directory.Identity() != binding.Root.Identity {
			return transfer.ErrRecoveryHeld
		}
		return nil
	}); err != nil {
		lease.Close()
		return nil, err
	}
	entry := &mcpTaskRuntime{lease: lease, binding: binding}
	actual, loaded := holder.tasks.LoadOrStore(id, entry)
	if loaded {
		lease.Close()
		return actual.(*mcpTaskRuntime), nil
	}
	return entry, nil
}

// Called after metadata Store, with Task/owner/status/main locks released.
// operationMu excludes deletion and MCP Pause through the actual FS and ACK.
func (d *Downloader) prepareMCPOutput(ctx context.Context, task *Task, run *transfer.RecoveryLease) (*os.File, error) {
	holder := d.mcpWriteHolder()
	if holder == nil || run == nil || ctx.Err() != nil {
		return nil, transfer.ErrRecoveryHeld
	}
	owned, err := d.readMCPTask(holder.dataset, task.ID)
	if err != nil || owned.MCPWrite == nil {
		return nil, transfer.ErrRecoveryHeld
	}
	state := *owned.MCPWrite
	if owned.Meta.Res == nil || len(owned.Meta.Res.Files) != 1 || owned.Meta.Res.Name != "" || owned.Meta.Res.Files[0].Path != "" || owned.Meta.Opts.Name != state.CreationBinding.FileName {
		return nil, transfer.ErrRecoveryHeld
	}
	entry, err := d.mcpOutputRuntime(holder, state, task.ID)
	if err != nil {
		return nil, err
	}
	entry.mu.Lock()
	lease, compatible := entry.lease, reflect.DeepEqual(entry.binding, state.CurrentExecutionBinding)
	entry.mu.Unlock()
	if lease == nil || !compatible {
		return nil, transfer.ErrRecoveryHeld
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if ctx.Err() != nil || run.Check() != nil || d.getTask(task.ID) != task || d.mcpWriteHolder() != holder {
		return nil, transfer.ErrRecoveryStale
	}
	var file *os.File
	switch state.Output.Phase {
	case MCPWriteOutputPlanned:
		var binding transfer.MCPOutputBinding
		file, binding, err = transfer.CreateVerifiedMCPOutput(lease, state.OperationID, state.CreationBinding.Root.Identity, state.CreationBinding.StagingName, state.CreationBinding.FileName)
		if err == nil {
			store, ok := d.storage.(AtomicStorage)
			if !ok {
				file.Close()
				return nil, transfer.ErrRecoveryHeld
			}
			var next *Task
			next, _, err = saveMCPWriteOutput(store, holder.dataset, run.Token, state, MCPWriteOutputState{Phase: MCPWriteOutputStaging, Binding: &binding}, nil)
			if err == nil {
				task.dataLock.Lock()
				task.MCPWrite = next.MCPWrite
				task.dataLock.Unlock()
			}
		}
	case MCPWriteOutputStaging:
		if state.Output.Binding == nil {
			return nil, transfer.ErrRecoveryHeld
		}
		file, err = transfer.OpenVerifiedMCPOutputWriter(lease, *state.Output.Binding)
	default:
		return nil, transfer.ErrRecoveryHeld
	}
	if err == nil && (ctx.Err() != nil || run.Check() != nil || d.mcpWriteHolder() != holder) {
		err = transfer.ErrRecoveryStale
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		return nil, err
	}
	entry.mu.Lock()
	entry.token = run.Token
	entry.mu.Unlock()
	return file, nil
}

func (d *Downloader) releaseMCPTaskLease(task *Task, tokens ...transfer.RecoveryToken) {
	d.mcpConfigMu.Lock()
	holder := d.mcpConfigured
	d.mcpConfigMu.Unlock()
	if holder == nil {
		return
	}
	value, ok := holder.tasks.Load(task.ID)
	if !ok {
		return
	}
	entry := value.(*mcpTaskRuntime)
	entry.mu.Lock()
	if len(tokens) > 0 && entry.token != tokens[0] {
		entry.mu.Unlock()
		return
	}
	lease := entry.lease
	entry.lease = nil
	entry.mu.Unlock()
	holder.tasks.CompareAndDelete(task.ID, entry)
	if lease != nil {
		_ = lease.Close()
	}
}

// All protocol writers have joined before entering the deletion/Pause Fence.
// File Sync/noReplace happen without Task/status/owner/persistence locks.
func (d *Downloader) completeMCPHTTP(ctx context.Context, task *Task, f *httpProtocol.Fetcher, generation uint64, run *transfer.RecoveryLease) (bool, error) {
	holder := d.mcpWriteHolder()
	if holder == nil || run == nil {
		return false, transfer.ErrRecoveryHeld
	}
	if err := f.JoinVerifiedWriters(ctx); err != nil {
		return false, err
	}
	if err := f.MCPOutputSyncResult(); err != nil {
		return false, err
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if ctx.Err() != nil || run.Check() != nil || d.getTask(task.ID) != task {
		return false, transfer.ErrRecoveryStale
	}
	owned, err := d.readMCPTask(holder.dataset, task.ID)
	if err != nil || owned.MCPWrite == nil || owned.MCPWrite.Output.Phase != MCPWriteOutputStaging {
		return false, transfer.ErrRecoveryHeld
	}
	state := *owned.MCPWrite
	value, ok := holder.tasks.Load(task.ID)
	if !ok {
		return false, transfer.ErrRecoveryHeld
	}
	entry := value.(*mcpTaskRuntime)
	entry.mu.Lock()
	lease := entry.lease
	current := entry.token == run.Token
	entry.mu.Unlock()
	if lease == nil || !current {
		return false, transfer.ErrRecoveryStale
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return false, transfer.ErrRecoveryHeld
	}
	task.lock.Lock()
	task.statusLock.Lock()
	currentRun := task.runGeneration == generation && task.getStatus() == base.DownloadStatusRunning
	task.statusLock.Unlock()
	if !currentRun {
		task.lock.Unlock()
		return false, transfer.ErrRecoveryStale
	}
	if err := d.saveTask(task); err != nil {
		task.lock.Unlock()
		return false, err
	}
	publishing := state.Output
	publishing.Phase = MCPWriteOutputPublishing
	next, _, err := saveMCPWriteOutput(store, holder.dataset, run.Token, state, publishing, nil)
	if err == nil {
		task.dataLock.Lock()
		task.MCPWrite = next.MCPWrite
		task.dataLock.Unlock()
	}
	task.lock.Unlock()
	if err != nil {
		return false, err
	}
	state = *next.MCPWrite
	readonly, err := transfer.OpenVerifiedMCPOutput(lease, *state.Output.Binding)
	if err != nil {
		return false, err
	}
	defer readonly.Close()
	info, err := readonly.Stat()
	if err != nil {
		return false, err
	}
	if owned.Meta.Res == nil || (owned.Meta.Res.Size > 0 && info.Size() != owned.Meta.Res.Size) || f.Progress().TotalDownloaded() != info.Size() {
		return false, transfer.ErrRecoveryHeld
	}
	if ctx.Err() != nil || run.Check() != nil {
		return false, transfer.ErrRecoveryStale
	}
	outcome, err := transfer.PublishVerifiedMCPOutput(lease, readonly, transfer.MCPOutputSeal{Binding: *state.Output.Binding, SealedSize: info.Size()})
	if err != nil || outcome != transfer.MCPOutputConfirmed {
		uncertain := state.Output
		uncertain.Phase = MCPWriteOutputUncertain
		_, _, _ = saveMCPWriteOutput(store, holder.dataset, run.Token, state, uncertain, nil)
		if err != nil {
			return false, err
		}
		return false, transfer.ErrRecoveryHeld
	}
	if ctx.Err() != nil || run.Check() != nil || d.mcpWriteHolder() != holder {
		return false, transfer.ErrRecoveryStale
	}
	progress := &Progress{Used: task.timer.Used(), Downloaded: info.Size()}
	published := state.Output
	published.Phase = MCPWriteOutputPublished
	next, fresh, err := saveMCPWriteOutput(store, holder.dataset, run.Token, state, published, progress)
	if err != nil || !fresh {
		return false, err
	}
	task.dataLock.Lock()
	task.MCPWrite, task.Status, task.Progress, task.UpdatedAt = next.MCPWrite, next.Status, next.Progress, next.UpdatedAt
	task.clearReadPublishedLocked()
	task.dataLock.Unlock()
	return true, nil
}
