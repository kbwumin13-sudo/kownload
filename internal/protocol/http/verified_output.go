package http

import (
	"fmt"
	"os"
)

// SetVerifiedOutput transfers an already-pinned writable staging handle from
// Core. It does not create/reopen a path or itself authorize a Task capability.
// Core persists its identity before handing over the first write permission.
func (f *Fetcher) SetVerifiedOutput(file *os.File) error {
	if file == nil {
		return fmt.Errorf("%w: missing verified output", ErrUnsafeResume)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: invalid verified output", ErrUnsafeResume)
	}
	f.fileMu.Lock()
	defer f.fileMu.Unlock()
	if f.file != nil || f.verifiedOutput != nil {
		return fmt.Errorf("%w: output already bound", ErrUnsafeResume)
	}
	f.verifiedOutput = file
	f.verifiedOutputPolicy.Store(true)
	return nil
}

// ReleaseVerifiedOutput only releases a not-yet-started binding. The ordinary
// worker/loop join owns closing a handle already consumed by Start.
func (f *Fetcher) ReleaseVerifiedOutput() {
	f.fileMu.Lock()
	defer f.fileMu.Unlock()
	if f.verifiedOutput != nil {
		_ = f.verifiedOutput.Close()
		f.verifiedOutput = nil
	}
}
