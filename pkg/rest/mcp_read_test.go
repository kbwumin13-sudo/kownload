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

type mcpReadTestSource struct {
	calls int
	err   error
}

func (s *mcpReadTestSource) MCPReadList(_ context.Context, limit int, cursor string) (download.MCPReadPage, error) {
	s.calls++
	return download.MCPReadPage{Tasks: []download.MCPReadTask{mcpReadTestTask()}}, s.err
}
func (s *mcpReadTestSource) MCPReadGetTask(context.Context, string) (download.MCPReadTask, error) {
	s.calls++
	return mcpReadTestTask(), s.err
}
func (s *mcpReadTestSource) MCPReadGetProgress(context.Context, string) (download.MCPReadProgress, error) {
	s.calls++
	return mcpReadTestTask().Progress, s.err
}
func (s *mcpReadTestSource) MCPReadGetStats(context.Context, string) (download.MCPReadStats, error) {
	s.calls++
	return download.MCPReadStats{}, s.err
}
func (s *mcpReadTestSource) MCPReadGetErrors(context.Context, string) (download.MCPReadErrors, error) {
	s.calls++
	return download.MCPReadErrors{}, s.err
}
func mcpReadTestTask() download.MCPReadTask {
	return download.MCPReadTask{ID: "task_1", Protocol: "http", Status: "pause", CreatedAt: "2026-11-11T00:00:00Z", UpdatedAt: "2026-11-11T00:01:00Z", Progress: download.MCPReadProgress{DownloadedBytes: 42, Phase: "downloading"}}
}
func mcpReadRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-Kownload-Read-Version", "1")
	return r
}

func TestMCPReadFiveSafeRoutes(t *testing.T) {
	source := &mcpReadTestSource{}
	handler := newMCPReadHandler(source)
	for _, path := range []string{mcpReadPrefix + "?limit=1", mcpReadPrefix + "/task_1", mcpReadPrefix + "/task_1/progress", mcpReadPrefix + "/task_1/stats", mcpReadPrefix + "/task_1/errors"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, mcpReadRequest("GET", path))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var payload struct {
			Version int             `json:"version"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload.Version != 1 {
			t.Fatal(w.Body.String())
		}
		if strings.HasSuffix(path, "stats") || strings.HasSuffix(path, "errors") {
			if string(payload.Data) != `{"available":false}` {
				t.Fatal(w.Body.String())
			}
		}
		if strings.HasSuffix(path, "progress") && !strings.Contains(string(payload.Data), `"totalBytes":null`) {
			t.Fatal("unknown total lost")
		}
	}
	if source.calls != 5 {
		t.Fatal(source.calls)
	}
}
func TestMCPReadRejectsWritesAndInvalidRequests(t *testing.T) {
	source := &mcpReadTestSource{}
	handler := newMCPReadHandler(source)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, mcpReadRequest(method, mcpReadPrefix+"/task_1"))
		if w.Code != 405 {
			t.Fatal(method, w.Code)
		}
	}
	for _, path := range []string{
		mcpReadPrefix, mcpReadPrefix + "?limit=0", mcpReadPrefix + "?limit=101", mcpReadPrefix + "?limit=1.5", mcpReadPrefix + "?limit=1&limit=2",
		mcpReadPrefix + "?limit=1&secret=PRIVATE_SENTINEL", mcpReadPrefix + "?limit=1&cursor=a&cursor=b", mcpReadPrefix + "?limit=1&cursor=" + strings.Repeat("a", 129),
		mcpReadPrefix + "/task_1?url=PRIVATE_SENTINEL", mcpReadPrefix + "/task_1/delete", mcpReadPrefix + "/../token", mcpReadPrefix + "/task_1/stats/more",
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, mcpReadRequest("GET", path))
		if w.Code < 400 || strings.Contains(w.Body.String(), "PRIVATE_SENTINEL") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, header := range []string{"", "2", "1,1"} {
		r := mcpReadRequest("GET", mcpReadPrefix+"?limit=1")
		r.Header.Set("X-Kownload-Read-Version", header)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatal(w.Code)
		}
	}
	r := mcpReadRequest("GET", mcpReadPrefix+"?limit=1")
	r.ContentLength = 1
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, mcpReadRequest("GET", mcpReadPrefix+"?limit=1").WithContext(ctx))
	if w.Code != 408 {
		t.Fatal(w.Code)
	}
	if source.calls != 0 {
		t.Fatalf("invalid request reached source: %d", source.calls)
	}
}
func TestMCPReadErrorsDoNotEchoPrivateData(t *testing.T) {
	for _, sample := range []struct {
		err    error
		status int
	}{
		{download.ErrMCPReadInvalid, 400}, {download.ErrTaskNotFound, 404}, {download.ErrMCPReadBusy, 503},
		{download.ErrMCPReadUnavailable, 503}, {context.Canceled, 408}, {errors.New("PRIVATE_SENTINEL token/path/url"), 503},
	} {
		source := &mcpReadTestSource{err: sample.err}
		w := httptest.NewRecorder()
		newMCPReadHandler(source).ServeHTTP(w, mcpReadRequest("GET", mcpReadPrefix+"/task_1"))
		if w.Code != sample.status || strings.Contains(w.Body.String(), "PRIVATE_SENTINEL") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestMCPReadExplicitRegistrationDoesNotInitializeCore(t *testing.T) {
	router := mux.NewRouter()
	RegisterMCPReadRoutes(router, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, mcpReadRequest("GET", mcpReadPrefix+"?limit=1"))
	if w.Code != 503 {
		t.Fatal("nil core should be unavailable", w.Code)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, mcpReadRequest("POST", mcpReadPrefix+"/task_1"))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, mcpReadRequest("GET", "/api/v1/tasks"))
	if w.Code != 404 {
		t.Fatal("registration reached legacy API")
	}
}
