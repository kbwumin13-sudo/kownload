package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
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
func mediaBridgeFixture(t *testing.T, ctx context.Context) (model.Config, context.Context, *mediaBridgeOwner) {
	t.Helper()
	python := os.Getenv("GOPEED_MEDIA_PYTHON")
	if python == "" {
		t.Fatal("set GOPEED_MEDIA_PYTHON to the fixed release interpreter")
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(entry, []byte("#!"+python+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	owner := &mediaBridgeOwner{ctx: ctx}
	lease := &transfer.RecoveryLease{Owner: owner, Token: transfer.RecoveryToken{TaskID: "media-fixture", Episode: "episode", Generation: 1, SourceRevision: 1}}
	return model.Config{YtDlpPath: entry}, mediaWorkContext(transfer.WithRecoveryLease(ctx, lease), "media/download"), owner
}

const bridgeMain = "yt_dlp.main(context['args'] + ['--downloader', 'native'])"

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
	cfg, ctx, owner := mediaBridgeFixture(t, ctx)
	run := func(script string, wantFailure bool) error {
		cmd, cleanup, err := mediaCommand(ctx, cfg, &base.Request{URL: server.URL}, &model.ReqExtra{}, []string{server.URL + "/redirect"})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		cmd.Args[2] = strings.Replace(cmd.Args[2], bridgeMain, script, 1)
		setProcessGroup(cmd)
		cmd.Cancel = func() error { return killProcessGroup(cmd) }
		out, err := cmd.CombinedOutput()
		if err != nil && !wantFailure {
			t.Fatalf("Python bridge: %v\n%s", err, out)
		}
		return err
	}
	script := `from yt_dlp.networking.impersonate import ImpersonateTarget
for options in ({'quiet': True, 'proxy': ''}, {'quiet': True, 'proxy': '', 'impersonate': ImpersonateTarget()}):
    with yt_dlp.YoutubeDL(options) as ydl:
        with ydl.urlopen(context['args'][0]) as response:
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
	failureScript := `with yt_dlp.YoutubeDL({'quiet': True, 'proxy': ''}) as ydl:
    try:
        ydl.urlopen(context['args'][0].replace('/redirect', '/failure'))
    except Exception:
        pass
    ydl.urlopen(context['args'][0])
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
	cfg, ctx, owner := mediaBridgeFixture(t, ctx)
	cmd, cleanup, err := mediaCommand(ctx, cfg, &base.Request{URL: server.URL}, &model.ReqExtra{}, []string{server.URL})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Args[2] = strings.Replace(cmd.Args[2], bridgeMain, `with yt_dlp.YoutubeDL({'quiet': True, 'proxy': ''}) as ydl:
    with ydl.urlopen(context['args'][0]) as response:
        response.read()
`, 1)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
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
