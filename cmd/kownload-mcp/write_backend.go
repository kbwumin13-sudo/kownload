package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// The only write is this dedicated operation POST. No redirects, arbitrary REST
// paths, legacy create fallback, retries, new operation IDs or background polling.
// The existing transport rechecks private UDS ownership and peer UID per dial.
func (b *backend) writeExchange(ctx context.Context, method, path string, body []byte) (json.RawMessage, string) {
	request, err := http.NewRequestWithContext(ctx, method, "http://kownload.local"+path, bytes.NewReader(body))
	if err != nil {
		return nil, "invalid_arguments"
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Kownload-Write-Version", "1")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if b.apiToken != "" {
		request.Header.Set("X-Api-Token", b.apiToken)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return nil, "app_unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, "app_authentication_failed"
	}
	if response.StatusCode != http.StatusOK {
		return nil, "write_response_unavailable"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBackend+1))
	if err != nil || len(data) > maxBackend {
		return nil, "invalid_app_response"
	}
	var result envelope
	shape := object(schema{"version": schema{"const": 1}, "data": schema{"type": "object"}}, "version", "data")
	if strictWriteJSON(data, shape, &result) != nil {
		return nil, "incompatible_write_api"
	}
	return result.Data, ""
}
func (b *backend) writeCapabilities(ctx context.Context, dataset, task string) (*writeCapability, string) {
	q := url.Values{"datasetId": {dataset}}
	if task != "" {
		q.Set("taskId", task)
	}
	data, code := b.writeExchange(ctx, http.MethodGet, writePrefix+"/capabilities?"+q.Encode(), nil)
	if code != "" {
		return nil, code
	}
	var v writeCapability
	if strictWriteJSON(data, writeCapabilitySchema(), &v) != nil || v.DatasetID != dataset || (task == "" && v.Task != nil) || (task != "" && v.Task != nil && v.Task.TaskID != task) {
		return nil, "invalid_app_response"
	}
	configured := v.PolicyID != nil && v.NetworkPolicyID != nil
	if (v.PolicyID == nil) != (v.NetworkPolicyID == nil) || (!configured && (v.Operations.Create || v.Operations.Pause || v.Operations.Resume || len(v.OutputRootIDs) != 0 || v.Task != nil || v.Reason == nil)) {
		return nil, "invalid_app_response"
	}
	return &v, ""
}
func (b *backend) submitWrite(ctx context.Context, c writeCommand) (*writeReceipt, string) {
	data, err := json.Marshal(c)
	if err != nil || len(data) > maxWriteArguments {
		return nil, "invalid_arguments"
	}
	response, code := b.writeExchange(ctx, http.MethodPost, writePrefix+"/operations", data)
	if code == "app_authentication_failed" {
		return nil, code
	}
	// Even malformed or truncated success bodies may follow a durable commit.
	// Never turn those into a fresh submit recommendation or a claimed rejection.
	if code != "" {
		return unknownWriteReceipt(c), ""
	}
	var v writeReceipt
	if strictWriteJSON(response, writeReceiptSchema(), &v) != nil || !validateWriteReceipt(&v, c) || v.State == "not_recorded" {
		return unknownWriteReceipt(c), ""
	}
	return &v, ""
}
func (b *backend) queryWrite(ctx context.Context, c writeCommand) (*writeReceipt, string) {
	q := url.Values{"datasetId": {c.DatasetID}, "policyId": {c.PolicyID}}
	data, code := b.writeExchange(ctx, http.MethodGet, writePrefix+"/operations/"+url.PathEscape(c.OperationID)+"?"+q.Encode(), nil)
	if code != "" {
		return nil, code
	}
	var v writeReceipt
	if strictWriteJSON(data, writeReceiptSchema(), &v) != nil || !validateWriteReceipt(&v, c) {
		return nil, "invalid_app_response"
	}
	return &v, ""
}
