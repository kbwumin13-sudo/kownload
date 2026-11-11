package transfer

import (
	"os"
	"time"
)

// SetStagingServerTimes preserves the existing non-Windows UseServerCtime
// behavior: access time is the caller's captured now, modification time is the
// saved server Last-Modified. Despite that option's name, neither the old
// non-Windows setft dependency nor this method explicitly sets birth time.
// Filesystems may implicitly clamp birth time when mtime is moved earlier.
// Darwin Futimes has microsecond precision; HTTP Last-Modified is whole seconds.
//
// The caller must have joined the exact run's writers and hold its lifecycle
// Fence. Call only in waiting_verify, BEFORE hashing. The effective option and
// server time come from the frozen policy/owned Meta, not later global config;
// accessedAt is the caller's captured current time. A nil server time remains
// the caller's original no-op; do not invent one here.
//
// Only the same pinned O_RDONLY, single-link, regular file descriptor is used;
// no pathname is opened or changed. Renaming/replacing its old pathname cannot
// redirect this operation. Namespace/identity checks before publication still
// belong to Complete. This method does not prove quiescence, change a Task or
// record, grant completion permission, close the borrowed fd, or clean files.
// Errors (including Sync after metadata changed) must stop completion.
func (d *CompletionDirectory) SetStagingServerTimes(file *os.File, record VerifiedCompletionRecord, accessedAt, modifiedAt time.Time) error {
	if d == nil || d.handle == nil || file == nil || record.Phase != CompletionWaiting ||
		ValidateVerifiedCompletionRecord(record) != nil || accessedAt.IsZero() || modifiedAt.IsZero() {
		return completionError(CompletionInvalid, nil)
	}
	if record.DirectoryIdentity != d.identity || record.StagingIdentity.Device != d.identity.Device {
		return completionError(CompletionIdentity, nil)
	}
	directoryID, err := CompletionIdentityOf(d.handle)
	if err != nil || directoryID != d.identity {
		return completionError(CompletionIdentity, err)
	}
	if err := completionReadOnly(file); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.Size {
		return completionError(CompletionIdentity, err)
	}
	identity, err := CompletionIdentityOf(file)
	if err != nil || identity != record.StagingIdentity {
		return completionError(CompletionIdentity, err)
	}
	if err := setCompletionDescriptorTimes(file, accessedAt, modifiedAt); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return completionError(CompletionIO, err)
	}
	return nil
}
