package transfer

import (
	"os"
	"strings"
	"unicode/utf8"
)

// CreateVerifiedMCPOutput creates an empty exclusive stage through a checked
// lease, returning an independently owned writable FILE fd and factual binding.
// No CompletionDirectory escapes its short WithDirectory callback.
//
// The caller must freeze root/names under its durable lifecycle admission before
// this call, then ACK the returned actual staging identity before the first byte.
// This function supplies neither M/Save ACK, network authority nor an IO permit.
// The caller retains lease through writer join, same-fd Sync/Close, readonly seal,
// publication and Fence exit, then closes its own file fds and lease. The adapter
// never closes the caller's lease or registry, creates roots or removes entries.
// There is no ordinary resume writer or recovery authority in this adapter.
func CreateVerifiedMCPOutput(lease *VerifiedRootLease, ownerID string, directory CompletionFileIdentity, stagingName, finalName string) (*os.File, MCPOutputBinding, error) {
	if ownerID == "" || len(ownerID) > 128 || !utf8.ValidString(ownerID) || strings.IndexByte(ownerID, 0) >= 0 ||
		!completionName(stagingName) || !completionName(finalName) || strings.EqualFold(stagingName, finalName) || directory.Inode == 0 {
		return nil, MCPOutputBinding{}, completionError(CompletionInvalid, nil)
	}
	var file *os.File
	var binding MCPOutputBinding
	err := lease.WithDirectory(func(d *CompletionDirectory) error {
		if d.Identity() != directory {
			return completionError(CompletionIdentity, nil)
		}
		var err error
		file, err = d.CreateStaging(stagingName)
		if err != nil {
			return err
		}
		identity, err := CompletionIdentityOf(file)
		if err != nil || identity.Inode == 0 || identity.Device != directory.Device {
			return completionError(CompletionIdentity, err)
		}
		binding = MCPOutputBinding{OwnerID: ownerID, DirectoryIdentity: directory, StagingIdentity: identity, StagingName: stagingName, FinalName: finalName}
		return nil
	})
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		// Creation may already have happened. Keep the entry, grant no usable fd
		// or binding, and never infer rollback or safe cleanup from this error.
		return nil, MCPOutputBinding{}, err
	}
	return file, binding, nil
}

// OpenVerifiedMCPOutput opens the exact acknowledged staging inode read-only via
// the original MCPOutput primitive. The independent file fd is caller-owned.
// It proves neither writer quiescence nor a right to resume downloading. Keep
// lease alive until publication and the caller's later Fence/commit have ended.
func OpenVerifiedMCPOutput(lease *VerifiedRootLease, binding MCPOutputBinding) (*os.File, error) {
	var file *os.File
	err := lease.WithDirectory(func(d *CompletionDirectory) error {
		var err error
		file, err = d.OpenMCPOutput(binding)
		return err
	})
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		return nil, err
	}
	return file, nil
}

// PublishVerifiedMCPOutput borrows the caller's sealed readonly fd and lease.
// It invokes the existing file-Sync/no-replace/directory-Sync publisher without
// changing Binding, Seal, or conflict semantics. Confirmed is only an FS result,
// never Task done, a durable ACK, or permission to deliver effects. Namespace
// exclusion, writer join and the lifecycle Fence still belong to the caller.
func PublishVerifiedMCPOutput(lease *VerifiedRootLease, readonly *os.File, seal MCPOutputSeal) (MCPOutputPublishOutcome, error) {
	outcome := MCPOutputNotAttempted
	err := lease.WithDirectory(func(d *CompletionDirectory) error {
		var err error
		outcome, err = d.PublishMCPOutput(readonly, seal)
		return err
	})
	return verifiedMCPPublishResult(outcome, err)
}

func verifiedMCPPublishResult(outcome MCPOutputPublishOutcome, err error) (MCPOutputPublishOutcome, error) {
	if err != nil && outcome == MCPOutputConfirmed {
		// The original publisher confirmed its FS work, but the outer lease
		// lifecycle rejected the call. Final may exist; do not claim rollback,
		// authorize commit, or overwrite/retry based on this result.
		return MCPOutputUncertain, err
	}
	return outcome, err
}
