package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
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

// restoreRangeCheckpoint restores a checkpoint as build 25 stores it, without
// a saved representation. The file holds the bytes each connection counts in
// its chunk and any further saved ranges [begin, end).
func restoreRangeCheckpoint(t *testing.T, raw string, payload []byte, conns []*connection, saved ...[2]int64) *Fetcher {
	t.Helper()
	opts := &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: len(conns)}}
	meta := &fetcher.FetcherMeta{Req: &base.Request{URL: raw, Extra: &fhttp.ReqExtra{}}, Opts: opts,
		Res: &base.Resource{Range: true, Size: int64(len(payload)), Files: []*base.FileInfo{{Name: "synthetic.bin", Size: int64(len(payload))}}}}
	local := make([]byte, len(payload))
	for _, c := range conns {
		copy(local[c.Chunk.Begin:], payload[c.Chunk.Begin:c.Chunk.Begin+c.Chunk.Downloaded])
	}
	for _, r := range saved {
		copy(local[r[0]:r[1]], payload[r[0]:r[1]])
	}
	if err := os.WriteFile(meta.SingleFilepath(), local, 0600); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(&fetcherData{Connections: conns})
	value, restore := (&FetcherManager{}).Restore()
	if err := json.Unmarshal(encoded, value); err != nil {
		t.Fatal(err)
	}
	f := restore(meta, value).(*Fetcher)
	f.Setup(buildFetcher().ctl)
	t.Cleanup(func() { f.Close() })
	return f
}

// The primary stream finished its chunk and took the tail of a slower one, so
// its chunk now starts mid-file, right after the slower stream's unfinished
// range. The resume check compares saved bytes only; build 25 resumed this
// checkpoint, also when the source sends validators that it did not save.
func TestOverlapCheckReadsOnlySavedBytes(t *testing.T) {
	const MiB = 1 << 20
	for _, validated := range []bool{false, true} {
		t.Run(fmt.Sprint("validators=", validated), func(t *testing.T) {
			payload := rangeProbePayload(8 * MiB)
			var mu sync.Mutex
			var checks [][2]int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b, e int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &b, &e); err == nil && e-b+1 <= 64<<10 {
					mu.Lock()
					checks = append(checks, [2]int64{b, e + 1})
					mu.Unlock()
				}
				if validated {
					w.Header().Set("ETag", `"stable-v1"`)
					w.Header().Set("Last-Modified", "Mon, 02 Jan 2023 03:04:05 GMT")
				}
				serveUnvalidatedRange(t, w, r, payload, 0, false)
			}))
			defer server.Close()
			// The primary downloaded [0, 2 MiB), then took [6 MiB, 8 MiB) from
			// conn 1, which has downloaded 1 MiB of [2 MiB, 6 MiB).
			conns := []*connection{
				{ID: 0, Role: rolePrimary, Chunk: &chunk{Begin: 6 * MiB, End: 8*MiB - 1, Downloaded: 256 << 10}, Downloaded: 2*MiB + 256<<10},
				{ID: 1, Role: roleWorker, Chunk: &chunk{Begin: 2 * MiB, End: 6*MiB - 1, Downloaded: 1 * MiB}, Downloaded: 1 * MiB},
			}
			f := restoreRangeCheckpoint(t, server.URL+"/file", payload, conns, [2]int64{0, 2 * MiB})
			err := f.Start()
			if err == nil {
				err = waitDone(t, f, 10*time.Second)
			}
			if err != nil {
				t.Fatalf("unchanged resume refused: %v", err)
			}
			if got, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(got, payload) {
				t.Fatal("resumed bytes differ")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(checks) == 0 {
				t.Fatal("no saved overlap was compared")
			}
			for _, c := range checks {
				// Not downloaded before the resume: [3 MiB, 6 MiB) and [6.25 MiB, 8 MiB).
				if c[0] < 6*MiB && c[1] > 3*MiB || c[1] > 6*MiB+256<<10 {
					t.Fatalf("overlap check read unsaved bytes [%d, %d)", c[0], c[1])
				}
			}
		})
	}
}

// Black-box form: two streams, the primary steals from the slow one, Pause,
// then Start in the same process.
func TestPrimaryStealPauseResume(t *testing.T) {
	const MiB = 1 << 20
	payload := rangeProbePayload(8 * MiB)
	slowFrom := 3 * MiB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begin, end := 0, len(payload)-1
		if v := r.Header.Get("Range"); v != "" {
			fmt.Sscanf(v, "bytes=%d-%d", &begin, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		if r.Header.Get("Range") != "" {
			w.WriteHeader(206)
		}
		for off := begin; off <= end; {
			last := min(off+16*1024, end+1)
			if _, err := w.Write(payload[off:last]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			off = last
			if begin >= slowFrom {
				time.Sleep(30 * time.Millisecond)
			}
		}
	}))
	defer server.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: server.URL + "/file"}, &base.Options{Path: t.TempDir(), Name: "f.bin", Extra: &fhttp.OptsExtra{Connections: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	stole := func() bool {
		f.connMu.Lock()
		defer f.connMu.Unlock()
		for _, c := range f.connections {
			if c.Role == rolePrimary && c.Chunk != nil && c.Chunk.Begin >= int64(slowFrom) && c.Downloaded > c.Chunk.Downloaded {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(20 * time.Second)
	for !stole() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !stole() {
		t.Skip("primary did not steal in this run")
	}
	time.Sleep(300 * time.Millisecond)
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	err := f.Start()
	if err == nil {
		err = waitDone(t, f, 60*time.Second)
	}
	if err != nil {
		t.Fatalf("pause/resume after a steal refused: %v", err)
	}
	if got, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(got, payload) {
		t.Fatal("resumed bytes differ")
	}
}
