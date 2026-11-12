//go:build darwin || linux

package transfer

import (
	"os"

	"golang.org/x/sys/unix"
)

// DuplicateVerifiedLayoutWriter borrows the exact acknowledged file identity
// for a child HTTP writer. Closing that independent CLOEXEC descriptor cannot
// close the parent's pin. It never opens a path, creates, truncates or changes
// publication state; inactive, read-only and canceled bindings stay held.
func DuplicateVerifiedLayoutWriter(storage *VerifiedLayoutStorage,index int) (*os.File,error) {
	if storage == nil { return nil,ErrLayoutStorageHeld }
	storage.mu.RLock()
	defer storage.mu.RUnlock()
	if storage.readOnly { return nil,ErrLayoutStorageHeld }
	if err := storage.check(index);err != nil { return nil,err }
	fd,err := unix.FcntlInt(storage.files[index].Fd(),unix.F_DUPFD_CLOEXEC,0)
	if err != nil { return nil,completionError(CompletionIdentity,err) }
	file := os.NewFile(uintptr(fd),storage.files[index].Name())
	if err := checkLayoutFile(file,storage.binding.Files[index].Identity);err != nil { _ = file.Close();return nil,err }
	if err := storage.guard();err != nil { _ = file.Close();return nil,err }
	return file,nil
}
