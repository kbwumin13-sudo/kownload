package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/gorilla/mux"
)

const mcpReadPrefix = "/api/v1/mcp/read/v1/tasks"

// RegisterMCPReadRoutes is an explicit integration point; it does not start a
// server or initialize a Downloader. Register only on the existing trusted UDS
// router (startCfg.Network == "unix"), retaining its access-control middleware.
// The version header is compatibility metadata, NOT authentication.
// BuildServer registers this only on its existing unix listener.
func RegisterMCPReadRoutes(router *mux.Router, downloader *download.Downloader) {
	router.PathPrefix("/api/v1/mcp/read/v1/").Handler(newMCPReadHandler(downloader))
}

type mcpReadSource interface {
	MCPReadList(context.Context, int, string) (download.MCPReadPage, error)
	MCPReadGetTask(context.Context, string) (download.MCPReadTask, error)
	MCPReadGetProgress(context.Context, string) (download.MCPReadProgress, error)
	MCPReadGetStats(context.Context, string) (download.MCPReadStats, error)
	MCPReadGetErrors(context.Context, string) (download.MCPReadErrors, error)
}

func newMCPReadHandler(source mcpReadSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			mcpReadFailure(w, http.StatusMethodNotAllowed, "read_only")
			return
		}
		if values := r.Header.Values("X-Kownload-Read-Version"); len(values) != 1 || values[0] != "1" {
			mcpReadFailure(w, http.StatusConflict, "incompatible_read_api")
			return
		}
		if len(r.URL.RawQuery) > 1024 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			mcpReadFailure(w, 400, "invalid_arguments")
			return
		}
		if err := r.Context().Err(); err != nil {
			mcpReadError(w, err)
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			mcpReadFailure(w, 400, "invalid_arguments")
			return
		}
		var result any
		if r.URL.Path == mcpReadPrefix {
			if len(query["limit"]) != 1 || len(query["cursor"]) > 1 || len(query) > 2 {
				mcpReadFailure(w, 400, "invalid_arguments")
				return
			}
			for key := range query {
				if key != "limit" && key != "cursor" {
					mcpReadFailure(w, 400, "invalid_arguments")
					return
				}
			}
			limit, parseErr := strconv.Atoi(query.Get("limit"))
			if parseErr != nil || limit < 1 || limit > download.MCPReadMaxPage || len(query.Get("cursor")) > 128 {
				mcpReadFailure(w, 400, "invalid_arguments")
				return
			}
			result, err = source.MCPReadList(r.Context(), limit, query.Get("cursor"))
		} else {
			if len(query) != 0 || !strings.HasPrefix(r.URL.Path, mcpReadPrefix+"/") {
				mcpReadFailure(w, 404, "not_found")
				return
			}
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, mcpReadPrefix+"/"), "/")
			if len(parts) > 2 || !download.MCPReadValidID(parts[0]) {
				mcpReadFailure(w, 400, "invalid_arguments")
				return
			}
			if len(parts) == 1 {
				result, err = source.MCPReadGetTask(r.Context(), parts[0])
			} else {
				switch parts[1] {
				case "progress":
					result, err = source.MCPReadGetProgress(r.Context(), parts[0])
				case "stats":
					result, err = source.MCPReadGetStats(r.Context(), parts[0])
				case "errors":
					result, err = source.MCPReadGetErrors(r.Context(), parts[0])
				default:
					mcpReadFailure(w, 404, "not_found")
					return
				}
			}
		}
		if err != nil {
			mcpReadError(w, err)
			return
		}
		if err = r.Context().Err(); err != nil {
			mcpReadError(w, err)
			return
		}
		payload, err := json.Marshal(struct {
			Version int `json:"version"`
			Data    any `json:"data"`
		}{1, result})
		if err != nil || len(payload) > 128<<10 {
			mcpReadFailure(w, 503, "snapshot_unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})
}
func mcpReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, download.ErrMCPReadInvalid):
		mcpReadFailure(w, 400, "invalid_arguments")
	case errors.Is(err, download.ErrTaskNotFound):
		mcpReadFailure(w, 404, "task_not_found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		mcpReadFailure(w, 408, "cancelled")
	case errors.Is(err, download.ErrMCPReadBusy):
		mcpReadFailure(w, 503, "snapshot_busy")
	default:
		mcpReadFailure(w, 503, "snapshot_unavailable")
	}
}
func mcpReadFailure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Only literal internal codes reach this function, never error.Error().
	_ = json.NewEncoder(w).Encode(struct {
		Version int    `json:"version"`
		Error   string `json:"error"`
	}{1, code})
}
