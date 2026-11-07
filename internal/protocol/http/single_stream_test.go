package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestSingleStreamEOFAndOneTimeResponse(t *testing.T) {
	for _, declared := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			for _, empty := range []bool{false, true} {
				t.Run(fmt.Sprintf("range=%t/known=%t/empty=%t", declared, known, empty), func(t *testing.T) {
					payload := []byte("synthetic stream with a clean EOF")
					if empty {
						payload = nil
					}
					var requests atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if requests.Add(1) != 1 {
							w.WriteHeader(http.StatusForbidden)
							return
						}
						if declared {
							w.Header().Set("Accept-Ranges", "bytes")
						}
						if known {
							w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
						} else {
							w.WriteHeader(http.StatusOK)
							w.(http.Flusher).Flush()
						}
						w.Write(payload)
					}))
					defer srv.Close()
					f := buildFetcher()
					defer f.Close()
					if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
						t.Fatal(err)
					}
					if err := f.Start(); err != nil {
						t.Fatal(err)
					}
					if err := waitDone(t, f, 3*time.Second); err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(f.Meta().SingleFilepath())
					if err != nil || !bytes.Equal(got, payload) || requests.Load() != 1 || f.Progress().TotalDownloaded() != int64(len(payload)) {
						t.Fatalf("one-time response: bytes=%d requests=%d progress=%d error=%v", len(got), requests.Load(), f.Progress().TotalDownloaded(), err)
					}
				})
			}
		}
	}
}

func TestSingleStreamFallbackMultipleRetries(t *testing.T) {
	payload := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUV")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		if n == 2 || n == 3 {
			w.Write(payload[:int(n)*2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		w.Write(payload)
	}))
	defer srv.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	// The missing saved response exercises the separate fallback retry loop.
	f.resolveRespLock.Lock()
	f.resolveResp.Body.Close()
	f.resolveResp = nil
	f.resolveRespLock.Unlock()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || !bytes.Equal(got, payload) || requests.Load() != 4 || f.Progress().TotalDownloaded() != int64(len(payload)) {
		t.Fatalf("full-GET retries: bytes=%d requests=%d progress=%d error=%v", len(got), requests.Load(), f.Progress().TotalDownloaded(), err)
	}
}

type singleStreamReader func([]byte) (int, error)

func (r singleStreamReader) Read(p []byte) (int, error) { return r(p) }

func TestSingleStreamWriteFailureIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.Write([]byte("data"))
	}))
	defer srv.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	f.resolveResp.Body.Close()
	f.resolveResp.Body = io.NopCloser(singleStreamReader(func(p []byte) (int, error) {
		f.fileMu.Lock()
		f.file.Close() // Fail WriteAt after the attempt's successful Truncate.
		f.fileMu.Unlock()
		return copy(p, "data"), io.EOF
	}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 3*time.Second); err == nil || !strings.Contains(err.Error(), "write download file") {
		t.Fatalf("write failure was not propagated: %v", err)
	}
	if f.Progress().TotalDownloaded() != 0 {
		t.Fatal("unwritten bytes were counted")
	}
}

func TestSingleStreamPauseDuringRetryReset(t *testing.T) {
	payload := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUV")
	var requests atomic.Int32
	retrying := make(chan struct{})
	allowHeaders := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 2 {
			close(retrying)
			<-allowHeaders
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		if n <= 2 {
			w.Write(payload[:4])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		w.Write(payload)
	}))
	defer srv.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if waitDone(t, f, 3*time.Second) == nil {
		t.Fatal("fixture must start with a short response")
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	<-retrying
	// The file is open but the response has no headers yet. Hold its lock
	// while letting reset and Pause meet, then allow both to finish.
	f.fileMu.Lock()
	close(allowHeaders)
	paused := make(chan error, 1)
	go func() { paused <- f.Pause() }()
	<-f.ctx.Done()
	f.fileMu.Unlock()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Pause did not join the reset attempt")
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || !bytes.Equal(got, payload) || f.Progress().TotalDownloaded() != int64(len(payload)) {
		t.Fatalf("pause/retry: bytes=%d progress=%d error=%v", len(got), f.Progress().TotalDownloaded(), err)
	}
}

func TestSingleStreamClosedFileStopsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("data")) }))
	defer srv.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	f.file = file
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &connection{ctx: ctx, Chunk: newChunk(0, 0)}
	f.runConnectionFallback(conn)
	if !conn.failed || !isLocalFileError(conn.lastErr) {
		t.Fatalf("closed disk was not terminal: %v", conn.lastErr)
	}
}
