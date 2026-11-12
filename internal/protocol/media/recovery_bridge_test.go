package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

type mediaBridgeOwner struct {
	ctx             context.Context
	entered, active atomic.Int64
	deny            atomic.Bool
}

func (o *mediaBridgeOwner) CurrentRecoveryRound(context.Context, transfer.RecoveryToken) (uint64, error) {
	return 0, nil
}
func (o *mediaBridgeOwner) CheckRecovery(transfer.RecoveryToken) error { return o.ctx.Err() }
func (o *mediaBridgeOwner) RetryRecovery(context.Context, transfer.RecoveryToken, uint64, string, []string) (*transfer.RecoveryPermit, error) {
	return nil, transfer.ErrRecoveryWork
}
func (o *mediaBridgeOwner) FinishRecovery(context.Context, transfer.RecoveryToken, uint64, string) error {
	return nil
}
func (o *mediaBridgeOwner) ReserveRecoveryAction(context.Context, transfer.RecoveryToken, uint64, transfer.RecoveryActionKind, string, []string, time.Time) (*transfer.RecoveryActionPermit, error) {
	return nil, transfer.ErrRecoveryWork
}
func (o *mediaBridgeOwner) BindRecoveryChild(context.Context, transfer.RecoveryToken, string, *transfer.HTTPExecutionContext) (*transfer.RecoveryLease, error) {
	return nil, transfer.ErrRecoveryWork
}
func (o *mediaBridgeOwner) CommitRecoverySource(context.Context, transfer.RecoveryToken, uint64, transfer.RecoverySourceSeal, json.RawMessage) (*transfer.RecoveryLease, error) {
	return nil, transfer.ErrRecoveryWork
}
func (o *mediaBridgeOwner) EnterRecoveryWork(ctx context.Context, _ transfer.RecoveryToken, work string) (*transfer.RecoveryWorkScope, error) {
	if o.deny.Load() || (work != "media/inspect" && work != "media/download") {
		return nil, transfer.ErrRecoveryWork
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.entered.Add(1)
	o.active.Add(1)
	return transfer.IssueRecoveryWorkScope(ctx, func(ctx context.Context) error { return ctx.Err() }, func() { o.active.Add(-1) }), nil
}

// Exercise the embedded bridge against real stdlib HTTP connections in ordinary
// CI. This requires Python, not a private yt-dlp/curl SDK. The packaged yt-dlp
// adapters are covered separately by the controlled SDK/Core integration run.
func mediaBridgeFixture(t *testing.T, ctx context.Context) (string, context.Context, *mediaBridgeOwner) {
	t.Helper()
	python := os.Getenv("GOPEED_MEDIA_PYTHON")
	if python == "" {
		python = "python3"
	}
	python, err := exec.LookPath(python)
	if err != nil {
		t.Fatalf("real Python interpreter required: %v", err)
	}
	owner := &mediaBridgeOwner{ctx: ctx}
	lease := &transfer.RecoveryLease{Owner: owner, Token: transfer.RecoveryToken{TaskID: "media-fixture", Episode: "episode", Generation: 1, SourceRevision: 1}}
	return python, mediaWorkContext(transfer.WithRecoveryLease(ctx, lease), "media/download"), owner
}

func mediaBridgeCommand(t *testing.T, ctx context.Context, python, url, script string) (*exec.Cmd, func()) {
	t.Helper()
	const prelude = `import json, os, sys, threading
import urllib.request, urllib.error
context = {'recovery': {'read': 3, 'write': 4}}
`
	const install = "\ninstall_recovery_bridge(urllib.request, None, urllib.error.HTTPError, urllib.error.URLError)\nopener = urllib.request.build_opener(urllib.request.ProxyHandler({}))\n"
	cmd := exec.CommandContext(ctx, python, "-c", prelude+recoveryWrapper+install+script, url)
	bridge, err := newMediaRecoveryBridge(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	return cmd, bridge.Close
}

func TestMediaRecoveryBridgeRequests(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/failure" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/body", http.StatusFound)
			return
		}
		w.Header().Set("Content-Length", "4")
		io.WriteString(w, "data")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	python, ctx, owner := mediaBridgeFixture(t, ctx)
	run := func(script string, wantFailure bool) error {
		cmd, cleanup := mediaBridgeCommand(t, ctx, python, server.URL+"/redirect", script)
		defer cleanup()
		out, err := cmd.CombinedOutput()
		if err != nil && !wantFailure {
			t.Fatalf("Python bridge: %v\n%s", err, out)
		}
		return err
	}
	script := `for _ in range(2):
    with opener.open(sys.argv[1]) as response:
        assert response.read() == b'data'
`
	if err := run(script, false); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 || owner.entered.Load() != 4 || owner.active.Load() != 0 {
		t.Fatalf("hops=%d admitted=%d active=%d", requests.Load(), owner.entered.Load(), owner.active.Load())
	}
	owner.deny.Store(true)
	if run(script, true) == nil {
		t.Fatal("denied request succeeded")
	}
	if requests.Load() != 4 || owner.active.Load() != 0 {
		t.Fatal("denial performed IO or leaked scope")
	}
	owner.deny.Store(false)
	failureScript := `try:
    opener.open(sys.argv[1].replace('/redirect', '/failure'))
except Exception:
    pass
opener.open(sys.argv[1])
`
	if run(failureScript, true) == nil {
		t.Fatal("failed hop was swallowed")
	}
	if requests.Load() != 5 || owner.active.Load() != 0 {
		t.Fatal("failed process performed a free fallback or leaked scope")
	}
}
func TestMediaRecoveryBridgeCancel(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(200)
		io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	python, ctx, owner := mediaBridgeFixture(t, ctx)
	cmd, cleanup := mediaBridgeCommand(t, ctx, python, server.URL, `with opener.open(sys.argv[1]) as response:
    response.read()
`)
	done := make(chan error, 1)
	go func() { _, err := cmd.CombinedOutput(); done <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		cleanup()
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancel returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process/pipe did not join")
	}
	cleanup()
	if owner.active.Load() != 0 {
		t.Fatalf("active scopes after child join: %d", owner.active.Load())
	}
}
