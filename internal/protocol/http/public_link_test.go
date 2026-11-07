package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// All named HTTPS hosts terminate at a private TLS server through this local
// CONNECT proxy. No DNS lookup or public-site request is made by these tests.
func publicLinkProxy(t *testing.T, handler http.Handler) *url.URL {
	t.Helper()
	target := httptest.NewTLSServer(handler)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(400)
			return
		}
		conn, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			conn.Close()
			return
		}
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer client.Close(); defer conn.Close(); io.Copy(conn, client) }()
		go func() { defer client.Close(); defer conn.Close(); io.Copy(client, conn) }()
	}))
	t.Cleanup(func() { proxy.Close(); target.Close() })
	u, _ := url.Parse(proxy.URL)
	return u
}

func TestPublicLinkRefreshPreservesRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		changed, refuseAll, pauseRefresh, restore bool
	}{
		{name: "stable resource"},
		{name: "pause store restore", restore: true},
		{name: "changed resource", changed: true},
		{name: "refresh exhausted", refuseAll: true},
		{name: "pause during refresh", pauseRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("A"), 2<<20)
			var dispatches, oldRanges, newRanges atomic.Int32
			refreshEntered := make(chan struct{})
			proxy := publicLinkProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/dispatcher" {
					n := dispatches.Add(1)
					if n == 2 && tc.pauseRefresh {
						close(refreshEntered)
						<-r.Context().Done()
						return
					}
					fmt.Fprint(w, `{"body":{"weblink_get":[{"url":"https://cloclo1.cloud.mail.ru/public/synthetic/g/no"}]}}`)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/public/synthetic/g/no/") {
					w.Header().Set("Location", fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file", dispatches.Load()))
					w.WriteHeader(301)
					return
				}
				if !strings.HasPrefix(r.URL.Path, "/public/get/synthetic/") {
					w.WriteHeader(404)
					return
				}
				old := strings.Contains(r.URL.Path, "/synthetic/1/")
				begin, end := 0, len(payload)-1
				ranged := r.Header.Get("Range") != ""
				limit := len(payload)
				if ranged {
					if old {
						if oldRanges.Add(1) > 1 || tc.refuseAll {
							w.WriteHeader(403)
							return
						}
						limit = 128 << 10
					} else {
						newRanges.Add(1)
						if tc.refuseAll {
							w.WriteHeader(403)
							return
						}
					}
					if r.Header.Get("If-Range") != `"stable"` {
						t.Error("range lost persisted validator")
					}
					fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
				}
				etag := `"stable"`
				body := payload
				if !old && tc.changed {
					etag = `"changed"`
					body = bytes.Repeat([]byte("B"), len(payload))
				}
				w.Header().Set("ETag", etag)
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
				if ranged {
					w.WriteHeader(206)
				}
				for n := begin; n <= end; {
					if n-begin >= limit {
						panic(http.ErrAbortHandler)
					}
					last := min(n+4096, end+1, begin+limit)
					if _, err := w.Write(body[n:last]); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					n = last
					time.Sleep(time.Millisecond)
				}
			}))
			f := buildFetcher()
			f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) { return http.ProxyURL(proxy) }
			defer func() { f.Close() }()
			if err := f.Resolve(&base.Request{URL: "https://cloud.mail.ru/public/aaaa/bbbb", SkipVerifyCert: true}, &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			if tc.restore {
				deadline := time.Now().Add(3 * time.Second)
				for f.Progress().TotalDownloaded() < 32<<10 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				data, err := (&FetcherManager{}).Store(f)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(data)
				value, restore := (&FetcherManager{}).Restore()
				if err := json.Unmarshal(encoded, value); err != nil {
					t.Fatal(err)
				}
				ctl := f.ctl
				meta := f.Meta()
				f.Close()
				f = restore(meta, value).(*Fetcher)
				f.Setup(ctl)
				if err := f.Start(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pauseRefresh {
				select {
				case <-refreshEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("refresh did not start")
				}
				started := time.Now()
				stored := make(chan error, 1)
				go func() { _, err := (&FetcherManager{}).Store(f); stored <- err }()
				select {
				case err := <-stored:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Store waited for an in-flight network refresh")
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				if time.Since(started) > time.Second {
					t.Fatal("Pause did not cancel public resolution")
				}
				return
			}
			err := waitDone(t, f, 8*time.Second)
			got, readErr := os.ReadFile(f.Meta().SingleFilepath())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.changed {
				if err == nil || bytes.Contains(got, []byte("B")) {
					t.Fatalf("changed resource wrote old file: %v", err)
				}
			} else if tc.refuseAll {
				if err == nil || !strings.Contains(err.Error(), "status=403") {
					t.Fatalf("refresh exhaustion lost refusal: %v", err)
				}
			} else if err != nil || !bytes.Equal(got, payload) || f.Progress().TotalDownloaded() != int64(len(payload)) {
				t.Fatalf("refreshed bytes=%d progress=%d err=%v", len(got), f.Progress().TotalDownloaded(), err)
			}
			if dispatches.Load() != 2 {
				t.Fatalf("source resolved %d times, want initial and one refresh", dispatches.Load())
			}
			if !tc.refuseAll && newRanges.Load() == 0 {
				t.Fatal("no resumed range reached fresh link")
			}
			stored, storeErr := (&FetcherManager{}).Store(f)
			if storeErr != nil || stored.(*fetcherData).PublicSource == nil || stored.(*fetcherData).PublicSource.ShareID != "aaaa/bbbb" {
				t.Fatal("public source identity was not persisted")
			}
		})
	}
}
