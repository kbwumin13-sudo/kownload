package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func rangeProbePayload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + i/251)
	}
	return data
}

func rangeProbeOptions(t *testing.T) *base.Options {
	t.Helper()
	return &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 4}}
}

func checkRangeProbeDownload(t *testing.T, f *Fetcher, data []byte) {
	t.Helper()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.doneCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download did not finish")
	}
	got, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("download content mismatch: bytes=%d, err=%v", len(got), err)
	}
}

// Hide ServeContent's declaration while preserving its real range behavior.
type hiddenRangeWriter struct {
	http.ResponseWriter
	wroteHeader bool
	delay       time.Duration
}

func (w *hiddenRangeWriter) WriteHeader(status int) {
	w.Header().Del("Accept-Ranges")
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *hiddenRangeWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if len(p) > 1 {
		time.Sleep(w.delay)
	}
	return w.ResponseWriter.Write(p)
}

func TestRangeProbeDownloadAndRestore(t *testing.T) {
	data := rangeProbePayload(8 * 1024 * 1024)
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%t", restore), func(t *testing.T) {
			var probes, ranged atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=0-0" {
					probes.Add(1)
				} else if r.Header.Get("Range") != "" {
					ranged.Add(1)
				}
				w.Header().Set("ETag", `"stable"`)
				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: time.Millisecond}, r,
					"payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
			}))
			defer server.Close()
			f := buildFetcher()
			defer func() { _ = f.Pause() }()
			if err := f.Resolve(&base.Request{URL: server.URL + "/payload.bin"}, rangeProbeOptions(t)); err != nil {
				t.Fatal(err)
			}
			if !f.Meta().Res.Range || probes.Load() != 1 {
				t.Fatalf("missing declaration was not probed: range=%t, probes=%d", f.Meta().Res.Range, probes.Load())
			}
			if restore {
				if err := f.Start(); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for (ranged.Load() < 2 || f.Progress().TotalDownloaded() == 0) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				before := f.Progress().TotalDownloaded()
				if before <= 0 || before >= int64(len(data)) {
					t.Fatalf("pause did not preserve a partial download: %d", before)
				}
				fm := &FetcherManager{}
				stored, err := fm.Store(f)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(struct {
					Meta *fetcher.FetcherMeta
					Data any
				}{f.Meta(), stored})
				if err != nil {
					t.Fatal(err)
				}
				var snapshot struct {
					Meta fetcher.FetcherMeta
					Data fetcherData
				}
				if err := json.Unmarshal(encoded, &snapshot); err != nil {
					t.Fatal(err)
				}
				_, restoreFetcher := fm.Restore()
				restored := restoreFetcher(&snapshot.Meta, &snapshot.Data).(*Fetcher)
				restored.Setup(f.ctl)
				f = restored
				if !f.Meta().Res.Range || f.Progress().TotalDownloaded() != before || snapshot.Data.RedirectURL == "" {
					t.Fatal("range capability, redirect or progress was lost in storage")
				}
			}
			checkRangeProbeDownload(t, f, data)
			if ranged.Load() < 2 || len(f.Stats().(*fhttp.Stats).Connections) < 2 || probes.Load() != 1 {
				t.Fatalf("expected multiple download connections and one probe: ranged=%d probes=%d", ranged.Load(), probes.Load())
			}
		})
	}
}

func TestRangeProbeFallbackPreservesOriginalStream(t *testing.T) {
	data := rangeProbePayload(1024 * 1024)
	for _, mode := range []string{"ignored", "forbidden", "single_use", "invalid", "wrong_total", "short", "long", "etag", "modified", "encoding", "type"} {
		t.Run(mode, func(t *testing.T) {
			var requests, probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := requests.Add(1)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("ETag", `"original"`)
				w.Header().Set("Last-Modified", "Tue, 14 Nov 2023 22:13:20 GMT")
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					_, _ = w.Write(data)
					return
				}
				probes.Add(1)
				if mode == "forbidden" || (mode == "single_use" && request > 1) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if mode == "ignored" {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					_, _ = w.Write(data)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(data)))
				body := data[:1]
				switch mode {
				case "invalid":
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 1-1/%d", len(data)))
				case "wrong_total":
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(data)+1))
				case "short":
					body = nil
				case "long":
					body = data[:2]
				case "etag":
					w.Header().Set("ETag", `"changed"`)
				case "modified":
					w.Header().Set("Last-Modified", "Wed, 15 Nov 2023 22:13:20 GMT")
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "type":
					w.Header().Set("Content-Type", "text/html")
				}
				w.WriteHeader(http.StatusPartialContent)
				// Use chunked framing to also test actual body size validation.
				w.(http.Flusher).Flush()
				_, _ = w.Write(body)
			}))
			defer server.Close()
			f := buildFetcher()
			defer f.Pause()
			if err := f.Resolve(&base.Request{URL: server.URL}, rangeProbeOptions(t)); err != nil {
				t.Fatal(err)
			}
			if f.Meta().Res.Range {
				t.Fatal("invalid probe enabled range downloads")
			}
			checkRangeProbeDownload(t, f, data)
			if requests.Load() != 2 || probes.Load() != 1 {
				t.Fatalf("fallback replayed the original stream: requests=%d probes=%d", requests.Load(), probes.Load())
			}
		})
	}
}

func TestRangeProbeEligibility(t *testing.T) {
	for _, mode := range []string{"post", "get_body", "preset_range", "single_connection", "small", "unknown", "none", "declared"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "none" {
					w.Header().Set("Accept-Ranges", "none")
				} else if mode == "declared" {
					w.Header().Set("Accept-Ranges", "bytes")
				}
				if mode != "unknown" {
					size := 1024 * 1024
					if mode == "small" {
						size--
					}
					w.Header().Set("Content-Length", strconv.Itoa(size))
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
			}))
			defer server.Close()
			req := &base.Request{URL: server.URL, Extra: &fhttp.ReqExtra{}}
			extra := req.Extra.(*fhttp.ReqExtra)
			opts := rangeProbeOptions(t)
			switch mode {
			case "post":
				extra.Method, extra.Body = "POST", "must not replay"
			case "get_body":
				extra.Body = "must not replay"
			case "preset_range":
				extra.Header = map[string]string{"Range": "bytes=0-9"}
			case "single_connection":
				opts.Extra.(*fhttp.OptsExtra).Connections = 1
			}
			f := buildFetcher()
			defer f.Pause()
			if err := f.Resolve(req, opts); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatalf("unsafe or unnecessary request was replayed: %d", requests.Load())
			}
		})
	}
}

func TestRangeProbeCancellationAndTimeout(t *testing.T) {
	data := rangeProbePayload(1024 * 1024)
	for _, mode := range []string{"cancel_headers", "cancel_body", "timeout_headers", "timeout_body"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					_, _ = w.Write(data)
					return
				}
				if strings.HasSuffix(mode, "body") {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(data)))
					w.WriteHeader(http.StatusPartialContent)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			f := buildFetcher()
			defer f.Pause()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resolved := make(chan error, 1)
			go func() {
				resolved <- f.ResolveContext(ctx, &base.Request{URL: server.URL}, rangeProbeOptions(t))
			}()
			<-started
			if strings.HasPrefix(mode, "cancel") {
				cancel()
			}
			deadline := time.Second
			if strings.HasPrefix(mode, "timeout") {
				deadline += rangeProbeTimeout
			}
			select {
			case err := <-resolved:
				if err != nil || f.Meta().Res.Range {
					t.Fatalf("probe cancellation did not fall back: range=%t err=%v", f.Meta().Res.Range, err)
				}
			case <-time.After(deadline):
				t.Fatal("probe did not stop within its bound")
			}
			checkRangeProbeDownload(t, f, data)
		})
	}
}

func TestRangeProbeRedirectCookiesAndCredentials(t *testing.T) {
	data := rangeProbePayload(4 * 1024 * 1024)
	for _, crossDomain := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross_domain=%t", crossDomain), func(t *testing.T) {
			var finalRequests atomic.Int32
			final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				finalRequests.Add(1)
				if crossDomain {
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Error("sensitive headers leaked across redirect domains")
					}
				} else {
					cookie, err := r.Cookie("session")
					if err != nil || cookie.Value != "issued" || r.Header.Get("Authorization") != "Bearer secret" {
						t.Error("probe lost redirect authentication or shared cookie jar")
					}
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: time.Millisecond}, r,
					"payload.bin", time.Time{}, bytes.NewReader(data))
			}))
			defer final.Close()
			redirectURL := final.URL
			if crossDomain {
				redirectURL = strings.Replace(redirectURL, "127.0.0.1", "localhost", 1)
			}
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "issued"})
				http.Redirect(w, r, redirectURL, http.StatusFound)
			}))
			defer origin.Close()
			f := buildFetcher()
			defer f.Pause()
			headers := map[string]string{"Authorization": "Bearer secret", "Cookie": "explicit=secret"}
			if err := f.Resolve(&base.Request{URL: origin.URL, Extra: &fhttp.ReqExtra{Header: headers}}, rangeProbeOptions(t)); err != nil {
				t.Fatal(err)
			}
			if !f.Meta().Res.Range || finalRequests.Load() != 2 || headers["Range"] != "" || headers["If-Range"] != "" {
				t.Fatal("probe failed or modified original request headers")
			}
			checkRangeProbeDownload(t, f, data)
			if finalRequests.Load() < 3 || len(f.connections) < 2 {
				t.Fatal("redirect credential scope was not exercised by range workers")
			}
		})
	}
}

func TestRangeProbeCookiesReachWorkers(t *testing.T) {
	data := rangeProbePayload(4 * 1024 * 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "issued"})
		} else if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "issued" {
			t.Error("range worker lost the resolve cookie")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: time.Millisecond}, r,
			"payload.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	f := buildFetcher()
	defer f.Pause()
	if err := f.Resolve(&base.Request{URL: server.URL}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if !f.meta.Res.Range {
		t.Fatal("cookie-dependent probe failed")
	}
	checkRangeProbeDownload(t, f, data)
}

func TestRangeProbeUsesProxyAndPolicies(t *testing.T) {
	var requests, policies atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Policy") != "applied" || r.URL.Host != "example.invalid" {
			t.Error("probe did not preserve request policy or proxy semantics")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Length", "1")
			w.Header().Set("Content-Range", "bytes 0-0/1048576")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{1})
			return
		}
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(rangeProbePayload(1024 * 1024))
	}))
	defer proxy.Close()
	f := buildFetcher()
	defer f.Pause()
	proxyURL, _ := url.Parse(proxy.URL)
	f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) { return http.ProxyURL(proxyURL) }
	f.SetRequestPolicy(func(r *http.Request) { r.Header.Set("X-Policy", "applied") })
	f.SetResponsePolicy(func(*http.Request, *http.Response) error { policies.Add(1); return nil })
	if err := f.Resolve(&base.Request{URL: "http://example.invalid/payload"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if !f.Meta().Res.Range || requests.Load() != 2 || policies.Load() != 2 {
		t.Fatal("probe did not reuse proxy and response policy")
	}
}

func TestRangeDownloadRejectsInvalidResponseBeforeWriting(t *testing.T) {
	for _, mode := range []string{"ignored", "wrong_begin", "wrong_total", "short_range", "malformed", "oversized_end", "valid"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				value := "bytes 100-199/1024"
				switch mode {
				case "ignored":
					_, _ = w.Write(bytes.Repeat([]byte{9}, 1024))
					return
				case "wrong_begin":
					value = "bytes 0-199/1024"
				case "wrong_total":
					value = "bytes 100-199/1025"
				case "short_range":
					value = "bytes 100-198/1024"
				case "malformed":
					value = "bytes +100-199/1024"
				case "oversized_end":
					value = "bytes 100-249/1024"
				}
				w.Header().Set("Content-Range", value)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(bytes.Repeat([]byte{9}, 150))
			}))
			defer server.Close()
			f := buildFetcher()
			f.meta.Req = &base.Request{URL: server.URL}
			f.meta.Res = &base.Resource{Range: true, Size: 1024}
			f.file = prefetchCopyFile(t, bytes.Repeat([]byte{1}, 1024))
			conn := &connection{ID: 0, Chunk: newChunk(100, 199), ctx: context.Background()}
			err := f.downloadChunkOnce(conn, f.buildClient(), make([]byte, 256))
			got, _ := os.ReadFile(f.file.Name())
			if mode == "valid" || mode == "oversized_end" {
				if err != nil || !bytes.Equal(got[100:200], bytes.Repeat([]byte{9}, 100)) || got[200] != 1 {
					t.Fatalf("valid range was rejected or wrote past the chunk: %v", err)
				}
			} else if !errors.Is(err, errInvalidRangeResponse) || !bytes.Equal(got, bytes.Repeat([]byte{1}, 1024)) {
				t.Fatalf("invalid range wrote data or did not fail: %v", err)
			}
		})
	}
}

func TestRangeDownloadInvalidResponsesHaveBoundedRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("ignored range"))
	}))
	defer server.Close()
	f := buildFetcher()
	f.meta.Req = &base.Request{URL: server.URL}
	f.meta.Res = &base.Resource{Range: true, Size: 1024}
	f.file = prefetchCopyFile(t, bytes.Repeat([]byte{1}, 1024))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &connection{ID: 0, Chunk: newChunk(100, 199), ctx: ctx}
	f.wg.Add(1)
	done := make(chan struct{})
	go func() { f.runConnection(conn); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("invalid range responses retried indefinitely")
	}
	if requests.Load() != 3 || conn.retryTimes != 3 || conn.State != connFailed || !errors.Is(conn.lastErr, errInvalidRangeResponse) {
		t.Fatalf("invalid range response did not exhaust three attempts: requests=%d state=%d retries=%d err=%v",
			requests.Load(), conn.State, conn.retryTimes, conn.lastErr)
	}
}

func TestParseContentRange(t *testing.T) {
	for _, value := range []string{"bytes */1024", "bytes 0-0/*", "bytes -1-0/1024", "bytes +0-0/1024", "bytes 1-0/1024", "bytes 0-1024/1024", "bytes 0-0/0", "bytes 0-0/9223372036854775808", "bytes 0-0/1024junk", "bytes 0-0/1024,bytes 2-3/1024"} {
		if _, _, _, valid := parseContentRange(value); valid {
			t.Errorf("accepted invalid Content-Range %q", value)
		}
	}
}

func TestRangeDownloadShortBodiesAreNotComplete(t *testing.T) {
	for _, mode := range []string{"declared_short", "truncated", "chunked_short"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", "bytes 100-199/1024")
				switch mode {
				case "declared_short":
					w.Header().Set("Content-Length", "50")
				case "truncated":
					w.Header().Set("Content-Length", "100")
				}
				w.WriteHeader(http.StatusPartialContent)
				w.(http.Flusher).Flush()
				_, _ = w.Write(bytes.Repeat([]byte{9}, 50))
			}))
			defer server.Close()
			f := buildFetcher()
			f.meta.Req = &base.Request{URL: server.URL}
			f.meta.Res = &base.Resource{Range: true, Size: 1024}
			f.file = prefetchCopyFile(t, bytes.Repeat([]byte{1}, 1024))
			conn := &connection{ID: 0, Chunk: newChunk(100, 199), ctx: context.Background()}
			err := f.downloadChunkOnce(conn, f.buildClient(), make([]byte, 256))
			if !errors.Is(err, errInvalidRangeResponse) || conn.Completed || conn.Chunk.remain() == 0 {
				t.Fatalf("short range body accepted as complete: %v", err)
			}
		})
	}
}
