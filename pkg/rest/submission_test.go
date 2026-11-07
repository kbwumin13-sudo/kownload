package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

func TestSubmissionRESTCreationKeepsTaskIDs(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer source.Close()
	server, listener, err := BuildServer(&model.StartConfig{Network: "tcp", Address: "127.0.0.1:0", Storage: model.StorageMem, StorageDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	go server.Serve(listener)
	defer func() { _ = server.Shutdown(context.Background()); _ = core.Close() }()
	endpoint := "http://" + listener.Addr().String()
	body := fmt.Sprintf(`{"req":{"url":%q},"opts":{"path":%q,"name":"synthetic.bin"}}`, source.URL, t.TempDir())
	post := func(path, id, body string) map[string]any {
		request, _ := http.NewRequest("POST", endpoint+path, strings.NewReader(body))
		request.Header.Set(operationHeader, id)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			return nil
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Error(err)
		}
		return result
	}
	var wg sync.WaitGroup
	results := make(chan map[string]any, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- post("/api/v1/tasks", "same-click", body) }()
	}
	wg.Wait()
	close(results)
	var id string
	for result := range results {
		got, _ := result["data"].(string)
		if id == "" {
			id = got
		}
		if got == "" || got != id {
			t.Fatalf("retry created a second task: %v", result)
		}
		submission, _ := result["submission"].(map[string]any)
		if submission["state"] != "created" || result["operationId"] != "same-click" {
			t.Fatalf("bad submission: %v", result)
		}
	}
	if len(core.GetTasks()) != 1 {
		t.Fatal("one operation created multiple tasks")
	}
	next := post("/api/v1/tasks", "new-explicit-click", body)
	if next["data"] == id || len(core.GetTasks()) != 2 {
		t.Fatal("different operations were merged by URL")
	}
	batch := fmt.Sprintf(`{"reqs":[%s,%s]}`, body, body)
	first := post("/api/v1/tasks/batch", "batch-click", batch)
	again := post("/api/v1/tasks/batch", "batch-click", batch)
	if fmt.Sprint(first["data"]) != fmt.Sprint(again["data"]) || len(core.GetTasks()) != 4 {
		t.Fatal("batch retry duplicated tasks")
	}
	if _, ok := first["data"].([]any); !ok {
		t.Fatal("legacy batch data shape changed")
	}
}
func TestSubmissionLedgerBoundsAndFailures(t *testing.T) {
	ledger := newSubmissionLedger()
	ledger.limit = 2
	now := time.Now()
	ledger.now = func() time.Time { return now }
	var calls atomic.Int32
	handler := ledger.wrap(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); WriteJson(w, model.NewOkResult("task")) })
	run := func(id, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set(operationHeader, id)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	run("one", "/api/v1/tasks")
	run("one", "/api/v1/tasks")
	if calls.Load() != 1 {
		t.Fatal("retry called handler")
	}
	if run("one", "/api/v1/tasks/batch").Code != 409 {
		t.Fatal("operation crossed endpoint shape")
	}
	now = now.Add(25 * time.Hour)
	run("one", "/api/v1/tasks")
	if calls.Load() != 2 {
		t.Fatal("expired operation was retained")
	}
	run("two", "/api/v1/tasks")
	run("three", "/api/v1/tasks")
	if len(ledger.entries) > 2 {
		t.Fatal("ledger exceeded bound")
	}
	for _, status := range []int{200, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			wrapped := newSubmissionLedger().wrap(func(w http.ResponseWriter, r *http.Request) {
				WriteStatusJson(w, status, map[string]any{"code": 1000, "msg": "https://private.test/a?signature=fixture-secret", "data": "fixture-secret"})
			})
			request := httptest.NewRequest("POST", "/api/v1/tasks", nil)
			request.Header.Set(operationHeader, "failed-operation")
			response := httptest.NewRecorder()
			wrapped(response, request)
			if response.Code != status || bytes.Contains(response.Body.Bytes(), []byte("fixture-secret")) {
				t.Fatal("failure status or privacy changed")
			}
			var result map[string]any
			_ = json.Unmarshal(response.Body.Bytes(), &result)
			if result["data"] != nil || result["submission"] != nil {
				t.Fatal("failure has success payload")
			}
		})
	}
	invalid := httptest.NewRequest("POST", "/api/v1/tasks", nil)
	invalid.Header.Set(operationHeader, "bad/id")
	w := httptest.NewRecorder()
	handler(w, invalid)
	if w.Code != 400 {
		t.Fatal("invalid operation ID accepted")
	}
}
func TestSubmissionConcurrentWaitCanCancel(t *testing.T) {
	ledger := newSubmissionLedger()
	ledger.limit = 1
	entered := make(chan struct{})
	release := make(chan struct{})
	wrapped := ledger.wrap(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		WriteJson(w, model.NewOkResult("task"))
	})
	request := httptest.NewRequest("POST", "/api/v1/tasks", nil)
	request.Header.Set(operationHeader, "pending")
	done := make(chan struct{})
	go func() { wrapped(httptest.NewRecorder(), request); close(done) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiting := httptest.NewRecorder()
	wrapped(waiting, request.WithContext(ctx))
	if waiting.Code != 408 {
		t.Fatal("waiter ignored cancellation")
	}
	other := httptest.NewRequest("POST", "/api/v1/tasks", nil)
	other.Header.Set(operationHeader, "another")
	full := httptest.NewRecorder()
	wrapped(full, other)
	if full.Code != 503 {
		t.Fatal("pending owner evicted at capacity")
	}
	close(release)
	<-done
	replay := httptest.NewRecorder()
	wrapped(replay, request)
	_, _ = io.Copy(io.Discard, replay.Result().Body)
}

// An operation ID belongs to one explicit click: a known ID replays its stored
// result, failures included; the next click carries a new ID and runs again
// (review finding P2-2 concerned the helper reusing IDs across clicks).
func TestSubmissionNewOperationRunsAgainAfterFailure(t *testing.T) {
	ledger := newSubmissionLedger()
	calls := 0
	handler := ledger.wrap(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			WriteJson(w, model.NewErrorResult("download directory is not in white list"))
			return
		}
		writeCreated(w, r, "task-1", []string{"task-1"})
	})
	send := func(id string) string {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{}`))
		req.Header.Set(operationHeader, id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Body.String()
	}
	first, replay := send("click-one"), send("click-one")
	if calls != 1 || first != replay || !strings.Contains(replay, "business_error") {
		t.Fatalf("calls=%d first=%s replay=%s", calls, first, replay)
	}
	next := send("click-two")
	if calls != 2 || !strings.Contains(next, `"state":"created"`) || !strings.Contains(next, "task-1") {
		t.Fatalf("calls=%d next=%s", calls, next)
	}
}
