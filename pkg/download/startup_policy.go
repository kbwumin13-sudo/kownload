package download

import "errors"

// StartupTaskPolicy controls implicit work during one launch, independently of
// the user's persisted settings. It does not grant permission to execute a task
// with an unsupported persisted state or a pending browser handoff.
type StartupTaskPolicy string

const (
	StartupTaskPolicyNormal         StartupTaskPolicy = "normal"
	StartupTaskPolicyImportedReview StartupTaskPolicy = "imported-review"
)

var (
	ErrStartupTaskPolicy = errors.New("unsupported startup task policy")
	ErrStartupTaskReview = errors.New("imported tasks require explicit review before continuing")
)

func (p StartupTaskPolicy) Valid() bool {
	return p == "" || p == StartupTaskPolicyNormal || p == StartupTaskPolicyImportedReview
}

func (d *Downloader) checkStartupContinue(filter *TaskFilter) error {
	if d.startupTaskPolicy == StartupTaskPolicyImportedReview && (filter == nil || len(filter.IDs) == 0) {
		return ErrStartupTaskReview
	}
	return nil
}
