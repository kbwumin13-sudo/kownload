package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

type taskCompletionFixture struct {
	domain       string
	selection    []*download.Task
	snapshots    map[string]download.TaskCompletionSnapshot
	identityErr  error
	snapshotErr  error
	selectedIDs  []string
	filter       *download.TaskFilter
	identityRead int
	viewRead     int
}

func (f *taskCompletionFixture) DatasetIdentity() (download.DatasetIdentity, error) {
	f.identityRead++
	return download.DatasetIdentity{Version: 1, DatasetID: f.domain, Persistent: true}, f.identityErr
}
func (f *taskCompletionFixture) GetTasksByFilter(filter *download.TaskFilter) []*download.Task {
	f.filter = filter
	return f.selection
}
func (f *taskCompletionFixture) TaskCompletionSnapshotsForDataset(domain string, ids []string) (map[string]download.TaskCompletionSnapshot, error) {
	f.viewRead++
	f.selectedIDs = append([]string{}, ids...)
	if domain != f.domain {
		return nil, download.ErrDatasetMismatch
	}
	return f.snapshots, f.snapshotErr
}

func taskCompletionCall(t *testing.T, core taskCompletionCore, path string) (int, datasetReply) {
	t.Helper()
	router := mux.NewRouter()
	registerTaskReadRoutes(router, core)
	return verifiedHandlerCall(t, router, http.MethodGet, path, "", "")
}

func completionSnapshotTask(id string) *download.Task {
	task := download.NewTask()
	task.ID, task.Status = id, base.DownloadStatusRunning
	task.Progress = &download.Progress{Downloaded: 123}
	return task
}

func TestTaskCompletionReadUsesOwnedPairNotSelectedLiveValue(t *testing.T) {
	domain := strings.Repeat("a", 32)
	task := completionSnapshotTask("task-one")
	task.Status = base.DownloadStatusDone
	task.Completion = &download.TaskCompletionRef{Version: 1, CompletionID: "completion-one"}
	matched := true
	view := &download.TaskCompletionView{Version: 1, ScopeVersion: 1, DatasetID: domain,
		TaskID: task.ID, CompletionID: task.Completion.CompletionID, Available: true,
		Phase: "committed", RecordedMatch: &matched}
	selected := completionSnapshotTask(task.ID)
	selected.Progress.Downloaded = 999
	fixture := &taskCompletionFixture{domain: domain, selection: []*download.Task{selected},
		snapshots: map[string]download.TaskCompletionSnapshot{task.ID: {Task: task, CompletionView: view}}}
	for _, path := range []string{"/api/v1/tasks/task-one", "/api/v1/tasks"} {
		status, reply := taskCompletionCall(t, fixture, path)
		if status != 200 || reply.Code != 0 || !bytes.Contains(reply.Data, []byte(`"downloaded":123`)) ||
			bytes.Contains(reply.Data, []byte(`999`)) || !bytes.Contains(reply.Data, []byte(`"phase":"committed"`)) ||
			!bytes.Contains(reply.Data, []byte(`"recordedMatch":true`)) {
			t.Fatalf("pair was split: %d %s", status, reply.Data)
		}
	}
	if fixture.identityRead != 2 || fixture.viewRead != 2 {
		t.Fatal("unexpected extra authority lookup", fixture.identityRead, fixture.viewRead)
	}
}

func TestTaskCompletionReadLegacyShapeAndRawNumbers(t *testing.T) {
	task := completionSnapshotTask("legacy-task")
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://example.invalid/file", Extra: map[string]any{"large": json.Number("900719925474099312345")}}, Opts: &base.Options{}}
	fixture := &taskCompletionFixture{domain: strings.Repeat("a", 32), snapshots: map[string]download.TaskCompletionSnapshot{task.ID: {Task: task}}}
	_, reply := taskCompletionCall(t, fixture, "/api/v1/tasks/"+task.ID)
	want, err := json.Marshal(task)
	if err != nil || !bytes.Equal(reply.Data, want) || bytes.Contains(reply.Data, []byte("completionView")) {
		t.Fatalf("legacy wire changed: %v %s %s", err, reply.Data, want)
	}
	// The decorator must preserve arbitrary-precision existing Task fields.
	view := unavailableCompletionView(fixture.domain, task.ID)
	raw, err := (taskCompletionResponse{task: task, view: &view}).MarshalJSON()
	if err != nil || !bytes.Contains(raw, []byte(`"downloaded":123`)) || !bytes.Contains(raw, []byte(`900719925474099312345`)) {
		t.Fatal("decorated Task lost original fields", err)
	}
}

func TestTaskCompletionReadSelectionOrderFilterAndConcurrentDeletion(t *testing.T) {
	first, removed, last := completionSnapshotTask("first"), completionSnapshotTask("removed"), completionSnapshotTask("last")
	fixture := &taskCompletionFixture{domain: strings.Repeat("a", 32), selection: []*download.Task{last, removed, first},
		snapshots: map[string]download.TaskCompletionSnapshot{first.ID: {Task: first}, last.ID: {Task: last}, "unselected": {Task: removed}}}
	status, reply := taskCompletionCall(t, fixture, "/api/v1/tasks?id=last&id=first&status=running&notStatus=done")
	var tasks []struct{ ID string }
	if err := json.Unmarshal(reply.Data, &tasks); err != nil || status != 200 || len(tasks) != 2 || tasks[0].ID != "last" || tasks[1].ID != "first" {
		t.Fatalf("selection changed: %d %s %v", status, reply.Data, err)
	}
	if !reflect.DeepEqual(fixture.selectedIDs, []string{"last", "removed", "first"}) ||
		!reflect.DeepEqual(fixture.filter, &download.TaskFilter{IDs: []string{"last", "first"}, Statuses: []base.Status{"running"}, NotStatuses: []base.Status{"done"}}) {
		t.Fatal("legacy filter or captured selection changed")
	}
	status, reply = taskCompletionCall(t, fixture, "/api/v1/tasks/removed")
	if status != 200 || reply.Code != int(model.CodeTaskNotFound) || string(reply.Data) != "null" {
		t.Fatal("deleted ID was recovered from live selection")
	}
}

func TestTaskCompletionReadUnconfirmedCAndErrorsReturnNoPartialTask(t *testing.T) {
	task := completionSnapshotTask("must-not-leak")
	task.Status = base.DownloadStatusDone
	for _, failure := range []struct {
		err     error
		status  int
		message string
	}{
		{&download.TaskCompletionStateError{Kind: download.TaskCompletionUnavailable, Err: errors.New("private-path-secret")}, 503, "completion_state_unavailable"},
		{&download.TaskCompletionStateError{Kind: download.TaskCompletionHeld}, 503, "completion_state_held"},
		{&download.TaskCompletionStateError{Kind: "private-path-secret"}, 503, "completion_state_unavailable"},
		{download.ErrDatasetMismatch, 409, "dataset_mismatch"},
		{errors.New("private-path-secret"), 503, "completion_state_unavailable"},
	} {
		fixture := &taskCompletionFixture{domain: strings.Repeat("a", 32), selection: []*download.Task{task},
			snapshotErr: failure.err, snapshots: map[string]download.TaskCompletionSnapshot{task.ID: {Task: task}}}
		for _, path := range []string{"/api/v1/tasks/" + task.ID, "/api/v1/tasks"} {
			status, reply := taskCompletionCall(t, fixture, path)
			if status != failure.status || reply.Msg != failure.message || string(reply.Data) != "null" ||
				bytes.Contains(reply.Data, []byte(task.ID)) || strings.Contains(reply.Msg, "secret") {
				t.Fatalf("error leaked partial Task or cause: %d %+v", status, reply)
			}
		}
	}
}

func TestTaskCompletionReadRejectsInvalidAuthorityBeforeView(t *testing.T) {
	for _, domain := range []string{"", "bad", strings.Repeat("A", 32)} {
		fixture := &taskCompletionFixture{domain: domain}
		status, reply := taskCompletionCall(t, fixture, "/api/v1/tasks/task")
		if status != 503 || fixture.viewRead != 0 || string(reply.Data) != "null" {
			t.Fatal("invalid dataset was used")
		}
	}
}

func TestTaskCompletionReadDoesNotBlessMismatchedProjection(t *testing.T) {
	domain, cid := strings.Repeat("a", 32), "own-completion"
	for _, mutate := range []func(*download.TaskCompletionSnapshot){
		func(s *download.TaskCompletionSnapshot) { s.CompletionView.DatasetID = strings.Repeat("b", 32) },
		func(s *download.TaskCompletionSnapshot) { s.CompletionView.TaskID = "other-task" },
		func(s *download.TaskCompletionSnapshot) { s.CompletionView.CompletionID = "other-completion" },
		func(s *download.TaskCompletionSnapshot) { s.Task.Completion = nil },
		func(s *download.TaskCompletionSnapshot) {
			s.CompletionView.Phase = "committed"
			yes := true
			s.CompletionView.RecordedMatch = &yes
		},
		func(s *download.TaskCompletionSnapshot) { s.CompletionView = nil },
	} {
		task := completionSnapshotTask("own-task")
		task.Completion = &download.TaskCompletionRef{Version: 1, CompletionID: cid}
		snapshot := download.TaskCompletionSnapshot{Task: task, CompletionView: &download.TaskCompletionView{
			Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: task.ID, CompletionID: cid, Available: true, Phase: "prepared"}}
		mutate(&snapshot)
		fixture := &taskCompletionFixture{domain: domain, snapshots: map[string]download.TaskCompletionSnapshot{task.ID: snapshot}}
		status, reply := taskCompletionCall(t, fixture, "/api/v1/tasks/"+task.ID)
		var public struct {
			View completionView `json:"completionView"`
		}
		if err := json.Unmarshal(reply.Data, &public); err != nil || status != 200 || public.View.Available ||
			public.View.Phase != nil || public.View.RecordedMatch != nil || public.View.CompletionID != nil {
			t.Fatalf("invalid association blessed: %s %v", reply.Data, err)
		}
	}
}

func TestTaskCompletionReadBuildServerAuthOwnerAndCapabilityPreserved(t *testing.T) {
	server, endpoint, core, _ := datasetServer(t, model.StorageBolt, "synthetic-read-token")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("owned legacy sample")) }))
	defer source.Close()
	id, err := core.CreateDirect(&base.Request{URL: source.URL}, &base.Options{Path: t.TempDir(), Name: "legacy.bin"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for core.GetTask(id).Status != base.DownloadStatusDone && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if core.GetTask(id).Status != base.DownloadStatusDone {
		t.Fatal("own legacy sample did not finish")
	}
	identity, err := core.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := core.VerifiedHTTPCapabilityForDataset(identity.DatasetID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(core.GetTasks())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/tasks", "/api/v1/tasks/" + id} {
		if status, _ := datasetGET(t, endpoint+path, ""); status != 401 {
			t.Fatal("task read bypassed auth")
		}
		previous := Downloader
		Downloader = nil
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Api-Token", "synthetic-read-token")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		Downloader = previous
		if response.Code != 200 || bytes.Contains(response.Body.Bytes(), []byte("completionView")) || !bytes.Contains(response.Body.Bytes(), []byte(id)) {
			t.Fatalf("router did not capture own Core: %d %s", response.Code, response.Body.String())
		}
		var reply datasetReply
		if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		want := before
		if path != "/api/v1/tasks" {
			want, err = json.Marshal(core.GetTask(id))
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(reply.Data, want) {
			t.Fatalf("ordinary Task snapshot changed: %s != %s", reply.Data, want)
		}
	}
	after, _ := json.Marshal(core.GetTasks())
	if !bytes.Equal(before, after) {
		t.Fatal("task reads changed Task data")
	}
	root := "/api/v1/datasets/" + identity.DatasetID + "/verified-http/v1"
	status, reply := datasetGET(t, endpoint+root+"/capability", "synthetic-read-token")
	var advertised verifiedCapabilityResult
	if err := json.Unmarshal(reply.Data, &advertised); err != nil || status != 200 || advertised.Supported != capability.Supported {
		t.Fatal("task reads changed Core capability", err)
	}
}

func TestTaskCompletionReadRealBoltOrphanIsUnavailableAndReadOnly(t *testing.T) {
	previousCore, previousServer, previousAES, previousPort := Downloader, srv, aesKey, runningPort
	t.Cleanup(func() { Downloader, srv, aesKey, runningPort = previousCore, previousServer, previousAES, previousPort })
	dir := t.TempDir()
	store := download.NewBoltStorage(dir)
	if err := store.Setup([]string{"task", "task_completion_required_v1"}); err != nil {
		t.Fatal(err)
	}
	task := completionSnapshotTask("orphan-task")
	task.Protocol, task.Status = "http", base.DownloadStatusError
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://127.0.0.1:1/must-not-contact"}, Opts: &base.Options{Path: t.TempDir(), Name: "orphan.bin"}}
	if err := store.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("task_completion_required_v1", task.ID, map[string]any{"version": 999, "private": "must-not-leak-anchor"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	server, listener, err := BuildServer(&model.StartConfig{Storage: model.StorageBolt, StorageDir: dir, ApiToken: "synthetic-read-token"})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close(); _ = core.Close() })
	before, err := os.ReadFile(filepath.Join(dir, "gopeed.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/tasks/" + task.ID, "/api/v1/tasks"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Api-Token", "synthetic-read-token")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		var reply datasetReply
		if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || reply.Code != 0 || !bytes.Contains(reply.Data, []byte(`"available":false`)) ||
			!bytes.Contains(reply.Data, []byte(`"recordedMatch":null`)) || !bytes.Contains(reply.Data, []byte(`"phase":null`)) ||
			bytes.Contains(reply.Data, []byte("must-not-leak-anchor")) || bytes.Contains(reply.Data, []byte(`"committed"`)) {
			t.Fatalf("orphan state became legacy or evidence: %d %s", response.Code, reply.Data)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, "gopeed.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("projection read wrote Bolt state", err)
	}
}
