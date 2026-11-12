//go:build !darwin && !linux

package transfer

func openPathWorkspaceDirectory(*CompletionDirectory, string, *CompletionFileIdentity) (*CompletionDirectory, error) {
	return nil, ErrLayoutStorageHeld
}
func linkPathWorkspaceFile(*CompletionDirectory, *CompletionDirectory, string) error {
	return ErrLayoutStorageHeld
}
