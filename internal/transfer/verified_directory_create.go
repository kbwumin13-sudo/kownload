package transfer

import (
	"path/filepath"
	"strings"
)

// CreateCompletionDirectory opens or creates an already frozen canonical root,
// pinning every component without following symlinks. It never resolves aliases
// or falls back to MkdirAll. Existing directories keep their permissions; new
// directories use mode 0700 and are synced along with their parent.
//
// The caller must confirm R/M and root intent before calling, hold its run
// Fence, then persist the returned directory identity with a confirmed ACK
// before creating staging. This function cannot authorize an unknown prepared
// recovery, perform that transaction, or grant IO/completion permission.
//
// On error every descriptor opened here is closed. Partially created or
// concurrently existing directories are retained, never deleted or silently
// cleaned up. On success the caller owns the same pinned handle used by the
// staging/publishing methods and must close it.
func CreateCompletionDirectory(root string) (*CompletionDirectory, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.IndexByte(root, 0) >= 0 {
		return nil, completionError(CompletionInvalid, nil)
	}
	return createCompletionDirectory(root)
}
