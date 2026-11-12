package download

import (
	"context"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func (d *Downloader) submitMCPTaskAction(ctx context.Context, holder *mcpWriteHolder, store AtomicStorage, command MCPWriteRequest) (MCPWriteReceipt, error) {
	id := command.Action.TaskID
	if !holder.authority.AllowTask(command.PolicyID, holder.dataset, string(command.Kind), id) {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWritePermissionDenied)
	}
	var owned *Task
	err := withDatasetReadView(d.storage, holder.dataset, func(reader datasetReader) error {
		var record recoveryRecord
		var err error
		owned, record, _, err = readSharedRecovery(reader, id)
		if err != nil {
			return err
		}
		if mcpTarget(record) != command.Action.Expected {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		if owned.Protocol != "http" || owned.Status == base.DownloadStatusDone {
			return mcpWriteErr(MCPWriteTaskUnavailable)
		}
		_, _, err = readMCPWriteTask(reader, id)
		if err == nil {
			err = checkCreationAssociationFrom(reader, owned)
		}
		return err
	})
	if err != nil {
		return MCPWriteReceipt{}, mcpWriteStoreError(err)
	}
	live := d.getTask(id)
	if live == nil {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteTaskUnavailable)
	}
	binding := MCPExecutionBinding{Version: 1, RequiredCapability: MCPWriteNamespace, DatasetID: holder.dataset,
		PolicyID: command.PolicyID, NetworkPolicyID: holder.network, PreserveGUI: true, EffectsVersion: "effects/v1", Effects: []string{}}
	var nextLease *transfer.VerifiedRootLease
	retain := false
	defer func() {
		if nextLease != nil && !retain {
			_ = nextLease.Close()
		}
	}()
	if owned.MCPWrite != nil {
		binding, err = cloneOperationJSON(owned.MCPWrite.CurrentExecutionBinding)
		if err != nil {
			return MCPWriteReceipt{}, mcpWriteErr(MCPWriteTaskUnavailable)
		}
		binding.PolicyID, binding.NetworkPolicyID = command.PolicyID, holder.network
		if command.Kind == MCPWriteResume {
			if owned.MCPWrite.Output.Phase != MCPWriteOutputStaging || !mcpCompatibleBinding(owned.MCPWrite.CreationBinding, binding) {
				return MCPWriteReceipt{}, mcpWriteErr(MCPWriteOutputConflict)
			}
			grant, lease, err := holder.authority.BorrowTaskOutput(command.PolicyID, holder.dataset, id, binding.Root.RootID)
			if err != nil {
				return MCPWriteReceipt{}, mcpWriteErr(MCPWritePermissionDenied)
			}
			nextLease = lease
			if grant.OutputRoot != binding.Root.Path {
				return MCPWriteReceipt{}, mcpWriteErr(MCPWriteOutputConflict)
			}
			if err := lease.WithDirectory(func(directory *transfer.CompletionDirectory) error {
				if directory.Identity() != binding.Root.Identity {
					return transfer.ErrRecoveryHeld
				}
				return nil
			}); err != nil {
				return MCPWriteReceipt{}, mcpWriteErr(MCPWriteOutputConflict)
			}
			binding.Effects = []string{}
			for _, effect := range owned.MCPWrite.CreationBinding.Effects {
				for _, current := range grant.Effects {
					if current == effect {
						binding.Effects = append(binding.Effects, effect)
						break
					}
				}
			}
		}
	}
	record, fresh, err := reserveMCPWrite(store, holder.dataset, command, binding)
	if err != nil {
		return MCPWriteReceipt{}, err
	}
	if !fresh {
		return record.receipt(), nil
	}
	// Stop and join the old writer before changing its binding. This revokes
	// the old run; it never grants IO if the receipt's ACK is later unknown.
	if err := d.doPauseChecked(live, func() error {
		return d.revokeMCPActionTarget(ctx, holder, live, command.Action.Expected)
	}); err != nil {
		return rejectMCPWrite(store, holder.dataset, record, mcpWriteRuntimeError(err))
	}
	live.statusLock.Lock()
	paused := live.pauseDone
	live.statusLock.Unlock()
	if paused != nil {
		select {
		case <-paused:
		case <-ctx.Done():
			return MCPWriteReceipt{}, mcpWriteErr(MCPWriteCancelled)
		}
	}
	if ctx.Err() != nil || d.mcpWriteHolder() != holder {
		return MCPWriteReceipt{}, mcpWriteErr(MCPWriteCancelled)
	}
	d.operationMu.Lock()
	if d.getTask(id) != live {
		d.operationMu.Unlock()
		return rejectMCPWrite(store, holder.dataset, record, MCPWriteTaskUnavailable)
	}
	live.lock.Lock()
	accepted, winner, err := commitMCPWriteAction(store, holder.dataset, record, command.Action.Expected)
	if err == nil && winner {
		live.dataLock.Lock()
		live.MCPWrite, live.UpdatedAt = accepted.MCPWrite, accepted.UpdatedAt
		live.dataLock.Unlock()
	}
	live.lock.Unlock()
	if err == nil && winner && command.Kind == MCPWriteResume && ctx.Err() == nil && d.mcpWriteHolder() == holder && nextLease != nil {
		holder.tasks.Store(id, &mcpTaskRuntime{lease: nextLease, binding: binding})
		retain = true
	}
	d.operationMu.Unlock()
	if err != nil {
		return MCPWriteReceipt{}, err
	}
	if winner && command.Kind == MCPWriteResume && ctx.Err() == nil && d.mcpWriteHolder() == holder {
		// GUI targets retain their ordinary fetcher/output/effects and budget.
		if accepted.MCPWrite == nil || retain {
			if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
				return d.QueryMCPWrite(ctx, MCPWriteQuery{1, command.DatasetID, command.PolicyID, command.OperationID})
			}
		}
	}
	return d.QueryMCPWrite(ctx, MCPWriteQuery{1, command.DatasetID, command.PolicyID, command.OperationID})
}

// Membership is pinned by operationMu before entering Task.statusLock.
// Called under Task.statusLock, which also serializes new-run generation
// admission. The authoritative read precedes the owner lock; its callback
// never takes live locks. Revoke under owner.mu so a source commit cannot
// advance S between the final target check and withdrawing IO qualification.
func (d *Downloader) revokeMCPActionTarget(ctx context.Context, holder *mcpWriteHolder, task *Task, expected MCPWriteTarget) error {
	if ctx.Err() != nil || d.mcpWriteHolder() != holder {
		return mcpWriteErr(MCPWriteCancelled)
	}
	if task.getStatus() == base.DownloadStatusDone {
		return mcpWriteErr(MCPWriteTaskUnavailable)
	}
	err := withDatasetReadView(d.storage, holder.dataset, func(reader datasetReader) error {
		_, record, _, err := readSharedRecovery(reader, task.ID)
		if err != nil {
			return err
		}
		if mcpTarget(record) != expected {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		return nil
	})
	if err != nil {
		return mcpWriteStoreError(err)
	}
	// A new run can be admitted before its asynchronous begin is persisted.
	if task.getStatus() == base.DownloadStatusRunning && task.runGeneration != expected.Generation {
		return mcpWriteErr(MCPWriteStaleTarget)
	}
	owner := task.recoveryOwner.Load()
	if owner == nil {
		if task.getStatus() == base.DownloadStatusRunning {
			return mcpWriteErr(MCPWriteStaleTarget)
		}
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if mcpTarget(owner.record) != expected {
		return mcpWriteErr(MCPWriteStaleTarget)
	}
	owner.invalidate()
	return nil
}
