package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

const scopedBatchNamespace = "task-batch.create/v1"

var scopedDatasetPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type datasetBatchCore interface {
	CreateBatchOperationForDataset(context.Context, string, download.BatchOperationRequest) (download.BatchOperationResult, error)
	QueryBatchOperationForDataset(string, string) (download.BatchOperationResult, error)
	CancelBatchOperationForDataset(string, string) (download.BatchOperationResult, error)
}

type scopedBatchResult struct {
	ScopeVersion int                           `json:"scopeVersion"`
	DatasetID    string                        `json:"datasetId"`
	Namespace    string                        `json:"namespace"`
	Operation    download.BatchOperationResult `json:"operation"`
}

// Dedicated paths fail closed on old servers. Register on the existing auth
// router and bind this server's core, never a mutable global or legacy fallback.
func registerScopedBatchOperationRoutes(router *mux.Router, core datasetBatchCore) {
	const root = "/api/v1/datasets/{datasetId}/task-batches/v1/operations"
	handler := func(w http.ResponseWriter, r *http.Request) {
		dataset, id := mux.Vars(r)["datasetId"], mux.Vars(r)["operationId"]
		if !scopedDatasetPattern.MatchString(dataset) {
			WriteStatusJson(w, http.StatusBadRequest, model.NewErrorResult("invalid_dataset_request"))
			return
		}
		var result download.BatchOperationResult
		var err error
		switch {
		case r.Method == http.MethodGet:
			result, err = core.QueryBatchOperationForDataset(dataset, id)
		case id != "":
			result, err = core.CancelBatchOperationForDataset(dataset, id)
		default:
			var request download.BatchOperationRequest
			decoder := json.NewDecoder(r.Body)
			decoder.UseNumber()
			if err = decoder.Decode(&request); err == nil {
				var extra any
				if decoder.Decode(&extra) != io.EOF {
					err = download.ErrOperationInvalid
				}
			}
			if err != nil {
				writeScopedBatchOperation(w, dataset, id, result, download.ErrOperationInvalid)
				return
			}
			id = request.OperationID
			result, err = core.CreateBatchOperationForDataset(r.Context(), dataset, request)
		}
		writeScopedBatchOperation(w, dataset, id, result, err)
	}
	router.Methods(http.MethodPost).Path(root).HandlerFunc(handler)
	router.Methods(http.MethodGet).Path(root + "/{operationId}").HandlerFunc(handler)
	router.Methods(http.MethodPost).Path(root + "/{operationId}/cancel").HandlerFunc(handler)
}

func writeScopedBatchOperation(w http.ResponseWriter, dataset, id string, result download.BatchOperationResult, err error) {
	status, message := http.StatusServiceUnavailable, "operation_unavailable_query_same_id"
	domainFailed := false
	switch {
	case errors.Is(err, download.ErrDatasetMismatch):
		status, message, domainFailed = http.StatusConflict, "dataset_mismatch", true
	case errors.Is(err, download.ErrDatasetMissing), errors.Is(err, download.ErrDatasetInvalid), errors.Is(err, download.ErrDatasetViewClosed):
		message, domainFailed = "dataset_unavailable", true
	case errors.Is(err, download.ErrDatasetUnsupported):
		status, message, domainFailed = http.StatusNotImplemented, "dataset_identity_storage_unsupported", true
	case errors.Is(err, download.ErrOperationInvalid):
		status, message = http.StatusBadRequest, "invalid_operation_request"
	case errors.Is(err, download.ErrOperationConflict):
		status, message = http.StatusConflict, "operation_conflict"
	case errors.Is(err, download.ErrOperationUnsupported):
		status, message = http.StatusNotImplemented, "atomic_operation_storage_unsupported"
	}
	// ForDataset methods only return a nonempty State from a domain-matched
	// read view. Empty/error-only receipts are not evidence of a matching view.
	trusted := !domainFailed && result.Version == 1 && result.OperationID == id && id != "" &&
		(result.State == "unknown" || result.State == "pending" || result.State == "settled")
	if err == nil && trusted {
		WriteJson(w, model.NewOkResult(scopedBatchResult{1, dataset, scopedBatchNamespace, result}))
		return
	}
	reply := model.NewErrorResult(message)
	if trusted {
		reply.Data = scopedBatchResult{1, dataset, scopedBatchNamespace, result}
	}
	WriteStatusJson(w, status, reply)
}
