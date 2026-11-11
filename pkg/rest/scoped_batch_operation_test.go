package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/gorilla/mux"
)

type scopedCoreFixture struct {
	calls   []string
	domain  string
	request download.BatchOperationRequest
	result  download.BatchOperationResult
	err     error
}

func (f *scopedCoreFixture) CreateBatchOperationForDataset(_ context.Context, domain string, request download.BatchOperationRequest) (download.BatchOperationResult, error) {
	f.calls = append(f.calls, "create")
	f.domain, f.request = domain, request
	return f.result, f.err
}
func (f *scopedCoreFixture) QueryBatchOperationForDataset(domain, id string) (download.BatchOperationResult, error) {
	f.calls = append(f.calls, "query:"+id)
	f.domain = domain
	return f.result, f.err
}
func (f *scopedCoreFixture) CancelBatchOperationForDataset(domain, id string) (download.BatchOperationResult, error) {
	f.calls = append(f.calls, "cancel:"+id)
	f.domain = domain
	return f.result, f.err
}

func scopeFixtureResult() download.BatchOperationResult {
	return download.BatchOperationResult{Version: 1, OperationID: "example-op", State: "unknown", Members: []download.BatchOperationMemberResult{}, TaskIDs: []string{}}
}

func scopeFixtureCall(t *testing.T, fixture *scopedCoreFixture, method, path, body string) (int, map[string]any) {
	t.Helper()
	router := mux.NewRouter()
	registerScopedBatchOperationRoutes(router, fixture)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var reply map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	return w.Code, reply
}

func TestScopedBatchRESTRoutingAndUnknownCapability(t *testing.T) {
	domain := strings.Repeat("a", 32)
	root := "/api/v1/datasets/" + domain + "/task-batches/v1/operations"
	for _, method := range []string{http.MethodGet, http.MethodPost, "cancel"} {
		fixture := &scopedCoreFixture{result: scopeFixtureResult()}
		path, body, want := root+"/example-op", "", "query:example-op"
		if method == http.MethodPost {
			path, body, want = root, `{"version":1,"operationId":"example-op","reqs":[{"req":{"url":"http://127.0.0.1/file"},"opts":{"extra":{"large":900719925474099312345}}}]}`, "create"
		} else if method == "cancel" {
			method, path, want = http.MethodPost, path+"/cancel", "cancel:example-op"
		}
		status, reply := scopeFixtureCall(t, fixture, method, path, body)
		if status != http.StatusOK || reply["code"] != float64(0) || len(fixture.calls) != 1 || fixture.calls[0] != want || fixture.domain != domain {
			t.Fatalf("routing: %d %+v %+v", status, reply, fixture)
		}
		data := reply["data"].(map[string]any)
		if data["scopeVersion"] != float64(1) || data["datasetId"] != domain || data["namespace"] != scopedBatchNamespace || data["operation"].(map[string]any)["state"] != "unknown" {
			t.Fatalf("unknown capability: %+v", reply)
		}
		if want == "create" && fixture.request.Reqs[0].Opts.Extra.(map[string]any)["large"] != json.Number("900719925474099312345") {
			t.Fatal("REST decoder changed request number")
		}
	}
}

func TestScopedBatchRESTPartialAndMismatchReplies(t *testing.T) {
	domain := strings.Repeat("a", 32)
	path := "/api/v1/datasets/" + domain + "/task-batches/v1/operations/example-op"
	partial := scopeFixtureResult()
	partial.State, partial.CreatedCount, partial.PendingCount = "pending", 1, 1
	partial.TaskIDs = []string{"example-task-a"}
	partial.Members = []download.BatchOperationMemberResult{{Index: 0, TaskID: "example-task-a", State: "created"}, {Index: 1, TaskID: "example-task-b", State: "pending"}}
	status, reply := scopeFixtureCall(t, &scopedCoreFixture{result: partial, err: download.ErrOperationUnavailable}, http.MethodGet, path, "")
	if status != http.StatusServiceUnavailable || reply["code"] != float64(1000) {
		t.Fatalf("partial status: %d %+v", status, reply)
	}
	data := reply["data"].(map[string]any)
	op := data["operation"].(map[string]any)
	if data["datasetId"] != domain || op["pendingCount"] != float64(1) || len(op["members"].([]any)) != 2 {
		t.Fatalf("partial changed: %+v", reply)
	}
	if _, guessed := op["members"].([]any)[0].(map[string]any)["taskPresent"]; guessed {
		t.Fatal("unknown task presence became false")
	}
	for _, err := range []error{download.ErrDatasetMismatch, download.ErrDatasetMissing, download.ErrDatasetInvalid, download.ErrDatasetUnsupported, download.ErrDatasetViewClosed} {
		status, reply := scopeFixtureCall(t, &scopedCoreFixture{result: partial, err: err}, http.MethodGet, path, "")
		if reply["data"] != nil || reply["code"] != float64(1000) {
			t.Fatalf("domain error leaked receipt: %v %d %+v", err, status, reply)
		}
		if errors.Is(err, download.ErrDatasetMismatch) && (status != http.StatusConflict || reply["msg"] != "dataset_mismatch") {
			t.Fatalf("mismatch: %d %+v", status, reply)
		}
	}
}

func TestScopedBatchRESTRejectsInvalidOrUnverifiedResults(t *testing.T) {
	root := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/task-batches/v1/operations"
	for _, body := range []string{`{`, `{} {}`, `{} null`} {
		fixture := &scopedCoreFixture{result: scopeFixtureResult()}
		status, reply := scopeFixtureCall(t, fixture, http.MethodPost, root, body)
		if status != http.StatusBadRequest || len(fixture.calls) != 0 || reply["data"] != nil {
			t.Fatalf("invalid JSON invoked core: %d %+v", status, reply)
		}
	}
	fixture := &scopedCoreFixture{result: scopeFixtureResult()}
	status, _ := scopeFixtureCall(t, fixture, http.MethodGet, strings.Replace(root, strings.Repeat("a", 32), "bad", 1)+"/example-op", "")
	if status != http.StatusBadRequest || len(fixture.calls) != 0 {
		t.Fatal("invalid domain reached core")
	}
	for _, result := range []download.BatchOperationResult{{Version: 1, OperationID: "example-op", TaskIDs: []string{"must-not-leak"}}, {Version: 1, OperationID: "other-op", State: "unknown", TaskIDs: []string{"must-not-leak"}}} {
		status, reply := scopeFixtureCall(t, &scopedCoreFixture{result: result}, http.MethodGet, root+"/example-op", "")
		if status != http.StatusServiceUnavailable || reply["data"] != nil {
			t.Fatalf("unverified result adopted: %d %+v", status, reply)
		}
	}
	legacy := httptest.NewRecorder()
	router := mux.NewRouter()
	registerScopedBatchOperationRoutes(router, fixture)
	router.ServeHTTP(legacy, httptest.NewRequest(http.MethodPost, "/api/v1/task-batches/operations", strings.NewReader(`{}`)))
	if legacy.Code != http.StatusNotFound || len(fixture.calls) != 0 {
		t.Fatal("scoped registration intercepted legacy path")
	}
}
