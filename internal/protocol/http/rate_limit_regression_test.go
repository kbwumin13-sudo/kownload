package http

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// refusingRanges accepts the first request with a declared range and refuses
// every ranged request with 429 and the given Retry-After (none when empty).
func refusingRanges(retryAfter func() string, ranges *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
			if v := retryAfter(); v != "" {
				w.Header().Set("Retry-After", v)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", "4194304")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

// A refusal that gives no usable wait (zero, a past date, none or a malformed
// value) is retried only after a backoff, never in a loop, and pausing ends
// the waiting streams at once.
func TestRefusalsWithoutUsableWaitBackOff(t *testing.T) {
	for name, header := range map[string]func() string{
		"zero":      func() string { return "0" },
		"past date": func() string { return time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat) },
		"missing":   func() string { return "" },
		"malformed": func() string { return "soon" },
	} {
		t.Run(name, func(t *testing.T) {
			var ranges atomic.Int64
			srv := httptest.NewServer(refusingRanges(header, &ranges))
			defer srv.Close()
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			defer budget.Close()
			f := newPooledFetcher(pooledController(pool, budget, nil))
			defer f.Close()
			if err := f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond)
			if n := ranges.Load(); n > 3 {
				t.Fatalf("%d refused range requests within 300ms", n)
			}
			time.Sleep(1200 * time.Millisecond)
			// The first episode waits 0.5–1s and the next 1–2s.
			if n := ranges.Load(); n > 6 {
				t.Fatalf("%d refused range requests within 1.5s; refusals are not backed off", n)
			}
			start := time.Now()
			if err := f.Pause(); err != nil {
				t.Fatal(err)
			}
			if waited := time.Since(start); waited > time.Second {
				t.Fatalf("pause took %v while streams waited out a backoff", waited)
			}
			before := ranges.Load()
			time.Sleep(1500 * time.Millisecond)
			if after := ranges.Load(); after != before {
				t.Fatalf("%d requests after pause", after-before)
			}
		})
	}
}

// A future Retry-After, in seconds or as a date, is honoured in full: no
// request reaches the server before the time it asked for.
func TestFutureRetryAfterIsHonoured(t *testing.T) {
	for name, header := range map[string]func() (string, time.Time){
		"seconds": func() (string, time.Time) { return "2", time.Now().Add(2 * time.Second) },
		"date": func() (string, time.Time) {
			at := time.Now().Add(3 * time.Second).Truncate(time.Second)
			return at.UTC().Format(http.TimeFormat), at
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := rangeProbePayload(1 << 20)
			var mu sync.Mutex
			var refusedAt, refusedUntil time.Time
			var early []time.Duration
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "" {
					mu.Lock()
					now := time.Now()
					if refusedUntil.IsZero() {
						value, until := header()
						refusedAt, refusedUntil = now, until
						mu.Unlock()
						w.Header().Set("Retry-After", value)
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
					// Requests sent before the refusal reached the client arrive
					// right after it; later ones must wait for the deadline.
					if now.After(refusedAt.Add(100*time.Millisecond)) && now.Before(refusedUntil) {
						early = append(early, refusedUntil.Sub(now))
					}
					mu.Unlock()
				}
				servePayload(data, true)(w, r)
			}))
			defer srv.Close()
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			defer budget.Close()
			f := newPooledFetcher(pooledController(pool, budget, nil))
			defer f.Close()
			if err := f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-f.doneCh:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("download did not finish after the server's wait")
			}
			checkFile(t, f.Meta().SingleFilepath(), data)
			mu.Lock()
			defer mu.Unlock()
			if refusedUntil.IsZero() {
				t.Fatal("the fixture never refused a request")
			}
			if len(early) > 0 {
				t.Fatalf("%d requests came before the server's deadline: %v", len(early), early)
			}
		})
	}
}

// Consecutive refusal episodes and deferrals without a successful response
// count against one failure budget; a refusal arriving during an episode does
// not count again; a success restarts the count; a wait beyond a day fails at
// once instead of being shortened.
func TestRateLimitEpisodesHaveAFailureBudget(t *testing.T) {
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(nil, budget, nil))
	f.meta.Req = &base.Request{URL: "http://limited.test/f.bin"}
	short := &RequestError{Code: 429, RetryAfter: time.Millisecond, HasRetryAfter: true}
	for i := 1; i <= maxDeferrals; i++ {
		time.Sleep(3 * time.Millisecond) // the previous episode is over
		if _, deferred, giveUp := f.rateLimit(short, i); deferred != nil || giveUp {
			t.Fatalf("episode %d: deferred=%v giveUp=%t", i, deferred, giveUp)
		}
	}
	time.Sleep(3 * time.Millisecond)
	if _, _, giveUp := f.rateLimit(short, maxDeferrals+1); !giveUp {
		t.Fatal("refusal episodes beyond the budget must give up and report the rate limit")
	}

	f.deferrals.Store(0) // a successful response resets the count
	long := &RequestError{Code: 429, RetryAfter: 500 * time.Millisecond, HasRetryAfter: true}
	time.Sleep(3 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, _, giveUp := f.rateLimit(long, i+1); giveUp {
			t.Fatal("responses within one episode must not exhaust the budget")
		}
	}
	if got := f.deferrals.Load(); got != 1 {
		t.Fatalf("five refusals within one episode counted %d times", got)
	}

	f.deferrals.Store(0)
	day := &RequestError{Code: 503, RetryAfter: 48 * time.Hour, HasRetryAfter: true}
	wait, deferred, giveUp := f.rateLimit(day, 1)
	if !giveUp || deferred != nil || wait < 47*time.Hour {
		t.Fatalf("a two-day wait gave wait=%v deferred=%v giveUp=%t", wait, deferred, giveUp)
	}
	if until := budget.Cooldown(f.sourceKey()); time.Until(until) < 47*time.Hour {
		t.Fatalf("the source cooldown was shortened to %v", time.Until(until))
	}

	// Without a budget the stream backs off by itself and keeps the same bound.
	g := newPooledFetcher(pooledController(nil, nil, nil))
	g.meta.Req = &base.Request{URL: "http://unbudgeted.test/f.bin"}
	zero := &RequestError{Code: 429, HasRetryAfter: true}
	for i := 1; i <= maxDeferrals; i++ {
		wait, _, giveUp := g.rateLimit(zero, i)
		if giveUp || wait < 400*time.Millisecond {
			t.Fatalf("attempt %d: wait=%v giveUp=%t", i, wait, giveUp)
		}
	}
	if _, _, giveUp := g.rateLimit(zero, maxDeferrals+1); !giveUp {
		t.Fatal("unbudgeted refusals beyond the budget must give up")
	}
}

// Taken from the independent acceptance (F02): a cooldown extended while the
// first request waits is honoured in full.
func TestResolveRechecksExtendedCooldown(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	defer f.Close()
	f.meta.Req = &base.Request{URL: srv.URL + "/f.bin"}
	key := f.sourceKey()
	budget.RateLimited(key, 429, 200*time.Millisecond, true)
	done := make(chan error, 1)
	go func() { done <- f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)) }()
	time.Sleep(60 * time.Millisecond)
	until := budget.RateLimited(key, 429, 2*time.Second, true)
	time.Sleep(300 * time.Millisecond)
	n := hits.Load()
	_ = f.Pause()
	<-done
	if n != 0 {
		t.Fatalf("Resolve sent %d request(s) %v before the extended cooldown ended", n, time.Until(until))
	}
}

// Several first requests waiting on one source: an extension by another task
// holds all of them, and after the cooldown one probe goes first while the
// others wait for its success.
func TestFirstRequestsWaitForExtendedCooldownAndProbe(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	var probeAnswered time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		first := len(arrivals) == 1
		mu.Unlock()
		if first {
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			probeAnswered = time.Now()
			mu.Unlock()
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	ctl := pooledController(pool, budget, nil)
	a, b := newPooledFetcher(ctl), newPooledFetcher(ctl)
	defer a.Close()
	defer b.Close()
	a.meta.Req = &base.Request{URL: srv.URL + "/f.bin"}
	key := a.sourceKey()
	budget.RateLimited(key, 429, 200*time.Millisecond, true)
	done := make(chan error, 2)
	for _, f := range []*Fetcher{a, b} {
		go func(f *Fetcher) { done <- f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)) }(f)
	}
	time.Sleep(60 * time.Millisecond)
	until := budget.RateLimited(key, 503, 1200*time.Millisecond, true)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("first requests did not proceed after the cooldown")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 2 {
		t.Fatalf("%d requests, want 2", len(arrivals))
	}
	for _, at := range arrivals {
		if at.Before(until) {
			t.Fatalf("a request arrived %v before the extended cooldown ended", until.Sub(at))
		}
	}
	if arrivals[1].Before(probeAnswered) {
		t.Fatal("the second request did not wait for the recovery probe to succeed")
	}
}

// A first request that would wait for a cooldown gives its running slot back
// at once while other tasks queue, and beyond DeferThreshold in any case; the
// deferral keeps the server's deadline. Without queued tasks a short cooldown
// is waited out, and pausing ends that wait without a request.
func TestFirstRequestDefersOrWaitsByQueue(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	var queued atomic.Bool
	ctl := pooledController(pool, budget, nil)
	ctl.QueueWaiting = queued.Load
	key := newPooledFetcher(ctl)
	key.meta.Req = &base.Request{URL: srv.URL + "/f.bin"}

	queued.Store(true)
	until := budget.RateLimited(key.sourceKey(), 429, 3*time.Second, true)
	f := newPooledFetcher(ctl)
	err := f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t))
	if deferred, ok := transfer.AsDeferred(err); !ok || !deferred.Until.Equal(until) {
		t.Fatalf("with tasks queued Resolve returned %v, want a deferral until %v", err, until)
	}

	queued.Store(false)
	g := newPooledFetcher(ctl)
	done := make(chan error, 1)
	go func() { done <- g.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)) }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	_ = g.Pause()
	select {
	case err := <-done:
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("paused wait returned %v after %v", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pause could not end the cooldown wait")
	}

	long := budget.RateLimited(key.sourceKey(), 429, 45*time.Second, true)
	h := newPooledFetcher(ctl)
	err = h.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t))
	if deferred, ok := transfer.AsDeferred(err); !ok || !deferred.Until.Equal(long) {
		t.Fatalf("a long cooldown gave %v, want a deferral until the server's deadline %v", err, long)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached a cooling source", n)
	}
}

// A first request refused with a wait beyond a day reports the error instead
// of retrying sooner, and the source keeps the full wait.
func TestFirstRequestRefusedForDaysFails(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "172800")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	f := newPooledFetcher(pooledController(pool, budget, nil))
	err := f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t))
	var re *RequestError
	if !errors.As(err, &re) || re.Code != 503 {
		t.Fatalf("Resolve returned %v, want the 503", err)
	}
	if until := budget.Cooldown(f.sourceKey()); time.Until(until) < 47*time.Hour {
		t.Fatalf("the source cooldown was shortened to %v", time.Until(until))
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// A budget closed with its downloader limits nothing: a first request is
// neither held nor failed by it, whether it was closed before or during the
// wait for a cooldown.
func TestFirstRequestIgnoresClosedBudget(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	for _, when := range []string{"before", "during"} {
		t.Run(when, func(t *testing.T) {
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			f := newPooledFetcher(pooledController(pool, budget, nil))
			defer f.Close()
			f.meta.Req = &base.Request{URL: srv.URL + "/f.bin"}
			if when == "before" {
				budget.Close()
			} else {
				budget.RateLimited(f.sourceKey(), 429, 10*time.Second, true)
			}
			before := hits.Load()
			done := make(chan error, 1)
			go func() { done <- f.Resolve(&base.Request{URL: srv.URL + "/f.bin"}, rangeProbeOptions(t)) }()
			if when == "during" {
				time.Sleep(100 * time.Millisecond)
				budget.Close()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("a closed budget held the first request")
			}
			if n := hits.Load() - before; n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}

// Taken from the build 17 re-acceptance (R01): the cooldown ends between the
// failed permit attempt and the wait (held open here by a slow queue check).
// The wait must still see that change instead of blocking until cancellation.
func TestAdmissionDoesNotMissCooldownEnd(t *testing.T) {
	b := transfer.NewBudget()
	defer b.Close()
	ctl := pooledController(nil, b, nil)
	f := newPooledFetcher(ctl)
	defer f.Close()
	f.meta.Req = &base.Request{URL: "http://review.invalid/file"}
	until := b.RateLimited(f.sourceKey(), 429, 2100*time.Millisecond, true)
	ctl.QueueWaiting = func() bool { time.Sleep(time.Until(until) + 100*time.Millisecond); return false }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := f.admitFirstRequest(ctx)
	if m != nil {
		m.Leave()
	}
	if err != nil {
		t.Fatalf("admission missed the cooldown end and stayed blocked until cancellation: %v", err)
	}
}

// A first request waiting out a short cooldown with nothing queued gives its
// slot up as soon as a task joins the queue, keeping the server's deadline;
// with no queue signal it keeps waiting.
func TestAdmissionDefersWhenTasksQueueDuringWait(t *testing.T) {
	b := transfer.NewBudget()
	defer b.Close()
	ctl := pooledController(nil, b, nil)
	var queued atomic.Bool
	signal := make(chan struct{})
	ctl.QueueWaiting = queued.Load
	ctl.QueueChanged = func() <-chan struct{} { return signal }
	f := newPooledFetcher(ctl)
	defer f.Close()
	f.meta.Req = &base.Request{URL: "http://review.invalid/file"}
	until := b.RateLimited(f.sourceKey(), 429, 4*time.Second, true)
	type result struct {
		m   *transfer.Member
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := f.admitFirstRequest(context.Background())
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("admission returned %v before any task queued", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	queued.Store(true)
	close(signal)
	select {
	case r := <-done:
		if r.m != nil {
			r.m.Leave()
		}
		if deferred, ok := transfer.AsDeferred(r.err); !ok || !deferred.Until.Equal(until) {
			t.Fatalf("after a task queued the admission returned %v, want a deferral until %v", r.err, until)
		}
	case <-time.After(time.Second):
		t.Fatal("a task joining the queue did not make the waiting admission give its slot up")
	}
}

// A download idling in a source cooldown gives its slot up when a task joins
// the queue while the stream supervisor is busy with another event (held here
// inside its connection lock): the queue signal it waits on was taken before
// the change, so the change is not lost.
func TestCoolingTransferSeesQueueChangeDuringOtherWork(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	var refused atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" && !strings.HasPrefix(rg, "bytes=0-") && refused.Add(1) == 1 {
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	b := transfer.NewBudget()
	defer b.Close()
	ctl := pooledController(pool, b, nil)
	var queued atomic.Bool
	var signalMu sync.Mutex
	signal := make(chan struct{})
	ctl.QueueWaiting = queued.Load
	ctl.QueueChanged = func() <-chan struct{} {
		signalMu.Lock()
		defer signalMu.Unlock()
		return signal
	}
	f := newPooledFetcher(ctl)
	defer f.Close()
	opts := &base.Options{Path: t.TempDir(), Name: "q.bin", Extra: &fhttp.OptsExtra{Connections: 4}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for refused.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no ranged request was refused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	// The cooldown is extended, which wakes the supervisor; it then waits for
	// the connection lock while a task joins the queue.
	f.connMu.Lock()
	until := b.RateLimited(f.sourceKey(), 429, 4*time.Second, true)
	time.Sleep(100 * time.Millisecond)
	queued.Store(true)
	signalMu.Lock()
	close(signal)
	signal = make(chan struct{})
	signalMu.Unlock()
	f.connMu.Unlock()

	select {
	case err := <-f.doneCh:
		if deferred, ok := transfer.AsDeferred(err); !ok || !deferred.Until.Equal(until) {
			t.Fatalf("the run ended with %v, want a deferral until %v", err, until)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a task joining the queue during other work did not make the cooling run give its slot up")
	}
}
