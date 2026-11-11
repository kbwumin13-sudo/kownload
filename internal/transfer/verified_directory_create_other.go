//go:build !darwin && !linux

package transfer

func createCompletionDirectory(string) (*CompletionDirectory, error) {
	return nil, completionError(CompletionUnsupported, nil)
}
