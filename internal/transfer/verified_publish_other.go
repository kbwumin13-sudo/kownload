//go:build !darwin && !linux

package transfer

import "os"

const VerifiedCompletionPlatformSupported = false

// No ordinary Rename or copy-to-final fallback. The capability must remain
// unavailable on platforms without this anchored no-replace implementation.
func openCompletionDirectory(string) (*CompletionDirectory, error) {
	return nil, completionError(CompletionUnsupported, nil)
}
func completionIdentityOf(*os.File) (CompletionFileIdentity, error) {
	return CompletionFileIdentity{}, completionError(CompletionUnsupported, nil)
}
func completionReadOnly(*os.File) error {
	return completionError(CompletionUnsupported, nil)
}
func (d *CompletionDirectory) openFile(string) (*os.File, error) {
	return nil, completionError(CompletionUnsupported, nil)
}
func (d *CompletionDirectory) link(string, string) error {
	return completionError(CompletionUnsupported, nil)
}
