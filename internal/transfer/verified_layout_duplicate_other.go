//go:build !darwin && !linux

package transfer

import "os"

func DuplicateVerifiedLayoutWriter(*VerifiedLayoutStorage,int) (*os.File,error) {
	return nil,ErrLayoutStorageHeld
}
