package download

import "github.com/GopeedLab/gopeed/internal/transfer"

func (t *Task) recoveryHeld() bool {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	return t.recoveryBlocked || (t.Recovery != nil && !supportedRecoveryVersion(t.Recovery.Version))
}

// Read-only capability check. Missing all three parts is a legacy task, never
// an instruction to enroll it. A reverse requirement survives old writers
// dropping Task.recovery. No repair, protocol restore or IO occurs here.
func (d *Downloader) checkTaskRecovery(task *Task) error {
	if err := d.checkMCPTaskExecution(task); err != nil {
		return err
	}
	return d.checkTaskRecoveryState(task)
}

// Setup caches only declaration compatibility. Current policy/holder admission
// is checked afresh at each execution boundary, so a trusted startup policy
// update cannot permanently poison an otherwise valid historical Task.
func (d *Downloader) checkTaskRecoveryState(task *Task) error {
	if err := d.checkUnsupportedCompletion(task); err != nil {
		return err
	}
	var record recoveryRecord
	var required recoveryRequirement
	r, err := d.storage.Get(bucketTaskRecovery, task.ID, &record)
	if err != nil {
		return transfer.ErrRecoveryUnavailable
	}
	q, err := d.storage.Get(bucketRecoveryRequired, task.ID, &required)
	if err != nil {
		return transfer.ErrRecoveryUnavailable
	}
	task.dataLock.RLock()
	ref := task.Recovery
	blocked := task.recoveryBlocked
	task.dataLock.RUnlock()
	if blocked {
		return transfer.ErrRecoveryHeld
	}
	if !r && !q && ref == nil {
		return nil
	}
	if ref == nil || !supportedRecoveryVersion(ref.Version) {
		return transfer.ErrRecoveryHeld
	}
	if ref.Version == recoverySharedVersion {
		identity, e := d.DatasetIdentity()
		if e != nil {
			return e
		}
		return withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error { _, _, _, e := readSharedRecovery(reader, task.ID); return e })
	}
	_, _, err = readRecovery(d.storage, task.ID)
	return err
}

func (d *Downloader) checkRecoverySelection(filter *TaskFilter) error {
	var tasks []*Task
	if filter == nil || filter.IsEmpty() {
		tasks = d.taskList()
	} else {
		tasks = d.getTasksByFilter(filter)
	}
	for _, task := range tasks {
		if err := d.checkTaskRecovery(task); err != nil {
			return err
		}
	}
	return nil
}
