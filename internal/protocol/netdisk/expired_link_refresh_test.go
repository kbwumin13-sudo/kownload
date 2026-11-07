package netdisk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpmodel "github.com/GopeedLab/gopeed/pkg/protocol/http"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// relinkProvider signs a new download link on every resolve.
type relinkProvider struct {
	fixtureProvider
	links atomic.Int32
}

func (p *relinkProvider) Resolve(_ context.Context, _ *cloud.APIClient, _ *cloud.Session, _ model.Source, f model.File) (*cloud.Download, error) {
	f.Size = p.size
	return &cloud.Download{URL: fmt.Sprintf("%s?link=%d", p.url, p.links.Add(1)), File: f}, nil
}

// The first link expires mid-download: its only stream breaks off and every
// later request is refused with 403. The HTTP child must report the 403 so the
// task refreshes its link and resumes the saved ranges instead of failing with
// a bare "download incomplete".
func TestExpiredLinkRefreshesAndResumes(t *testing.T) {
	// Above the size a first response may finish alone, so ranges are used.
	body := strings.Repeat("cloud-file-content", 100000)
	var firstLinkRanges, secondLinkRanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		link := r.URL.Query().Get("link")
		start, end := 0, len(body)-1
		raw := r.Header.Get("Range")
		limit := len(body)
		if raw != "" {
			switch link {
			case "1":
				if firstLinkRanges.Add(1) > 1 {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				limit = len(body) / 3
			case "2":
				secondLinkRanges.Add(1)
			}
			fmt.Sscanf(raw, "bytes=%d-%d", &start, &end)
			if end >= len(body) {
				end = len(body) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		if raw != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		for n := start; n <= end && n-start < limit; {
			last := min(n+4096, end+1, start+limit)
			if _, err := w.Write([]byte(body[n:last])); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			n = last
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	p := &relinkProvider{fixtureProvider: fixtureProvider{url: srv.URL + "/signed-file", size: int64(len(body))}}
	service := cloud.NewService(p)
	defer service.Close()
	capture, err := service.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/shared",
		InstanceID: "browser", ContextID: "tab", AccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	extra := &model.ReqExtra{Provider: "quark", Source: capture.Source, SessionID: capture.SessionID, AccountID: "account",
		File: model.File{ID: "file", Name: "cloud.bin", Path: "folder", Size: int64(len(body))}}
	req := &base.Request{URL: capture.Source.URL, Protocol: "netdisk", Extra: extra}
	opts := &base.Options{Path: t.TempDir(), Extra: &httpmodel.OptsExtra{Connections: 1}}

	f := (&FetcherManager{Service: service}).Build().(*Fetcher)
	f.Setup(controller.NewController())
	defer f.Close()
	if err := f.Resolve(req, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("download did not finish")
	}
	if err != nil {
		t.Fatalf("expired link was not refreshed: %v", err)
	}
	if p.links.Load() < 2 || secondLinkRanges.Load() == 0 {
		t.Fatalf("finished without resuming on a refreshed link: links=%d ranges on new link=%d", p.links.Load(), secondLinkRanges.Load())
	}
	content, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || string(content) != body {
		t.Fatalf("content mismatch after refresh: bytes=%d err=%v", len(content), err)
	}
}
