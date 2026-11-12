package rest

import (
	"bytes"
	"encoding/json"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"io"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"unicode/utf8"
)

type browserHandoffCore interface {
	BrowserHandoff(download.BrowserHandoffRequest) (download.BrowserHandoffResult, error)
	PendingBrowserHandoffs(string, string) ([]download.BrowserHandoffResult, error)
}

// Task metadata, rather than the volatile REST submission cache, is authoritative.
func BrowserHandoff(w http.ResponseWriter, r *http.Request) {
	browserHandoffHandler(Downloader)(w, r)
}

func browserHandoffHandler(core browserHandoffCore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// This is a carrier version, not an execution capability. The client
			// separately checks the dataset's HTTP path/SHA capability before POST.
			WriteJson(w, model.NewOkResult(map[string]any{"version": 1, "pathCreateVersion": 1}))
			return
		}
		req, typed, ok := readBrowserHandoff(w, r)
		if !ok {
			return
		}
		if req.Action == "query" && req.Pending && req.OperationID == "" {
			items, err := core.PendingBrowserHandoffs(req.ClientID, req.Browser)
			if err != nil {
				WriteJson(w, model.NewErrorResult(err.Error()))
				return
			}
			WriteJson(w, model.NewOkResult(map[string]any{"version": 1, "items": items}))
			return
		}
		result, err := core.BrowserHandoff(req)
		if err != nil {
			if typed {
				WriteJson(w, model.NewErrorResult("handoff_unconfirmed"))
				return
			}
			WriteJson(w, model.NewErrorResult(err.Error()))
			return
		}
		if typed && result.PathCreate != nil {
			p := result.PathCreate
			if p.Version != 1 || p.DatasetID != req.DatasetID || p.Kind != download.HTTPPathCreateKind ||
				!(slices.Equal(p.RequiredCapabilities, []string{download.HTTPPathRequiredCapability}) ||
					slices.Equal(p.RequiredCapabilities, []string{download.HTTPPathRequiredCapability, verifiedHTTPCapability}) ||
					result.State == "unknown" && len(result.TaskIDs) == 0 && p.RequiredCapabilities != nil && len(p.RequiredCapabilities) == 0) {
				WriteJson(w, model.NewErrorResult("handoff_unconfirmed"))
				return
			}
		}
		if typed && req.Action != "query" && result.PathCreate == nil {
			// A legacy receipt may be queried, never acknowledged as a typed create.
			WriteJson(w, model.NewErrorResult("handoff_unconfirmed"))
			return
		}
		if typed && req.Action == "prepare" && result.PathCreate != nil && result.State != "unknown" &&
			!slices.Equal(result.PathCreate.RequiredCapabilities, req.PathCreate.RequiredCapabilities) {
			WriteJson(w, model.NewErrorResult("handoff_unconfirmed"))
			return
		}
		WriteJson(w, model.NewOkResult(result))
	}
}

func readBrowserHandoff(w http.ResponseWriter, r *http.Request) (download.BrowserHandoffRequest, bool, bool) {
	var req download.BrowserHandoffRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
	var fields map[string]json.RawMessage
	if err != nil || len(raw) > 4<<20 || !utf8.Valid(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		WriteJson(w, model.NewErrorResult("invalid_handoff_request"))
		return req, false, false
	}
	_, hasPath := fields["pathCreate"]
	_, hasDataset := fields["datasetId"]
	typed := hasPath || hasDataset
	if !typed {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		ok := ReadJson(r, w, &req)
		return req, false, ok
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	valid := mediaErr == nil && media == "application/json" && mcpWriteUniqueJSON(raw) &&
		decoder.Decode(&req) == nil && scopedDatasetPattern.MatchString(req.DatasetID) &&
		verifiedOperationID.MatchString(req.OperationID) && !(req.Pending && req.OperationID == "")
	if req.Action == "prepare" {
		p := req.PathCreate
		valid = valid && hasPath && p != nil
		if p != nil {
			valid = valid && p.Version == 1 && p.OperationID == req.OperationID && p.Request != nil &&
				len(p.Reqs) == 0 && p.BatchID == "" && p.PathIntent.Version == 1 &&
				reflect.DeepEqual(p.Request.Req, req.Req) && reflect.DeepEqual(p.Request.Opts, req.Opts)
			if p.Request != nil {
				valid = valid && p.Request.Opts != nil && p.Request.Opts.Path == p.PathIntent.Path
			}
			valid = valid && (slices.Equal(p.RequiredCapabilities, []string{download.HTTPPathRequiredCapability}) && p.Integrity == nil ||
				slices.Equal(p.RequiredCapabilities, []string{download.HTTPPathRequiredCapability, verifiedHTTPCapability}) && p.Integrity != nil)
			switch p.PathIntent.Origin {
			case "default", "user_selected", "manual_category", "api_explicit", "source_suggested", "unknown":
			default:
				valid = false
			}
		}
	} else {
		valid = valid && !hasPath && (req.Action == "query" || req.Action == "commit" || req.Action == "abort")
	}
	if !valid {
		WriteJson(w, model.NewErrorResult("invalid_handoff_request"))
		return req, true, false
	}
	return req, true, true
}
