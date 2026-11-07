package http

import (
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/GopeedLab/gopeed/internal/transfer"
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
			if err == nil {
				err = waitDone(t, f, 5*time.Second)
			}
			if changed {
				if !errors.Is(err, ErrUnsafeResume) {
					t.Fatalf("changed overlap was accepted: %v", err)
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

// isOverlapCheck tells the resume check's small comparison requests from
// stream requests, which cover at least the rest of a 256 KiB chunk here.
func isOverlapCheck(r *http.Request) bool {
	var b, e int64
	_, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &b, &e)
	return err == nil && e-b+1 <= 64<<10
}

// oneSavedRange is a checkpoint whose single stream saved 512 KiB of 2 MiB.
func oneSavedRange(t *testing.T, raw string, payload []byte) *Fetcher {
	t.Helper()
	conns := []*connection{{ID: 0, Role: rolePrimary, Chunk: &chunk{Begin: 0, End: int64(len(payload)) - 1, Downloaded: 512 << 10}, Downloaded: 512 << 10}}
	return restoreRangeCheckpoint(t, raw, payload, conns)
}

// startAndWait starts f and returns the run's result.
func startAndWait(t *testing.T, f *Fetcher, limit time.Duration) error {
	t.Helper()
	if err := f.Start(); err != nil {
		return err
	}
	return waitDone(t, f, limit)
}

// Sixteen saved streams at a source with WAN latency: the resume compares a
// sample of three spans, each request with its own timeout, instead of every
// span under one three-second limit. build 25 resumed without a check.
func TestOverlapCheckSamplesWithOwnTimeouts(t *testing.T) {
	const MiB = 1 << 20
	payload := rangeProbePayload(16 * MiB)
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOverlapCheck(r) {
			checks.Add(1)
			time.Sleep(1200 * time.Millisecond) // three of them exceed the old 3 s limit
		}
		serveUnvalidatedRange(t, w, r, payload, 0, false)
	}))
	defer server.Close()
	var conns []*connection
	for i := 0; i < 16; i++ {
		begin := int64(i) * MiB
		conns = append(conns, &connection{ID: i, Role: roleWorker, Chunk: &chunk{Begin: begin, End: begin + MiB - 1, Downloaded: 512 << 10}, Downloaded: 512 << 10})
	}
	conns[0].Role = rolePrimary
	f := restoreRangeCheckpoint(t, server.URL+"/file", payload, conns)
	if err := startAndWait(t, f, 30*time.Second); err != nil {
		t.Fatalf("unchanged slow resume refused: %v", err)
	}
	checkFile(t, f.Meta().SingleFilepath(), payload)
	if n := checks.Load(); n != 3 {
		t.Fatalf("compared %d spans, want a sample of 3", n)
	}
}

// A rate limit at the resume check follows Retry-After and the source
// cooldown like any request. build 25 waited, then resumed.
func TestOverlapCheckRateLimitWaits(t *testing.T) {
	for _, tc := range []struct {
		status int
		budget bool
	}{{429, false}, {503, false}, {429, true}} {
		t.Run(fmt.Sprintf("status=%d/budget=%t", tc.status, tc.budget), func(t *testing.T) {
			payload := rangeProbePayload(2 << 20)
			var limited atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "" && limited.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(tc.status)
					return
				}
				serveUnvalidatedRange(t, w, r, payload, 0, false)
			}))
			defer server.Close()
			f := oneSavedRange(t, server.URL+"/file", payload)
			if tc.budget {
				budget := transfer.NewBudget()
				defer budget.Close()
				f.ctl.Budget = budget
			}
			started := time.Now()
			if err := startAndWait(t, f, 20*time.Second); err != nil {
				t.Fatalf("rate-limited resume check became terminal: %v", err)
			}
			if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
				t.Fatalf("Retry-After not honoured: resumed after %v", elapsed)
			}
			checkFile(t, f.Meta().SingleFilepath(), payload)
			if f.Stats().(*fhttp.Stats).RateLimits < 1 {
				t.Fatal("rate limit was not recorded")
			}
		})
	}
}

// A Retry-After too long to hold the task's running slot defers the run at
// the resume check, as for a stream; nothing is refused or written.
func TestOverlapCheckLongRateLimitDefers(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		serveUnvalidatedRange(t, w, r, payload, 0, false)
	}))
	defer server.Close()
	f := oneSavedRange(t, server.URL+"/file", payload)
	before, _ := os.ReadFile(f.Meta().SingleFilepath())
	budget := transfer.NewBudget()
	defer budget.Close()
	f.ctl.Budget = budget
	err := startAndWait(t, f, 10*time.Second)
	deferred, ok := transfer.AsDeferred(err)
	if !ok || errors.Is(err, ErrUnsafeResume) || time.Until(deferred.Until) < time.Minute {
		t.Fatalf("long rate limit at the resume check: %v", err)
	}
	if after, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(before, after) {
		t.Fatal("deferred check changed the file")
	}
}

// A network error at the resume check is retried; it says nothing about the
// saved bytes.
func TestOverlapCheckRetriesNetworkErrors(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOverlapCheck(r) && checks.Add(1) == 1 {
			panic(http.ErrAbortHandler) // connection closed without a response
		}
		serveUnvalidatedRange(t, w, r, payload, 0, false)
	}))
	defer server.Close()
	f := oneSavedRange(t, server.URL+"/file", payload)
	if err := startAndWait(t, f, 20*time.Second); err != nil {
		t.Fatalf("network error at the resume check became terminal: %v", err)
	}
	checkFile(t, f.Meta().SingleFilepath(), payload)
	if checks.Load() < 2 {
		t.Fatal("resume check was not retried")
	}
}

// Unusable answers are retried like a stream's counted failures, then end the
// task without claiming that the source changed. A full 200 says nothing
// about the saved offsets; a refusal keeps status=403, which wrappers use to
// refresh an expired link.
func TestOverlapCheckUnusableAnswersAreNotIdentityFailures(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	for _, tc := range []struct {
		name, want string
		status     int
	}{{"ignored range", "ignored the range request", http.StatusOK}, {"refused", "status=403", http.StatusForbidden}} {
		t.Run(tc.name, func(t *testing.T) {
			var checks atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				if tc.status == http.StatusOK {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					w.Write(payload)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			f := oneSavedRange(t, server.URL+"/file", payload)
			before, _ := os.ReadFile(f.Meta().SingleFilepath())
			err := startAndWait(t, f, 20*time.Second)
			if err == nil || errors.Is(err, ErrUnsafeResume) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unusable answer: %v", err)
			}
			if tc.status == http.StatusForbidden && !IsExpiredLinkError(err) {
				t.Fatal("refusal lost its status for link refresh")
			}
			if n := checks.Load(); n != 3 {
				t.Fatalf("requests=%d, want three attempts", n)
			}
			if after, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(before, after) {
				t.Fatal("failed check changed the file")
			}
		})
	}
}

// Other bytes or another total length in a legal answer refuse the resume at
// once and keep the file.
func TestOverlapCheckChangedSourceIsRefused(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	for _, mode := range []string{"bytes", "206 length", "416 length"} {
		t.Run(mode, func(t *testing.T) {
			var checks atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				switch mode {
				case "bytes":
					serveUnvalidatedRange(t, w, r, bytes.Repeat([]byte("B"), len(payload)), 0, false)
				case "206 length":
					var b, e int
					fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &b, &e)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", b, e, len(payload)+1))
					w.WriteHeader(http.StatusPartialContent)
					w.Write(payload[b : e+1])
				case "416 length":
					w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", 1024))
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				}
			}))
			defer server.Close()
			f := oneSavedRange(t, server.URL+"/file", payload)
			before, _ := os.ReadFile(f.Meta().SingleFilepath())
			if err := startAndWait(t, f, 10*time.Second); !errors.Is(err, ErrUnsafeResume) {
				t.Fatalf("changed source accepted: %v", err)
			}
			if checks.Load() != 1 {
				t.Fatalf("requests=%d, want a single refusal", checks.Load())
			}
			if !strings.Contains(f.Stats().(*fhttp.Stats).LastError, "preserved") {
				t.Fatal("identity refusal missing from task diagnostics")
			}
			if after, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(before, after) {
				t.Fatal("refusal changed the file")
			}
		})
	}
}

// Start returns before the resume check reaches the network, so the task
// locks held around Start never wait for it, and Pause stops it at once.
func TestOverlapCheckDoesNotBlockStartOrPause(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	f := oneSavedRange(t, server.URL+"/file", payload)
	before, _ := os.ReadFile(f.Meta().SingleFilepath())
	started := time.Now()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Start waited %v for the network", elapsed)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume check did not start")
	}
	paused := time.Now()
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(paused); elapsed > time.Second {
		t.Fatalf("Pause waited %v for the resume check", elapsed)
	}
	select {
	case err := <-f.doneCh:
		t.Fatalf("paused check reported %v", err)
	default:
	}
	if f.getState() != statePaused {
		t.Fatalf("state=%d after Pause", f.getState())
	}
	if after, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(before, after) {
		t.Fatal("paused check changed the file")
	}
}
