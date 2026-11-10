package rest

import (
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"net/http"
)

// Task metadata, rather than the volatile REST submission cache, is authoritative.
func BrowserHandoff(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		WriteJson(w, model.NewOkResult(map[string]any{"version": 1}))
		return
	}
	var req download.BrowserHandoffRequest
	if !ReadJson(r, w, &req) {
		return
	}
	if req.Action == "query" && req.Pending && req.OperationID == "" {
		items, err := Downloader.PendingBrowserHandoffs(req.ClientID, req.Browser)
		if err != nil {
			WriteJson(w, model.NewErrorResult(err.Error()))
			return
		}
		WriteJson(w, model.NewOkResult(map[string]any{"version": 1, "items": items}))
		return
	}
	result, err := Downloader.BrowserHandoff(req)
	if err != nil {
		WriteJson(w, model.NewErrorResult(err.Error()))
		return
	}
	WriteJson(w, model.NewOkResult(result))
}
