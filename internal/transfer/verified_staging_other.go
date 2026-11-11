//go:build !darwin && !linux

package transfer

import "os"

func (d *CompletionDirectory) createStaging(string) (*os.File, error) {
	return nil, completionError(CompletionUnsupported, nil)
}
func (d *CompletionDirectory) openStagingWriter(string) (*os.File, error) {
	return nil, completionError(CompletionUnsupported, nil)
}
