package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

const verifiedHTTPCapability = "http-sha256-completion/v1"

var (
	errVerifiedHTTPUnsupported = errors.New("verified_http_unsupported")
	verifiedOperationID        = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
	verifiedSHA256             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// These are receiver DTOs, not persistent Core intent/record types. The Core
// adapter must freeze typed integrity together with R and its original options.
type verifiedHTTPRequest struct {
	Version            int                       `json:"version"`
	RequiredCapability string                    `json:"requiredCapability"`
	OperationID        string                    `json:"operationId"`
	Request            *base.CreateTaskBatchItem `json:"request"`
	Integrity          struct {
		Algorithm      string `json:"algorithm"`
		ExpectedSHA256 string `json:"expectedSHA256"`
	} `json:"integrity"`
}

type verifiedHTTPResult struct {
	// Must label the same coherent, domain-checked Core snapshot as Operation
	// and Views. Never fill this from an earlier REST identity preflight.
	DatasetID string
	Operation download.BatchOperationResult
	Views     []completionView
}

type verifiedHTTPCore interface {
	capability(string) (bool, error)
	create(context.Context, string, verifiedHTTPRequest) (verifiedHTTPResult, error)
	query(string, string) (verifiedHTTPResult, error)
	cancel(string, string) (verifiedHTTPResult, error)
}

type verifiedCapabilityResult struct {
	ScopeVersion      int               `json:"scopeVersion"`
	DatasetID         string            `json:"datasetId"`
	Capability        string            `json:"capability"`
	RequestVersion    int               `json:"requestVersion"`
	ProjectionVersion int               `json:"projectionVersion"`
	Supported         bool              `json:"supported"`
	Methods           map[string]string `json:"methods"`
}

type verifiedOperationReply struct {
	ScopeVersion    int                           `json:"scopeVersion"`
	DatasetID       string                        `json:"datasetId"`
	Namespace       string                        `json:"namespace"`
	Capability      string                        `json:"capability"`
	Operation       download.BatchOperationResult `json:"operation"`
	CompletionViews []completionView              `json:"completionViews,omitempty"`
}

func registerVerifiedHTTPRoutes(router *mux.Router, core verifiedHTTPCore) {
	const root = "/api/v1/datasets/{datasetId}/verified-http/v1"
	validDomain := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		domain := mux.Vars(r)["datasetId"]
		if !scopedDatasetPattern.MatchString(domain) {
			WriteStatusJson(w, http.StatusBadRequest, model.NewErrorResult("invalid_dataset_request"))
			return "", false
		}
		return domain, true
	}
	router.Methods(http.MethodGet).Path(root + "/capability").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain, valid := validDomain(w, r)
		if !valid {
			return
		}
		supported, err := core.capability(domain)
		if err != nil {
			writeVerifiedOperation(w, domain, "", verifiedHTTPResult{}, err)
			return
		}
		WriteJson(w, model.NewOkResult(verifiedCapabilityResult{1, domain, verifiedHTTPCapability, 1, 1, supported,
			map[string]string{"create": http.MethodPost, "query": http.MethodGet, "cancel": http.MethodPost}}))
	})
	handler := func(w http.ResponseWriter, r *http.Request) {
		domain, valid := validDomain(w, r)
		if !valid {
			return
		}
		id := mux.Vars(r)["operationId"]
		var result verifiedHTTPResult
		var err error
		if id != "" && !verifiedOperationID.MatchString(id) {
			err = download.ErrOperationInvalid
		} else if r.Method == http.MethodGet {
			result, err = core.query(domain, id)
		} else if id != "" {
			result, err = core.cancel(domain, id)
		} else {
			request, decodeErr := readVerifiedHTTPRequest(r)
			id, err = request.OperationID, decodeErr
			if err == nil {
				result, err = core.create(r.Context(), domain, request)
			}
		}
		writeVerifiedOperation(w, domain, id, result, err)
	}
	router.Methods(http.MethodPost).Path(root + "/operations").HandlerFunc(handler)
	router.Methods(http.MethodGet).Path(root + "/operations/{operationId}").HandlerFunc(handler)
	router.Methods(http.MethodPost).Path(root + "/operations/{operationId}/cancel").HandlerFunc(handler)
}

func readVerifiedHTTPRequest(r *http.Request) (request verifiedHTTPRequest, err error) {
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return request, download.ErrOperationInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || request.Version != 1 || request.RequiredCapability != verifiedHTTPCapability ||
		!verifiedOperationID.MatchString(request.OperationID) || request.Integrity.Algorithm != "sha256" ||
		!verifiedSHA256.MatchString(request.Integrity.ExpectedSHA256) || request.Request == nil || request.Request.Req == nil {
		return request, download.ErrOperationInvalid
	}
	req := request.Request.Req
	u, parseErr := url.Parse(req.URL)
	if parseErr != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		(req.Protocol != "" && req.Protocol != "http") {
		return request, download.ErrOperationInvalid
	}
	// Do not resolve, reinterpret opaque Extra or inherit global options here.
	// In particular same-op replay must reuse the original R's effective options.
	return request, nil
}

func writeVerifiedOperation(w http.ResponseWriter, domain, id string, result verifiedHTTPResult, err error) {
	status, message, domainFailed := http.StatusServiceUnavailable, "operation_unavailable_query_same_id", false
	switch {
	case errors.Is(err, download.ErrDatasetMismatch):
		status, message, domainFailed = http.StatusConflict, "dataset_mismatch", true
	case errors.Is(err, download.ErrDatasetMissing), errors.Is(err, download.ErrDatasetInvalid), errors.Is(err, download.ErrDatasetViewClosed):
		message, domainFailed = "dataset_unavailable", true
	case errors.Is(err, download.ErrDatasetUnsupported):
		status, message, domainFailed = http.StatusNotImplemented, "dataset_identity_storage_unsupported", true
	case errors.Is(err, errVerifiedHTTPUnsupported), errors.Is(err, download.ErrVerifiedHTTPUnsupported):
		status, message, domainFailed = http.StatusNotImplemented, "verified_http_unsupported", true
	case errors.Is(err, download.ErrVerifiedHTTPInvalid):
		status, message = http.StatusBadRequest, "invalid_verified_http_request"
	case errors.Is(err, download.ErrVerifiedHTTPUnsupportedOption):
		status, message = http.StatusBadRequest, "unsupported_option"
	case errors.Is(err, download.ErrOperationInvalid):
		status, message = http.StatusBadRequest, "invalid_operation_request"
	case errors.Is(err, download.ErrOperationConflict):
		status, message = http.StatusConflict, "operation_conflict"
	case errors.Is(err, download.ErrOperationUnsupported):
		status, message, domainFailed = http.StatusNotImplemented, "atomic_operation_storage_unsupported", true
	}
	trusted := !domainFailed && result.DatasetID == domain && validVerifiedReceipt(result.Operation, id)
	var data any
	if trusted {
		data = verifiedOperationReply{1, domain, scopedBatchNamespace, verifiedHTTPCapability,
			result.Operation, completionViewsForOperation(result.Views, domain, result.Operation)}
	}
	if err == nil && trusted {
		WriteJson(w, model.NewOkResult(data))
		return
	}
	reply := model.NewErrorResult(message)
	reply.Data = data
	WriteStatusJson(w, status, reply)
}

func validVerifiedReceipt(result download.BatchOperationResult, id string) bool {
	if result.Version != 1 || result.OperationID != id || !verifiedOperationID.MatchString(id) {
		return false
	}
	if result.State == "unknown" {
		return len(result.Members) == 0 && len(result.TaskIDs) == 0 && result.CreatedCount == 0 && result.FailedCount == 0 && result.PendingCount == 0
	}
	if result.State != "pending" && result.State != "settled" || len(result.Members) != 1 {
		return false
	}
	m := result.Members[0]
	if m.Index != 0 || m.TaskID == "" || (m.State != "failed" && m.Error != "") {
		return false
	}
	switch m.State {
	case "created":
		return result.State == "settled" && result.CreatedCount == 1 && result.FailedCount == 0 && result.PendingCount == 0 && len(result.TaskIDs) == 1 && result.TaskIDs[0] == m.TaskID
	case "pending":
		return result.State == "pending" && result.CreatedCount == 0 && result.FailedCount == 0 && result.PendingCount == 1 && len(result.TaskIDs) == 0
	case "failed":
		// Preserve known creation failures; arbitrary record/error text must
		// never expose request/path/auth material through a partial receipt.
		switch m.Error {
		case "invalid_request", "unsupported_protocol", "invalid_options", "cancelled", "unsupported_option":
			return result.State == "settled" && result.CreatedCount == 0 && result.FailedCount == 1 && result.PendingCount == 0 && len(result.TaskIDs) == 0
		}
	}
	return false
}
