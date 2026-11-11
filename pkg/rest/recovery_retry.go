package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

var recoveryTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type recoveryRetryCore interface {
	QueryRecoveryForDataset(string, string) (download.RecoveryTaskState, error)
	QueryRecoveryActionForDataset(string, string, download.RecoveryRetryRequest) (download.RecoveryTaskState, error)
	RetryRecoveryForDataset(context.Context, string, string, download.RecoveryRetryRequest) (download.RecoveryTaskState, error)
}

type recoveryReply struct {
	ScopeVersion int                        `json:"scopeVersion"`
	DatasetID    string                     `json:"datasetId"`
	Recovery     download.RecoveryTaskState `json:"recovery"`
}

// Register only on the existing authenticated router. MCP's read/resume
// allowlist is deliberately unrelated to this explicit reset action.
func registerRecoveryRetryRoutes(router *mux.Router, core recoveryRetryCore) {
	const root = "/api/v1/datasets/{datasetId}/tasks/{taskId}/recovery/v1"
	handler := func(w http.ResponseWriter, r *http.Request) {
		dataset, id := mux.Vars(r)["datasetId"], mux.Vars(r)["taskId"]
		if !scopedDatasetPattern.MatchString(dataset) || !recoveryTaskID.MatchString(id) {
			WriteStatusJson(w, 400, model.NewErrorResult("invalid_explicit_retry"))
			return
		}
		var state download.RecoveryTaskState
		var err error
		if r.Method == http.MethodGet {
			query := r.URL.Query()
			if len(query) == 0 {
				state, err = core.QueryRecoveryForDataset(dataset, id)
			} else {
				if len(query) != 3 || len(query["actionId"]) != 1 || len(query["expectedEpisode"]) != 1 || len(query["expectedSourceRevision"]) != 1 {
					WriteStatusJson(w, 400, model.NewErrorResult("invalid_explicit_retry"))
					return
				}
				revision, parseErr := strconv.ParseUint(query.Get("expectedSourceRevision"), 10, 64)
				if parseErr != nil || strconv.FormatUint(revision, 10) != query.Get("expectedSourceRevision") {
					WriteStatusJson(w, 400, model.NewErrorResult("invalid_explicit_retry"))
					return
				}
				request := download.RecoveryRetryRequest{Version: 1, ActionID: query.Get("actionId"), ExpectedEpisode: query.Get("expectedEpisode"), ExpectedSourceRevision: revision, Decision: "retry"}
				if !download.ValidRecoveryRetryRequest(request) {
					WriteStatusJson(w, 400, model.NewErrorResult("invalid_explicit_retry"))
					return
				}
				state, err = core.QueryRecoveryActionForDataset(dataset, id, request)
			}
		} else {
			if r.URL.RawQuery != "" {
				WriteStatusJson(w, 400, model.NewErrorResult("invalid_explicit_retry"))
				return
			}
			var request download.RecoveryRetryRequest
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
			decoder.DisallowUnknownFields()
			decodeErr := decoder.Decode(&request)
			if decodeErr == nil {
				var extra any
				if decoder.Decode(&extra) != io.EOF {
					decodeErr = download.ErrRecoveryRetryInvalid
				}
			}
			if decodeErr != nil || !download.ValidRecoveryRetryRequest(request) {
				WriteStatusJson(w, 400, model.NewErrorResult("invalid_explicit_retry"))
				return
			}
			state, err = core.RetryRecoveryForDataset(r.Context(), dataset, id, request)
		}
		if err != nil {
			status, message := 503, "explicit_retry_unavailable_query_same_action"
			switch {
			case errors.Is(err, download.ErrRecoveryRetryInvalid):
				status, message = 400, "invalid_explicit_retry"
			case errors.Is(err, download.ErrRecoveryRetryConflict):
				status, message = 409, "explicit_retry_conflict"
			case errors.Is(err, download.ErrRecoveryRetryStale):
				status, message = 409, "explicit_retry_stale"
			case errors.Is(err, download.ErrDatasetMismatch):
				status, message = 409, "dataset_mismatch"
			case errors.Is(err, download.ErrTaskNotFound):
				status, message = 404, "task_not_found"
			case errors.Is(err, transfer.ErrRecoveryHeld), errors.Is(err, transfer.ErrRecoveryStale):
				status, message = 409, "recovery_held"
			}
			WriteStatusJson(w, status, model.NewErrorResult(message))
			return
		}
		if state.Version != 1 || state.TaskID != id {
			WriteStatusJson(w, 503, model.NewErrorResult("explicit_retry_unavailable_query_same_action"))
			return
		}
		WriteJson(w, model.NewOkResult(recoveryReply{1, dataset, state}))
	}
	router.Methods(http.MethodGet).Path(root).HandlerFunc(handler)
	router.Methods(http.MethodPost).Path(root + "/retry").HandlerFunc(handler)
}
