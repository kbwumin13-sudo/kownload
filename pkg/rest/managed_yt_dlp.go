package rest

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/GopeedLab/gopeed/internal/protocol/media"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

const managedYtDlpPath = "/api/v1/media/tools/v1/managed/yt-dlp"

func registerManagedYtDlpRoutes(router *mux.Router, core *download.Downloader, managed media.ManagedYtDlp) {
	handler := func(action func(context.Context, any) (media.ManagedYtDlpState, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authorizeMediaToolsRequest(w, r) {
				return
			}
			cfg, err := core.GetConfig()
			if err != nil || cfg == nil {
				managedYtDlpError(w, err)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			defer cancel()
			result, err := action(ctx, cfg.ProtocolConfig["media"])
			if err != nil {
				managedYtDlpError(w, err)
				return
			}
			WriteJson(w, model.NewOkResult(result))
		}
	}
	router.Methods(http.MethodGet).Path(managedYtDlpPath).HandlerFunc(handler(func(_ context.Context, stored any) (media.ManagedYtDlpState, error) {
		return managed.State(stored)
	}))
	router.Methods(http.MethodPost).Path(managedYtDlpPath + "/update").HandlerFunc(handler(managed.Update))
	router.Methods(http.MethodPost).Path(managedYtDlpPath + "/rollback").HandlerFunc(handler(managed.Rollback))
}

func managedYtDlpError(w http.ResponseWriter, err error) {
	for _, cause := range []error{
		media.ErrManagedYtDlpBusy, media.ErrManagedYtDlpLocation, media.ErrManagedYtDlpPython,
		media.ErrManagedYtDlpDownload, media.ErrManagedYtDlpSignature, media.ErrManagedYtDlpAsset,
		media.ErrManagedYtDlpNoPrevious, media.ErrManagedYtDlpOlder,
	} {
		if errors.Is(err, cause) {
			WriteStatusJson(w, http.StatusConflict, model.NewErrorResult(cause.Error()))
			return
		}
	}
	WriteStatusJson(w, http.StatusServiceUnavailable, model.NewErrorResult("managed_ytdlp_unavailable"))
}
