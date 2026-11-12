package http

import "errors"

var ErrMCPOutputSync = errors.New("mcp_output_sync_failed")

type mcpOutputSyncError struct{ err error }

// RequireMCPOutput is called by Core before Resolve, after strict MCP binding
// validation. It disables pathname/prefetch writes and requires the pinned
// descriptor to Sync before this protocol closes it. It grants no write right.
func (f *Fetcher) RequireMCPOutput() {
	f.RequireVerifiedOutput()
	f.mcpOutputRequired.Store(true)
}

// Caller is the loop's joined finalization or Pause after all writers exited.
// No Core owner/Task/main lock is acquired here.
func (f *Fetcher) syncMCPOutputBeforeClose() error {
	if !f.mcpOutputRequired.Load() {
		return nil
	}
	if failed := f.mcpOutputSyncFailure.Load(); failed != nil {
		return failed.err
	}
	f.fileMu.Lock()
	defer f.fileMu.Unlock()
	if f.file == nil {
		if f.mcpOutputSynced.Load() {
			return nil
		}
		// Resolve may have been cancelled before there was a target fd. No
		// bytes need syncing; the separate publication result remains false.
		return nil
	}
	if err := f.file.Sync(); err != nil {
		f.mcpOutputSyncFailure.Store(&mcpOutputSyncError{ErrMCPOutputSync})
		return ErrMCPOutputSync
	}
	f.mcpOutputSynced.Store(true)
	return nil
}

// MCPOutputSyncResult is read only after the captured run and all writers joined.
// It is a protocol-local fact, not a persistent output phase or IO permission.
func (f *Fetcher) MCPOutputSyncResult() error {
	if !f.mcpOutputRequired.Load() || !f.mcpOutputSynced.Load() {
		return ErrMCPOutputSync
	}
	if failed := f.mcpOutputSyncFailure.Load(); failed != nil {
		return failed.err
	}
	return nil
}
