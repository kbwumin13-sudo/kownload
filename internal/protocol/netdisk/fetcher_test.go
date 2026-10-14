package netdisk

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureProvider struct {
	url  string
	size int64
	hash string
}

func (*fixtureProvider) Info() model.ProviderInfo {
	return model.ProviderInfo{ID: "quark", Name: "fixture", Hosts: []string{"pan.quark.cn"}}
}
func (*fixtureProvider) List(context.Context, *cloud.APIClient, *cloud.Session, model.Source, string) (*model.Listing, error) {
	return nil, fmt.Errorf("unused")
}
func (p *fixtureProvider) Resolve(_ context.Context, _ *cloud.APIClient, _ *cloud.Session, _ model.Source, f model.File) (*cloud.Download, error) {
	f.Size = p.size
	f.Hash = p.hash
	return &cloud.Download{URL: p.url, File: f, Header: map[string]string{"Authorization": "Bearer secret-fixture"}}, nil
}
func (*fixtureProvider) Transfer(context.Context, *cloud.APIClient, *cloud.Session, model.Source, []model.File, string) (*model.TransferReceipt, error) {
	return nil, fmt.Errorf("unused")
}
func (*fixtureProvider) Cleanup(context.Context, *cloud.APIClient, *cloud.Session, *model.TransferReceipt) error {
	return fmt.Errorf("unused")
}
func fixture(t *testing.T, slow bool) (*FetcherManager, *base.Request, *base.Options, func()) {
	t.Helper()
	body := strings.Repeat("cloud-file-content", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		start, end := 0, len(body)-1
		if h := r.Header.Get("Range"); h != "" {
			fmt.Sscanf(h, "bytes=%d-%d", &start, &end)
			if end >= len(body) {
				end = len(body) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(206)
		}
		for n := start; n <= end; {
			last := n + 4096
			if last > end+1 {
				last = end + 1
			}
			if _, e := w.Write([]byte(body[n:last])); e != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			n = last
			if slow {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}))
	p := &fixtureProvider{url: srv.URL + "/signed-file?auth=secret-url", size: int64(len(body))}
	service := cloud.NewService(p)
	cap, e := service.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/shared", InstanceID: "browser", ContextID: "tab", AccountID: "account"})
	if e != nil {
		t.Fatal(e)
	}
	extra := &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "account", File: model.File{ID: "file", Name: "cloud.bin", Path: "folder", Size: int64(len(body))}}
	return &FetcherManager{Service: service}, &base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: extra}, &base.Options{Path: t.TempDir()}, srv.Close
}
func TestDownloadAndSnapshotKeepCredentialsOut(t *testing.T) {
	m, req, opts, stop := fixture(t, false)
	defer stop()
	f := m.Build().(*Fetcher)
	f.Setup(controller.NewController())
	defer f.Close()
	if e := f.Resolve(req, opts); e != nil {
		t.Fatal(e)
	}
	data, e := m.Store(f)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal([]any{f.Meta(), data})
	if strings.Contains(string(b), "secret-") {
		t.Fatal("authorization persisted")
	}
	if e = f.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download hung")
	}
	content, e := os.ReadFile(f.Meta().SingleFilepath())
	if e != nil {
		t.Fatal(e)
	}
	if string(content) != strings.Repeat("cloud-file-content", 20000) {
		t.Fatal("file content mismatch")
	}
}
func TestPauseRestoreNeedsSameAccountAndResumes(t *testing.T) {
	m, req, opts, stop := fixture(t, true)
	defer stop()
	f := m.Build().(*Fetcher)
	f.Setup(controller.NewController())
	if e := f.Resolve(req, opts); e != nil {
		t.Fatal(e)
	}
	if e := f.Start(); e != nil {
		t.Fatal(e)
	}
	time.Sleep(12 * time.Millisecond)
	paused := make(chan error, 1)
	go func() { paused <- f.Pause() }()
	select {
	case e := <-paused:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pause deadlocked")
	}
	data, e := m.Store(f)
	if e != nil {
		t.Fatal(e)
	}
	meta := clone(f.Meta())
	f.Close()
	m.Service.Close()
	newManager := &FetcherManager{Service: m.Service}
	v, restore := newManager.Restore()
	b, _ := json.Marshal(data)
	json.Unmarshal(b, v)
	restored := restore(meta, v).(*Fetcher)
	restored.Setup(controller.NewController())
	defer restored.Close()
	if e = restored.Start(); e != nil {
		t.Fatal(e)
	}
	if e = restored.Wait(); e == nil {
		t.Fatal("restored task reused old auth")
	}
	_, e = newManager.Service.Capture("", "", model.Capture{Provider: "quark", SourcePage: req.URL, InstanceID: "browser", ContextID: "fresh-tab", AccountID: "account"})
	if e != nil {
		t.Fatal(e)
	}
	if e = restored.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- restored.Wait() }()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resume hung")
	}
	content, e := os.ReadFile(restored.Meta().SingleFilepath())
	if e != nil || len(content) != int(meta.Res.Size) {
		t.Fatal(e, "bad restored size")
	}
}
func TestRedirectDoesNotForwardAuthorization(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.Header().Set("Content-Length", "3")
		w.Write([]byte("abc"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing scoped authorization")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	p := &fixtureProvider{url: origin.URL, size: 3}
	s := cloud.NewService(p)
	cap, _ := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/shared", InstanceID: "browser", ContextID: "tab", AccountID: "a"})
	m := &FetcherManager{Service: s}
	f := m.Build().(*Fetcher)
	f.Setup(controller.NewController())
	defer f.Close()
	e := f.Resolve(&base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "a", File: model.File{ID: "file", Name: "a", Size: 3}}}, &base.Options{Path: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	if leaked.Load() {
		t.Fatal("authorization crossed origins")
	}
}

var _ fetcher.Fetcher = (*Fetcher)(nil)
