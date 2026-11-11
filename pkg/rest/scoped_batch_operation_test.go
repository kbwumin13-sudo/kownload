package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
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

func TestScopedBatchRESTActualAuthDomainAndUniqueTask(t *testing.T) {
	for _, kind := range []model.Storage{model.StorageMem, model.StorageBolt} {
		t.Run(string(kind), func(t *testing.T) {
			_, endpoint, core, dir := datasetServer(t, kind, "synthetic-scoped-token")
			status, identityReply := datasetGET(t, endpoint+"/api/v1/dataset/identity", "synthetic-scoped-token")
			var identity download.DatasetIdentity
			if err := json.Unmarshal(identityReply.Data, &identity); err != nil || status != http.StatusOK {
				t.Fatal("identity", err, status)
			}
			root := endpoint + "/api/v1/datasets/" + identity.DatasetID + "/task-batches/v1/operations"
			var hits atomic.Int32
			payload := []byte("actual scoped REST fixture")
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				_, _ = w.Write(payload)
			}))
			defer origin.Close()
			output := t.TempDir()
			request := download.BatchOperationRequest{Version: 1, OperationID: "scoped-rest-actual", Reqs: []*base.CreateTaskBatchItem{{Req: &base.Request{URL: origin.URL}, Opts: &base.Options{Path: output, Name: "only.bin"}}}}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			call := func(method, address, token string, body []byte) (int, datasetReply) {
				t.Helper()
				r, err := http.NewRequest(method, address, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Content-Type", "application/json")
				if token != "" {
					r.Header.Set("X-Api-Token", token)
				}
				response, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				var reply datasetReply
				if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
					t.Fatal(err)
				}
				return response.StatusCode, reply
			}
			var before []byte
			if kind == model.StorageBolt {
				before, _ = os.ReadFile(filepath.Join(dir, "gopeed.db"))
			}
			for _, method := range []string{http.MethodGet, http.MethodPost, "cancel"} {
				address := root
				if method == http.MethodGet {
					address += "/" + request.OperationID
				} else if method == "cancel" {
					address += "/" + request.OperationID + "/cancel"
					method = http.MethodPost
				}
				for _, token := range []string{"", "wrong-synthetic-token"} {
					if status, _ := call(method, address, token, raw); status != http.StatusUnauthorized {
						t.Fatalf("scoped auth bypass: %d", status)
					}
				}
				wrong := strings.Repeat("0", 32)
				if wrong == identity.DatasetID {
					wrong = strings.Repeat("1", 32)
				}
				status, reply := call(method, strings.Replace(address, identity.DatasetID, wrong, 1), "synthetic-scoped-token", raw)
				if status != http.StatusConflict || reply.Msg != "dataset_mismatch" || string(reply.Data) != "null" {
					t.Fatalf("actual mismatch: %d %+v", status, reply)
				}
			}
			status, unknown := call(http.MethodGet, root+"/"+request.OperationID, "synthetic-scoped-token", nil)
			var initial scopedBatchResult
			if err := json.Unmarshal(unknown.Data, &initial); err != nil || status != http.StatusOK || initial.Operation.State != "unknown" || initial.DatasetID != identity.DatasetID {
				t.Fatalf("actual unknown: %d %s %v", status, unknown.Data, err)
			}
			if len(core.GetTasks()) != 0 || hits.Load() != 0 {
				t.Fatal("preflight/auth/mismatch created tasks or protocol requests")
			}
			if kind == model.StorageBolt {
				after, _ := os.ReadFile(filepath.Join(dir, "gopeed.db"))
				if !bytes.Equal(before, after) {
					t.Fatal("auth/mismatch/query wrote operation or task storage")
				}
			}
			status, created := call(http.MethodPost, root, "synthetic-scoped-token", raw)
			var receipt scopedBatchResult
			if err := json.Unmarshal(created.Data, &receipt); err != nil || status != http.StatusOK || len(receipt.Operation.TaskIDs) != 1 {
				t.Fatalf("actual create: %d %s %v", status, created.Data, err)
			}
			id := receipt.Operation.TaskIDs[0]
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, taskReply := datasetGET(t, endpoint+"/api/v1/tasks/"+id, "synthetic-scoped-token")
				var task struct{ Status string }
				if err := json.Unmarshal(taskReply.Data, &task); err != nil {
					t.Fatal(err)
				}
				if task.Status == "done" {
					break
				}
				if task.Status == "error" || time.Now().After(deadline) {
					t.Fatalf("synthetic task did not complete: %s", task.Status)
				}
				time.Sleep(time.Millisecond)
			}
			file, err := os.ReadFile(filepath.Join(output, "only.bin"))
			if err != nil || sha256.Sum256(file) != sha256.Sum256(payload) {
				t.Fatal("actual output digest differs", err)
			}
			requests := hits.Load()
			status, replay := call(http.MethodPost, root, "synthetic-scoped-token", raw)
			var same scopedBatchResult
			if err := json.Unmarshal(replay.Data, &same); err != nil || status != http.StatusOK || len(same.Operation.TaskIDs) != 1 || same.Operation.TaskIDs[0] != id || hits.Load() != requests || len(core.GetTasks()) != 1 {
				t.Fatal("same operation replay created or restarted a task")
			}
			status, cancelled := call(http.MethodPost, root+"/"+request.OperationID+"/cancel", "synthetic-scoped-token", nil)
			if status != http.StatusOK || json.Unmarshal(cancelled.Data, &same) != nil || same.Operation.TaskIDs[0] != id {
				t.Fatal("same-domain cancel changed creation history")
			}
			status, legacy := datasetGET(t, endpoint+"/api/v1/task-batches/operations/"+request.OperationID, "synthetic-scoped-token")
			var old download.BatchOperationResult
			if status != http.StatusOK || json.Unmarshal(legacy.Data, &old) != nil || len(old.TaskIDs) != 1 || old.TaskIDs[0] != id || strings.Contains(string(legacy.Data), "scopeVersion") {
				t.Fatal("legacy query shape changed")
			}
		})
	}
}
