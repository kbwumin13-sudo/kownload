package netdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolvedHashBecomesVerificationBaseline(t *testing.T) {
	h := sha256.Sum256([]byte("abc"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "3")
		w.Write([]byte("bad"))
	}))
	defer srv.Close()
	p := &fixtureProvider{url: srv.URL, size: 3, hash: "sha256:" + hex.EncodeToString(h[:])}
	s := cloud.NewService(p)
	cap, _ := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "browser", ContextID: "tab", AccountID: "account"})
	f := (&FetcherManager{Service: s}).Build().(*Fetcher)
	f.Setup(controller.NewController())
	defer f.Close()
	req := &base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "account", File: model.File{ID: "file", Name: "a", Size: 3}}}
	if e := f.Resolve(req, &base.Options{Path: t.TempDir()}); e != nil {
		t.Fatal(e)
	}
	if e := f.Start(); e != nil {
		t.Fatal(e)
	}
	if e := f.Wait(); e == nil || !strings.Contains(e.Error(), "校验") {
		t.Fatal("wrong content accepted", e)
	}
}
func TestRangePolicyRefusesFullResponseAndWrongOffset(t *testing.T) {
	r, _ := http.NewRequest("GET", "https://example.invalid", nil)
	r.Header.Set("Range", "bytes=5-9")
	check := rangeResponse(10)
	for _, c := range []struct {
		status int
		value  string
	}{{200, ""}, {206, "bytes 0-4/10"}, {206, "bytes 5-9/20"}} {
		res := &http.Response{StatusCode: c.status, Header: http.Header{"Content-Range": []string{c.value}}}
		if check(r, res) == nil {
			t.Fatalf("unsafe range accepted: %+v", c)
		}
	}
	if e := check(r, &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": []string{"bytes 5-9/10"}}}); e != nil {
		t.Fatal(e)
	}
}
func TestCloseWakesWaitAndPatchCannotChangeBatchIdentity(t *testing.T) {
	m, req, opts, stop := fixture(t, false)
	defer stop()
	f := m.Build().(*Fetcher)
	f.Setup(controller.NewController())
	if e := f.Resolve(req, opts); e != nil {
		t.Fatal(e)
	}
	bad := clone(f.Meta().Req)
	base.ParseReqExtra[model.ReqExtra](bad)
	bad.Extra.(*model.ReqExtra).BatchID = "other-batch"
	if f.Patch(bad, nil) == nil {
		t.Fatal("batch changed")
	}
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	if e := f.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("close signaled success")
		}
	case <-time.After(time.Second):
		t.Fatal("closed waiter leaked")
	}
	if f.Start() == nil {
		t.Fatal("closed fetcher restarted")
	}
}
func TestSafeErrorPreservesCancellationButNotSignedURL(t *testing.T) {
	e := safeError(&urlErrorForTest{context.Canceled})
	if strings.Contains(e.Error(), "signature") || strings.Contains(e.Error(), "https://") {
		t.Fatal(e)
	}
	if errors.Unwrap(e) == nil {
		t.Fatal("error cause discarded")
	}
}

type urlErrorForTest struct{ error }

func (e *urlErrorForTest) Error() string {
	return "GET https://cdn.invalid/file?signature=private: " + e.error.Error()
}

type blockingResolveProvider struct {
	fixtureProvider
	block   atomic.Bool
	entered chan struct{}
}

func (p *blockingResolveProvider) Resolve(ctx context.Context, c *cloud.APIClient, s *cloud.Session, src model.Source, file model.File) (*cloud.Download, error) {
	if p.block.Load() {
		close(p.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.fixtureProvider.Resolve(ctx, c, s, src, file)
}
func TestRefreshingTaskPollingDoesNotBlockPause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "3")
		w.Write([]byte("abc"))
	}))
	defer srv.Close()
	p := &blockingResolveProvider{fixtureProvider: fixtureProvider{url: srv.URL, size: 3}, entered: make(chan struct{})}
	s := cloud.NewService(p)
	cap, _ := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "browser", ContextID: "tab", AccountID: "account"})
	m := &FetcherManager{Service: s}
	f := m.Build().(*Fetcher)
	f.Setup(controller.NewController())
	defer f.Close()
	req := &base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "account", File: model.File{ID: "file", Name: "a", Size: 3}}}
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	p.block.Store(true)
	f.mu.Lock()
	f.refresh = true
	f.mu.Unlock()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	polled := make(chan error, 1)
	go func() { f.Progress(); f.Stats(); _, err := m.Store(f); polled <- err }()
	select {
	case err := <-polled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("polling blocked on network")
	}
	paused := make(chan error, 1)
	go func() { paused <- f.Pause() }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pause failed to cancel refresh")
	}
	if f.Progress().TotalDownloaded() != 0 {
		t.Fatal("cancelled refresh started downloading")
	}
}
