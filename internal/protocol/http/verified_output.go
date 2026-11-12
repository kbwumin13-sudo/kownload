package http

import (
	"context"
	"fmt"
	"os"
)

// Called only by the required-capability Core path before Resolve. It avoids
// resolve-phase file writes before the managed stage's identity is ACKed.
func (f *Fetcher) RequireVerifiedOutput() { f.verifiedOutputPolicy.Store(true) }

func (f *Fetcher) JoinVerifiedWriters(ctx context.Context) error {
	if !f.verifiedOutputPolicy.Load() {
		return ErrUnsafeResume
	}
	if err := f.CaptureRunQuiescence()(ctx); err != nil {
		return err
	}
	f.wg.Wait()
	return ctx.Err()
}

// Borrow the same pinned descriptor for local checkpoint checks. Callers must
// release before any network probe; legacy callers still own a pathname read.
func (f *Fetcher) openResumeFile() (*os.File, func(), error) {
	f.fileMu.RLock()
	if f.verifiedOutputPolicy.Load() {
		file := f.verifiedOutput
		if file == nil {
			file = f.file
		}
		if file == nil {
			f.fileMu.RUnlock()
			return nil, func() {}, ErrUnsafeResume
		}
		return file, f.fileMu.RUnlock, nil
	}
	f.fileMu.RUnlock()
	file, err := os.Open(f.meta.SingleFilepath())
	return file, func() {
		if file != nil {
			_ = file.Close()
		}
	}, err
}

func (f *Fetcher) localOutputInfo() (os.FileInfo, error) {
	if !f.verifiedOutputPolicy.Load() {
		return os.Stat(f.meta.SingleFilepath())
	}
	file, release, err := f.openResumeFile()
	if err != nil {
		return nil, err
	}
	defer release()
	return file.Stat()
}

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
	if f.mcpOutputRequired.Load() {
		f.mcpOutputSynced.Store(false)
		f.mcpOutputSyncFailure.Store(nil)
	}
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

// A complete restored checkpoint has no download loop to consume or Sync the
// newly bound descriptor. Confirm that exact handle before signaling Done;
// a previous run's Sync cannot cover this binding.
func (f *Fetcher) syncCompletedLocalMCPOutput() error {
	if !f.mcpOutputRequired.Load() {
		return nil
	}
	f.fileMu.Lock()
	if f.file != nil || f.verifiedOutput == nil {
		f.fileMu.Unlock()
		return ErrMCPOutputSync
	}
	file := f.verifiedOutput
	f.file, f.verifiedOutput = file, nil
	f.fileMu.Unlock()
	err := f.syncMCPOutputBeforeClose()
	f.fileMu.Lock()
	_ = file.Close()
	f.file = nil
	f.fileMu.Unlock()
	return err
}
