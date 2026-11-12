package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

type mcpReceiverFixture struct {
	submits int
	query   download.MCPWriteQuery
}

func (f *mcpReceiverFixture) MCPWriteCapabilities(_ context.Context, r download.MCPWriteCapabilityRequest) (download.MCPWriteCapability, error) {
	return download.MCPWriteCapability{Version: 1, DatasetID: r.DatasetID, OutputRootIDs: []string{}}, nil
}
func (f *mcpReceiverFixture) SubmitMCPWrite(_ context.Context, r download.MCPWriteRequest) (download.MCPWriteReceipt, error) {
	f.submits++
	kind, id := r.Kind, "real_task"
	return download.MCPWriteReceipt{Version: 1, DatasetID: r.DatasetID, PolicyID: r.PolicyID, OperationID: r.OperationID, Kind: &kind, State: download.MCPWriteSettled, Result: &download.MCPWriteResult{Outcome: download.MCPWriteCreated, TaskID: &id}}, nil
}
func (f *mcpReceiverFixture) QueryMCPWrite(_ context.Context, r download.MCPWriteQuery) (download.MCPWriteReceipt, error) {
	f.query = r
	return download.MCPWriteReceipt{Version: 1, DatasetID: r.DatasetID, PolicyID: r.PolicyID, OperationID: r.OperationID, State: download.MCPWriteNotRecorded}, nil
}

func TestMCPWriteReceiverScopeAndCanonicalInput(t *testing.T) {
	f := new(mcpReceiverFixture)
	h := newMCPWriteHandler(f)
	dataset, policy := strings.Repeat("a", 32), "mcp-authority-v1:"+strings.Repeat("b", 64)
	body := `{"version":1,"datasetId":"` + dataset + `","policyId":"` + policy + `","operationId":"create_1","kind":"create","create":{"url":"https://example.com/file","outputRootId":"root","fileName":"one.bin"}}`
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, mcpWritePrefix+"/operations", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(body); w.Code != 200 || f.submits != 1 {
		t.Fatalf("fixed real policy ID refused: %d %s", w.Code, w.Body.String())
	}
	if w := post(strings.Replace(body, `"version":1`, `"version":1,"version":1`, 1)); w.Code != 400 || f.submits != 1 {
		t.Fatal("duplicate intent reached Core")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, mcpWritePrefix+"/operations/create_1?datasetId="+dataset+"&policyId="+policy, nil))
	if w.Code != 200 || f.query.PolicyID != policy || f.query.OperationID != "create_1" || f.submits != 1 {
		t.Fatal("historical P query changed scope or submitted")
	}
	var reply struct {
		Version int                      `json:"version"`
		Data    download.MCPWriteReceipt `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Version != 1 || reply.Data.Kind != nil || reply.Data.Result != nil || reply.Data.QueryRequired || reply.Data.State != download.MCPWriteNotRecorded {
		t.Fatal("not_recorded nullable contract changed")
	}
}

func TestMCPWriteTrustedStartupInput(t *testing.T) {
	var cfg model.StartConfig
	if err := json.Unmarshal([]byte(`{"mcpWritePolicyFile":"/untrusted","MCPWritePolicyFile":"/also-untrusted"}`), &cfg); err != nil || cfg.MCPWritePolicyFile != "" || cfg.MCPWriteHTTPClient != nil {
		t.Fatal("serialized config injected authority")
	}
	for _, bad := range [][2]string{{"127.0.0.1/8", ""}, {"127.0.0.0/8", "0"}, {"127.0.0.0/8,", "443"}} {
		if _, err := mcpWriteLaunchNetwork(bad[0], bad[1]); err == nil {
			t.Fatal("invalid launch network accepted")
		}
	}
	if _, err := mcpWriteLaunchNetwork("127.0.0.1/32", "443,60875"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOWNLOAD_MCP_WRITE_POLICY_FILE", "")
	t.Setenv("KOWNLOAD_MCP_WRITE_ALLOWED_CIDRS", "")
	t.Setenv("KOWNLOAD_MCP_WRITE_ALLOWED_PORTS", "")
	if err := configureMCPWriteStartup(nil, &model.StartConfig{}); err != nil {
		t.Fatal("default write-off startup changed")
	}
	if err := configureMCPWriteStartup(nil, &model.StartConfig{Network: "tcp", MCPWritePolicyFile: "/not-opened"}); err == nil {
		t.Fatal("TCP accepted write startup")
	}
}
