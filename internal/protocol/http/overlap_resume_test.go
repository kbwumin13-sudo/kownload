package http

import (
	"bytes"
	"fmt"
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

func serveUnvalidatedRange(t *testing.T, w http.ResponseWriter, r *http.Request, data []byte, limit int, slow bool) {
	t.Helper()
	begin, end := 0, len(data)-1
	ranged := r.Header.Get("Range") != ""
	if ranged {
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end >= len(data) || begin > end {
			t.Error("invalid synthetic range")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(data)))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
	if ranged {
		w.WriteHeader(206)
	}
	written := 0
	for offset := begin; offset <= end; {
		if limit > 0 && written >= limit {
			panic(http.ErrAbortHandler)
		}
		last := min(offset+16*1024, end+1)
		if _, err := w.Write(data[offset:last]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		written += last - offset
		offset = last
		if slow {
			time.Sleep(time.Millisecond)
		}
	}
}

func TestNoValidatorAutomaticRetry(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	var ranges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := 64 * 1024
		if r.Header.Get("Range") != "" {
			limit = 0
			if ranges.Add(1) == 1 {
				limit = 128 * 1024
			}
		}
		serveUnvalidatedRange(t, w, r, payload, limit, false)
	}))
	defer server.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: server.URL}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	if f.hasResumeValidator() {
		t.Fatal("fixture unexpectedly has a validator")
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || !bytes.Equal(got, payload) || ranges.Load() < 2 {
		t.Fatalf("automatic retry did not preserve bytes: ranges=%d error=%v", ranges.Load(), err)
	}
}

func TestNoValidatorPauseResumeOverlap(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint("changed=", changed), func(t *testing.T) {
			payload := bytes.Repeat([]byte("A"), 4<<20)
			var replacement atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := payload
				if replacement.Load() {
					data = bytes.Repeat([]byte("B"), len(payload))
				}
				serveUnvalidatedRange(t, w, r, data, 0, true)
			}))
			defer server.Close()
			f := buildFetcher()
			defer f.Close()
			if err := f.Resolve(&base.Request{URL: server.URL}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for f.Progress().TotalDownloaded() < 256*1024 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := f.Pause(); err != nil {
				t.Fatal(err)
			}
			if f.Progress().TotalDownloaded() <= 0 || f.Progress().TotalDownloaded() >= int64(len(payload)) {
				t.Fatal("fixture did not pause mid-transfer")
			}
			before, err := os.ReadFile(f.Meta().SingleFilepath())
			if err != nil {
				t.Fatal(err)
			}
			replacement.Store(changed)
			err = f.Start()
			if changed {
				if err == nil {
					t.Fatal("changed overlap was accepted")
				}
				after, _ := os.ReadFile(f.Meta().SingleFilepath())
				if !bytes.Equal(before, after) {
					t.Fatal("overlap refusal changed old bytes")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := waitDone(t, f, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(f.Meta().SingleFilepath())
			if !bytes.Equal(got, payload) {
				t.Fatal("resumed bytes differ")
			}
			if !strings.Contains(f.Stats().(*fhttp.Stats).RangeReason, "仅按重叠比对确认") {
				t.Fatal("overlap-only diagnostic missing")
			}
		})
	}
}

func TestNoValidatorRestoredPatchChecks64KiB(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	prefix := 128*1024 + 37
	var overlaps atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", prefix-64*1024, prefix-1) {
			overlaps.Add(1)
		}
		serveUnvalidatedRange(t, w, r, payload, 0, false)
	}))
	defer server.Close()
	f := representationPrefix(t, "http://127.0.0.1:1/expired", nil, payload)
	f.connections[0].Downloaded = int64(prefix)
	f.connections[0].Chunk.Downloaded = int64(prefix)
	if err := os.WriteFile(f.Meta().SingleFilepath(), payload[:prefix], 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Patch(&base.Request{URL: server.URL + "/renewed"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f.Meta().SingleFilepath())
	if overlaps.Load() != 1 || !bytes.Equal(got, payload) {
		t.Fatalf("restored patch: overlaps=%d", overlaps.Load())
	}
}
