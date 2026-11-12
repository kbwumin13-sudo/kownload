package download

import (
	"context"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func mcpWriteFailure(code MCPWriteErrorCode) error { return &MCPWriteError{Code: code} }

// MCPWriteCapabilities inspects the current configured instance. A returned
// tuple is an observation, never a permit; Submit must CAS the target again.
func (d *Downloader) MCPWriteCapabilities(ctx context.Context, request MCPWriteCapabilityRequest) (MCPWriteCapability, error) {
	if ctx == nil || ctx.Err() != nil {
		return MCPWriteCapability{}, mcpWriteFailure(MCPWriteCancelled)
	}
	if request.Version != 1 || !datasetIDPattern.MatchString(request.DatasetID) {
		return MCPWriteCapability{}, mcpWriteFailure(MCPWriteInvalidRequest)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return MCPWriteCapability{}, mcpWriteFailure(MCPWriteStorageUnavailable)
	}
	if identity.DatasetID != request.DatasetID {
		return MCPWriteCapability{}, mcpWriteFailure(MCPWriteDatasetMismatch)
	}
	result := MCPWriteCapability{Version: 1, DatasetID: identity.DatasetID, OutputRootIDs: []string{}}
	holder := d.mcpWriteHolder()
	if holder == nil || holder.dataset != identity.DatasetID {
		reason := MCPWriteCapabilityUnavailable
		result.Reason = &reason
		return result, nil
	}
	policyID, networkID := holder.authority.PolicyID(), holder.network
	result.PolicyID, result.NetworkPolicyID = &policyID, &networkID
	for _, scope := range holder.policy.Datasets {
		if scope.ID == identity.DatasetID && holder.policy.Operations.Create {
			result.OutputRootIDs = append([]string{}, scope.RootIDs...)
		}
	}
	result.Operations.Create = len(result.OutputRootIDs) > 0
	if request.TaskID == "" {
		return result, nil
	}
	var stored *Task
	var record recoveryRecord
	err = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
		var required recoveryRequirement
		var err error
		stored, record, required, err = readSharedRecovery(reader, request.TaskID)
		_ = required
		if err != nil {
			return err
		}
		if stored.Protocol != "http" || stored.Status == base.DownloadStatusDone || record.Generation == 0 {
			return transfer.ErrRecoveryHeld
		}
		return checkCreationAssociationFrom(reader, stored)
	})
	if err != nil || d.getTask(request.TaskID) == nil {
		reason := MCPWriteTaskUnavailable
		result.Reason = &reason
		return result, nil
	}
	result.Task = &MCPWriteTaskCapability{TaskID: stored.ID, Expected: MCPWriteTarget{
		Episode: record.Episode, Generation: record.Generation, SourceRevision: record.SourceRevision}}
	result.Operations.Pause = holder.authority.AllowTask(policyID, identity.DatasetID, "pause", stored.ID)
	result.Operations.Resume = holder.authority.AllowTask(policyID, identity.DatasetID, "resume", stored.ID)
	if ctx.Err() != nil || d.mcpWriteHolder() != holder {
		return MCPWriteCapability{}, mcpWriteFailure(MCPWriteCancelled)
	}
	return result, nil
}
