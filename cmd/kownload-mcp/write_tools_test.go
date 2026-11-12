package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var writeTestP = "mcp-authority-v1:" + strings.Repeat("a", 64)
var writeTestN = "mcp-network/v1:" + strings.Repeat("b", 64)

func writeString(v string) *string { return &v }

var writeTestT = writeTuple{Episode: "episode_1", Generation: 2, SourceRevision: 1}

func writeTestPolicy(t *testing.T) *writePolicy {
	t.Helper()
	p, err := parseWritePolicy(policyBytes(t, policyFixture()))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func writeSession(t *testing.T, path string, p *writePolicy) *mcp.ClientSession {
	t.Helper()
	b := newBackend(path, "")
	t.Cleanup(b.close)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServerWithPolicy(b, p).Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "write-tests", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}
func writeTestCap(task string) writeCapability {
	c := writeCapability{Version: 1, DatasetID: policyDataset, PolicyID: writeString(writeTestP), NetworkPolicyID: writeString(writeTestN), Operations: writeOperations{true, true, true}, OutputRootIDs: []string{"downloads"}}
	if task != "" {
		c.Task = &writeCapabilityTask{TaskID: task, Expected: writeTestT}
	}
	return c
}
func writeTestArgs(kind, mode string) map[string]any {
	a := map[string]any{"mode": mode, "datasetId": policyDataset}
	if mode == "inspect" {
		if kind == "create" {
			a["outputRootId"] = "downloads"
		} else {
			a["taskId"] = "task_1"
		}
		return a
	}
	a["policyId"] = writeTestP
	a["operationId"] = "op-" + kind
	if mode == "submit" {
		if kind == "create" {
			a["create"] = writeCreate{URL: "https://example.invalid/sample", OutputRootID: "downloads", FileName: "sample.bin"}
		} else {
			a["action"] = writeAction{TaskID: "task_1", Expected: writeTestT}
		}
	}
	return a
}
func writeTestReceipt(c writeCommand) writeReceipt {
	r := *unknownWriteReceipt(c)
	r.Kind = writeString(c.Kind)
	r.State = "settled"
	r.QueryRequired = false
	r.Result = &writeOutcome{TaskID: writeString("task_1"), Outcome: "created"}
	if c.Kind != "create" {
		r.Result.Outcome = "applied"
		r.Result.Expected = &writeTuple{writeTestT.Episode, writeTestT.Generation, writeTestT.SourceRevision}
		r.Result.Accepted = &writeTuple{writeTestT.Episode, writeTestT.Generation, writeTestT.SourceRevision}
	}
	return r
}
func decodeWriteResult(t *testing.T, r *mcp.CallToolResult) writeToolResult {
	t.Helper()
	if r.IsError || len(r.Content) != 1 {
		t.Fatalf("unexpected tool failure: %+v", r)
	}
	text, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("not JSON text")
	}
	var v writeToolResult
	if err := strictWriteJSON([]byte(text.Text), writeOutputSchema(), &v); err != nil {
		t.Fatal(err, text.Text)
	}
	return v
}
func TestWriteToolsLifecycleAndScope(t *testing.T) {
	var mu sync.Mutex
	var posts, gets int
	receipts := map[string]writeReceipt{}
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("X-Kownload-Write-Version") != "1" {
			t.Error("missing version")
		}
		if r.URL.Path == writePrefix+"/capabilities" {
			gets++
			reply(w, writeTestCap(r.URL.Query().Get("taskId")))
			return
		}
		if r.Method == "POST" && r.URL.Path == writePrefix+"/operations" {
			posts++
			var c writeCommand
			if json.NewDecoder(r.Body).Decode(&c) != nil {
				t.Error("bad body")
			}
			if c.DatasetID != policyDataset || c.PolicyID != writeTestP || c.Version != 1 {
				t.Error("lost intent")
			}
			old, ok := receipts[c.OperationID]
			if !ok {
				old = writeTestReceipt(c)
				receipts[c.OperationID] = old
			}
			reply(w, old)
			return
		}
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, writePrefix+"/operations/") {
			gets++
			if r.URL.Query().Get("datasetId") != policyDataset || r.URL.Query().Get("policyId") != writeTestP {
				t.Error("query identity")
			}
			reply(w, receipts[strings.TrimPrefix(r.URL.Path, writePrefix+"/operations/")])
			return
		}
		t.Error("unexpected route", r.Method, r.URL.Path)
		w.WriteHeader(404)
	})
	s := writeSession(t, path, writeTestPolicy(t))
	listed, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 8 {
		t.Fatal("expected five reads plus three writes", len(listed.Tools))
	}
	names := map[string]bool{}
	for _, v := range listed.Tools {
		names[v.Name] = true
	}
	for _, def := range append(append([]toolDefinition{}, definitions...), writeDefinitions...) {
		if !names[def.name] {
			t.Fatal("tool missing", def.name)
		}
	}
	for _, kind := range []string{"create", "pause", "resume"} {
		tool := "kownload_" + kind + "_task"
		inspected := decodeWriteResult(t, call(t, s, tool, writeTestArgs(kind, "inspect")))
		if inspected.Capability == nil || inspected.Receipt != nil || (inspected.Capability.PolicyID == nil || *inspected.Capability.PolicyID != writeTestP) {
			t.Fatal("bad inspect")
		}
		submitted := decodeWriteResult(t, call(t, s, tool, writeTestArgs(kind, "submit")))
		queried := decodeWriteResult(t, call(t, s, tool, writeTestArgs(kind, "query")))
		if submitted.Receipt == nil || queried.Receipt == nil || submitted.Receipt.State != "settled" || queried.Receipt.State != "settled" || submitted.Receipt.OperationID != queried.Receipt.OperationID {
			t.Fatal("receipt lost")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 3 || gets != 6 {
		t.Fatal("inspect/query mutated or extra requests", posts, gets)
	}
}
func TestWriteToolsRejectBeforeUDS(t *testing.T) {
	var hits atomic.Int32
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) })
	s := writeSession(t, path, writeTestPolicy(t))
	cases := map[string]func(map[string]any){
		"confirmed":    func(a map[string]any) { a["confirmed"] = true },
		"policyObject": func(a map[string]any) { a["policy"] = policyFixture() },
		"wrongDataset": func(a map[string]any) { a["datasetId"] = policyOtherDataset },
		"missingP":     func(a map[string]any) { delete(a, "policyId") },
		"badP":         func(a map[string]any) { a["policyId"] = "yes" },
		"otherRoot":    func(a map[string]any) { v := a["create"].(writeCreate); v.OutputRootID = "other"; a["create"] = v },
		"pathName":     func(a map[string]any) { v := a["create"].(writeCreate); v.FileName = "../private"; a["create"] = v },
		"backslash":    func(a map[string]any) { v := a["create"].(writeCreate); v.FileName = "a\\b"; a["create"] = v },
		"magnet":       func(a map[string]any) { v := a["create"].(writeCreate); v.URL = "magnet:?xt=private"; a["create"] = v },
		"userinfo": func(a map[string]any) {
			v := a["create"].(writeCreate)
			v.URL = "https://user:password@example.invalid/file"
			a["create"] = v
		},
		"dualPayload":   func(a map[string]any) { a["action"] = writeAction{TaskID: "task_1", Expected: writeTestT} },
		"operationPath": func(a map[string]any) { a["operationId"] = "../secret" },
		"effects":       func(a map[string]any) { a["allowedEffects"] = []string{"script"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := writeTestArgs("create", "submit")
			change(a)
			r, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "kownload_create_task", Arguments: a})
			if err == nil && (r == nil || !r.IsError) {
				t.Fatal("invalid intent accepted")
			}
		})
	}
	for _, kind := range []string{"pause", "resume"} {
		a := writeTestArgs(kind, "submit")
		a["action"] = writeAction{TaskID: "unscoped_task", Expected: writeTestT}
		if r := call(t, s, "kownload_"+kind+"_task", a); !r.IsError {
			t.Fatal("unscoped task accepted")
		}
	}
	for _, raw := range []string{
		`{"mode":"query","mode":"submit","datasetId":"` + policyDataset + `"}`,
		`{"mode":"query","datasetId":"` + policyDataset + `","DatasetId":"` + policyDataset + `","policyId":"` + writeTestP + `","operationId":"op"}`,
	} {
		r, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "kownload_create_task", Arguments: json.RawMessage(raw)})
		if err == nil && (r == nil || !r.IsError) {
			t.Fatal("duplicate/case alias accepted")
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid/local denied inputs reached UDS", hits.Load())
	}
}
func TestWriteToolsUnknownACKOnlyQueries(t *testing.T) {
	for _, mode := range []string{"disconnect", "status503", "redirect", "wrongOperation", "extraPrivateField", "duplicate", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			var posts, queries atomic.Int32
			var accepted writeCommand
			var mu sync.Mutex
			path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == "GET" {
					queries.Add(1)
					reply(w, writeTestReceipt(accepted))
					return
				}
				posts.Add(1)
				if json.NewDecoder(r.Body).Decode(&accepted) != nil {
					t.Error("body")
				}
				switch mode {
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				case "status503":
					w.WriteHeader(503)
					io.WriteString(w, "SENTINEL_SECRET raw URL/token")
				case "redirect":
					w.Header().Set("Location", "http://outside.invalid/private")
					w.WriteHeader(307)
				case "wrongOperation":
					r := writeTestReceipt(accepted)
					r.OperationID = "another"
					reply(w, r)
				case "extraPrivateField":
					r := writeTestReceipt(accepted)
					data, _ := json.Marshal(r)
					io.WriteString(w, `{"version":1,"data":`+strings.TrimSuffix(string(data), "}")+`,"path":"SENTINEL_SECRET"}}`)
				case "duplicate":
					io.WriteString(w, `{"version":1,"version":1,"data":{}}`)
				case "oversize":
					io.WriteString(w, strings.Repeat("SENTINEL_SECRET", maxBackend))
				}
			})
			s := writeSession(t, path, writeTestPolicy(t))
			result := decodeWriteResult(t, call(t, s, "kownload_create_task", writeTestArgs("create", "submit")))
			if result.Receipt == nil || result.Receipt.State != "unknown" || !result.Receipt.QueryRequired || result.Receipt.OperationID != "op-create" {
				t.Fatal("uncertain submission lost original query identity", result)
			}
			if posts.Load() != 1 || queries.Load() != 0 {
				t.Fatal("automatic retry/query", posts.Load(), queries.Load())
			}
			result = decodeWriteResult(t, call(t, s, "kownload_create_task", writeTestArgs("create", "query")))
			if result.Receipt.State != "settled" || posts.Load() != 1 || queries.Load() != 1 {
				t.Fatal("query resubmitted")
			}
		})
	}
}
func TestWriteToolsCapabilityAndQueryPrivacy(t *testing.T) {
	for _, mode := range []string{"foreignTask", "foreignD", "foreignP", "unknownCategory", "wrongKind", "privateNetwork", "extraField", "authentication"} {
		t.Run(mode, func(t *testing.T) {
			path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "authentication" {
					w.WriteHeader(401)
					io.WriteString(w, "SENTINEL_SECRET")
					return
				}
				if mode == "privateNetwork" {
					v := writeTestCap("task_1")
					v.NetworkPolicyID = writeString("SENTINEL_SECRET")
					reply(w, v)
					return
				}
				c := writeCommand{Version: 1, DatasetID: policyDataset, PolicyID: writeTestP, OperationID: "op-pause", Kind: "pause"}
				v := writeTestReceipt(c)
				switch mode {
				case "foreignTask":
					v.Result.TaskID = writeString("unscoped_task")
				case "foreignD":
					v.DatasetID = policyOtherDataset
				case "foreignP":
					v.PolicyID = "mcp-authority-v1:" + strings.Repeat("c", 64)
				case "unknownCategory":
					bad := "SENTINEL_SECRET"
					v.Result.ErrorCode = &bad
					v.Result.Outcome = "rejected"
				case "wrongKind":
					v.Kind = writeString("resume")
				case "extraField":
					data, _ := json.Marshal(v)
					fmt.Fprintf(w, `{"version":1,"data":%s,"raw":"SENTINEL_SECRET"}`, data)
					return
				}
				reply(w, v)
			})
			s := writeSession(t, path, writeTestPolicy(t))
			modeArg := "query"
			if mode == "privateNetwork" {
				modeArg = "inspect"
			}
			r := call(t, s, "kownload_pause_task", writeTestArgs("pause", modeArg))
			if !r.IsError {
				t.Fatal("unsafe response accepted")
			}
			encoded, _ := json.Marshal(r)
			if strings.Contains(string(encoded), "SENTINEL_SECRET") {
				t.Fatal("private error leaked")
			}
		})
	}
}
func TestWriteToolsOldPolicyQueryPreservesReference(t *testing.T) {
	oldP := "mcp-authority-v1:" + strings.Repeat("c", 64)
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("policyId") != oldP {
			t.Error("old receipt reference changed")
		}
		reply(w, writeTestReceipt(writeCommand{Version: 1, DatasetID: policyDataset, PolicyID: oldP, OperationID: "op-pause", Kind: "pause"}))
	})
	s := writeSession(t, path, writeTestPolicy(t))
	a := writeTestArgs("pause", "query")
	a["policyId"] = oldP
	if decodeWriteResult(t, call(t, s, "kownload_pause_task", a)).Receipt.PolicyID != oldP {
		t.Fatal("old binding was rewritten")
	}
}

func TestWriteToolsDisabledPolicyAndEmptyScopes(t *testing.T) {
	for _, which := range []string{"operations", "tasks", "roots"} {
		t.Run(which, func(t *testing.T) {
			v := policyFixture()
			switch which {
			case "operations":
				v["operations"] = map[string]any{}
			case "tasks":
				v["datasets"] = []any{map[string]any{"id": policyDataset, "taskIds": []string{}, "outputRoots": []string{"downloads"}}}
			case "roots":
				v["datasets"] = []any{map[string]any{"id": policyDataset, "taskIds": []string{"task_1"}, "outputRoots": []string{}}}
			}
			p, err := parseWritePolicy(policyBytes(t, v))
			if err != nil {
				t.Fatal(err)
			}
			var hits atomic.Int32
			path := fixture(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) })
			s := writeSession(t, path, p)
			for _, kind := range []string{"create", "pause", "resume"} {
				if (which == "tasks" && kind == "create") || (which == "roots" && kind != "create") {
					continue
				}
				for _, mode := range []string{"inspect", "submit", "query"} {
					if r := call(t, s, "kownload_"+kind+"_task", writeTestArgs(kind, mode)); !r.IsError {
						t.Fatal("disabled scope accepted", kind, mode)
					}
				}
			}
			if hits.Load() != 0 {
				t.Fatal("denied scope reached UDS")
			}
		})
	}
}
func TestWriteToolsCancelledSubmitKeepsOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var posts atomic.Int32
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) { posts.Add(1); cancel(); <-r.Context().Done() })
	b := newBackend(path, "")
	defer b.close()
	c := writeCommand{Version: 1, DatasetID: policyDataset, PolicyID: writeTestP, OperationID: "cancel-op", Kind: "create", Create: &writeCreate{URL: "https://example.invalid/f", OutputRootID: "downloads", FileName: "f"}}
	receipt, code := b.submitWrite(ctx, c)
	if code != "" || receipt == nil || receipt.State != "unknown" || !receipt.QueryRequired || receipt.OperationID != c.OperationID || posts.Load() != 1 {
		t.Fatal("cancel erased operation identity", code, receipt, posts.Load())
	}
}
func TestWriteToolsAPIAuthenticationAndTokenScope(t *testing.T) {
	var hits atomic.Int32
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Api-Token") != "synthetic-private-key" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Kownload-Write-Version") != "1" {
			t.Error("wrong auth/header")
		}
		w.WriteHeader(401)
		io.WriteString(w, "synthetic-private-key raw secret")
	})
	b := newBackend(path, "synthetic-private-key")
	defer b.close()
	c := writeCommand{Version: 1, DatasetID: policyDataset, PolicyID: writeTestP, OperationID: "op", Kind: "create", Create: &writeCreate{URL: "https://example.invalid/f", OutputRootID: "downloads", FileName: "f"}}
	if _, code := b.writeCapabilities(context.Background(), policyDataset, ""); code != "app_authentication_failed" {
		t.Fatal(code)
	}
	if _, code := b.submitWrite(context.Background(), c); code != "app_authentication_failed" {
		t.Fatal(code)
	}
	if _, code := b.queryWrite(context.Background(), c); code != "app_authentication_failed" {
		t.Fatal(code)
	}
	if hits.Load() != 3 {
		t.Fatal("extra requests", hits.Load())
	}
}

func TestWriteToolsNullableReceiptStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*writeReceipt)
		valid  bool
	}{
		{"unknown", func(r *writeReceipt) { r.State = "unknown"; r.Kind = nil; r.Result = nil; r.QueryRequired = true }, true},
		{"notRecorded", func(r *writeReceipt) { r.State = "not_recorded"; r.Kind = nil; r.Result = nil; r.QueryRequired = false }, true},
		{"pending", func(r *writeReceipt) { r.State = "pending"; r.Result = nil; r.QueryRequired = true }, true},
		{"rejectedNoTask", func(r *writeReceipt) {
			r.Result.TaskID = nil
			r.Result.Outcome = "rejected"
			r.Result.ErrorCode = writeString("permission_denied")
		}, true},
		{"unknownNoQuery", func(r *writeReceipt) { r.State = "unknown"; r.Kind = nil; r.Result = nil; r.QueryRequired = false }, false},
		{"pendingNoKind", func(r *writeReceipt) { r.State = "pending"; r.Kind = nil; r.Result = nil; r.QueryRequired = true }, false},
		{"notRecordedHasKind", func(r *writeReceipt) { r.State = "not_recorded"; r.Result = nil; r.QueryRequired = false }, false},
		{"createdNoTask", func(r *writeReceipt) { r.Result.TaskID = nil }, false},
		{"createdHasError", func(r *writeReceipt) { r.Result.ErrorCode = writeString("storage_unavailable") }, false},
		{"rejectedNoError", func(r *writeReceipt) { r.Result.Outcome = "rejected" }, false},
		{"settledStillQuery", func(r *writeReceipt) { r.QueryRequired = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := writeCommand{Version: 1, DatasetID: policyDataset, PolicyID: writeTestP, OperationID: "op-create", Kind: "create"}
			r := writeTestReceipt(c)
			tc.change(&r)
			path := fixture(t, func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" {
					t.Error("query performed write")
				}
				reply(w, r)
			})
			s := writeSession(t, path, writeTestPolicy(t))
			out := call(t, s, "kownload_create_task", writeTestArgs("create", "query"))
			if tc.valid {
				decoded := decodeWriteResult(t, out)
				if decoded.Receipt.State != r.State {
					t.Fatal("state invented")
				}
			} else if !out.IsError {
				t.Fatal("contradictory receipt accepted")
			}
		})
	}
}
func TestWriteToolsUnconfiguredCapability(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			c := writeCapability{Version: 1, DatasetID: policyDataset, OutputRootIDs: []string{}, Reason: writeString("capability_unavailable")}
			c.Operations.Create = bad
			path := fixture(t, func(w http.ResponseWriter, r *http.Request) { reply(w, c) })
			s := writeSession(t, path, writeTestPolicy(t))
			out := call(t, s, "kownload_create_task", writeTestArgs("create", "inspect"))
			if bad {
				if !out.IsError {
					t.Fatal("nil authority granted create")
				}
				return
			}
			v := decodeWriteResult(t, out)
			if v.Capability.PolicyID != nil || v.Capability.NetworkPolicyID != nil || v.Capability.Operations.Create {
				t.Fatal("missing authority fabricated")
			}
		})
	}
}
