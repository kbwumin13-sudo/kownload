//go:build !windows

package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

const mcpPublishedToken = "SYNTHETIC_PUBLISHED_API_TOKEN"

type mcpPublishedFixture struct {
	root, socket string
	client       *http.Client
}

// Empty real Bolt store and actual BuildServer; no seeded Tasks or cached values.
// Tests are deliberately serial because BuildServer uses package globals.
func newMCPPublishedFixture(t *testing.T) *mcpPublishedFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "kmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	oldD, oldSrv, oldPort, oldAES := Downloader, srv, runningPort, aesKey
	var server *http.Server
	var listener net.Listener
	var d *download.Downloader
	var client *http.Client
	var finished chan error
	t.Cleanup(func() {
		if client != nil {
			client.CloseIdleConnections()
		}
		if server != nil {
			server.Close()
		}
		if listener != nil {
			listener.Close()
		}
		if finished != nil {
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Error("private server did not stop")
			}
		}
		if d != nil {
			d.Close()
		} else if Downloader != nil && Downloader != oldD {
			Downloader.Close()
		}
		Downloader, srv, runningPort, aesKey = oldD, oldSrv, oldPort, oldAES
	})
	socket := filepath.Join(root, "api.sock")
	server, listener, err = BuildServer(&model.StartConfig{Network: "unix", Address: socket, ApiToken: mcpPublishedToken, Storage: model.StorageBolt, StorageDir: filepath.Join(root, "store"), StartupTaskPolicy: "imported-review"})
	if err != nil {
		t.Fatal(err)
	}
	d = Downloader
	finished = make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client = &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &mcpPublishedFixture{root: root, socket: socket, client: client}
}

func (f *mcpPublishedFixture) request(t *testing.T, method, path string, payload any, read bool) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://kownload.local"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Token", mcpPublishedToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if read {
		req.Header.Set("X-Kownload-Read-Version", "1")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if read {
		f.noPrivate(t, b)
	}
	return resp.StatusCode, b
}
func (f *mcpPublishedFixture) noPrivate(t *testing.T, b []byte) {
	t.Helper()
	for _, secret := range []string{"SYNTHETIC_", f.root, "http://", "https://", "Authorization", "Cookie"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("private field escaped read response")
		}
	}
}
func (f *mcpPublishedFixture) control(t *testing.T, method, path string, payload any) json.RawMessage {
	t.Helper()
	status, b := f.request(t, method, path, payload, false)
	var r struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if status != 200 || json.Unmarshal(b, &r) != nil || r.Code != 0 {
		t.Fatalf("control %s %s failed: status=%d code=%d", method, path, status, r.Code)
	}
	return r.Data
}
func (f *mcpPublishedFixture) read(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		status, b := f.request(t, "GET", mcpReadPrefix+path, nil, true)
		if status == 503 && string(b) == "{\"version\":1,\"error\":\"snapshot_busy\"}\n" && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			continue
		}
		var r struct {
			Version int            `json:"version"`
			Data    map[string]any `json:"data"`
		}
		if status != 200 || json.Unmarshal(b, &r) != nil || r.Version != 1 || r.Data == nil {
			t.Fatalf("MCP read failed: status=%d body=%s", status, b)
		}
		return r.Data
	}
}
func (f *mcpPublishedFixture) wait(t *testing.T, path, expect string, predicate func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		v := f.read(t, path)
		if predicate(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("real publisher did not provide %s; last=%v", expect, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func (f *mcpPublishedFixture) create(t *testing.T, url, name string) string {
	t.Helper()
	b := f.control(t, "POST", "/api/v1/tasks", map[string]any{"req": map[string]any{"url": url, "extra": map[string]any{"header": map[string]string{"Authorization": "Bearer SYNTHETIC_SOURCE_SECRET"}}}, "opts": map[string]any{"path": f.root, "name": name, "extra": map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false}}})
	var id string
	if json.Unmarshal(b, &id) != nil || !download.MCPReadValidID(id) {
		t.Fatal("no valid created task ID")
	}
	return id
}
func publishedStatsPositive(v map[string]any) bool {
	if v["available"] != true {
		return false
	}
	for _, key := range []string{"activeStreams", "targetStreams"} {
		if n, ok := v[key].(float64); ok && n > 0 {
			return true
		}
	}
	return false
}
func (f *mcpPublishedFixture) clear(t *testing.T, id string) {
	t.Helper()
	for _, kind := range []string{"stats", "errors"} {
		v := f.read(t, "/"+id+"/"+kind)
		if len(v) != 1 || v["available"] != false {
			t.Fatalf("%s cache not cleared: %v", kind, v)
		}
	}
}
func (f *mcpPublishedFixture) five(t *testing.T, id string) {
	t.Helper()
	before := f.read(t, "?limit=100")
	cfg := f.control(t, "GET", "/api/v1/config", nil)
	for _, path := range []string{"?limit=100", "/" + id, "/" + id + "/progress", "/" + id + "/stats", "/" + id + "/errors"} {
		f.read(t, path)
	}
	after := f.read(t, "?limit=100")
	ids := func(v map[string]any) []string {
		var out []string
		for _, raw := range v["tasks"].([]any) {
			out = append(out, raw.(map[string]any)["id"].(string))
		}
		return out
	}
	if !reflect.DeepEqual(ids(before), ids(after)) || !bytes.Equal(cfg, f.control(t, "GET", "/api/v1/config", nil)) {
		t.Fatal("reads changed task membership or config")
	}
}

type mcpPublishedPlan struct {
	kind                   string
	entered, release       chan struct{}
	enterOnce, releaseOnce sync.Once
}

func newMCPPublishedPlan(t *testing.T, kind string) *mcpPublishedPlan {
	p := &mcpPublishedPlan{kind: kind, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(p.open)
	return p
}
func (p *mcpPublishedPlan) open() { p.releaseOnce.Do(func() { close(p.release) }) }
func (p *mcpPublishedPlan) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real HTTP request never reached gate")
	}
}

type mcpPublishedSource struct {
	server  *httptest.Server
	plan    atomic.Pointer[mcpPublishedPlan]
	payload []byte
}

func newMCPPublishedSource(t *testing.T, p *mcpPublishedPlan) *mcpPublishedSource {
	s := &mcpPublishedSource{payload: bytes.Repeat([]byte("kownload-payload"), 2048)} // 32 KiB
	s.plan.Store(p)
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan := s.plan.Load()
		if plan.kind == "auth" || plan.kind == "late-auth" {
			plan.enterOnce.Do(func() { close(plan.entered) })
			if plan.kind == "late-auth" {
				<-plan.release
			} // Deliberately release an old response after pause/new-run.
			http.Error(w, "SYNTHETIC_SOURCE_SECRET", http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"same-synthetic-resource"`)
		if plan.kind == "body" {
			http.ServeContent(&mcpPublishedGateWriter{ResponseWriter: w, ctx: r.Context(), plan: plan}, r, "SYNTHETIC_REMOTE.bin", time.Unix(1700000000, 0), bytes.NewReader(s.payload))
			return
		}
		http.ServeContent(w, r, "SYNTHETIC_REMOTE.bin", time.Unix(1700000000, 0), bytes.NewReader(s.payload))
	}))
	// Registered after plans: open every live request before server.Close waits.
	t.Cleanup(func() { s.plan.Load().open(); p.open(); s.server.Close() })
	return s
}

type mcpPublishedGateWriter struct {
	http.ResponseWriter
	ctx  context.Context
	plan *mcpPublishedPlan
}

func (w *mcpPublishedGateWriter) Write(b []byte) (int, error) {
	if len(b) <= 1024 {
		return w.ResponseWriter.Write(b)
	}
	n, err := w.ResponseWriter.Write(b[:1024])
	if err != nil {
		return n, err
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	w.plan.enterOnce.Do(func() { close(w.plan.entered) })
	select {
	case <-w.plan.release:
	case <-w.ctx.Done():
		return n, w.ctx.Err()
	}
	rest, err := w.ResponseWriter.Write(b[1024:])
	return n + rest, err
}

func TestMCPPublishedHTTPActivePauseSuccess(t *testing.T) {
	f := newMCPPublishedFixture(t)
	p := newMCPPublishedPlan(t, "body")
	s := newMCPPublishedSource(t, p)
	id := f.create(t, s.server.URL+"/SYNTHETIC_RESOURCE?token=SYNTHETIC_QUERY", "sample.bin")
	p.waitEntered(t)
	f.wait(t, "/"+id+"/stats", "HTTP active/target > 0", publishedStatsPositive)
	f.five(t, id)
	f.control(t, "PUT", "/api/v1/tasks/"+id+"/pause", nil)
	f.clear(t, id)
	p.open()
	next := newMCPPublishedPlan(t, "body")
	s.plan.Store(next)
	f.control(t, "PUT", "/api/v1/tasks/"+id+"/continue", nil)
	next.waitEntered(t)
	f.wait(t, "/"+id+"/stats", "new-run HTTP statistics", publishedStatsPositive)
	if v := f.read(t, "/"+id+"/errors"); v["available"] != false {
		t.Fatal("old error survived new run")
	}
	next.open()
	f.wait(t, "/"+id, "done", func(v map[string]any) bool { return v["status"] == "done" })
	f.clear(t, id)
	b, err := os.ReadFile(filepath.Join(f.root, "sample.bin"))
	if err != nil || sha256.Sum256(b) != sha256.Sum256(s.payload) {
		t.Fatal("real HTTP output mismatch", err)
	}
	taskBefore := f.control(t, "GET", "/api/v1/tasks/"+id, nil)
	f.five(t, id)
	afterBytes, afterErr := os.ReadFile(filepath.Join(f.root, "sample.bin"))
	if afterErr != nil || sha256.Sum256(afterBytes) != sha256.Sum256(b) || !bytes.Equal(taskBefore, f.control(t, "GET", "/api/v1/tasks/"+id, nil)) {
		t.Fatal("completed object or output changed during five reads")
	}
}

func TestMCPPublishedHTTPFailuresAndSourceReset(t *testing.T) {
	t.Run("authentication_source_reset", func(t *testing.T) {
		f := newMCPPublishedFixture(t)
		p := newMCPPublishedPlan(t, "auth")
		s := newMCPPublishedSource(t, p)
		lower := time.Now().UnixMilli()
		id := f.create(t, s.server.URL+"/SYNTHETIC_AUTH", "auth.bin")
		f.wait(t, "/"+id, "error", func(v map[string]any) bool { return v["status"] == "error" })
		event := f.wait(t, "/"+id+"/errors", "accepted authentication event", func(v map[string]any) bool { return v["available"] == true })
		assertMCPPublishedError(t, event, "authentication", false, lower)
		f.five(t, id)
		next := newMCPPublishedPlan(t, "body")
		s.plan.Store(next)
		f.control(t, "PATCH", "/api/v1/tasks/"+id, map[string]any{"req": map[string]any{"url": s.server.URL + "/SYNTHETIC_REPLACEMENT"}})
		f.clear(t, id)
		f.control(t, "PUT", "/api/v1/tasks/"+id+"/continue", nil)
		next.waitEntered(t)
		f.wait(t, "/"+id+"/stats", "replacement HTTP statistics", publishedStatsPositive)
		if v := f.read(t, "/"+id+"/errors"); v["available"] != false {
			t.Fatal("source/run kept old classified error")
		}
		next.open()
		f.wait(t, "/"+id, "done", func(v map[string]any) bool { return v["status"] == "done" })
		f.clear(t, id)
	})
	t.Run("connection_refused", func(t *testing.T) {
		f := newMCPPublishedFixture(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		lower := time.Now().UnixMilli()
		id := f.create(t, "http://"+address+"/SYNTHETIC_REFUSED", "refused.bin")
		f.wait(t, "/"+id, "error", func(v map[string]any) bool { return v["status"] == "error" })
		event := f.wait(t, "/"+id+"/errors", "accepted network event", func(v map[string]any) bool { return v["available"] == true })
		assertMCPPublishedError(t, event, "network", true, lower)
		f.five(t, id)
	})
}
func assertMCPPublishedError(t *testing.T, v map[string]any, category string, retryable bool, lower int64) {
	t.Helper()
	stamp, ok := v["occurredAt"].(string)
	if !ok {
		t.Fatal("missing occurrence time")
	}
	when, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || when.UnixMilli() < lower || when.UnixMilli() > time.Now().UnixMilli() || v["category"] != category || v["retryable"] != retryable {
		t.Fatalf("wrong accepted error: %v", v)
	}
}

func TestMCPPublishedHTTPLateOldResponse(t *testing.T) {
	f := newMCPPublishedFixture(t)
	old := newMCPPublishedPlan(t, "late-auth")
	s := newMCPPublishedSource(t, old)
	id := f.create(t, s.server.URL+"/SYNTHETIC_LATE", "late.bin")
	old.waitEntered(t)
	f.control(t, "PUT", "/api/v1/tasks/"+id+"/pause", nil)
	f.clear(t, id)
	next := newMCPPublishedPlan(t, "body")
	s.plan.Store(next)
	f.control(t, "PUT", "/api/v1/tasks/"+id+"/continue", nil)
	next.waitEntered(t)
	f.wait(t, "/"+id+"/stats", "new-run live statistics", publishedStatsPositive)
	old.open()
	// Give the released old HTTP response and existing progress sampler time to
	// run. This is test observation only, never a new production timer.
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if v := f.read(t, "/"+id+"/errors"); v["available"] != false {
			t.Fatal("late old response overwrote current run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	next.open()
	f.wait(t, "/"+id, "done", func(v map[string]any) bool { return v["status"] == "done" })
	f.clear(t, id)
}

// Opt-in binary is built from the current bridge source. No App or installer.
func TestMCPPublishedHTTPCLI(t *testing.T) {
	binary := os.Getenv("KOWNLOAD_MCP_TEST_BINARY")
	if binary == "" {
		t.Skip("set KOWNLOAD_MCP_TEST_BINARY for real stdio/mcporter integration")
	}
	cli, err := exec.LookPath("mcporter")
	if err != nil {
		t.Fatal(err)
	}
	f := newMCPPublishedFixture(t)
	p := newMCPPublishedPlan(t, "body")
	s := newMCPPublishedSource(t, p)
	id := f.create(t, s.server.URL+"/SYNTHETIC_CLI", "cli.bin")
	p.waitEntered(t)
	f.wait(t, "/"+id+"/stats", "real HTTP publisher before CLI", publishedStatsPositive)
	home := filepath.Join(f.root, "client-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(f.root, "token")
	if err := os.WriteFile(token, []byte(mcpPublishedToken), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(f.root, "mcporter.json")
	configBytes, _ := json.Marshal(map[string]any{"imports": []string{}, "mcpServers": map[string]any{"kownload": map[string]any{"command": binary, "args": []string{"--socket", f.socket, "--api-token-file", token}}}})
	if err := os.WriteFile(config, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, append([]string{"--config", config}, args...)...)
		cmd.Dir = f.root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("real CLI failed: %T", err)
		}
		return out
	}
	listed := invoke("list", "kownload", "--schema", "--json")
	var listing struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(listed, &listing) != nil || len(listing.Tools) != 5 {
		t.Fatal("not exactly five tools")
	}
	toolsJSON, _ := json.Marshal(listing.Tools)
	f.noPrivate(t, toolsJSON)
	expected := map[string]bool{"kownload_list_tasks": true, "kownload_get_task": true, "kownload_get_progress": true, "kownload_get_stats": true, "kownload_get_errors": true}
	for _, raw := range listing.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &tool) != nil || !expected[tool.Name] {
			t.Fatal("unexpected or duplicate tool")
		}
		delete(expected, tool.Name)
	}
	taskIDs := func(v map[string]any) []string {
		var ids []string
		for _, raw := range v["tasks"].([]any) {
			ids = append(ids, raw.(map[string]any)["id"].(string))
		}
		return ids
	}
	readFive := func(taskID, state string, count int, lower int64) {
		before := f.read(t, "?limit=100")
		cfg := f.control(t, "GET", "/api/v1/config", nil)
		for _, name := range []string{"list_tasks", "get_task", "get_progress", "get_stats", "get_errors"} {
			args := fmt.Sprintf(`{"id":%q}`, taskID)
			if name == "list_tasks" {
				args = `{"limit":100}`
			}
			out := invoke("call", "kownload.kownload_"+name, "--args", args, "--output", "json")
			f.noPrivate(t, out)
			var v map[string]any
			if json.Unmarshal(out, &v) != nil {
				t.Fatal("CLI did not return JSON")
			}
			switch name {
			case "get_stats":
				if state == "active" && !publishedStatsPositive(v) {
					t.Fatal("real statistics lost in stdio")
				}
				if state == "cleared" && v["available"] != false {
					t.Fatal("cleared statistics remained visible")
				}
			case "get_errors":
				if state == "authentication" {
					assertMCPPublishedError(t, v, "authentication", false, lower)
				} else if v["available"] != false {
					t.Fatal("old classified error remained visible")
				}
			case "get_task":
				if v["id"] != taskID {
					t.Fatal("wrong task")
				}
			case "list_tasks":
				if len(v["tasks"].([]any)) != count {
					t.Fatal("MCP changed task count")
				}
			case "get_progress":
				if state == "active" && v["totalBytes"] != nil {
					t.Fatal("active total fabricated")
				}
			}
		}
		after := f.read(t, "?limit=100")
		saved, _ := os.ReadFile(config)
		if len(before["tasks"].([]any)) != count || len(after["tasks"].([]any)) != count || !reflect.DeepEqual(taskIDs(before), taskIDs(after)) || !bytes.Equal(cfg, f.control(t, "GET", "/api/v1/config", nil)) || !bytes.Equal(configBytes, saved) {
			t.Fatal("reads changed task/config/client state")
		}
	}
	readFive(id, "active", 1, 0)
	f.control(t, "PUT", "/api/v1/tasks/"+id+"/pause", nil)
	f.clear(t, id)
	readFive(id, "cleared", 1, 0)
	p.open()
	next := newMCPPublishedPlan(t, "body")
	s.plan.Store(next)
	f.control(t, "PUT", "/api/v1/tasks/"+id+"/continue", nil)
	next.waitEntered(t)
	f.wait(t, "/"+id+"/stats", "resumed HTTP publisher", publishedStatsPositive)
	readFive(id, "active", 1, 0)
	next.open()
	f.wait(t, "/"+id, "done", func(v map[string]any) bool { return v["status"] == "done" })
	f.clear(t, id)
	readFive(id, "cleared", 1, 0)
	auth := newMCPPublishedPlan(t, "auth")
	s.plan.Store(auth)
	lower := time.Now().UnixMilli()
	failed := f.create(t, s.server.URL+"/SYNTHETIC_CLI_AUTH", "auth-cli.bin")
	f.wait(t, "/"+failed+"/errors", "real error before stdio", func(v map[string]any) bool { return v["available"] == true })
	readFive(failed, "authentication", 2, lower)
}

func TestMCPPublishedFixtureCleanup(t *testing.T) {
	oldD, oldServer, oldPort, oldAES := Downloader, srv, runningPort, append([]byte(nil), aesKey...)
	var root string
	t.Run("empty_private_core", func(t *testing.T) {
		f := newMCPPublishedFixture(t)
		root = f.root
		v := f.read(t, "?limit=100")
		if len(v["tasks"].([]any)) != 0 {
			t.Fatal("fixture seeded task data")
		}
	})
	if Downloader != oldD || srv != oldServer || runningPort != oldPort || !bytes.Equal(aesKey, oldAES) {
		t.Fatal("fixture polluted server globals")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("fixture storage/listener directory retained")
	}
}
