package rest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

func BatchOperation(w http.ResponseWriter, r *http.Request) {
	var result download.BatchOperationResult
	var err error
	if r.Method == http.MethodGet {
		result, err = Downloader.QueryBatchOperation(mux.Vars(r)["operationId"])
	} else if id := mux.Vars(r)["operationId"]; id != "" {
		result, err = Downloader.CancelBatchOperation(id)
	} else {
		var req download.BatchOperationRequest
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err = decoder.Decode(&req); err != nil {
			writeBatchOperation(w, result, download.ErrOperationInvalid)
			return
		}
		var extra any
		if err = decoder.Decode(&extra); err != io.EOF {
			writeBatchOperation(w, result, download.ErrOperationInvalid)
			return
		}
		result, err = Downloader.CreateBatchOperation(r.Context(), req)
	}
	writeBatchOperation(w, result, err)
}
func writeBatchOperation(w http.ResponseWriter, result download.BatchOperationResult, err error) {
	if err == nil {
		WriteJson(w, model.NewOkResult(result))
		return
	}
	status := http.StatusServiceUnavailable
	message := "operation_unavailable_query_same_id"
	switch {
	case errors.Is(err, download.ErrOperationInvalid):
		status = http.StatusBadRequest
		message = "invalid_operation_request"
	case errors.Is(err, download.ErrOperationConflict):
		status = http.StatusConflict
		message = "operation_conflict"
	case errors.Is(err, download.ErrOperationUnsupported):
		status = http.StatusNotImplemented
		message = "atomic_operation_storage_unsupported"
	}
	reply := model.NewErrorResult(message)
	if result.State != "" {
		reply.Data = result
	} else if result.OperationID != "" {
		reply.Data = map[string]any{"version": 1, "operationId": result.OperationID, "queryRequired": true}
	}
	WriteStatusJson(w, status, reply)
}
