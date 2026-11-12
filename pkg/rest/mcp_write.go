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
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/gorilla/mux"
)

const mcpWritePrefix = "/api/v1/mcp-write/v1"
const mcpWriteRequestLimit = 16 * 1024
const mcpWriteMaxInteger = uint64(9007199254740991)

var mcpWritePolicyID = regexp.MustCompile(`^mcp-authority-v1:[0-9a-f]{64}$`)
var mcpWriteNetworkID = regexp.MustCompile(`^mcp-network/v1:[0-9a-f]{64}$`)

type mcpWriteSource interface {
	MCPWriteCapabilities(context.Context, download.MCPWriteCapabilityRequest) (download.MCPWriteCapability, error)
	SubmitMCPWrite(context.Context, download.MCPWriteRequest) (download.MCPWriteReceipt, error)
	QueryMCPWrite(context.Context, download.MCPWriteQuery) (download.MCPWriteReceipt, error)
}

// Register only on the captured Unix server, before the Web catch-all and inside
// its existing authentication middleware. No legacy task endpoint is a fallback.
func registerMCPWriteRoutes(router *mux.Router, source mcpWriteSource) {
	router.PathPrefix(mcpWritePrefix).Handler(newMCPWriteHandler(source))
}

func newMCPWriteHandler(source mcpWriteSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || r.URL.RawPath != "" {
			mcpWriteFailure(w, http.StatusBadRequest, download.MCPWriteInvalidRequest)
			return
		}
		switch {
		case r.URL.Path == mcpWritePrefix+"/capabilities":
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				mcpWriteFailure(w, http.StatusMethodNotAllowed, download.MCPWriteInvalidRequest)
				return
			}
			if !mcpWriteQueryFields(query, "datasetId", "taskId") ||
				!scopedDatasetPattern.MatchString(query.Get("datasetId")) ||
				(query.Has("taskId") && !download.MCPReadValidID(query.Get("taskId"))) {
				mcpWriteFailure(w, http.StatusBadRequest, download.MCPWriteInvalidRequest)
				return
			}
			req := download.MCPWriteCapabilityRequest{Version: 1, DatasetID: query.Get("datasetId"), TaskID: query.Get("taskId")}
			capability, err := source.MCPWriteCapabilities(r.Context(), req)
			if err != nil {
				mcpWriteError(w, err)
				return
			}
			if capability.Version != 1 || capability.DatasetID != req.DatasetID ||
				(capability.PolicyID != nil && !mcpWritePolicyID.MatchString(*capability.PolicyID)) ||
				(capability.NetworkPolicyID != nil && !mcpWriteNetworkID.MatchString(*capability.NetworkPolicyID)) ||
				((capability.PolicyID == nil) != (capability.NetworkPolicyID == nil)) ||
				((capability.Operations.Create || capability.Operations.Pause || capability.Operations.Resume) &&
					(capability.PolicyID == nil || !mcpWritePolicyID.MatchString(*capability.PolicyID) || capability.NetworkPolicyID == nil || !mcpWriteNetworkID.MatchString(*capability.NetworkPolicyID))) ||
				(capability.Reason != nil && !capability.Reason.Valid()) ||
				(capability.Task != nil && (capability.Task.TaskID != req.TaskID || !mcpWriteTargetValid(capability.Task.Expected))) {
				mcpWriteFailure(w, http.StatusServiceUnavailable, download.MCPWriteStorageUnavailable)
				return
			}
			if capability.OutputRootIDs == nil {
				capability.OutputRootIDs = []string{}
			}
			mcpWriteSuccess(w, capability)
		case r.URL.Path == mcpWritePrefix+"/operations":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				mcpWriteFailure(w, http.StatusMethodNotAllowed, download.MCPWriteInvalidRequest)
				return
			}
			req, err := readMCPWriteRequest(r)
			if err != nil || len(query) != 0 {
				mcpWriteFailure(w, http.StatusBadRequest, download.MCPWriteInvalidRequest)
				return
			}
			receipt, err := source.SubmitMCPWrite(r.Context(), req)
			if err != nil {
				mcpWriteError(w, err)
				return
			}
			if receipt.State == download.MCPWriteNotRecorded || !mcpWriteReceiptValid(receipt, req.DatasetID, req.PolicyID, req.OperationID) ||
				(receipt.Kind != nil && *receipt.Kind != req.Kind) {
				mcpWriteFailure(w, http.StatusServiceUnavailable, download.MCPWriteStorageUnavailable)
				return
			}
			if req.Action != nil && receipt.Result != nil && receipt.Result.Outcome == download.MCPWriteApplied &&
				(receipt.Result.TaskID == nil || *receipt.Result.TaskID != req.Action.TaskID || receipt.Result.Expected == nil || *receipt.Result.Expected != req.Action.Expected) {
				mcpWriteFailure(w, http.StatusServiceUnavailable, download.MCPWriteStorageUnavailable)
				return
			}
			mcpWriteSuccess(w, receipt)
		case strings.HasPrefix(r.URL.Path, mcpWritePrefix+"/operations/"):
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				mcpWriteFailure(w, http.StatusMethodNotAllowed, download.MCPWriteInvalidRequest)
				return
			}
			op := strings.TrimPrefix(r.URL.Path, mcpWritePrefix+"/operations/")
			if !verifiedOperationID.MatchString(op) || !mcpWriteQueryFields(query, "datasetId", "policyId") ||
				len(query) != 2 || !scopedDatasetPattern.MatchString(query.Get("datasetId")) ||
				!mcpWritePolicyID.MatchString(query.Get("policyId")) {
				mcpWriteFailure(w, http.StatusBadRequest, download.MCPWriteInvalidRequest)
				return
			}
			// P is the original operation scope. A trusted policy reload does not
			// rewrite historical binding; Core authorizes this read. No current
			// capability lookup, attach, restore or execution happens here.
			req := download.MCPWriteQuery{Version: 1, DatasetID: query.Get("datasetId"), PolicyID: query.Get("policyId"), OperationID: op}
			receipt, err := source.QueryMCPWrite(r.Context(), req)
			if err != nil {
				mcpWriteError(w, err)
				return
			}
			if !mcpWriteReceiptValid(receipt, req.DatasetID, req.PolicyID, req.OperationID) {
				mcpWriteFailure(w, http.StatusServiceUnavailable, download.MCPWriteStorageUnavailable)
				return
			}
			mcpWriteSuccess(w, receipt)
		default:
			mcpWriteFailure(w, http.StatusNotFound, download.MCPWriteCapabilityUnavailable)
		}
	})
}

func mcpWriteQueryFields(query url.Values, allowed ...string) bool {
	for key, values := range query {
		if len(values) != 1 {
			return false
		}
		found := false
		for _, candidate := range allowed {
			found = found || key == candidate
		}
		if !found {
			return false
		}
	}
	return true
}

func readMCPWriteRequest(r *http.Request) (download.MCPWriteRequest, error) {
	var request download.MCPWriteRequest
	if r.Body == nil {
		return request, errors.New("invalid_request")
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return request, errors.New("invalid_request")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, mcpWriteRequestLimit+1))
	if err != nil || len(raw) > mcpWriteRequestLimit || !utf8.Valid(raw) || !mcpWriteUniqueJSON(raw) {
		return request, errors.New("invalid_request")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Version != 1 ||
		!scopedDatasetPattern.MatchString(request.DatasetID) || !mcpWritePolicyID.MatchString(request.PolicyID) ||
		!verifiedOperationID.MatchString(request.OperationID) {
		return download.MCPWriteRequest{}, errors.New("invalid_request")
	}
	switch request.Kind {
	case download.MCPWriteCreate:
		if request.Create == nil || request.Action != nil || !download.MCPReadValidID(request.Create.OutputRootID) {
			return download.MCPWriteRequest{}, errors.New("invalid_request")
		}
		name := request.Create.FileName
		target, err := url.Parse(request.Create.URL)
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") ||
			err != nil || target.Host == "" || target.User != nil || target.Opaque != "" ||
			(target.Scheme != "http" && target.Scheme != "https") {
			return download.MCPWriteRequest{}, errors.New("invalid_request")
		}
	case download.MCPWritePause, download.MCPWriteResume:
		if request.Action == nil || request.Create != nil ||
			!download.MCPReadValidID(request.Action.TaskID) || !mcpWriteTargetValid(request.Action.Expected) {
			return download.MCPWriteRequest{}, errors.New("invalid_request")
		}
	default:
		return download.MCPWriteRequest{}, errors.New("invalid_request")
	}
	return request, nil
}

// encoding/json rejects unknown fields but accepts duplicate keys. Reject
// duplicates before typed decode so the caller's canonical intent is singular.
func mcpWriteUniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() bool
	value = func() bool {
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] || !value() {
					return false
				}
				seen[name] = true
			}
			last, err := d.Token()
			return err == nil && last == json.Delim('}')
		case '[':
			for d.More() {
				if !value() {
					return false
				}
			}
			last, err := d.Token()
			return err == nil && last == json.Delim(']')
		default:
			return false
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || !value() {
		return false
	}
	_, err := d.Token()
	return errors.Is(err, io.EOF)
}

func mcpWriteTargetValid(target download.MCPWriteTarget) bool {
	return download.MCPReadValidID(target.Episode) &&
		target.Generation <= mcpWriteMaxInteger && target.SourceRevision <= mcpWriteMaxInteger
}

func mcpWriteReceiptValid(receipt download.MCPWriteReceipt, dataset, policy, operation string) bool {
	if receipt.Version != 1 || receipt.DatasetID != dataset || receipt.PolicyID != policy || receipt.OperationID != operation {
		return false
	}
	switch receipt.State {
	case download.MCPWriteUnknown:
		return receipt.QueryRequired && receipt.Result == nil && (receipt.Kind == nil || mcpWriteKindValid(*receipt.Kind))
	case download.MCPWriteNotRecorded:
		return !receipt.QueryRequired && receipt.Result == nil && receipt.Kind == nil
	case download.MCPWritePending:
		return receipt.QueryRequired && receipt.Result == nil && receipt.Kind != nil && mcpWriteKindValid(*receipt.Kind)
	case download.MCPWriteSettled:
		if receipt.QueryRequired || receipt.Kind == nil || !mcpWriteKindValid(*receipt.Kind) || receipt.Result == nil {
			return false
		}
	default:
		return false
	}
	result := receipt.Result
	if (result.Expected != nil && !mcpWriteTargetValid(*result.Expected)) ||
		(result.Accepted != nil && !mcpWriteTargetValid(*result.Accepted)) {
		return false
	}
	switch result.Outcome {
	case download.MCPWriteCreated:
		return *receipt.Kind == download.MCPWriteCreate && result.TaskID != nil &&
			download.MCPReadValidID(*result.TaskID) && result.ErrorCode == nil && result.Expected == nil && result.Accepted == nil
	case download.MCPWriteApplied:
		return *receipt.Kind != download.MCPWriteCreate && result.TaskID != nil &&
			download.MCPReadValidID(*result.TaskID) && result.ErrorCode == nil && result.Expected != nil && result.Accepted != nil
	case download.MCPWriteRejected:
		return result.ErrorCode != nil && result.ErrorCode.Valid() &&
			(result.TaskID == nil || download.MCPReadValidID(*result.TaskID))
	}
	return false
}

func mcpWriteKindValid(kind download.MCPWriteKind) bool {
	return kind == download.MCPWriteCreate || kind == download.MCPWritePause || kind == download.MCPWriteResume
}

func mcpWriteSuccess(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Version int `json:"version"`
		Data    any `json:"data"`
	}{1, data})
}

func mcpWriteFailure(w http.ResponseWriter, status int, code download.MCPWriteErrorCode) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Version int                        `json:"version"`
		Error   download.MCPWriteErrorCode `json:"error"`
	}{1, code})
}

func mcpWriteError(w http.ResponseWriter, err error) {
	code, status := download.MCPWriteStorageUnavailable, http.StatusServiceUnavailable
	var failure *download.MCPWriteError
	if errors.As(err, &failure) && failure != nil && failure.Code.Valid() {
		code = failure.Code
		switch code {
		case download.MCPWriteInvalidRequest:
			status = http.StatusBadRequest
		case download.MCPWritePermissionDenied, download.MCPWriteNetworkDenied:
			status = http.StatusForbidden
		case download.MCPWriteCapabilityUnavailable:
			status = http.StatusNotImplemented
		case download.MCPWriteTaskUnavailable:
			status = http.StatusNotFound
		case download.MCPWriteDatasetMismatch, download.MCPWritePolicyMismatch, download.MCPWriteOperationConflict, download.MCPWriteStaleTarget, download.MCPWriteOutputConflict:
			status = http.StatusConflict
		case download.MCPWriteExecutionFailed:
			status = http.StatusBadGateway
		case download.MCPWriteCancelled:
			status = http.StatusRequestTimeout
		}
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code, status = download.MCPWriteCancelled, http.StatusRequestTimeout
	}
	mcpWriteFailure(w, status, code)
}
