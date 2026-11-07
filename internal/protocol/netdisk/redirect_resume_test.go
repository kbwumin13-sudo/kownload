package netdisk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpmodel "github.com/GopeedLab/gopeed/pkg/protocol/http"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type redirectResumeReader struct{ *bytes.Reader }

func (r redirectResumeReader) Read(p []byte) (int, error) {
	if len(p) > 8192 {
		p = p[:8192]
	}
	time.Sleep(time.Millisecond)
	return r.Reader.Read(p)
}

func TestRedirectedDownloadResumesAfterRestore(t *testing.T) {
	for _, restoreSaved := range []bool{false, true} {
		name := "fresh"
		if restoreSaved {
			name = "restored"
		}
		t.Run(name, func(t *testing.T) {
			body := make([]byte, 2*1024*1024+123)
			for i := range body {
				body[i] = byte((i*31 + i/251) % 251)
			}
			var restored atomic.Bool
			var originRanges, finalRanges atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/origin":
					if restored.Load() && r.Header.Get("Range") != "" {
						originRanges.Add(1)
					}
					http.Redirect(w, r, "/content", http.StatusFound)
				case "/content":
					if restored.Load() && r.Header.Get("Range") != "" {
						finalRanges.Add(1)
					}
					http.ServeContent(w, r, "remote.bin", time.Unix(1700000000, 0), redirectResumeReader{bytes.NewReader(body)})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			hash := sha256.Sum256(body)
			p := &fixtureProvider{url: srv.URL + "/origin", size: int64(len(body)), hash: "sha256:" + hex.EncodeToString(hash[:])}
			service := cloud.NewService(p)
			t.Cleanup(service.Close)
			cap, err := service.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/redirect-fixture", InstanceID: "fixture", ContextID: "fixture", AccountID: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			m := &FetcherManager{Service: service}
			req := &base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "fixture", File: model.File{ID: "file", Name: "renamed.bin", Path: "folder", Size: int64(len(body)), Hash: p.hash}}}
			f := m.Build().(*Fetcher)
			f.Setup(controller.NewController())
			t.Cleanup(func() { _ = f.Close() })
			if err := f.Resolve(req, &base.Options{Path: t.TempDir(), Extra: &httpmodel.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			if restoreSaved {
				if err := f.Start(); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for f.Progress().TotalDownloaded() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				if got := f.Progress().TotalDownloaded(); got <= 0 || got >= int64(len(body)) {
					t.Fatalf("fixture did not pause mid-file: %d", got)
				}
				data, err := m.Store(f)
				if err != nil {
					t.Fatal(err)
				}
				meta := clone(f.Meta())
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				template, restore := m.Restore()
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, template); err != nil {
					t.Fatal(err)
				}
				restored.Store(true)
				f = restore(meta, template).(*Fetcher)
				f.Setup(controller.NewController())
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- f.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("download failed: %v; restored origin Range requests=%d, final Range requests=%d", err, originRanges.Load(), finalRanges.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("download stalled: restored origin Range requests=%d, final Range requests=%d; stats=%+v", originRanges.Load(), finalRanges.Load(), f.Stats())
			}
			if restoreSaved && (originRanges.Load() == 0 || finalRanges.Load() == 0) {
				t.Fatalf("restore did not exercise redirected Range: origin=%d, final=%d", originRanges.Load(), finalRanges.Load())
			}
			got, err := os.ReadFile(f.Meta().SingleFilepath())
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("redirected download content changed: %v", err)
			}
		})
	}
}
