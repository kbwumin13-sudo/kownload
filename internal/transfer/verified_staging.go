package transfer

import "os"

// CreateStaging creates one empty, exclusive file relative to this pinned
// directory. Names must already be durably frozen by the caller's lifecycle
// Fence. On success the caller owns the O_RDWR handle; it must pin its identity
// and confirm the required-state Save ACK before handing this SAME descriptor
// to HTTP. This method never writes body bytes, stores a Task, or grants IO.
//
// Existing names are never adopted. On failure after creation the entry is
// retained: no automatic deletion of a file with unconfirmed durable ownership.
func (d *CompletionDirectory) CreateStaging(name string) (*os.File, error) {
	if d == nil || d.handle == nil || !completionName(name) {
		return nil, completionError(CompletionInvalid, nil)
	}
	file, err := d.createStaging(name)
	if err != nil {
		return nil, err
	}
	return finishCreatedStaging(file, d.handle.Sync)
}

// A failed directory Sync cannot grant a usable descriptor or infer that it is
// safe to unlink an entry. Keep the failure branch testable without global hooks.
func finishCreatedStaging(file *os.File, syncDirectory func() error) (*os.File, error) {
	if err := syncDirectory(); err != nil {
		_ = file.Close()
		return nil, completionError(CompletionIO, err)
	}
	return file, nil
}

// OpenStagingWriter reopens ONLY an acknowledged, pinned prepared artifact.
// Sealed/hash/published/committed phases can never reopen a writer. The caller
// must authorize the same run and saved checkpoint outside these file methods.
// It receives a non-truncating O_RDWR handle to the same single-link regular
// inode. Link count >1 is rejected, including a formerly published artifact
// whose older DB snapshot still says prepared. This is not a readonly hash fd.
func (d *CompletionDirectory) OpenStagingWriter(record VerifiedCompletionRecord) (*os.File, error) {
	if d == nil || d.handle == nil || record.Phase != CompletionPrepared ||
		ValidateVerifiedCompletionRecord(record) != nil || !completionName(record.StagingName) ||
		!completionName(record.FinalName) || record.DirectoryIdentity != d.identity ||
		record.StagingIdentity.Inode == 0 || record.StagingIdentity.Device != d.identity.Device {
		return nil, completionError(CompletionIdentity, nil)
	}
	file, err := d.openStagingWriter(record.StagingName)
	if err != nil {
		return nil, err
	}
	identity, err := CompletionIdentityOf(file)
	if err != nil || identity != record.StagingIdentity {
		_ = file.Close()
		return nil, completionError(CompletionIdentity, err)
	}
	return file, nil
}
