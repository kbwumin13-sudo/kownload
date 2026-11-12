//go:build !windows

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

// Real BuildServer, original authentication, original listener and a private
// synthetic Bolt store. No App, production paths, credential discovery or fetch.
type mcpBuiltFixture struct {
	root, socket string
	server       *http.Server
	client       *http.Client
	downloader   *download.Downloader
}

func newMCPBuiltFixture(t *testing.T, network, token string, web, webAuth bool, doneTotals ...int64) *mcpBuiltFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "kmr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	seed := download.NewBoltStorage(store)
	if err := seed.Setup([]string{"task"}); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	task := &download.Task{ID: "task_1", Protocol: "http", Status: base.DownloadStatusPause,
		CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 11, 0, 1, 0, 0, time.UTC),
		Progress: &download.Progress{Downloaded: 42},
		Meta:     &fetcher.FetcherMeta{Req: &base.Request{URL: "https://SYNTHETIC_PRIVATE.invalid"}, Opts: &base.Options{Path: root}, Res: &base.Resource{Size: 99}},
	}
	tasks := []*download.Task{task}
	for i, total := range doneTotals {
		tasks = append(tasks, &download.Task{
			ID: fmt.Sprintf("done_%d", i), Protocol: "http", Status: base.DownloadStatusDone,
			CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
			Progress: &download.Progress{Downloaded: total}, Meta: task.Meta,
		})
	}
	for _, seeded := range tasks {
		if err := seed.Put("task", seeded.ID, seeded); err != nil {
			seed.Close()
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	address := filepath.Join(root, "api.sock")
	if network == "tcp" {
		address = "127.0.0.1:0"
	}
	cfg := &model.StartConfig{Network: network, Address: address, ApiToken: token, Storage: model.StorageBolt, StorageDir: store, StartupTaskPolicy: "imported-review", WebEnable: web,
		WebFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("WEB_FALLBACK_SENTINEL")}},
	}
	if webAuth {
		cfg.WebAuth = &model.WebAuth{Username: "synthetic", Password: "SYNTHETIC_WEB_SECRET"}
	}
	oldDownloader, oldServer, oldAES := Downloader, srv, aesKey
	server, listener, err := BuildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := Downloader
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		server.Close()
		listener.Close()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("fixture server did not stop")
		}
		d.Close()
		Downloader, srv, aesKey = oldDownloader, oldServer, oldAES
	})
	return &mcpBuiltFixture{root: root, socket: address, server: server, client: client, downloader: d}
}

func (f *mcpBuiltFixture) request(t *testing.T, method, path, token, version string) (int, string) {
	t.Helper()
	r, err := http.NewRequest(method, "http://kownload.local"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("X-Api-Token", token)
	}
	if version != "" {
		r.Header.Set("X-Kownload-Read-Version", version)
	}
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SYNTHETIC_") || strings.Contains(string(data), f.root) {
		t.Fatal("response leaked fixture secret or path")
	}
	return response.StatusCode, string(data)
}

func TestMCPReadBuildServerAuthentication(t *testing.T) {
	for _, sample := range []struct {
		name, token, header string
		web, webAuth        bool
		status              int
	}{
		{name: "no-auth", status: 200},
		{name: "token-missing", token: "SYNTHETIC_API_SECRET", status: 401},
		{name: "token-wrong", token: "SYNTHETIC_API_SECRET", header: "SYNTHETIC_WRONG", status: 401},
		{name: "token-correct", token: "SYNTHETIC_API_SECRET", header: "SYNTHETIC_API_SECRET", status: 200},
		{name: "both-correct-api", token: "SYNTHETIC_API_SECRET", header: "SYNTHETIC_API_SECRET", web: true, webAuth: true, status: 200},
		{name: "both-missing", token: "SYNTHETIC_API_SECRET", web: true, webAuth: true, status: 401},
		{name: "both-wrong-api", token: "SYNTHETIC_API_SECRET", header: "SYNTHETIC_WRONG", web: true, webAuth: true, status: 401},
		{name: "web-only", web: true, webAuth: true, status: 401},
		{name: "web-only-api-header", header: "SYNTHETIC_API_SECRET", web: true, webAuth: true, status: 401},
		{name: "web-fallback", web: true, status: 200},
	} {
		t.Run(sample.name, func(t *testing.T) {
			f := newMCPBuiltFixture(t, "unix", sample.token, sample.web, sample.webAuth)
			status, body := f.request(t, "GET", mcpReadPrefix+"?limit=1", sample.header, "1")
			// The real core's progress loop briefly owns the membership lock.
			// Authentication success does not guarantee a TryLock snapshot on the
			// first attempt. Retry only that exact transient response, never auth
			// rejection, unavailable data or another 503, and keep a fixed bound.
			deadline := time.Now().Add(time.Second)
			for sample.status == http.StatusOK && status == http.StatusServiceUnavailable && body == "{\"version\":1,\"error\":\"snapshot_busy\"}\n" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				status, body = f.request(t, "GET", mcpReadPrefix+"?limit=1", sample.header, "1")
			}
			if status != sample.status {
				t.Fatalf("status=%d want=%d body=%q", status, sample.status, body)
			}
			if status == 200 && (!strings.Contains(body, `"version":1`) || !strings.Contains(body, `"id":"task_1"`)) {
				t.Fatal("read route lost to Web fallback")
			}
			// Bad protocol and write requests distinguish auth rejection from entering
			// the read handler: auth must run before version/method checks or getters.
			want := 409
			if sample.status == 401 {
				want = 401
			}
			if status, _ := f.request(t, "GET", mcpReadPrefix+"?limit=1", sample.header, ""); status != want {
				t.Fatalf("auth/version ordering: %d want %d", status, want)
			}
			want = 405
			if sample.status == 401 {
				want = 401
			}
			for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD"} {
				if status, _ := f.request(t, method, mcpReadPrefix+"/task_1", sample.header, "1"); status != want {
					t.Fatalf("auth/method ordering: %s %d want %d", method, status, want)
				}
			}
		})
	}
}

// S2 routes and the MCP read namespace must coexist before the Web fallback.
// Empty/invalid writes only exercise validation; no operation or task is created.
func TestMCPReadBuildServerS2RoutesCoexist(t *testing.T) {
	const token = "SYNTHETIC_API_SECRET"
	f := newMCPBuiltFixture(t, "unix", token, true, false)
	before, err := f.downloader.MCPReadList(context.Background(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		method, path, marker string
		status               int
	}{
		{"GET", "/api/v1/task-batches/operations/missing-fixture", `"state":"unknown"`, 200},
		{"POST", "/api/v1/task-batches/operations", "invalid_operation_request", 400},
		{"POST", "/api/v1/task-batches/operations/@/cancel", "invalid_operation_request", 400},
	} {
		if status, _ := f.request(t, sample.method, sample.path, "", ""); status != 401 {
			t.Fatalf("S2 route bypassed auth: %s %s status=%d", sample.method, sample.path, status)
		}
		status, body := f.request(t, sample.method, sample.path, token, "")
		if status != sample.status || !strings.Contains(body, sample.marker) {
			t.Fatalf("S2 route lost to fallback: %s %s status=%d", sample.method, sample.path, status)
		}
	}
	if status, body := f.request(t, "GET", mcpReadPrefix+"?limit=100", token, "1"); status != 200 || !strings.Contains(body, `"id":"task_1"`) {
		t.Fatal("MCP route lost after S2 requests")
	}
	after, err := f.downloader.MCPReadList(context.Background(), 100, "")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("route coexistence checks changed tasks")
	}
}

// A successful fixture baseline can encounter the production nonblocking read
// contract while the Core is sampling. Retry only typed Busy, for at most one
// second (or the caller's earlier deadline); other failures remain failures.
func readMCPFixtureTask(ctx context.Context, read func(context.Context, string) (download.MCPReadTask, error), id string) (download.MCPReadTask, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return download.MCPReadTask{}, err
		}
		value, err := read(ctx, id)
		if !errors.Is(err, download.ErrMCPReadBusy) {
			return value, err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return download.MCPReadTask{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func TestMCPReadFixtureTaskBusyContract(t *testing.T) {
	want := download.MCPReadTask{ID: "task_1", Status: "pause"}
	for _, sample := range []struct {
		name  string
		first error
		retry bool
	}{
		{"busy", download.ErrMCPReadBusy, true},
		{"wrapped_busy", fmt.Errorf("snapshot: %w", download.ErrMCPReadBusy), true},
		{"unavailable", download.ErrMCPReadUnavailable, false},
		{"invalid", download.ErrMCPReadInvalid, false},
		{"missing", download.ErrTaskNotFound, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"same_text_not_typed", errors.New(download.ErrMCPReadBusy.Error()), false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			calls := 0
			got, err := readMCPFixtureTask(context.Background(), func(ctx context.Context, id string) (download.MCPReadTask, error) {
				calls++
				if id != want.ID || ctx.Err() != nil {
					t.Fatal("read identity or context changed")
				}
				if calls == 1 {
					return download.MCPReadTask{}, sample.first
				}
				return want, nil
			}, want.ID)
			if sample.retry {
				if err != nil || calls != 2 || !reflect.DeepEqual(got, want) {
					t.Fatalf("transient typed Busy did not reach the unchanged snapshot: calls=%d err=%v", calls, err)
				}
			} else if err != sample.first || calls != 1 {
				t.Fatalf("non-Busy error was retried or replaced: calls=%d err=%v", calls, err)
			}
		})
	}
	t.Run("persistent_busy", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		calls := 0
		got, err := readMCPFixtureTask(ctx, func(context.Context, string) (download.MCPReadTask, error) {
			calls++
			return download.MCPReadTask{}, download.ErrMCPReadBusy
		}, want.ID)
		if !errors.Is(err, context.DeadlineExceeded) || calls == 0 || !reflect.DeepEqual(got, download.MCPReadTask{}) {
			t.Fatalf("persistent Busy did not fail at the deadline: calls=%d err=%v", calls, err)
		}
	})
	t.Run("already_cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := readMCPFixtureTask(ctx, func(context.Context, string) (download.MCPReadTask, error) {
			t.Fatal("cancelled baseline called the getter")
			return want, nil
		}, want.ID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
	})
}

func TestMCPReadBuildServerFiveReadsAndRecovery(t *testing.T) {
	f := newMCPBuiltFixture(t, "unix", "SYNTHETIC_API_SECRET", true, false)
	before, err := readMCPFixtureTask(context.Background(), f.downloader.MCPReadGetTask, "task_1")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{mcpReadPrefix + "?limit=1", mcpReadPrefix + "/task_1", mcpReadPrefix + "/task_1/progress", mcpReadPrefix + "/task_1/stats", mcpReadPrefix + "/task_1/errors"}
	for pass := 0; pass < 2; pass++ {
		for _, path := range paths {
			status, body := f.request(t, "GET", path, "SYNTHETIC_API_SECRET", "1")
			if status != 200 {
				t.Fatalf("read failed: %d", status)
			}
			if strings.HasSuffix(path, "stats") || strings.HasSuffix(path, "errors") {
				if !strings.Contains(body, `"available":false`) {
					t.Fatal("missing unavailable")
				}
			}
			if strings.HasSuffix(path, "progress") && !strings.Contains(body, `"totalBytes":null`) {
				t.Fatal("total guessed")
			}
		}
		if pass == 0 {
			if err := os.Remove(f.socket); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if info, err := os.Lstat(f.socket); err == nil && info.Mode()&os.ModeSocket != 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("UDS did not recover")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	after, err := readMCPFixtureTask(context.Background(), f.downloader.MCPReadGetTask, "task_1")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("reads/rebind changed task")
	}
}

func TestMCPReadBuildServerNotOnTCP(t *testing.T) {
	for _, web := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "web"}[web], func(t *testing.T) {
			f := newMCPBuiltFixture(t, "tcp", "SYNTHETIC_API_SECRET", web, false)
			status, body := f.request(t, "GET", mcpReadPrefix+"?limit=1", "SYNTHETIC_API_SECRET", "1")
			if strings.Contains(body, `"version":1`) || strings.Contains(body, `"tasks"`) {
				t.Fatal("MCP route exposed on TCP")
			}
			if !web && status != 404 {
				t.Fatalf("unexpected TCP status %d", status)
			}
		})
	}
}

// Opt-in with a real built stdio bridge and installed mcporter. The service is
// still the actual BuildServer chain backed exclusively by a synthetic store.
func TestMCPReadBuildServerCLI(t *testing.T) {
	binary := os.Getenv("KOWNLOAD_MCP_TEST_BINARY")
	if binary == "" {
		t.Skip("set KOWNLOAD_MCP_TEST_BINARY for real mcporter integration")
	}
	cli, err := exec.LookPath("mcporter")
	if err != nil {
		t.Fatal("mcporter required")
	}
	f := newMCPBuiltFixture(t, "unix", "SYNTHETIC_API_SECRET", true, true, 42, 0)
	file := filepath.Join(f.root, "token")
	if err := os.WriteFile(file, []byte("SYNTHETIC_API_SECRET\n"), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(f.root, "client-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(f.root, "mcporter.json")
	configure := func(withFile bool) {
		args := []string{"--socket", f.socket}
		if withFile {
			args = append(args, "--api-token-file", file)
		}
		data, _ := json.Marshal(map[string]any{"imports": []string{}, "mcpServers": map[string]any{"kownload": map[string]any{"command": binary, "args": args}}})
		if err := os.WriteFile(config, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, append([]string{"--config", config}, args...)...)
		cmd.Dir = f.root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "SYNTHETIC_") || (args[0] != "list" && strings.Contains(string(out), file)) {
			t.Fatal("CLI leaked token or token path")
		}
		if ctx.Err() != nil {
			t.Fatal("CLI timed out")
		}
		if err != nil && !strings.Contains(string(out), "app_authentication_failed") {
			t.Fatalf("CLI %s failed without leaking diagnostics: %T", args[0], err)
		}
		return string(out)
	}
	// Startup rejection must emit only a fixed diagnostic, before any protocol.
	for _, sample := range []struct {
		name, data string
		mode       os.FileMode
	}{
		{"empty", "", 0600}, {"public", "SYNTHETIC_API_SECRET", 0644}, {"injection", "SYNTHETIC_API_SECRET\r\nInjected:x", 0600},
	} {
		bad := filepath.Join(f.root, "bad-"+sample.name)
		if err := os.WriteFile(bad, []byte(sample.data), sample.mode); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, binary, "--socket", f.socket, "--api-token-file", bad)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || string(out) != "kownload-mcp: session ended\n" {
			t.Fatal("invalid credential startup did not fail with fixed diagnostic")
		}
	}
	configure(true)
	listed := invoke("list", "kownload", "--schema", "--json")
	// mcporter adds local startup command metadata in transport/source. It is
	// not the MCP tools/list result; only tools may be sent on to the model.
	var listing struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal([]byte(listed), &listing) != nil || len(listing.Tools) != 5 {
		t.Fatal("invalid tool listing")
	}
	toolBytes, _ := json.Marshal(listing.Tools)
	listed = string(toolBytes)
	if strings.Contains(listed, file) {
		t.Fatal("tool schema leaked credential path")
	}
	for _, name := range []string{"list_tasks", "get_task", "get_progress", "get_stats", "get_errors"} {
		if !strings.Contains(listed, "kownload_"+name) {
			t.Fatal("tool missing")
		}
	}
	if strings.Contains(listed, "api-token") || strings.Contains(listed, "kownload_create_task") {
		t.Fatal("credential or write tool exposed")
	}
	readFive := func() {
		for _, name := range []string{"list_tasks", "get_task", "get_progress", "get_stats", "get_errors"} {
			args := `{"id":"task_1"}`
			if name == "list_tasks" {
				args = `{"limit":1}`
			}
			out := invoke("call", "kownload.kownload_"+name, "--args", args, "--output", "json")
			var value map[string]any
			if json.Unmarshal([]byte(out), &value) != nil {
				t.Fatal("CLI result is not JSON")
			}
			switch name {
			case "list_tasks":
				if _, ok := value["tasks"].([]any); !ok {
					t.Fatal("task list unavailable")
				}
			case "get_task":
				if value["id"] != "task_1" {
					t.Fatal("wrong task")
				}
			case "get_progress":
				if value["totalBytes"] != nil || value["downloadedBytes"] != float64(42) {
					t.Fatal("progress mismatch")
				}
			default:
				if value["available"] != false {
					t.Fatal("unknown data claimed available")
				}
			}
		}
	}
	readFive()
	// Exercise non-null v1 totals through the real stdio bridge and mcporter,
	// including numeric zero (which must not become null/omitted).
	for i, want := range []int64{42, 0} {
		id := fmt.Sprintf("done_%d", i)
		args := fmt.Sprintf(`{"id":%q}`, id)
		for _, name := range []string{"get_task", "get_progress"} {
			out := invoke("call", "kownload.kownload_"+name, "--args", args, "--output", "json")
			var value map[string]any
			if json.Unmarshal([]byte(out), &value) != nil {
				t.Fatal("completed CLI result is not JSON")
			}
			if name == "get_task" {
				if value["id"] != id || value["status"] != "done" {
					t.Fatal("completed task identity/status mismatch")
				}
				var ok bool
				value, ok = value["progress"].(map[string]any)
				if !ok {
					t.Fatal("completed task progress missing")
				}
			}
			if value["totalBytes"] != float64(want) || value["downloadedBytes"] != float64(want) || value["phase"] != "done" {
				t.Fatal("completed CLI total lost numeric value")
			}
		}
	}
	// Keep the same configuration and stable credential path. Each CLI call
	// launches a new bridge, so replacing the file exercises restart semantics.
	originalConfig, _ := os.ReadFile(config)
	if err := os.WriteFile(file, []byte("SYNTHETIC_WRONG"), 0600); err != nil {
		t.Fatal(err)
	}
	out := invoke("call", "kownload.kownload_list_tasks", "--args", `{"limit":1}`, "--output", "json")
	if !strings.Contains(out, "app_authentication_failed") {
		t.Fatal("wrong token did not fail closed")
	}
	if err := os.WriteFile(file, []byte("SYNTHETIC_API_SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.socket); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Lstat(f.socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not recovered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	readFive()
	sameConfig, _ := os.ReadFile(config)
	if string(sameConfig) != string(originalConfig) {
		t.Fatal("client configuration changed")
	}
	configure(false)
	out = invoke("call", "kownload.kownload_list_tasks", "--args", `{"limit":1}`, "--output", "json")
	if !strings.Contains(out, "app_authentication_failed") {
		t.Fatal("missing token bypassed auth")
	}
}
