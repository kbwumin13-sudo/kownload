package rest

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

// Capture this server's storage owner instead of consulting a later global
// Downloader instance for its dataset label. Authentication remains on router.
func registerDatasetCatalogRoutes(router *mux.Router, core *download.Downloader) {
	router.Methods(http.MethodGet).Path("/api/v1/dataset/identity").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := core.DatasetIdentity()
		if err != nil {
			writeDatasetError(w, err)
			return
		}
		WriteJson(w, model.NewOkResult(identity))
	})
	router.Methods(http.MethodGet).Path("/api/v1/task-batches/operations").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		for _, name := range []string{"filter", "limit", "cursor"} {
			if len(params[name]) > 1 {
				writeDatasetError(w, download.ErrCatalogInvalid)
				return
			}
		}
		query := download.BatchCatalogQuery{Filter: params.Get("filter"), Cursor: params.Get("cursor")}
		if values, provided := params["filter"]; provided && values[0] == "" {
			writeDatasetError(w, download.ErrCatalogInvalid)
			return
		}
		if values, provided := params["limit"]; provided {
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 100 {
				writeDatasetError(w, download.ErrCatalogInvalid)
				return
			}
			query.Limit = limit
		}
		page, err := core.ListBatchOperationCatalog(query)
		if err != nil {
			writeDatasetError(w, err)
			return
		}
		WriteJson(w, model.NewOkResult(page))
	})
}

func writeDatasetError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "dataset_unavailable"
	switch {
	case errors.Is(err, download.ErrDatasetUnsupported):
		status, message = http.StatusNotImplemented, "dataset_identity_storage_unsupported"
	case errors.Is(err, download.ErrCatalogInvalid):
		status, message = http.StatusBadRequest, "invalid_catalog_request"
	case errors.Is(err, download.ErrCatalogCursorDomain):
		status, message = http.StatusConflict, "dataset_cursor_mismatch"
	}
	WriteStatusJson(w, status, model.NewErrorResult(message))
}
