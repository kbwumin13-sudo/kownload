package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const writePrefix = "/api/v1/mcp-write/v1"
const maxWriteArguments = 4096
const maxJSONInteger = int64(9007199254740991)

// Wire-only copies of the dedicated Core contract. No policy, execution binding,
// filesystem path, headers or free-form backend error can enter a tool result.
type writeTuple struct {
	Episode        string `json:"episode"`
	Generation     int64  `json:"generation"`
	SourceRevision int64  `json:"sourceRevision"`
}
type writeCreate struct {
	URL          string `json:"url"`
	OutputRootID string `json:"outputRootId"`
	FileName     string `json:"fileName"`
}
type writeAction struct {
	TaskID   string     `json:"taskId"`
	Expected writeTuple `json:"expected"`
}
type writeCommand struct {
	Version     int          `json:"version"`
	DatasetID   string       `json:"datasetId"`
	PolicyID    string       `json:"policyId"`
	OperationID string       `json:"operationId"`
	Kind        string       `json:"kind"`
	Create      *writeCreate `json:"create,omitempty"`
	Action      *writeAction `json:"action,omitempty"`
}
type writeOperations struct {
	Create bool `json:"create"`
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
}
type writeCapabilityTask struct {
	TaskID   string     `json:"taskId"`
	Expected writeTuple `json:"expected"`
}
type writeCapability struct {
	Version         int                  `json:"version"`
	DatasetID       string               `json:"datasetId"`
	PolicyID        *string              `json:"policyId"`
	NetworkPolicyID *string              `json:"networkPolicyId"`
	Operations      writeOperations      `json:"operations"`
	OutputRootIDs   []string             `json:"outputRootIds"`
	Task            *writeCapabilityTask `json:"task"`
	Reason          *string              `json:"reason"`
}
type writeOutcome struct {
	TaskID    *string     `json:"taskId"`
	Outcome   string      `json:"outcome"`
	ErrorCode *string     `json:"errorCode"`
	Expected  *writeTuple `json:"expected"`
	Accepted  *writeTuple `json:"accepted"`
}
type writeReceipt struct {
	Version       int           `json:"version"`
	DatasetID     string        `json:"datasetId"`
	PolicyID      string        `json:"policyId"`
	OperationID   string        `json:"operationId"`
	Kind          *string       `json:"kind"`
	State         string        `json:"state"`
	Result        *writeOutcome `json:"result"`
	QueryRequired bool          `json:"queryRequired"`
}
type writeToolArguments struct {
	Mode         string       `json:"mode"`
	DatasetID    string       `json:"datasetId"`
	PolicyID     string       `json:"policyId,omitempty"`
	OperationID  string       `json:"operationId,omitempty"`
	OutputRootID string       `json:"outputRootId,omitempty"`
	TaskID       string       `json:"taskId,omitempty"`
	Create       *writeCreate `json:"create,omitempty"`
	Action       *writeAction `json:"action,omitempty"`
}
type writeToolResult struct {
	Mode       string           `json:"mode"`
	Capability *writeCapability `json:"capability"`
	Receipt    *writeReceipt    `json:"receipt"`
}

var errInvalidWrite = errors.New("invalid write response")

// JSON is rejected before decoding into structs: duplicate keys and case-folded
// aliases must not change the intent hashed by Core. The schema checks exact
// key spelling, required fields, mutually exclusive modes and scalar ranges.
func strictWriteJSON(data []byte, shape schema, dst any) error {
	if len(data) == 0 || !utf8.Valid(data) {
		return errInvalidWrite
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if uniqueWriteJSONValue(d, 0) != nil {
		return errInvalidWrite
	}
	if _, err := d.Token(); err != io.EOF {
		return errInvalidWrite
	}
	var raw any
	if json.Unmarshal(data, &raw) != nil || compileSchema(shape).Validate(raw) != nil {
		return errInvalidWrite
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil {
		return errInvalidWrite
	}
	return nil
}
func uniqueWriteJSONValue(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errInvalidWrite
	}
	token, err := d.Token()
	if err != nil {
		return errInvalidWrite
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return errInvalidWrite
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return errInvalidWrite
			}
			seen[key] = true
			if uniqueWriteJSONValue(d, depth+1) != nil {
				return errInvalidWrite
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errInvalidWrite
		}
	case '[':
		for d.More() {
			if uniqueWriteJSONValue(d, depth+1) != nil {
				return errInvalidWrite
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errInvalidWrite
		}
	default:
		return errInvalidWrite
	}
	return nil
}
func validWriteCreate(v *writeCreate) bool {
	if v == nil || len(v.URL) > 2048 || len(v.FileName) == 0 || len(v.FileName) > 255 || strings.TrimSpace(v.FileName) != v.FileName || v.FileName == "." || v.FileName == ".." || strings.ContainsAny(v.FileName, "/\\:") {
		return false
	}
	for _, text := range []string{v.URL, v.FileName} {
		for _, r := range text {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	u, err := url.Parse(v.URL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}
func unknownWriteReceipt(c writeCommand) *writeReceipt {
	return &writeReceipt{Version: 1, DatasetID: c.DatasetID, PolicyID: c.PolicyID, OperationID: c.OperationID, Kind: nil, State: "unknown", QueryRequired: true}
}
func validateWriteReceipt(v *writeReceipt, c writeCommand) bool {
	if v.Version != 1 || v.DatasetID != c.DatasetID || v.PolicyID != c.PolicyID || v.OperationID != c.OperationID {
		return false
	}
	switch v.State {
	case "unknown":
		return v.QueryRequired && v.Result == nil && (v.Kind == nil || *v.Kind == c.Kind)
	case "not_recorded":
		return !v.QueryRequired && v.Result == nil && v.Kind == nil
	case "pending":
		return v.QueryRequired && v.Result == nil && v.Kind != nil && *v.Kind == c.Kind
	case "settled":
		if v.QueryRequired || v.Result == nil || v.Kind == nil || *v.Kind != c.Kind {
			return false
		}
	default:
		return false
	}
	r := v.Result
	if r.Outcome != "rejected" && r.TaskID == nil {
		return false
	}
	if c.Kind == "create" {
		if r.Expected != nil || r.Accepted != nil || r.Outcome == "applied" {
			return false
		}
	} else {
		if r.Outcome == "created" || (c.Action != nil && r.TaskID != nil && *r.TaskID != c.Action.TaskID) {
			return false
		}
		if c.Action != nil && r.Expected != nil && *r.Expected != c.Action.Expected {
			return false
		}
		if r.Outcome == "applied" && (r.Expected == nil || r.Accepted == nil) {
			return false
		}
	}
	return (r.Outcome == "rejected") == (r.ErrorCode != nil)
}
