package download

// MCPTaskEffectAllowed is an effect-policy check, not a completion or IO
// permit. Callers keep their existing success/Fence checks and call this outside
// live Task locks. The configured holder is captured before entering a DB view.
func (d *Downloader) MCPTaskEffectAllowed(task *Task, effect string) bool {
	if d == nil || task == nil || !mcpEffectKnown(effect) {
		return false
	}
	task.dataLock.RLock()
	id := task.ID
	declared := task.MCPWrite != nil || task.CreationOperation != nil && task.CreationOperation.Namespace == MCPWriteNamespace
	task.dataLock.RUnlock()
	if id == "" {
		return false
	}
	// MCP membership is immutable from birth. Check persisted markers as well:
	// dropping a public Task slot must not turn a managed Task into legacy.
	if !declared {
		if d.storage == nil {
			return true // Existing pre-Setup/synthetic legacy consumers.
		}
		var saved Task
		present, err := d.storage.Get(bucketTask, id, &saved)
		if err != nil {
			return false
		}
		var link operationTaskLink
		linked, err := d.storage.Get(bucketOperationTasks, id, &link)
		if err != nil {
			return false
		}
		declared = saved.MCPWrite != nil || saved.CreationOperation != nil && saved.CreationOperation.Namespace == MCPWriteNamespace || linked && link.Ref.Namespace == MCPWriteNamespace
		if !declared {
			if !present {
				return !linked && task.CreationOperation == nil
			}
			return checkCreationAssociationFrom(d.storage, &saved) == nil
		}
	}
	holder := d.mcpWriteHolder()
	if holder == nil || holder.authority == nil || !mcpEffectContains(holder.policy.Effects, effect) {
		return false
	}
	policyID := holder.authority.PolicyID()
	allowed := false
	err := withDatasetReadView(d.storage, holder.dataset, func(reader datasetReader) error {
		owned, _, err := readMCPWriteTask(reader, id)
		if err != nil || owned == nil || owned.MCPWrite == nil {
			return mcpWriteErr(MCPWriteTaskUnavailable)
		}
		if _, _, _, err = readSharedRecovery(reader, id); err != nil {
			return err
		}
		state := owned.MCPWrite
		current := state.CurrentExecutionBinding
		allowed = current.DatasetID == holder.dataset && current.PolicyID == policyID && current.NetworkPolicyID == holder.network &&
			mcpEffectContains(state.CreationBinding.Effects, effect) && mcpEffectContains(current.Effects, effect)
		return nil
	})
	return err == nil && allowed && !holder.closing.Load() && !d.closed.Load()
}

func mcpEffectKnown(effect string) bool {
	switch effect {
	case "script", "webhook", "auto_open", "post_process":
		return true
	}
	return false
}

func mcpEffectContains(effects []string, effect string) bool {
	for _, candidate := range effects {
		if candidate == effect {
			return true
		}
	}
	return false
}
