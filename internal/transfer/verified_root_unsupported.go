//go:build !darwin || !cgo

package transfer

import "os"

func openVerifiedRootDirectory(string) (*CompletionDirectory, error) {
	return nil, ErrVerifiedRootUnsupported
}
func duplicateVerifiedRootDirectory(*CompletionDirectory) (*CompletionDirectory, error) {
	return nil, ErrVerifiedRootUnsupported
}
func verifyRootHandle(*os.File, CompletionFileIdentity) error { return ErrVerifiedRootUnsupported }
