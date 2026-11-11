package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixture(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "kmcp-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	path := filepath.Join(root, "api.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return path
}
func session(t *testing.T, path string) *mcp.ClientSession {
	t.Helper()
	b := newBackend(path)
	t.Cleanup(b.close)
	st, ct := mcp.NewInMemoryTransports()
	serverSession, err := newServer(b).Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "bridge-tests", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}
func call(t *testing.T, s *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func exampleProgress() Progress {
	return Progress{Phase: "downloading", DownloadedBytes: 42, TotalBytes: nil}
}
func exampleTask() Task {
	return Task{ID: "task_1", Protocol: "http", Status: "pause", CreatedAt: "2026-11-11T00:00:00Z", UpdatedAt: "2026-11-11T00:01:00Z", Progress: exampleProgress()}
}
func reply(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"version": 1, "data": data})
}

func TestFiveReadTools(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Method != "GET" || r.Header.Get("X-Kownload-Read-Version") != "1" {
			t.Error("unexpected request")
		}
		switch r.URL.Path {
		case readPrefix:
			if r.URL.Query().Get("limit") != "1" {
				t.Error("missing bound")
			}
			reply(w, TaskList{Tasks: []Task{exampleTask()}})
		case readPrefix + "/task_1":
			reply(w, exampleTask())
		case readPrefix + "/task_1/progress":
			reply(w, exampleProgress())
		case readPrefix + "/task_1/stats":
			reply(w, Stats{Available: false})
		case readPrefix + "/task_1/errors":
			reply(w, LastError{Available: false})
		default:
			t.Errorf("unsafe route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	s := session(t, path)
	if got := s.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("protocol=%s", got)
	}
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 5 {
		t.Fatalf("tools=%d", len(list.Tools))
	}
	for _, def := range definitions {
		args := map[string]any{"id": "task_1"}
		if def.kind == "list" {
			args = map[string]any{"limit": 1}
		}
		result := call(t, s, def.name, args)
		if result.IsError || result.StructuredContent == nil {
			t.Fatalf("%s: %+v", def.name, result)
		}
		data, _ := json.Marshal(result.StructuredContent)
		var value any
		json.Unmarshal(data, &value)
		if err := compileSchema(outputSchema(def.kind)).Validate(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"kownload_create_task", "kownload_pause_task", "kownload_resume_task", "delete_task", "exec"} {
		_, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err == nil {
			t.Fatalf("unknown write accepted: %s", name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 {
		t.Fatalf("unexpected backend calls: %v", paths)
	}
}
func TestRejectInvalidInputsBeforeUDS(t *testing.T) {
	s := session(t, "/not/a/real/socket")
	for _, args := range []any{map[string]any{"id": "../secret"}, map[string]any{"id": "task_1", "path": "/etc/passwd"}, map[string]any{"id": "https://secret"}, map[string]any{"id": strings.Repeat("a", 65)}, map[string]any{}, nil} {
		_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "kownload_get_task", Arguments: args})
		if err == nil {
			t.Errorf("invalid input accepted: %v", args)
		}
	}
	for _, limit := range []any{0, 101, 1.5, -1, "1"} {
		_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "kownload_list_tasks", Arguments: map[string]any{"limit": limit}})
		if err == nil {
			t.Errorf("invalid limit accepted: %v", limit)
		}
	}
}
func TestBackendCannotLeakOrRedirect(t *testing.T) {
	for _, mode := range []string{"private_field", "private_enum", "missing_field", "huge", "redirect", "old_api", "wrong_version", "wrong_task", "invalid_time", "unavailable_with_data", "error_private_text", "too_many_tasks"} {
		t.Run(mode, func(t *testing.T) {
			path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "private_field":
					reply(w, map[string]any{"available": false, "token": "SENTINEL_SECRET"})
				case "private_enum":
					reply(w, map[string]any{"available": true, "category": "SENTINEL_SECRET", "retryable": false, "occurredAt": "2026-11-11T00:00:00Z"})
				case "missing_field":
					reply(w, map[string]any{})
				case "huge":
					io.WriteString(w, strings.Repeat("SENTINEL_SECRET", maxBackend))
				case "redirect":
					w.Header().Set("Location", "http://127.0.0.1/private?SENTINEL_SECRET")
					w.WriteHeader(302)
				case "old_api":
					http.Error(w, "SENTINEL_SECRET", 404)
				case "wrong_version":
					io.WriteString(w, `{"version":2,"data":{"available":false}}`)
				case "wrong_task":
					task := exampleTask()
					task.ID = "another"
					reply(w, task)
				case "invalid_time":
					task := exampleTask()
					task.CreatedAt = "2026-99-99T00:00:00Z"
					reply(w, task)
				case "unavailable_with_data":
					reply(w, map[string]any{"available": false, "category": "network"})
				case "error_private_text":
					http.Error(w, "SENTINEL_SECRET", 500)
				case "too_many_tasks":
					reply(w, TaskList{Tasks: []Task{exampleTask(), exampleTask()}})
				}
			})
			name := "kownload_get_errors"
			args := map[string]any{"id": "task_1"}
			if mode == "wrong_task" || mode == "invalid_time" {
				name = "kownload_get_task"
			}
			if mode == "too_many_tasks" {
				name = "kownload_list_tasks"
				args = map[string]any{"limit": 1}
			}
			result := call(t, session(t, path), name, args)
			data, _ := json.Marshal(result)
			if !result.IsError || strings.Contains(string(data), "SENTINEL_SECRET") {
				t.Fatalf("unsafe result: %s", data)
			}
		})
	}
}
func TestCancellationAndConcurrency(t *testing.T) {
	entered := make(chan struct{}, 4)
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() })
	s := session(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.CallTool(ctx, &mcp.CallToolParams{Name: "kownload_get_stats", Arguments: map[string]any{"id": "task_1"}})
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("not entered")
		}
	}
	result := call(t, s, "kownload_get_stats", map[string]any{"id": "task_1"})
	if !result.IsError || result.Content[0].(*mcp.TextContent).Text != "busy" {
		t.Fatal("unbounded concurrency")
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation stalled")
	}
}
func TestSocketTrust(t *testing.T) {
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) { reply(w, Stats{}) })
	if err := checkSocket(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if err := checkSocket(path); err == nil {
		t.Fatal("world writable socket accepted")
	}
	os.Chmod(path, 0600)
	alias := filepath.Join(filepath.Dir(path), "alias.sock")
	os.Symlink(path, alias)
	if err := checkSocket(alias); err == nil {
		t.Fatal("socket symlink accepted")
	}
	os.Chmod(filepath.Dir(path), 0755)
	if err := checkSocket(path); err == nil {
		t.Fatal("non-private socket parent accepted")
	}
}
func TestLineBounds(t *testing.T) {
	for _, n := range []int{maxInput - 2, maxInput + 1} {
		reader := newLineReader(io.NopCloser(strings.NewReader(strings.Repeat("a", n) + "\n")))
		data, err := io.ReadAll(reader)
		if n < maxInput && (err != nil || len(data) != n+1) {
			t.Fatal("valid frame rejected")
		}
		if n > maxInput && err == nil {
			t.Fatal("oversized frame accepted")
		}
	}
}

func TestBackendDeadline(t *testing.T) {
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	b := newBackend(path)
	defer b.close()
	start := time.Now()
	_, code := b.get(context.Background(), readPrefix)
	if code != "app_unavailable" || time.Since(start) > 4*time.Second {
		t.Fatalf("deadline: %s %v", code, time.Since(start))
	}
}
func TestWriterBoundsAndStoppedReader(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	writer := &boundedWriter{file: write}
	if _, err = writer.Write(make([]byte, maxOutput+1)); err == nil {
		t.Fatal("oversized output accepted")
	}
	start := time.Now()
	if _, err = writer.Write(make([]byte, maxOutput)); err == nil {
		t.Fatal("stopped reader did not time out")
	}
	if time.Since(start) > 7*time.Second {
		t.Fatal("write exceeded bound")
	}
}
