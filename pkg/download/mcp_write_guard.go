package download

import (
	"reflect"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func mcpTaskPresence(task *Task) bool {
	return task != nil && (task.MCPWrite != nil || task.CreationOperation != nil &&
		task.CreationOperation.Namespace == MCPWriteNamespace)
}

// This check does not borrow or open a root. The existing creation association
// guard also catches reverse-only MCP declarations whose Task fields were lost.
func (d *Downloader) checkMCPTaskExecution(task *Task) error {
	task.dataLock.RLock()
	presence := mcpTaskPresence(task)
	task.dataLock.RUnlock()
	if !presence {
		return nil
	}
	holder := d.mcpWriteHolder()
	if holder == nil {
		return transfer.ErrRecoveryHeld
	}
	var owned *Task
	err := withDatasetReadView(d.storage, holder.dataset, func(reader datasetReader) error {
		var err error
		owned, _, err = readMCPWriteTask(reader, task.ID)
		return err
	})
	if err != nil || owned.MCPWrite == nil {
		return transfer.ErrRecoveryHeld
	}
	state := owned.MCPWrite
	binding := state.CurrentExecutionBinding
	if binding.PolicyID != holder.authority.PolicyID() || binding.DatasetID != holder.dataset || binding.NetworkPolicyID != holder.network || !mcpCompatibleBinding(state.CreationBinding, binding) {
		return transfer.ErrRecoveryHeld
	}
	if state.Output.Phase == MCPWriteOutputPublished {
		return nil // Read/restore may inspect a finished record; no writer is opened.
	}
	if state.Output.Phase != MCPWriteOutputPlanned && state.Output.Phase != MCPWriteOutputStaging {
		return transfer.ErrRecoveryHeld
	}
	entry, live := holder.tasks.Load(task.ID)
	if live {
		runtime := entry.(*mcpTaskRuntime)
		runtime.mu.Lock()
		valid := runtime.lease != nil && reflect.DeepEqual(runtime.binding, binding)
		runtime.mu.Unlock()
		if valid {
			return nil
		}
	}
	// A reopened historical Task does not inherit the original new-M RAM
	// qualification. Resume requires the current policy's explicit Task scope.
	if state.Output.Phase == MCPWriteOutputPlanned || !holder.authority.AllowTask(binding.PolicyID, holder.dataset, "resume", task.ID) {
		return transfer.ErrRecoveryHeld
	}
	return nil
}
