package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// A range parked during a cooldown is resumed by exactly one stream after a
// pause, so its bytes are fetched once and land in the right place.
func TestParkedRangeIsNotDownloadedTwiceAfterPause(t *testing.T) {
	data := rangeProbePayload(4 * 1024 * 1024)
	var mu sync.Mutex
	starts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.Header.Get("Range"); raw != "" {
			mu.Lock()
			starts[raw]++
			mu.Unlock()
		}
		http.ServeContent(&slowWriter{ResponseWriter: w, delay: 6 * time.Millisecond}, r, "payload.bin",
			time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	// Another task's 429 cools the source down just before this one starts.
	budget.RateLimited(f.sourceKey(), 429, 1500*time.Millisecond, true)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	f.connMu.Lock()
	parked := len(f.connections) == 1 && f.connections[0].parked
	f.connMu.Unlock()
	if !parked {
		t.Fatal("the first range should be parked during the cooldown")
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	// Another task joining and leaving the source wakes every waiter, which
	// must not launch the resumed range a second time.
	for i := 0; i < 3; i++ {
		time.Sleep(100 * time.Millisecond)
		budget.Join(f.sourceKey(), 1, false).Leave()
	}
	select {
	case err := <-f.doneCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("resumed download did not finish")
	}
	checkFile(t, f.Meta().SingleFilepath(), data)
	if got := f.Progress().TotalDownloaded(); got != int64(len(data)) {
		t.Fatalf("progress %d, want %d", got, len(data))
	}
	mu.Lock()
	defer mu.Unlock()
	for raw, n := range starts {
		if n > 1 {
			t.Fatalf("range %s was requested %d times", raw, n)
		}
	}
}

// A single-stream body cut off mid-way is an error, not a finished download.
func TestTruncatedSingleStreamIsNotReportedDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1024*1024))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 100*1024))
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer srv.Close()
	f := buildFetcher()
	opts := &base.Options{Path: t.TempDir(), Name: "cut.bin", Extra: &fhttp.OptsExtra{Connections: 1}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/cut.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.doneCh:
		if err == nil {
			t.Fatal("a truncated download was reported complete")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no result")
	}
}

func TestResolveCooldownWaitEndsOnPause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may reach a cooling source")
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	f.meta.Req = &base.Request{URL: srv.URL + "/f.bin"}
	budget.RateLimited(f.sourceKey(), 429, 20*time.Second, true)
	done := make(chan error, 1)
	go func() { done <- f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)) }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	_ = f.Pause()
	select {
	case err := <-done:
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("cancelled wait returned %v after %v", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pause could not end the resolve cooldown wait")
	}
}

func TestResumeInsideLongCooldownDefersAtOnce(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	srv := httptest.NewServer(servePayload(data, true))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	until := budget.RateLimited(f.sourceKey(), 503, 2*time.Minute, true)
	start := time.Now()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.doneCh:
		deferred, ok := transfer.AsDeferred(err)
		if !ok || !deferred.Until.Equal(until) || time.Since(start) > 2*time.Second {
			t.Fatalf("expected an immediate deferral until %v, got %v", until, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a run inside a long cooldown kept its slot")
	}
}

func TestRepeatedDeferralsGiveUp(t *testing.T) {
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	f.meta.Req = &base.Request{URL: "http://limited.test/f.bin"}
	re := &RequestError{Code: 429, RetryAfter: time.Hour, HasRetryAfter: true}
	for i := 1; i <= maxDeferrals; i++ {
		if _, deferred, giveUp := f.rateLimit(re, i); deferred == nil || giveUp {
			t.Fatalf("deferral %d: deferred=%v giveUp=%t", i, deferred, giveUp)
		}
	}
	if _, deferred, giveUp := f.rateLimit(re, maxDeferrals+1); deferred != nil || !giveUp {
		t.Fatal("deferrals beyond the cap must give up and report the rate limit")
	}
	f.deferrals.Store(0) // a successful response resets the count
	if _, deferred, _ := f.rateLimit(re, 1); deferred == nil {
		t.Fatal("deferrals resume after a success")
	}
}

func TestShedStreamReturnsItsReservationOnce(t *testing.T) {
	budget := transfer.NewBudget()
	defer budget.Close()
	key := transfer.SourceKey("origin|http://shed.test|direct")
	m := budget.Join(key, 4, true)
	defer m.Leave()
	other := budget.Join(key, 4, true)
	defer other.Leave()
	f := buildFetcher()
	f.member.Store(m)
	conn := &connection{ID: 1, Chunk: newChunk(0, 10), ctx: context.Background()}
	if !m.TryAcquire() || !m.TryAcquire() || !other.TryAcquire() {
		t.Fatal("setup")
	}
	conn.permit = true
	conn.shedding = true
	f.releasePermit(conn)
	if conn.shedding {
		t.Fatal("the shed reservation is consumed by the first release")
	}
	// The stream gets a fresh permit after a 429 and later exits normally.
	if !m.TryAcquire() {
		t.Fatal("reacquire")
	}
	conn.permit = true
	f.releasePermit(conn)
	if snap := m.Snapshot(); snap.Active != 1 || snap.SourceActive != 2 {
		t.Fatalf("a second release must not use another stream's reservation: %+v", snap)
	}
}
