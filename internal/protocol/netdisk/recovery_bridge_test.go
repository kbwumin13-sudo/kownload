package netdisk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type lateRecoveryProvider struct {
	fixtureProvider
	block atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *lateRecoveryProvider) Resolve(ctx context.Context, client *cloud.APIClient, session *cloud.Session, source model.Source, file model.File) (*cloud.Download, error) {
	if p.block.Load() {
		close(p.entered)
		// Deliberately return a successful link after cancellation. The parent
		// must reject this result before it constructs another HTTP child.
		<-p.release
	}
	return p.fixtureProvider.Resolve(ctx, client, session, source, file)
}

func TestCancelledProviderRefreshCannotPublishLateChild(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", "3")
		_, _ = w.Write([]byte("abc"))
	}))
	t.Cleanup(server.Close)
	provider := &lateRecoveryProvider{fixtureProvider: fixtureProvider{url: server.URL, size: 3}, entered: make(chan struct{}), release: make(chan struct{})}
	service := cloud.NewService(provider)
	t.Cleanup(service.Close)
	capture, err := service.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "fixture", ContextID: "fixture", AccountID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	f := (&FetcherManager{Service: service}).Build().(*Fetcher)
	f.Setup(controller.NewController())
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(provider.release) }); _ = f.Close() })
	req := &base.Request{URL: capture.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: capture.Source, SessionID: capture.SessionID, AccountID: "fixture", File: model.File{ID: "file", Name: "a", Size: 3}}}
	if err = f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	original := f.child
	f.refresh = true
	f.mu.Unlock()
	before := requests.Load()
	provider.block.Store(true)
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("provider refresh did not start")
	}
	paused := make(chan error, 1)
	go func() { paused <- f.Pause() }()
	// CancelResolve observes the same operation context as Pause; the late
	// provider remains under our control until both cancellation and invalidation.
	f.CancelResolve()
	f.mu.Lock()
	if f.child != original {
		t.Error("child changed while provider was blocked")
	}
	f.mu.Unlock()
	release.Do(func() { close(provider.release) })
	select {
	case err = <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pause did not join the cancelled refresh")
	}
	f.mu.Lock()
	unchanged := f.child == original && !f.running
	f.mu.Unlock()
	if !unchanged || requests.Load() != before {
		t.Fatalf("late provider published or sent HTTP: same child=%v HTTP before=%d after=%d", unchanged, before, requests.Load())
	}
}
