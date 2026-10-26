package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/test"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// pooledController wires a fetcher to a shared pool and budget the way the
// downloader does.
func pooledController(pool *transfer.TransportPool, budget *transfer.Budget, tune *tuningConfig) *controller.Controller {
	fm := new(FetcherManager)
	ctl := controller.NewController()
	ctl.GetConfig = func(v any) {
		cfg := fm.DefaultConfig().(*config)
		cfg.Tuning = tune
		_ = json.Unmarshal([]byte(test.ToJson(cfg)), v)
	}
	ctl.GetProxyPolicy = func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string) {
		return nil, "direct"
	}
	ctl.Transports = pool
	ctl.Budget = budget
	return ctl
}

func newPooledFetcher(ctl *controller.Controller) *Fetcher {
	f := new(FetcherManager).Build().(*Fetcher)
	f.Setup(ctl)
	return f
}

func boolPtr(v bool) *bool { return &v }

type connCounter struct{ opened atomic.Int64 }

func (c *connCounter) hook(_ net.Conn, state http.ConnState) {
	if state == http.StateNew {
		c.opened.Add(1)
	}
}

// slowWriter paces ServeContent so downloads last long enough to expand.
type slowWriter struct {
	http.ResponseWriter
	delay time.Duration
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseWriter.Write(p)
}

func servePayload(data []byte, declare bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if declare {
			http.ServeContent(&slowWriter{ResponseWriter: w, delay: 2 * time.Millisecond}, r, "payload.bin",
				time.Unix(1700000000, 0), bytes.NewReader(data))
			return
		}
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: 2 * time.Millisecond}, r, "payload.bin",
			time.Unix(1700000000, 0), bytes.NewReader(data))
	}
}

func download(t *testing.T, f *Fetcher, rawURL, dir, name string, connections int, skipVerify bool) error {
	t.Helper()
	opts := &base.Options{Path: dir, Name: name, Extra: &fhttp.OptsExtra{Connections: connections}}
	if err := f.Resolve(&base.Request{URL: rawURL, SkipVerifyCert: skipVerify}, opts); err != nil {
		return err
	}
	if err := f.Start(); err != nil {
		return err
	}
	select {
	case err := <-f.doneCh:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("download did not finish")
		return nil
	}
}

func checkFile(t *testing.T, path string, data []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("content mismatch at %s: bytes=%d err=%v", filepath.Base(path), len(got), err)
	}
}

func TestPooledSmallFilesReuseOneConnection(t *testing.T) {
	data := rangeProbePayload(48 * 1024)
	var counter connCounter
	srv := httptest.NewUnstartedServer(servePayload(data, true))
	srv.Config.ConnState = counter.hook
	srv.Start()
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	ctl := pooledController(pool, budget, nil)
	dir := t.TempDir()
	var last *fhttp.Stats
	for i := 0; i < 100; i++ {
		f := newPooledFetcher(ctl)
		name := "small-" + strconv.Itoa(i) + ".bin"
		if err := download(t, f, srv.URL+"/"+name, dir, name, 4, false); err != nil {
			t.Fatal(err)
		}
		checkFile(t, filepath.Join(dir, name), data)
		last = f.Stats().(*fhttp.Stats)
	}
	if got := counter.opened.Load(); got != 1 {
		t.Fatalf("100 sequential same-origin small files opened %d connections, want 1", got)
	}
	if last.ReusedConnections < 1 || last.NewConnections != 0 || last.Protocol != "HTTP/1.1" || last.Phase != "done" {
		t.Fatalf("last download should run on a reused connection: %+v", last)
	}
}

func TestPooledDownloadNegotiatesHTTP2WhenEnabled(t *testing.T) {
	data := rangeProbePayload(4 * 1024 * 1024)
	for _, h2server := range []bool{true, false} {
		t.Run(fmt.Sprintf("server_h2=%t", h2server), func(t *testing.T) {
			var counter connCounter
			srv := httptest.NewUnstartedServer(servePayload(data, true))
			srv.Config.ConnState = counter.hook
			srv.EnableHTTP2 = h2server
			srv.StartTLS()
			defer srv.Close()
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			defer budget.Close()
			f := newPooledFetcher(pooledController(pool, budget, &tuningConfig{HTTP2: boolPtr(true)}))
			if err := download(t, f, srv.URL+"/payload.bin", t.TempDir(), "payload.bin", 4, true); err != nil {
				t.Fatal(err)
			}
			checkFile(t, f.Meta().SingleFilepath(), data)
			stats := f.Stats().(*fhttp.Stats)
			want := "HTTP/1.1"
			if h2server {
				want = "HTTP/2.0"
			}
			if stats.Protocol != want || stats.RangeState != rangeDeclared {
				t.Fatalf("negotiated %q range %q, want %q", stats.Protocol, stats.RangeState, want)
			}
			if len(stats.Connections) < 2 || counter.opened.Load() < 2 {
				t.Fatalf("parallel streams must keep separate connections: streams=%d conns=%d",
					len(stats.Connections), counter.opened.Load())
			}
		})
	}
}

// rateLimitServer answers the first `limited` range requests with 429 and
// records when every request arrived.
type rateLimitServer struct {
	mu        sync.Mutex
	data      []byte
	limited   int
	retry     string
	times     []time.Time
	limitedAt []time.Time
}

func (s *rateLimitServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	now := time.Now()
	s.times = append(s.times, now)
	limit := r.Header.Get("Range") != "" && s.limited > 0
	if limit {
		s.limited--
		s.limitedAt = append(s.limitedAt, now)
	}
	s.mu.Unlock()
	if limit {
		if s.retry != "" {
			w.Header().Set("Retry-After", s.retry)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	http.ServeContent(&slowWriter{ResponseWriter: w, delay: 2 * time.Millisecond}, r, "payload.bin",
		time.Unix(1700000000, 0), bytes.NewReader(s.data))
}

func TestRetryAfterCoolsDownTheWholeSource(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	limited := &rateLimitServer{data: data, limited: 1, retry: "2"}
	srv := httptest.NewServer(limited)
	defer srv.Close()
	other := httptest.NewServer(servePayload(data, true))
	defer other.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	ctl := pooledController(pool, budget, nil)
	f := newPooledFetcher(ctl)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- download(t, f, srv.URL+"/payload.bin", t.TempDir(), "limited.bin", 4, false) }()
	// While the first source cools down, another source downloads normally.
	time.Sleep(300 * time.Millisecond)
	g := newPooledFetcher(ctl)
	if err := download(t, g, other.URL+"/payload.bin", t.TempDir(), "other.bin", 4, false); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 1800*time.Millisecond {
		t.Fatal("an unrelated source waited for another source's cooldown")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	checkFile(t, f.Meta().SingleFilepath(), data)
	limited.mu.Lock()
	defer limited.mu.Unlock()
	if len(limited.limitedAt) != 1 {
		t.Fatalf("expected one rate-limited request, got %d", len(limited.limitedAt))
	}
	for _, at := range limited.times {
		if at.After(limited.limitedAt[0]) && at.Sub(limited.limitedAt[0]) < 1900*time.Millisecond {
			t.Fatalf("request %v after the 429 came before Retry-After elapsed", at.Sub(limited.limitedAt[0]))
		}
	}
	stats := f.Stats().(*fhttp.Stats)
	if stats.RateLimits != 1 || stats.Timings.CooldownMs < 1500 {
		t.Fatalf("rate limit not reflected in stats: %+v %+v", stats, stats.Timings)
	}
}

func TestLongRetryAfterDefersTheRun(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	limited := &rateLimitServer{data: data, limited: 1000, retry: "120"}
	srv := httptest.NewServer(limited)
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	start := time.Now()
	err := download(t, f, srv.URL+"/payload.bin", t.TempDir(), "deferred.bin", 4, false)
	deferred, ok := transfer.AsDeferred(err)
	if !ok || deferred.Status != http.StatusTooManyRequests {
		t.Fatalf("expected a deferral, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a long Retry-After must release the run instead of waiting")
	}
	if wait := time.Until(deferred.Until); wait < 110*time.Second || wait > 121*time.Second {
		t.Fatalf("deferral until %v from now", wait)
	}
	if f.liveStreams.Load() != 0 || f.getState() != stateError || f.Stats().(*fhttp.Stats).Phase != "deferred" {
		t.Fatal("streams must stop and the run report a deferral")
	}
	limited.mu.Lock()
	requests := len(limited.times)
	limited.mu.Unlock()
	if requests > 3 {
		t.Fatalf("a rate-limited source got %d requests instead of a single wait", requests)
	}
	if !budget.Cooldown(f.sourceKey()).Equal(deferred.Until) {
		t.Fatal("the source cooldown must carry the server's wait")
	}
}

func TestResolveRateLimitDefersAndLaterRequestsWait(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	var first atomic.Bool
	data := rangeProbePayload(1024 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if !first.Swap(true) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	ctl := pooledController(pool, budget, nil)
	f := newPooledFetcher(ctl)
	err := f.Resolve(&base.Request{URL: srv.URL + "/a"}, rangeProbeOptions(t))
	if d, ok := transfer.AsDeferred(err); !ok || d.Status != http.StatusServiceUnavailable {
		t.Fatalf("first-request 503 must defer, got %v", err)
	}
	g := newPooledFetcher(ctl)
	if err := g.Resolve(&base.Request{URL: srv.URL + "/b"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	defer g.Pause()
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 || times[1].Sub(times[0]) < 900*time.Millisecond {
		t.Fatal("a second task contacted the source during its cooldown")
	}
}

func TestPauseEndsCooldownWaitPromptly(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	limited := &rateLimitServer{data: data, limited: 1000, retry: "20"}
	srv := httptest.NewServer(limited)
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.Stats().(*fhttp.Stats).Phase != "cooldown" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if phase := f.Stats().(*fhttp.Stats).Phase; phase != "cooldown" {
		t.Fatalf("phase %q, want cooldown", phase)
	}
	paused := time.Now()
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(paused); took > time.Second {
		t.Fatalf("pause waited %v for a cooldown", took)
	}
	if f.liveStreams.Load() != 0 || f.member.Load() != nil {
		t.Fatal("pause must stop streams and leave the budget")
	}
}

func TestStatsExplainRangeDecisions(t *testing.T) {
	large := rangeProbePayload(2 * 1024 * 1024)
	small := rangeProbePayload(64 * 1024)
	cases := []struct {
		name, state, reason string
		data                []byte
		handler             http.HandlerFunc
		active              int
	}{
		{"declared", rangeDeclared, "", large, servePayload(large, true), 0},
		{"probed", rangeProbed, "", large, servePayload(large, false), 0},
		{"small", rangeNotProbed, "small", small, servePayload(small, false), 0},
		{"ignored", rangeUnsupported, "ignored", large, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(large)))
			_, _ = w.Write(large)
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			defer budget.Close()
			f := newPooledFetcher(pooledController(pool, budget, nil))
			if err := download(t, f, srv.URL+"/payload.bin", t.TempDir(), "payload.bin", 4, false); err != nil {
				t.Fatal(err)
			}
			checkFile(t, f.Meta().SingleFilepath(), tc.data)
			stats := f.Stats().(*fhttp.Stats)
			if stats.RangeState != tc.state || stats.RangeReason != tc.reason || stats.ConfiguredStreams != 4 {
				t.Fatalf("range %q/%q configured %d, want %q/%q/4", stats.RangeState, stats.RangeReason,
					stats.ConfiguredStreams, tc.state, tc.reason)
			}
			if tc.active == 1 && len(stats.Connections) != 1 {
				t.Fatalf("configured 4 but ran %d streams; stats must show the single stream", len(stats.Connections))
			}
			raw, _ := json.Marshal(stats)
			for _, secret := range []string{srv.URL, "127.0.0.1"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatalf("stats leak request details: %s", raw)
				}
			}
		})
	}
}

func TestStatsReportProbeTimeout(t *testing.T) {
	data := rangeProbePayload(1024 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	f := buildFetcher()
	defer f.Pause()
	start := time.Now()
	if err := f.Resolve(&base.Request{URL: srv.URL}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > rangeProbeTimeout+500*time.Millisecond {
		t.Fatalf("probe exceeded its budget: %v", took)
	}
	stats := f.Stats().(*fhttp.Stats)
	if stats.RangeState != rangeProbeFailed || stats.RangeReason != "timeout" || f.Meta().Res.Range {
		t.Fatalf("probe timeout reported as %q/%q", stats.RangeState, stats.RangeReason)
	}
}

// Shedding a stream hands its remaining range back without losing or double
// counting bytes, whether a helper or a new stream finishes it.
func TestParkedRangesCompleteExactly(t *testing.T) {
	data := rangeProbePayload(8 * 1024 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: 2 * time.Millisecond}, r,
			"payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	f := buildFetcher()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	parked := 0
	deadline := time.Now().Add(10 * time.Second)
	for parked < 3 && time.Now().Before(deadline) {
		if f.parkOne() {
			parked++
		}
		time.Sleep(30 * time.Millisecond)
	}
	if parked == 0 {
		t.Fatal("no stream could be shed")
	}
	select {
	case err := <-f.doneCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("download with parked ranges did not finish")
	}
	checkFile(t, f.Meta().SingleFilepath(), data)
	if got := f.Progress().TotalDownloaded(); got != int64(len(data)) {
		t.Fatalf("progress %d after parking, want exactly %d", got, len(data))
	}
	f.connMu.Lock()
	defer f.connMu.Unlock()
	for _, c := range f.connections {
		if c.parked || c.Chunk.remain() > 0 {
			t.Fatalf("connection %d left work behind: parked=%t remain=%d", c.ID, c.parked, c.Chunk.remain())
		}
	}
}

func TestPausedBudgetMembersReleasePermits(t *testing.T) {
	data := rangeProbePayload(8 * 1024 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: 10 * time.Millisecond}, r,
			"payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	ctl := pooledController(pool, budget, nil)
	var fetchers []*Fetcher
	for i := 0; i < 3; i++ {
		f := newPooledFetcher(ctl)
		if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, &base.Options{
			Path: t.TempDir(), Name: "p.bin", Extra: &fhttp.OptsExtra{Connections: 4}}); err != nil {
			t.Fatal(err)
		}
		if err := f.Start(); err != nil {
			t.Fatal(err)
		}
		fetchers = append(fetchers, f)
	}
	time.Sleep(200 * time.Millisecond)
	probe := budget.Join(fetchers[0].sourceKey(), 1, false)
	defer probe.Leave()
	if snap := probe.Snapshot(); snap.SourceActive == 0 || snap.SourceActive > 12 {
		t.Fatalf("three tasks of 4 streams hold %d streams", snap.SourceActive)
	}
	for _, f := range fetchers {
		start := time.Now()
		if err := f.Pause(); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("pause took longer than a second")
		}
	}
	if snap := probe.Snapshot(); snap.SourceActive != 0 || snap.Members != 1 {
		t.Fatalf("permits leaked after pause: %+v", snap)
	}
	if stats := pool.Stats(); stats.Leased != 0 {
		t.Fatalf("transport leases leaked after pause: %+v", stats)
	}
	// The paused downloads resume to the correct content.
	for _, f := range fetchers {
		if err := f.Start(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-f.doneCh:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("resume did not finish")
		}
		checkFile(t, f.Meta().SingleFilepath(), data)
	}
}

func TestRequestErrorKeepsRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"7"}}}
	re := newResponseError(resp)
	if re.Code != 429 || !re.HasRetryAfter || re.RetryAfter != 7*time.Second {
		t.Fatalf("%+v", re)
	}
	if re := newResponseError(&http.Response{StatusCode: 500, Header: http.Header{"Retry-After": {"7"}}}); re.HasRetryAfter {
		t.Fatal("only 429/503 carry a retry wait")
	}
	var target *RequestError
	if !errors.As(fmt.Errorf("wrapped: %w", re), &target) {
		t.Fatal("RequestError must stay unwrappable")
	}
	_ = context.Background()
}

func TestShortCooldownYieldsSlotWhenTasksQueue(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			limited := &rateLimitServer{data: data, limited: 1, retry: "3"}
			srv := httptest.NewServer(limited)
			defer srv.Close()
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			defer budget.Close()
			ctl := pooledController(pool, budget, nil)
			ctl.QueueWaiting = func() bool { return queued }
			f := newPooledFetcher(ctl)
			start := time.Now()
			err := download(t, f, srv.URL+"/payload.bin", t.TempDir(), "short.bin", 4, false)
			if queued {
				if _, ok := transfer.AsDeferred(err); !ok || time.Since(start) > 2*time.Second {
					t.Fatalf("with tasks queued a 3s cooldown must release the slot at once: %v", err)
				}
				return
			}
			if err != nil || time.Since(start) < 2900*time.Millisecond {
				t.Fatalf("without a queue the task waits in place and completes: %v after %v", err, time.Since(start))
			}
			checkFile(t, f.Meta().SingleFilepath(), data)
		})
	}
}
