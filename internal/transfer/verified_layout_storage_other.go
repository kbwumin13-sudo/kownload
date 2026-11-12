//go:build !darwin && !linux

package transfer

import "os"

func openLayoutDirectory(*CompletionDirectory, []string, bool) (*CompletionDirectory, error) {
	return nil, completionError(CompletionUnsupported, nil)
}
func checkLayoutFile(*os.File, CompletionFileIdentity) error {
	return completionError(CompletionUnsupported, nil)
}
