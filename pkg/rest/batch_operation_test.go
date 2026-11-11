package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

func TestBatchOperationRESTPartialReplayAndConflict(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("batch-content")) }))
	defer source.Close()
	server, listener, err := BuildServer(&model.StartConfig{Storage: model.StorageMem, StorageDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	defer listener.Close()
	defer core.Close()
	path := "/api/v1/task-batches/operations"
	body := fmt.Sprintf(`{"version":1,"operationId":"batch-rest-1","reqs":[{"req":{"url":%q},"opts":{"path":%q,"name":"first.bin"}},{"req":{"url":"unsupported://bad"}}]}`, source.URL, t.TempDir())
	request := func(method, path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		server.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var v map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("non-JSON response status=%d: %s", rec.Code, rec.Body.String())
		}
		return rec.Code, v
	}
	code, first := request("POST", path, body)
	if code != 200 || first["code"] != float64(0) {
		t.Fatalf("create: %d %v", code, first)
	}
	data, ok := first["data"].(map[string]any)
	if !ok || data["createdCount"] != float64(1) || data["failedCount"] != float64(1) || data["pendingCount"] != float64(0) {
		t.Fatalf("partial receipt: %v", first)
	}
	ids := fmt.Sprint(data["taskIds"])
	_, again := request("POST", path, body)
	if fmt.Sprint(again["data"].(map[string]any)["taskIds"]) != ids || len(core.GetTasks()) != 1 {
		t.Fatal("replay duplicated partial batch")
	}
	_, query := request("GET", path+"/batch-rest-1", "")
	if fmt.Sprint(query["data"].(map[string]any)["taskIds"]) != ids {
		t.Fatal("query lost committed prefix")
	}
	code, _ = request("POST", path, strings.Replace(body, "first.bin", "different.bin", 1))
	if code != http.StatusConflict || len(core.GetTasks()) != 1 {
		t.Fatal("same operation accepted different intent")
	}
}

func TestBatchOperationRESTPrecisionAndSafeReplies(t *testing.T) {
	server, listener, err := BuildServer(&model.StartConfig{Storage: model.StorageMem, StorageDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	defer listener.Close()
	defer core.Close()
	endpoint := "/api/v1/task-batches/operations"
	body := fmt.Sprintf(`{"version":1,"operationId":"large-extra","reqs":[{"req":{"url":"http://127.0.0.1:1/no-network","extra":{"opaque":9007199254740993,"header":{"Cookie":"synthetic-secret"}}},"opts":{"path":%q}}]}`, t.TempDir())
	// A disconnected client leaves a durable pending reservation, not a task.
	req := httptest.NewRequest("POST", endpoint, strings.NewReader(body))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "synthetic-secret") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("unsafe pending receipt: %s", rec.Body.String())
	}
	changed := strings.Replace(body, "9007199254740993", "9007199254740992", 1)
	rec = httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest("POST", endpoint, strings.NewReader(changed)))
	if rec.Code != 409 || len(core.GetTasks()) != 0 {
		t.Fatalf("rounded numeric fingerprint: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest("GET", endpoint+"/large-extra", nil))
	if rec.Code != 200 || len(core.GetTasks()) != 0 {
		t.Fatal("GET caused creation")
	}
	rec = httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest("POST", endpoint+"/large-extra/cancel", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	for _, invalid := range []string{body + `{}`, strings.Replace(body, `"version":1`, `"version":2`, 1)} {
		rec = httptest.NewRecorder()
		server.Handler.ServeHTTP(rec, httptest.NewRequest("POST", endpoint, strings.NewReader(invalid)))
		if rec.Code != 400 {
			t.Fatal("invalid version/envelope accepted", rec.Code)
		}
	}
}
