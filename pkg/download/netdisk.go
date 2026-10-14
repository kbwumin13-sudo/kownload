package download

import (
	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/internal/protocol/netdisk"
)

func (d *Downloader) NetdiskService() *cloud.Service {
	for _, m := range d.cfg.FetchManagers {
		if n, ok := m.(*netdisk.FetcherManager); ok {
			if n.Service == nil {
				n.Build()
			}
			return n.Service
		}
	}
	return nil
}
func (d *Downloader) NetdiskController() *controller.Controller { return d.protocolController("http") }
