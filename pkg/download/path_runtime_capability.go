package download

import (
	"github.com/GopeedLab/gopeed/internal/fetcher"
	btProtocol "github.com/GopeedLab/gopeed/internal/protocol/bt"
	ed2kProtocol "github.com/GopeedLab/gopeed/internal/protocol/ed2k"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	mediaProtocol "github.com/GopeedLab/gopeed/internal/protocol/media"
	netdiskProtocol "github.com/GopeedLab/gopeed/internal/protocol/netdisk"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

type pathFreezeRequirement interface { RequirePathFreeze(string) error }

// Exact registered factories plus their compiled strict first-write adapter.
// No Build/Setup/Restore, filesystem access or protocol IO occurs on this read.
// Protocol names alone cannot self-certify a custom manager's implementation.
func (d *Downloader) pathExecutionAvailable(protocol string) bool {
	if d.closed.Load() || d.cfg == nil || !pathFreezePlatformSupported || !transfer.VerifiedCompletionPlatformSupported || !supportsSharedRecoveryDomain(d.storage) { return false }
	d.configSnapshotMu.RLock()
	configured := d.cfg.DownloaderStoreConfig != nil
	d.configSnapshotMu.RUnlock()
	if !configured { return false }
	for _,manager := range d.cfg.FetchManagers {
		var prototype fetcher.Fetcher
		switch protocol {
		case "http":
			if _,ok := manager.(*httpProtocol.FetcherManager);ok { prototype = (*httpProtocol.Fetcher)(nil) }
		case "bt":
			if _,ok := manager.(*btProtocol.FetcherManager);ok { prototype = (*btProtocol.Fetcher)(nil) }
		case "ed2k":
			if _,ok := manager.(*ed2kProtocol.FetcherManager);ok { prototype = (*ed2kProtocol.Fetcher)(nil) }
		case "netdisk":
			if _,ok := manager.(*netdiskProtocol.FetcherManager);ok { prototype = (*netdiskProtocol.Fetcher)(nil) }
		case "media":
			if _,ok := manager.(*mediaProtocol.FetcherManager);ok { prototype = (*mediaProtocol.Fetcher)(nil) }
		default: return false
		}
		if prototype != nil { _,ok := prototype.(pathFreezeRequirement);return ok }
	}
	return false
}

func requireTaskPathFreeze(task *Task) error {
	if task.PathFreeze == nil { return nil }
	required,ok := task.fetcher.(pathFreezeRequirement)
	if !ok { return ErrPathFreezeHeld }
	return required.RequirePathFreeze(task.ID)
}
