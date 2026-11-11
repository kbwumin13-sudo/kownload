package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

type verifiedCoreFixture struct {
	calls     []string
	domain    string
	request   verifiedHTTPRequest
	result    verifiedHTTPResult
	err       error
	supported bool
}

func (f *verifiedCoreFixture) capability(domain string) (bool, error) {
	f.calls, f.domain = append(f.calls, "capability"), domain
	return f.supported, f.err
}
func (f *verifiedCoreFixture) create(_ context.Context, domain string, request verifiedHTTPRequest) (verifiedHTTPResult, error) {
	f.calls, f.domain, f.request = append(f.calls, "create"), domain, request
	return f.result, f.err
}
func (f *verifiedCoreFixture) query(domain, id string) (verifiedHTTPResult, error) {
	f.calls, f.domain = append(f.calls, "query:"+id), domain
	return f.result, f.err
}
func (f *verifiedCoreFixture) cancel(domain, id string) (verifiedHTTPResult, error) {
	f.calls, f.domain = append(f.calls, "cancel:"+id), domain
	return f.result, f.err
}

func verifiedFixtureBody() string {
	return `{"version":1,"requiredCapability":"http-sha256-completion/v1","operationId":"example-op","request":{"req":{"url":"https://example.invalid/file","extra":{"header":{"Cookie":"private-synthetic"},"large":900719925474099312345}},"opts":{"extra":{"autoExtract":null,"autoTorrent":false,"large":900719925474099312346}}},"integrity":{"algorithm":"sha256","expectedSHA256":"` + strings.Repeat("a", 64) + `"}}`
}

func verifiedFixtureCall(t *testing.T, core verifiedHTTPCore, method, path, body string) (int, datasetReply) {
	t.Helper()
	router := mux.NewRouter()
	registerVerifiedHTTPRoutes(router, core)
	return verifiedHandlerCall(t, router, method, path, body, "")
}
func verifiedHandlerCall(t *testing.T, handler http.Handler, method, path, body, token string) (int, datasetReply) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-Api-Token", token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var reply datasetReply
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	return w.Code, reply
}

func TestVerifiedHTTPReceiverVersionRoutingAndExactIntent(t *testing.T) {
	domain := strings.Repeat("a", 32)
	root := "/api/v1/datasets/" + domain + "/verified-http/v1"
	for _, action := range []string{"capability", "create", "query", "cancel"} {
		t.Run(action, func(t *testing.T) {
			fixture := &verifiedCoreFixture{supported: true, result: verifiedHTTPResult{DatasetID: domain, Operation: scopeFixtureResult()}}
			method, path, body, call := http.MethodGet, root+"/capability", "", action
			switch action {
			case "create":
				method, path, body = http.MethodPost, root+"/operations", verifiedFixtureBody()
			case "query":
				path, call = root+"/operations/example-op", "query:example-op"
			case "cancel":
				method, path, call = http.MethodPost, root+"/operations/example-op/cancel", "cancel:example-op"
			}
			status, reply := verifiedFixtureCall(t, fixture, method, path, body)
			if status != 200 || reply.Code != 0 || !reflect.DeepEqual(fixture.calls, []string{call}) || fixture.domain != domain {
				t.Fatalf("route: %d %+v %v", status, reply, fixture.calls)
			}
			if action == "capability" {
				var cap verifiedCapabilityResult
				if err := json.Unmarshal(reply.Data, &cap); err != nil || !cap.Supported || cap.ScopeVersion != 1 || cap.RequestVersion != 1 || cap.ProjectionVersion != 1 || cap.DatasetID != domain || cap.Capability != verifiedHTTPCapability || !reflect.DeepEqual(cap.Methods, map[string]string{"create": "POST", "query": "GET", "cancel": "POST"}) {
					t.Fatalf("cap: %s %v", reply.Data, err)
				}
			} else {
				var result verifiedOperationReply
				if err := json.Unmarshal(reply.Data, &result); err != nil || result.ScopeVersion != 1 || result.DatasetID != domain || result.Namespace != scopedBatchNamespace || result.Capability != verifiedHTTPCapability || result.Operation.State != "unknown" {
					t.Fatalf("unknown wrapper: %s %v", reply.Data, err)
				}
			}
			if action == "create" {
				r := fixture.request
				if r.Request.Req.Extra.(map[string]any)["large"] != json.Number("900719925474099312345") || r.Request.Opts.Extra.(map[string]any)["large"] != json.Number("900719925474099312346") || r.Request.Opts.Extra.(map[string]any)["autoExtract"] != nil || r.Integrity.ExpectedSHA256 != strings.Repeat("a", 64) {
					t.Fatal("decoder changed frozen raw intent")
				}
			}
		})
	}
}

func TestVerifiedHTTPReceiverRejectsBeforeBackend(t *testing.T) {
	root := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/verified-http/v1/operations"
	valid := verifiedFixtureBody()
	cases := []string{"{", valid + "{}", valid + "null", `{}`, "null",
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"requiredCapability":"http-sha256-completion/v1",`, "", 1),
		strings.Replace(valid, `"operationId":"example-op"`, `"operationId":"bad id"`, 1),
		strings.Replace(valid, `"algorithm":"sha256"`, `"algorithm":"SHA256"`, 1),
		strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		strings.Replace(valid, `"https://example.invalid/file"`, `"magnet:?xt=urn:btih:example"`, 1),
		strings.Replace(valid, `"https://example.invalid/file"`, `"http:///no-host"`, 1),
		strings.Replace(valid, `"req":{`, `"rid":"legacy-resolve","req":{`, 1),
		strings.Replace(valid, `"url":`, `"protocol":"media","url":`, 1),
		strings.Replace(valid, `"request":`, `"unknown":true,"request":`, 1)}
	for i, body := range cases {
		fixture := &verifiedCoreFixture{}
		status, reply := verifiedFixtureCall(t, fixture, http.MethodPost, root, body)
		if status != http.StatusBadRequest || reply.Code != 1000 || string(reply.Data) != "null" || len(fixture.calls) != 0 || strings.Contains(reply.Msg, "private-synthetic") {
			t.Fatalf("case %d reached backend: %d %+v %v", i, status, reply, fixture.calls)
		}
	}
	for _, path := range []string{strings.Replace(root, strings.Repeat("a", 32), "bad", 1) + "/example-op", root + "/bad%20id"} {
		fixture := &verifiedCoreFixture{}
		status, _ := verifiedFixtureCall(t, fixture, http.MethodGet, path, "")
		if status != 400 || len(fixture.calls) != 0 {
			t.Fatalf("bad path reached backend: %s %d", path, status)
		}
	}
	// Registration has no route to the legacy writer and does not redirect.
	router := mux.NewRouter()
	fixture := &verifiedCoreFixture{}
	registerVerifiedHTTPRoutes(router, fixture)
	for _, path := range []string{"/api/v1/tasks", "/api/v1/task-batches/operations"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(valid)))
		if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || len(fixture.calls) != 0 {
			t.Fatal("legacy fallback or redirect")
		}
	}
}

func verifiedCreatedReceipt() download.BatchOperationResult {
	return download.BatchOperationResult{Version: 1, OperationID: "example-op", State: "settled", Members: []download.BatchOperationMemberResult{{Index: 0, TaskID: "example-task", State: "created"}}, TaskIDs: []string{"example-task"}, CreatedCount: 1}
}

func TestVerifiedHTTPReceiverPartialAndUntrustedDomain(t *testing.T) {
	domain := strings.Repeat("a", 32)
	path := "/api/v1/datasets/" + domain + "/verified-http/v1/operations/example-op"
	fixture := &verifiedCoreFixture{result: verifiedHTTPResult{DatasetID: domain, Operation: verifiedCreatedReceipt()}, err: errors.New("private-path-and-cookie")}
	status, reply := verifiedFixtureCall(t, fixture, http.MethodGet, path, "")
	if status != 503 || reply.Code != 1000 || reply.Msg != "operation_unavailable_query_same_id" || !bytes.Contains(reply.Data, []byte(`"taskIds":["example-task"]`)) || bytes.Contains(reply.Data, []byte("taskPresent")) || bytes.Contains(reply.Data, []byte("completionViews")) {
		t.Fatalf("partial: %d %+v", status, reply)
	}
	for _, err := range []error{download.ErrDatasetMismatch, download.ErrDatasetMissing, download.ErrDatasetInvalid, download.ErrDatasetViewClosed, download.ErrDatasetUnsupported, errVerifiedHTTPUnsupported} {
		fixture.err = err
		status, reply := verifiedFixtureCall(t, fixture, http.MethodGet, path, "")
		if string(reply.Data) != "null" || reply.Code != 1000 || (errors.Is(err, download.ErrDatasetMismatch) && (status != 409 || reply.Msg != "dataset_mismatch")) {
			t.Fatalf("domain error leaked IDs: %d %+v", status, reply)
		}
	}
	fixture.err, fixture.result.DatasetID = nil, strings.Repeat("b", 32)
	if _, reply := verifiedFixtureCall(t, fixture, http.MethodGet, path, ""); string(reply.Data) != "null" {
		t.Fatal("receipt from another dataset adopted")
	}
	fixture.result.DatasetID = domain
	for _, mutate := range []func(*download.BatchOperationResult){
		func(r *download.BatchOperationResult) { r.OperationID = "other-op" },
		func(r *download.BatchOperationResult) { r.Version = 2 },
		func(r *download.BatchOperationResult) { r.Members[0].Error = "private-path-and-cookie" },
		func(r *download.BatchOperationResult) { r.TaskIDs = []string{"other-task"} },
		func(r *download.BatchOperationResult) { r.CreatedCount = 2 },
	} {
		fixture.result.Operation = verifiedCreatedReceipt()
		mutate(&fixture.result.Operation)
		if status, reply := verifiedFixtureCall(t, fixture, http.MethodGet, path, ""); status != 503 || string(reply.Data) != "null" {
			t.Fatalf("malformed receipt adopted: %d %+v", status, reply)
		}
	}
	// Known failed creation receipts remain historical, without inventing a
	// completion phase or treating cancellation as proof that POST never ran.
	failed := download.BatchOperationResult{Version: 1, OperationID: "example-op", State: "settled", Members: []download.BatchOperationMemberResult{{Index: 0, TaskID: "example-task", State: "failed", Error: "cancelled"}}, TaskIDs: []string{}, FailedCount: 1}
	fixture.result.Operation, fixture.err = failed, nil
	if status, reply := verifiedFixtureCall(t, fixture, http.MethodGet, path, ""); status != 200 || !bytes.Contains(reply.Data, []byte(`"error":"cancelled"`)) || bytes.Contains(reply.Data, []byte("completionViews")) {
		t.Fatalf("failed receipt changed: %d %s", status, reply.Data)
	}
	phase, cid, match := "verified", "example-completion", true
	fixture.result.Operation = verifiedCreatedReceipt()
	fixture.result.Views = []completionView{{Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: "example-task", CompletionID: &cid, Available: true, Phase: &phase, RecordedMatch: &match}}
	if status, reply := verifiedFixtureCall(t, fixture, http.MethodGet, path, ""); status != 200 || !bytes.Contains(reply.Data, []byte(`"recordedMatch":true`)) || !bytes.Contains(reply.Data, []byte(`"completionId":"example-completion"`)) {
		t.Fatalf("authoritative view not wrapped: %d %s", status, reply.Data)
	}
}

func TestVerifiedHTTPBuildServerUnsupportedReadOnlyAndAuth(t *testing.T) {
	for _, kind := range []model.Storage{model.StorageMem, model.StorageBolt} {
		t.Run(string(kind), func(t *testing.T) {
			server, endpoint, core, dir := datasetServer(t, kind, "synthetic-verified-token")
			identity, err := core.DatasetIdentity()
			if err != nil {
				t.Fatal(err)
			}
			root := "/api/v1/datasets/" + identity.DatasetID + "/verified-http/v1"
			var hits atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				_, _ = io.WriteString(w, "must-not-resolve")
			}))
			defer source.Close()
			body := strings.Replace(verifiedFixtureBody(), "https://example.invalid/file", source.URL, 1)
			_, configBefore := datasetGET(t, endpoint+"/api/v1/config", "synthetic-verified-token")
			catalogBefore, err := core.ListBatchOperationCatalog(download.BatchCatalogQuery{})
			if err != nil {
				t.Fatal(err)
			}
			var before []byte
			if kind == model.StorageBolt {
				before, err = os.ReadFile(filepath.Join(dir, "gopeed.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, action := range []struct{ method, path, body string }{
				{http.MethodGet, root + "/capability", ""},
				{http.MethodPost, root + "/operations", body},
				{http.MethodGet, root + "/operations/example-op", ""},
				{http.MethodPost, root + "/operations/example-op/cancel", ""},
			} {
				for _, token := range []string{"", "wrong-synthetic-token"} {
					if status, _ := verifiedHandlerCall(t, server.Handler, action.method, action.path, action.body, token); status != 401 {
						t.Fatal("auth bypass", status)
					}
				}
				status, reply := verifiedHandlerCall(t, server.Handler, action.method, action.path, action.body, "synthetic-verified-token")
				if strings.HasSuffix(action.path, "/capability") {
					var cap verifiedCapabilityResult
					if err := json.Unmarshal(reply.Data, &cap); err != nil || status != 200 || cap.Supported || cap.Capability != verifiedHTTPCapability || cap.DatasetID != identity.DatasetID {
						t.Fatalf("unsupported capability: %d %s", status, reply.Data)
					}
				} else if status != 501 || reply.Msg != "verified_http_unsupported" || string(reply.Data) != "null" {
					t.Fatalf("unsupported path: %d %+v", status, reply)
				}
				wrong := strings.Repeat("0", 32)
				if wrong == identity.DatasetID {
					wrong = strings.Repeat("1", 32)
				}
				status, reply = verifiedHandlerCall(t, server.Handler, action.method, strings.Replace(action.path, identity.DatasetID, wrong, 1), action.body, "synthetic-verified-token")
				if status != 409 || reply.Msg != "dataset_mismatch" || string(reply.Data) != "null" {
					t.Fatalf("mismatch: %d %+v", status, reply)
				}
			}
			// An earlier server keeps its owner even after another BuildServer
			// changes the legacy package global. No later domain is mislabeled.
			_, _, other, _ := datasetServer(t, model.StorageMem, "")
			otherID, err := other.DatasetIdentity()
			if err != nil || otherID.DatasetID == identity.DatasetID {
				t.Fatal("fixture domain", err)
			}
			if status, reply := verifiedHandlerCall(t, server.Handler, http.MethodGet, root+"/capability", "", "synthetic-verified-token"); status != 200 || !bytes.Contains(reply.Data, []byte(identity.DatasetID)) {
				t.Fatalf("owner changed: %d %s", status, reply.Data)
			}
			Downloader = core // Legacy config route below uses its original owner.
			_, configAfter := datasetGET(t, endpoint+"/api/v1/config", "synthetic-verified-token")
			catalogAfter, err := core.ListBatchOperationCatalog(download.BatchCatalogQuery{})
			if err != nil || !reflect.DeepEqual(catalogBefore, catalogAfter) || !bytes.Equal(configBefore.Data, configAfter.Data) || len(core.GetTasks()) != 0 || hits.Load() != 0 {
				t.Fatal("unsupported receiver mutated state or performed IO", err)
			}
			if kind == model.StorageBolt {
				after, err := os.ReadFile(filepath.Join(dir, "gopeed.db"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("receiver wrote Bolt", err)
				}
			}
		})
	}
}
