package transfer

import (
	"errors"
	"os"
)

// OpenVerifiedMCPOutputWriter reopens only the acknowledged, single-link stage
// inode. It supplies no persistence or resume permission. Before calling, Core
// must hold the lifecycle/namespace Fence, join the previous writer and confirm
// the CURRENT Task's writable staging phase and same-D binding with known ACK.
// Published, publishing, uncertain and unknown states must never call this seam,
// even if the final pathname happens to be absent. Retain lease until the new
// writer, publication and Fence have all ended.
func OpenVerifiedMCPOutputWriter(lease *VerifiedRootLease, binding MCPOutputBinding) (*os.File, error) {
	var writer *os.File
	err := lease.WithDirectory(func(directory *CompletionDirectory) error {
		if err := directory.checkMCPOutputBinding(binding); err != nil {
			return err
		}
		absent := func() error {
			file, err := directory.openFile(binding.FinalName)
			if file != nil {
				_ = file.Close()
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return completionError(CompletionConflict, nil)
		}
		if err := absent(); err != nil {
			return err
		}
		readonly, err := directory.OpenMCPOutput(binding)
		if err != nil {
			return err
		}
		defer readonly.Close()
		before, err := readonly.Stat()
		if err != nil {
			return err
		}
		writer, err = directory.openStagingWriter(binding.StagingName)
		if err != nil {
			return err
		}
		identity, err := CompletionIdentityOf(writer)
		if err != nil || identity != binding.StagingIdentity {
			return completionError(CompletionIdentity, err)
		}
		after, err := writer.Stat()
		if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return completionError(CompletionIdentity, err)
		}
		return absent()
	})
	if err != nil {
		if writer != nil {
			_ = writer.Close()
		}
		return nil, err
	}
	return writer, nil
}
