package nativehost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Request struct {
	Method           string          `json:"method"`
	OperationID      string          `json:"operationId,omitempty"`
	Meta             map[string]any  `json:"meta,omitempty"`
	Params           json.RawMessage `json:"params,omitempty"`
	IdentityProtocol *int            `json:"identityProtocol,omitempty"`
	BridgeBinding    *BridgeBinding  `json:"bridgeBinding,omitempty"`
}
type Submission struct {
	State   string   `json:"state"`
	TaskIDs []string `json:"taskIds"`
}
type Response struct {
	Code         int         `json:"code"`
	Data         any         `json:"data"`
	Message      string      `json:"message"`
	OperationID  string      `json:"operationId,omitempty"`
	HTTPStatus   *int        `json:"httpStatus"`
	BusinessCode *int        `json:"businessCode"`
	Submission   *Submission `json:"submission,omitempty"`
	Error        string      `json:"error,omitempty"`
}
type IPC interface {
	Available(context.Context) bool
	Post(context.Context, string, Request) (int, []byte, error)
}
type Wakeup interface {
	Ensure(context.Context, bool) error
}
type Dispatcher struct {
	Client   IPC
	Wakeup   Wakeup
	Identity *HostIdentity
}

var operationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func failure(id, kind string, status, business *int) Response {
	return Response{Code: 1, Message: "请求未完成（" + kind + "）", OperationID: id, HTTPStatus: status, BusinessCode: business, Error: kind}
}

func (d Dispatcher) Dispatch(ctx context.Context, body []byte) Response {
	var req Request
	if json.Unmarshal(body, &req) != nil || (req.OperationID != "" && !operationID.MatchString(req.OperationID)) {
		return failure("", "invalid_request", nil, nil)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return failure("", "invalid_request", nil, nil)
	}
	if _, present := fields["operationId"]; present && !operationID.MatchString(req.OperationID) {
		return failure("", "invalid_request", nil, nil)
	}
	if v, ok := req.Meta["silent"]; ok {
		if _, ok = v.(bool); !ok {
			return failure(req.OperationID, "invalid_request", nil, nil)
		}
	}
	if kind := d.validateBinding(req); kind != "" {
		return failure(req.OperationID, kind, nil, nil)
	}
	silent, _ := req.Meta["silent"].(bool)
	success := Response{OperationID: req.OperationID, Message: ""}
	switch req.Method {
	case "hostInfo":
		return d.hostInfo(ctx, req)
	case "ping":
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		success.Data = d.Client.Available(probe)
		return success
	case "wakeup":
		wake, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if d.Wakeup.Ensure(wake, silent) != nil {
			return failure(req.OperationID, "wakeup_failed", nil, nil)
		}
		return success
	case "browser":
		var params struct {
			Action   string `json:"action"`
			ClientID string `json:"clientId"`
			Browser  string `json:"browser"`
		}
		if json.Unmarshal(req.Params, &params) != nil || !operationID.MatchString(params.ClientID) || (params.Browser != "chrome" && params.Browser != "edge") {
			return failure(req.OperationID, "invalid_request", nil, nil)
		}
		switch params.Action {
		case "sync", "settings", "submission", "mediaPoll", "mediaResult",
			"handoffCapabilities", "handoffPrepare", "handoffQuery", "handoffCommit", "handoffAbort":
		case "openSettings", "updateSettings", "mediaOpen":
			wake, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := d.Wakeup.Ensure(wake, params.Action == "updateSettings")
			cancel()
			if err != nil {
				return failure(req.OperationID, "wakeup_failed", nil, nil)
			}
		default:
			return failure(req.OperationID, "invalid_request", nil, nil)
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if !d.Client.Available(requestCtx) {
			return failure(req.OperationID, "ipc_unavailable", nil, nil)
		}
		if kind := d.validateBinding(req); kind != "" {
			return failure(req.OperationID, kind, nil, nil)
		}
		status, reply, err := d.Client.Post(requestCtx, "/browser", req)
		if err != nil {
			if status > 0 {
				return failureWithBusiness(req.OperationID, "request_failed", status, nil)
			}
			return failure(req.OperationID, "request_failed", nil, nil)
		}
		return interpret(req, status, reply, false)
	case "create", "forward":
		// The official Gopeed extension sends create params as a base64 JSON
		// string. Normalize this legacy envelope before validating/forwarding;
		// forward requests still require the structured REST envelope.
		if req.Method == "create" {
			var encoded string
			if json.Unmarshal(req.Params, &encoded) == nil {
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || !utf8.Valid(decoded) {
					return failure(req.OperationID, "invalid_request", nil, nil)
				}
				req.Params = decoded
			}
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(req.Params, &object) != nil || object == nil {
			return failure(req.OperationID, "invalid_request", nil, nil)
		}
		creation := req.Method == "create"
		budget := 10 * time.Second
		if req.Method == "forward" {
			var forward struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			}
			if json.Unmarshal(req.Params, &forward) != nil || !validForward(forward.Method, forward.Path) {
				return failure(req.OperationID, "invalid_request", nil, nil)
			}
			creation = forward.Method == "POST" && (strings.TrimSuffix(forward.Path, "/") == "/api/v1/tasks" || strings.TrimSuffix(forward.Path, "/") == "/api/v1/tasks/batch")
			budget = 60 * time.Second
		}
		if req.Method == "create" {
			wake, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := d.Wakeup.Ensure(wake, silent)
			cancel()
			if err != nil {
				return failure(req.OperationID, "wakeup_failed", nil, nil)
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if kind := d.validateBinding(req); kind != "" {
			return failure(req.OperationID, kind, nil, nil)
		}
		status, reply, err := d.Client.Post(requestCtx, "/"+req.Method, req)
		if err != nil {
			if status > 0 {
				if status < 200 || status >= 300 {
					return failureWithBusiness(req.OperationID, "http_error", status, nil)
				}
				return failure(req.OperationID, "request_failed", &status, nil)
			}
			return failure(req.OperationID, "request_failed", nil, nil)
		}
		return interpret(req, status, reply, creation)
	default:
		return failure(req.OperationID, "unknown_method", nil, nil)
	}
}

func validForward(method, path string) bool {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return false
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/api/v1/") || strings.Contains(u.Path, "//") {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return !strings.Contains(u.Path, "\\")
}

func interpret(req Request, status int, body []byte, creation bool) Response {
	var envelope map[string]json.RawMessage
	valid := utf8.Valid(body) && json.Unmarshal(body, &envelope) == nil && envelope != nil
	var code *int
	if valid {
		var value int
		if raw, ok := envelope["code"]; ok && string(raw) != "null" && json.Unmarshal(raw, &value) == nil {
			code = &value
		}
	}
	kind := ""
	switch {
	case status < 200 || status >= 300:
		kind = "http_error"
	case code != nil && *code != 0:
		kind = "business_error"
	case !valid || code == nil:
		kind = "invalid_response"
	}
	if kind != "" {
		return failureWithBusiness(req.OperationID, kind, status, code)
	}
	if raw, ok := envelope["operationId"]; ok {
		var id string
		if json.Unmarshal(raw, &id) != nil || id != req.OperationID {
			return failureWithBusiness(req.OperationID, "invalid_response", status, code)
		}
	} else if req.OperationID != "" {
		return failureWithBusiness(req.OperationID, "invalid_response", status, code)
	}
	var submission *Submission
	if raw, ok := envelope["submission"]; ok {
		var value Submission
		if json.Unmarshal(raw, &value) != nil || value.TaskIDs == nil {
			return failureWithBusiness(req.OperationID, "invalid_response", status, code)
		}
		switch value.State {
		case "accepted":
			if len(value.TaskIDs) != 0 || req.Method != "create" {
				return failureWithBusiness(req.OperationID, "invalid_response", status, code)
			}
		case "created":
			if len(value.TaskIDs) == 0 {
				return failureWithBusiness(req.OperationID, "invalid_response", status, code)
			}
			for _, id := range value.TaskIDs {
				if strings.TrimSpace(id) == "" {
					return failureWithBusiness(req.OperationID, "invalid_response", status, code)
				}
			}
		default:
			return failureWithBusiness(req.OperationID, "invalid_response", status, code)
		}
		submission = &value
	}
	if creation && submission == nil {
		return failureWithBusiness(req.OperationID, "invalid_response", status, code)
	}
	return Response{Data: json.RawMessage(body), Message: "", OperationID: req.OperationID, HTTPStatus: &status, BusinessCode: code, Submission: submission}
}

func failureWithBusiness(id, kind string, status int, code *int) Response {
	result := failure(id, kind, &status, code)
	if code != nil {
		result.Data = map[string]any{"code": *code, "msg": result.Message, "data": nil}
	}
	return result
}
