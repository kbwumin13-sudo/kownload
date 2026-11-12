package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

type pathOperationCore interface {
	PathCapabilityForDataset(string, string, []string) (download.HTTPPathCapability, error)
	PreviewPathForDataset(context.Context, string, string, download.HTTPPathCreateRequest) (pathpolicy.Preparation, error)
	CreatePathOperationForDataset(context.Context, string, string, download.HTTPPathCreateRequest) (download.HTTPPathOperationResult, error)
	QueryPathOperationForDataset(string, string, string) (download.HTTPPathOperationResult, error)
	CancelPathOperationForDataset(string, string, string) (download.HTTPPathOperationResult, error)
}

type pathRouteVariant struct{ prefix, kind, capability string }

var pathRouteVariants = []pathRouteVariant{
	{"http-path", "http-path-create/v1", "single-http-path-freeze/v1"},
	{"bt-path", "bt-path-create/v1", "bt-path-freeze/v1"},
	{"ed2k-path", "ed2k-path-create/v1", "ed2k-path-freeze/v1"},
	{"netdisk-path", "netdisk-path-create/v1", "netdisk-path-freeze/v1"},
	{"media-path", "media-path-create/v1", "media-path-freeze/v1"},
}

// The route fixes the interpreted variant. A request cannot rename the kind or
// borrow HTTP capability for another protocol. Core verifies the real manager.
func registerPathOperationRoutes(router *mux.Router, core pathOperationCore) {
	for _, variant := range pathRouteVariants {
		root := "/api/v1/datasets/{datasetId}/" + variant.prefix + "/v1"
		h := pathOperationHandler{core, variant}
		router.Methods("GET").Path(root + "/capability").HandlerFunc(h.capability)
		router.Methods("POST").Path(root + "/preview").HandlerFunc(h.preview)
		router.Methods("POST").Path(root + "/operations").HandlerFunc(h.operation)
		router.Methods("GET").Path(root + "/operations/{operationId}").HandlerFunc(h.operation)
		router.Methods("POST").Path(root + "/operations/{operationId}/cancel").HandlerFunc(h.operation)
	}
}

type pathOperationHandler struct {
	core    pathOperationCore
	variant pathRouteVariant
}

func (h pathOperationHandler) domain(w http.ResponseWriter, r *http.Request) (string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	d := mux.Vars(r)["datasetId"]
	if !scopedDatasetPattern.MatchString(d) || r.URL.RawPath != "" {
		pathOperationFailure(w, 400, "invalid_path_request")
		return "", false
	}
	return d, true
}
func (h pathOperationHandler) caps(sha bool) []string {
	caps := []string{h.variant.capability}
	if sha {
		caps = append(caps, verifiedHTTPCapability)
	}
	return caps
}
func (h pathOperationHandler) validCaps(caps []string) bool {
	return reflect.DeepEqual(caps, h.caps(false)) || h.variant.prefix == "http-path" && reflect.DeepEqual(caps, h.caps(true))
}
func (h pathOperationHandler) capability(w http.ResponseWriter, r *http.Request) {
	d, ok := h.domain(w, r)
	if !ok {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !mcpWriteQueryFields(q, "integrity") || len(q) > 0 && (h.variant.prefix != "http-path" || q.Get("integrity") != "sha256") {
		pathOperationFailure(w, 400, "invalid_path_request")
		return
	}
	caps := h.caps(q.Has("integrity"))
	c, err := h.core.PathCapabilityForDataset(d, h.variant.kind, caps)
	if err != nil {
		writePathOperationError(w, err)
		return
	}
	if c.ScopeVersion != 1 || c.DatasetID != d || c.Kind != h.variant.kind || c.RequestVersion != 1 || !reflect.DeepEqual(c.RequiredCapabilities, caps) {
		pathOperationFailure(w, 503, "path_freeze_unconfirmed")
		return
	}
	if c.Supported && (c.Methods.Create != "POST" || c.Methods.Query != "GET" || c.Methods.Cancel != "POST" || c.Methods.Preview != "POST") {
		pathOperationFailure(w, 503, "path_freeze_unconfirmed")
		return
	}
	WriteJson(w, model.NewOkResult(c))
}
func (h pathOperationHandler) preview(w http.ResponseWriter, r *http.Request) {
	d, ok := h.domain(w, r)
	if !ok {
		return
	}
	request, err := h.read(r)
	if err != nil {
		pathOperationFailure(w, 400, "invalid_path_request")
		return
	}
	prepared, err := h.core.PreviewPathForDataset(r.Context(), d, h.variant.kind, request)
	if err != nil {
		writePathOperationError(w, err)
		return
	}
	if pathpolicy.ValidatePreparation(prepared) != nil {
		pathOperationFailure(w, 503, "path_freeze_unconfirmed")
		return
	}
	WriteJson(w, model.NewOkResult(struct {
		Version int `json:"version"`
		pathpolicy.Preparation
	}{1, prepared}))
}
func (h pathOperationHandler) read(r *http.Request) (download.HTTPPathCreateRequest, error) {
	var request download.HTTPPathCreateRequest
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.Body == nil || r.URL.RawQuery != "" {
		return request, download.ErrPathFreezeInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
	if err != nil || len(raw) > 4<<20 || !utf8.Valid(raw) || !mcpWriteUniqueJSON(raw) {
		return request, download.ErrPathFreezeInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Version != 1 || !verifiedOperationID.MatchString(request.OperationID) || !h.validCaps(request.RequiredCapabilities) || request.PathIntent.Version != 1 {
		return request, download.ErrPathFreezeInvalid
	}
	switch request.PathIntent.Origin {
	case pathpolicy.Default, pathpolicy.UserSelected, pathpolicy.ManualCategory, pathpolicy.APIExplicit, pathpolicy.SourceSuggested, pathpolicy.Unknown:
	default:
		return request, download.ErrPathFreezeInvalid
	}
	items := request.Reqs
	if h.variant.prefix == "netdisk-path" {
		if request.Request != nil || len(items) == 0 || !download.MCPReadValidID(request.BatchID) {
			return request, download.ErrPathFreezeInvalid
		}
	} else {
		if request.Request == nil || len(items) != 0 || request.BatchID != "" {
			return request, download.ErrPathFreezeInvalid
		}
		items = []*base.CreateTaskBatchItem{request.Request}
	}
	for _, item := range items {
		if item == nil || item.Req == nil || item.Opts == nil || item.Req.URL == "" || item.Opts.Path != request.PathIntent.Path {
			return request, download.ErrPathFreezeInvalid
		}
		if h.variant.prefix == "http-path" {
			u, e := url.Parse(item.Req.URL)
			if e != nil || u.Host == "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
				return request, download.ErrPathFreezeInvalid
			}
		}
	}
	sha := len(request.RequiredCapabilities) == 2
	if sha {
		i := request.Integrity
		if i == nil || i.Version != 1 || i.RequiredCapability != verifiedHTTPCapability || i.Algorithm != "sha256" || len(i.ExpectedSHA256) != 64 || strings.Trim(i.ExpectedSHA256, "0123456789abcdef") != "" {
			return request, download.ErrPathFreezeInvalid
		}
	} else if request.Integrity != nil {
		return request, download.ErrPathFreezeInvalid
	}
	return request, nil
}

type pathOperationReply struct {
	ScopeVersion         int                           `json:"scopeVersion"`
	DatasetID            string                        `json:"datasetId"`
	Namespace            string                        `json:"namespace"`
	Kind                 string                        `json:"kind"`
	RequiredCapabilities []string                      `json:"requiredCapabilities"`
	Operation            download.BatchOperationResult `json:"operation"`
	CompletionViews      []completionView              `json:"completionViews,omitempty"`
}

func (h pathOperationHandler) operation(w http.ResponseWriter, r *http.Request) {
	d, ok := h.domain(w, r)
	if !ok {
		return
	}
	id := mux.Vars(r)["operationId"]
	var result download.HTTPPathOperationResult
	var err error
	if id == "" {
		var request download.HTTPPathCreateRequest
		request, err = h.read(r)
		id = request.OperationID
		if err == nil {
			result, err = h.core.CreatePathOperationForDataset(r.Context(), d, h.variant.kind, request)
		}
	} else if !verifiedOperationID.MatchString(id) || r.URL.RawQuery != "" {
		err = download.ErrPathFreezeInvalid
	} else if r.Method == "GET" {
		result, err = h.core.QueryPathOperationForDataset(d, h.variant.kind, id)
	} else {
		result, err = h.core.CancelPathOperationForDataset(d, h.variant.kind, id)
	}
	trusted := validPathOperationReceipt(result.Operation, id)
	empty := result.Operation.State == "unknown" && len(result.RequiredCapabilities) == 0
	if !h.validCaps(result.RequiredCapabilities) && !(err == nil && r.Method == "GET" && empty) {
		trusted = false
	}
	var reply any
	if trusted {
		caps := append([]string{}, result.RequiredCapabilities...)
		var views []completionView
		for _, view := range result.CompletionViews {
			views = append(views, adaptCompletionView(view))
		}
		reply = pathOperationReply{1, d, scopedBatchNamespace, h.variant.kind, caps, result.Operation, completionViewsForOperation(views, d, result.Operation)}
	}
	if err == nil && trusted {
		WriteJson(w, model.NewOkResult(reply))
		return
	}
	status, message := pathOperationError(err)
	if err == nil {
		status, message = 503, "path_freeze_unconfirmed"
	}
	failure := model.NewErrorResult(message)
	if trusted && !errors.Is(err, download.ErrDatasetMismatch) && !errors.Is(err, download.ErrDatasetInvalid) && !errors.Is(err, download.ErrDatasetMissing) {
		failure.Data = reply
	}
	WriteStatusJson(w, status, failure)
}
func validPathOperationReceipt(result download.BatchOperationResult, id string) bool {
	if result.Version != 1 || result.OperationID != id || !verifiedOperationID.MatchString(id) {
		return false
	}
	if result.State == "unknown" {
		return len(result.Members) == 0 && len(result.TaskIDs) == 0 && result.CreatedCount == 0 && result.PendingCount == 0 && result.FailedCount == 0
	}
	if (result.State != "pending" && result.State != "settled") || len(result.Members) == 0 {
		return false
	}
	created, pending, failed := 0, 0, 0
	ids := []string{}
	seen := map[string]bool{}
	for index, m := range result.Members {
		if m.Index != index || !download.MCPReadValidID(m.TaskID) || seen[m.TaskID] {
			return false
		}
		seen[m.TaskID] = true
		switch m.State {
		case "created":
			if m.Error != "" {
				return false
			}
			created++
			ids = append(ids, m.TaskID)
		case "pending":
			if m.Error != "" {
				return false
			}
			pending++
		case "failed":
			switch m.Error {
			case "invalid_request", "unsupported_protocol", "invalid_options", "cancelled", "unsupported_option":
			default:
				return false
			}
			failed++
		default:
			return false
		}
	}
	return created == result.CreatedCount && pending == result.PendingCount && failed == result.FailedCount && slices.Equal(ids, result.TaskIDs) && (result.State == "pending") == (pending > 0)
}
func pathOperationFailure(w http.ResponseWriter, status int, code string) {
	WriteStatusJson(w, status, model.NewErrorResult(code))
}
func writePathOperationError(w http.ResponseWriter, err error) {
	status, code := pathOperationError(err)
	pathOperationFailure(w, status, code)
}
func pathOperationError(err error) (int, string) {
	switch {
	case errors.Is(err, download.ErrPathFreezeInvalid), errors.Is(err, download.ErrOperationInvalid):
		return 400, "invalid_path_request"
	case errors.Is(err, download.ErrDatasetMismatch):
		return 409, "dataset_mismatch"
	case errors.Is(err, download.ErrPathFreezeHeld):
		return 409, "path_freeze_held"
	case errors.Is(err, download.ErrPathFreezeConflict), errors.Is(err, download.ErrOperationConflict):
		return 409, "operation_conflict"
	case errors.Is(err, download.ErrDatasetUnsupported), errors.Is(err, download.ErrOperationUnsupported):
		return 501, "path_freeze_unsupported"
	case errors.Is(err, download.ErrDatasetInvalid), errors.Is(err, download.ErrDatasetMissing):
		return 503, "dataset_unavailable"
	}
	return 503, "path_freeze_unconfirmed"
}
