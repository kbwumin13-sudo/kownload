package download

import (
	"github.com/GopeedLab/gopeed/internal/transfer"
	"os"
	"path/filepath"
)

// This runner can read completion references but does not implement their
// execution capability. Presence, including malformed/unknown values, must
// never fall through to legacy Restore/Start. This check neither enrolls nor
// repairs tasks and deliberately does not reinterpret a committed record.
func (d *Downloader) checkUnsupportedCompletion(task *Task) error {
	task.dataLock.RLock()
	present := task.Completion != nil
	task.dataLock.RUnlock()
	for _, bucket := range []string{"task_completion_v1", "task_completion_required_v1"} {
		var value any
		exists, err := d.storage.Get(bucket, task.ID, &value)
		if err != nil {
			return transfer.ErrRecoveryHeld
		}
		present = present || exists
	}
	if !present {
		return nil
	}
	if !transfer.VerifiedCompletionPlatformSupported || d.verifiedCompleter == nil {
		return transfer.ErrRecoveryHeld
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return transfer.ErrRecoveryHeld
	}
	owned, record, err := d.readVerifiedTask(identity.DatasetID, task.ID)
	if err != nil {
		return transfer.ErrRecoveryHeld
	}
	if record.Phase == transfer.CompletionPrepared && record.StagingName != "" && record.StagingIdentity.Inode == 0 {
		_, err := os.Lstat(filepath.Join(owned.Meta.Opts.Path, record.StagingName))
		if !os.IsNotExist(err) {
			return transfer.ErrRecoveryHeld
		}
	}
	return nil
}
