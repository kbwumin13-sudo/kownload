package rest

import (
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/GopeedLab/gopeed/internal/protocol/media"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

const mediaToolPath = "/api/v1/media/tools/v1/status"

// BuildServer registers this only on UDS, before the catch-all and under the
// existing token middleware. The macOS boot path owns a private IPC directory.
func registerMediaToolRoute(router *mux.Router, core *download.Downloader) {
	active := make(chan struct{}, 1)
	router.Methods(http.MethodGet).Path(mediaToolPath).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !privateMediaSocket(r) || r.Header.Get("Origin") != "" {
			WriteStatusJson(w, http.StatusForbidden, model.NewErrorResult("local_media_tools_required"))
			return
		}
		if r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			WriteStatusJson(w, http.StatusBadRequest, model.NewErrorResult("invalid_arguments"))
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			WriteStatusJson(w, http.StatusServiceUnavailable, model.NewErrorResult("media_tools_busy"))
			return
		}
		cfg, err := core.GetConfig()
		if err == nil && cfg != nil {
			var status media.ToolStatus
			status, err = media.CheckTools(r.Context(), cfg.ProtocolConfig["media"])
			if err == nil {
				WriteJson(w, model.NewOkResult(status))
				return
			}
		}
		WriteStatusJson(w, http.StatusServiceUnavailable, model.NewErrorResult("media_tools_unavailable"))
	})
}

func privateMediaSocket(r *http.Request) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(*net.UnixAddr)
	if !ok || !filepath.IsAbs(addr.Name) {
		return false
	}
	parent, err := os.Lstat(filepath.Dir(addr.Name))
	return err == nil && parent.IsDir() && parent.Mode().Perm() == 0700
}
