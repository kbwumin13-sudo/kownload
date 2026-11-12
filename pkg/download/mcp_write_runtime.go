package download

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/util"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

type mcpTaskRuntime struct {
	mu      sync.Mutex
	lease   *transfer.VerifiedRootLease
	binding MCPExecutionBinding
	token   transfer.RecoveryToken
}

func (d *Downloader) QueryMCPWrite(ctx context.Context, query MCPWriteQuery) (MCPWriteReceipt, error) {
	if ctx == nil || ctx.Err() != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteCancelled)
	}
	var receipt MCPWriteReceipt
	err := withDatasetReadView(d.storage, query.DatasetID, func(reader datasetReader) error {
		var err error
		receipt, err = queryMCPWrite(reader, query)
		return err
	})
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteStoreError(err)
	}
	if ctx.Err() != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteCancelled)
	}
	return receipt, nil
}

func (d *Downloader) SubmitMCPWrite(ctx context.Context, command MCPWriteRequest) (MCPWriteReceipt, error) {
	if ctx == nil || ctx.Err() != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteCancelled)
	}
	if !mcpWriteCommandValid(command) {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteInvalidRequest)
	}
	command, err := cloneOperationJSON(command)
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteInvalidRequest)
	}
	holder := d.mcpWriteHolder()
	if holder == nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteCapabilityUnavailable)
	}
	if command.DatasetID != holder.dataset {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteDatasetMismatch)
	}
	if command.PolicyID != holder.authority.PolicyID() {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWritePolicyMismatch)
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteStorageUnavailable)
	}
	// Replay is resolved before borrowing a root or re-reading defaults. It
	// cannot resume a pending command, attach RAM or invoke a Task method.
	var prior *mcpWriteRecord
	err = withDatasetReadView(d.storage, holder.dataset, func(reader datasetReader) error {
		var err error
		prior, err = readMCPWriteRecord(reader, holder.dataset, command.OperationID)
		return err
	})
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteStoreError(err)
	}
	if prior != nil {
		if prior.RawHash != mcpWriteHash(command) {
			return MCPWriteReceipt{}, mcpWriteErr(MCPWriteOperationConflict)
		}
		return prior.receipt(), nil
	}
	if command.Kind != MCPWriteCreate {
		return d.submitMCPTaskAction(ctx, holder, store, command)
	}
	if util.SafeFilename(command.Create.FileName) != command.Create.FileName {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteInvalidRequest)
	}
	grant, lease, err := holder.authority.BorrowCreate(command.PolicyID, holder.dataset, command.Create.OutputRootID)
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWritePermissionDenied)
	}
	retain := false
	defer func() {
		if !retain {
			_ = lease.Close()
		}
	}()
	var root transfer.CompletionFileIdentity
	if err = lease.WithDirectory(func(directory *transfer.CompletionDirectory) error { root = directory.Identity(); return nil }); err != nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWritePermissionDenied)
	}
	binding := MCPExecutionBinding{Version: 1, RequiredCapability: MCPWriteNamespace, DatasetID: holder.dataset,
		PolicyID: command.PolicyID, NetworkPolicyID: holder.network,
		Root:     &MCPWriteRootBinding{RootID: command.Create.OutputRootID, Path: grant.OutputRoot, Identity: root},
		FileName: command.Create.FileName, StagingName: "." + command.OperationID + ".mcp-part", EffectsVersion: "effects/v1", Effects: append([]string{}, grant.Effects...)}
	sort.Strings(binding.Effects)
	record, freshR, err := reserveMCPWrite(store, holder.dataset, command, binding)
	if err != nil {
		return MCPWriteReceipt{}, err
	}
	if !freshR {
		return record.receipt(), nil
	}
	reject := func(code MCPWriteErrorCode) (MCPWriteReceipt, error) {
		return rejectMCPWrite(store, holder.dataset, record, code)
	}
	if ctx.Err() != nil || d.mcpWriteHolder() != holder {
		return reject(MCPWriteCancelled)
	}
	task := NewTask()
	task.ID = record.Member.TaskID
	task.Protocol = "http"
	task.Recovery = &TaskRecoveryRef{Version: recoverySharedVersion}
	task.Progress = &Progress{}
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: command.Create.URL, Extra: &fhttp.ReqExtra{}},
		Opts: &base.Options{Path: binding.Root.Path, Name: binding.FileName, SelectFiles: []int{}, Extra: &fhttp.OptsExtra{}}}
	manager := new(httpProtocol.FetcherManager)
	f := manager.Build().(*httpProtocol.Fetcher)
	d.setupFetcher(manager, f)
	f.Meta().Req, f.Meta().Opts = task.Meta.Req, task.Meta.Opts
	f.RequireMCPOutput()
	if err = f.SetMCPHTTPTransport(holder.client, holder.network); err != nil {
		return reject(MCPWriteCapabilityUnavailable)
	}
	snapshot, supported, err := f.CaptureRecoveryContext(task.Meta.Req)
	if err != nil || !supported {
		return reject(MCPWriteCapabilityUnavailable)
	}
	if proxy := snapshot.Proxy(); proxy != nil {
		request, e := http.NewRequestWithContext(ctx, "GET", command.Create.URL, nil)
		if e != nil {
			return reject(MCPWriteInvalidRequest)
		}
		if target, e := proxy(request); e != nil || target != nil {
			return reject(MCPWriteCapabilityUnavailable)
		}
	}
	episode, err := gonanoid.New()
	if err != nil {
		return reject(MCPWriteStorageUnavailable)
	}
	sourceHash, err := recoverySourceHash(task.Meta.Req, snapshot.DefaultsHash())
	if err != nil {
		return reject(MCPWriteInvalidRequest)
	}
	initial := recoveryRecord{Version: recoverySharedVersion, TaskID: task.ID, Episode: episode, SourceRevision: 1,
		SourceHash: sourceHash, ExecutionDefaultsHash: snapshot.DefaultsHash(), InitialAttempt: recoveryInitialAttempt{State: "ungranted"}, Actions: []recoveryAction{}}
	owned, freshM, err := createMCPWriteTask(store, holder.dataset, record, task, initial)
	if err != nil {
		return MCPWriteReceipt{}, err
	}
	if !freshM || owned == nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteStorageUnavailable)
	}
	// Only the current, confirmed M winner attaches the private first-run
	// qualification. No stored state string, query or process restart does.
	initTask(owned)
	owned.fetcherManager, owned.fetcher = manager, f
	owner, err := newTaskRecoveryOwner(store, owned, snapshot.DefaultsHash())
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteStoreError(err)
	}
	owner.sharedInitialEligible.Store(true)
	owner.coverageNewEnrollment.Store(true)
	owner.closed = &d.closed
	owned.recoveryOwner.Store(owner)
	if err = owner.bindSharedDataset(holder.dataset); err != nil {
		return MCPWriteReceipt{}, mcpWriteStoreError(err)
	}
	d.operationMu.Lock()
	if ctx.Err() == nil && d.mcpWriteHolder() == holder {
		d.lock.Lock()
		d.tasks = append(d.tasks, owned)
		d.lock.Unlock()
		holder.tasks.Store(owned.ID, &mcpTaskRuntime{lease: lease, binding: binding})
		retain = true
	}
	d.operationMu.Unlock()
	if retain {
		if err := d.Continue(&TaskFilter{IDs: []string{owned.ID}}); err != nil {
			return d.QueryMCPWrite(ctx, MCPWriteQuery{Version: 1, DatasetID: command.DatasetID, PolicyID: command.PolicyID, OperationID: command.OperationID})
		}
	}
	return d.QueryMCPWrite(ctx, MCPWriteQuery{Version: 1, DatasetID: command.DatasetID, PolicyID: command.PolicyID, OperationID: command.OperationID})
}

func mcpTarget(record recoveryRecord) MCPWriteTarget {
	return MCPWriteTarget{record.Episode, record.Generation, record.SourceRevision}
}

func mcpCompatibleBinding(a, b MCPExecutionBinding) bool {
	return mcpWriteSameOutput(a, b) && a.DatasetID == b.DatasetID && a.NetworkPolicyID == b.NetworkPolicyID
}

func mcpWriteRuntimeError(err error) MCPWriteErrorCode {
	var fixed *MCPWriteError
	if errors.As(err, &fixed) {
		return fixed.Code
	}
	if errors.Is(err, context.Canceled) {
		return MCPWriteCancelled
	}
	if errors.Is(err, transfer.ErrRecoveryStale) {
		return MCPWriteStaleTarget
	}
	if errors.Is(err, transfer.ErrMCPNetworkDenied) {
		return MCPWriteNetworkDenied
	}
	return MCPWriteExecutionFailed
}
