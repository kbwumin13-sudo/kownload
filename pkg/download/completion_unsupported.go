package download

import "github.com/GopeedLab/gopeed/internal/transfer"

// This runner can read completion references but does not implement their
// execution capability. Presence, including malformed/unknown values, must
// never fall through to legacy Restore/Start. This check neither enrolls nor
// repairs tasks and deliberately does not reinterpret a committed record.
func (d *Downloader) checkUnsupportedCompletion(task *Task) error {
	task.dataLock.RLock()
	present := task.Completion != nil
	task.dataLock.RUnlock()
	if present {
		return transfer.ErrRecoveryHeld
	}
	for _, bucket := range []string{"task_completion_v1", "task_completion_required_v1"} {
		var value any
		exists, err := d.storage.Get(bucket, task.ID, &value)
		if err != nil || exists {
			return transfer.ErrRecoveryHeld
		}
	}
	return nil
}
