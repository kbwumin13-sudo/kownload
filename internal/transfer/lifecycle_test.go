package transfer

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func pendingTimers(c *fakeClock) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// Taken from the independent acceptance (F05): after 20 batches of sources and
// API gates that were rate limited and then left alone, nothing is retained
// once their cooldowns are long over.
func TestExpiredUnusedCooldownsAreReclaimed(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	for i := 0; i < 20; i++ {
		k := SourceKey(fmt.Sprintf("origin-%d", i))
		b.RateLimited(k, 429, time.Second, true)
		m := b.Join(SourceKey(fmt.Sprintf("running-%d", i)), 4, false)
		m.RateLimited(429, time.Second, true)
		m.Leave()
		release, err := b.AcquireAPI(context.Background(), SourceKey(fmt.Sprintf("api-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		b.APIRateLimited(SourceKey(fmt.Sprintf("api-%d", i)), time.Second, true)
		release()
	}
	c.Advance(48 * time.Hour)
	if len(b.sources) != 0 || len(b.apis) != 0 || pendingTimers(c) != 0 {
		t.Fatalf("expired unused entries retained: sources=%d APIs=%d timers=%d", len(b.sources), len(b.apis), pendingTimers(c))
	}
}

// A source stays while its cooldown lasts, however it was extended and
// whoever joins again, and goes once the cooldown is over and nobody uses it.
func TestBudgetKeepsCoolingSourcesUntilTheyEnd(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	key := SourceKey("origin|https://cool.test|direct")

	// The last member leaves during a cooldown that is then extended.
	m := b.Join(key, 2, false)
	m.RateLimited(429, time.Second, true)
	m.Leave()
	c.Advance(500 * time.Millisecond)
	b.RateLimited(key, 429, 2500*time.Millisecond, true)
	c.Advance(time.Second)
	if b.sources[key] == nil {
		t.Fatal("a source was forgotten while its extended cooldown lasted")
	}
	if until := b.Cooldown(key); !until.Equal(c.Now().Add(1500 * time.Millisecond)) {
		t.Fatalf("cooldown until %v, want the extended end", until)
	}

	// Joining again during the cooldown keeps it in force.
	again := b.Join(key, 2, false)
	if again.TryAcquire() {
		t.Fatal("a member joining during the cooldown got a permit")
	}
	c.Advance(1500 * time.Millisecond)
	if b.sources[key] == nil {
		t.Fatal("a source with a member was forgotten")
	}
	if !again.TryAcquire() {
		t.Fatal("the cooldown ended but the member got no probe permit")
	}
	again.Succeeded()
	again.Release()
	again.Leave()
	if b.sources[key] != nil || pendingTimers(c) != 0 {
		t.Fatalf("an idle source outside any cooldown was kept: sources=%d timers=%d", len(b.sources), pendingTimers(c))
	}
}

// First requests that keep being refused escalate the backoff although no
// member stays joined in between; the unused source is still forgotten
// strikeMemory after its cooldown, and a later refusal starts over.
func TestBackoffEscalatesAcrossShortLivedMembers(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	key := SourceKey("origin|https://resolve.test|direct")
	for i, want := range []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second} {
		m := b.Join(key, 1, false)
		if !m.TryAcquire() {
			t.Fatalf("attempt %d got no permit after the cooldown", i+1)
		}
		until, _ := m.RateLimitedEpisode(429, 0, true)
		m.Leave()
		if got := until.Sub(c.Now()); got != want {
			t.Fatalf("attempt %d waits %v, want %v", i+1, got, want)
		}
		c.Advance(want)
	}
	c.Advance(strikeMemory)
	if b.sources[key] != nil || pendingTimers(c) != 0 {
		t.Fatalf("an unused source was kept past its strike memory: timers=%d", pendingTimers(c))
	}
	m := b.Join(key, 1, false)
	defer m.Leave()
	if until, _ := m.RateLimitedEpisode(429, 0, true); until.Sub(c.Now()) != 750*time.Millisecond {
		t.Fatal("a refusal long after the last episode must start the backoff over")
	}
}

// Close stops every timer, wakes waiters and forgets everything.
func TestBudgetCloseStopsTimersAndForgets(t *testing.T) {
	b, c := testBudget()
	m := b.Join("origin|https://close.test|direct", 1, false)
	m.RateLimited(429, time.Hour, true)
	b.RateLimited("origin|https://other.test|direct", 503, time.Hour, true)
	b.APIRateLimited("api|quark|acct", time.Hour, true)
	waiting := make(chan error, 2)
	go func() { waiting <- m.Acquire(context.Background()) }()
	go func() {
		_, err := b.AcquireAPI(context.Background(), "api|quark|acct")
		waiting <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-waiting:
			if err != ErrLeft {
				t.Fatalf("waiter returned %v after Close, want ErrLeft", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not wake a waiter")
		}
	}
	if len(b.sources) != 0 || len(b.apis) != 0 || pendingTimers(c) != 0 {
		t.Fatalf("after Close: sources=%d APIs=%d timers=%d", len(b.sources), len(b.apis), pendingTimers(c))
	}
	m.Leave()
}

// A zero, past or missing wait from a source or an API still starts a
// backoff that grows per consecutive episode; refusals arriving during an
// episode do not extend or escalate it; a future wait is kept in full.
func TestRateLimitsWithoutUsableWaitBackOff(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	m := b.Join("origin|https://zero.test|direct", 4, false)
	defer m.Leave()
	until, fresh := m.RateLimitedEpisode(429, 0, true)
	if !fresh || until.Sub(c.Now()) != 750*time.Millisecond {
		t.Fatalf("Retry-After 0 gave a %v cooldown (fresh=%t), want the first backoff step", until.Sub(c.Now()), fresh)
	}
	if again, fresh := m.RateLimitedEpisode(429, 0, true); fresh || !again.Equal(until) {
		t.Fatal("a refusal during an episode must not extend or restart it")
	}
	c.Advance(750 * time.Millisecond)
	if next, _ := m.RateLimitedEpisode(503, 0, false); next.Sub(c.Now()) != 1500*time.Millisecond {
		t.Fatalf("the second episode waits %v, want 1.5s", next.Sub(c.Now()))
	}
	c.Advance(1500 * time.Millisecond)
	if long, _ := m.RateLimitedEpisode(429, 48*time.Hour, true); long.Sub(c.Now()) != 48*time.Hour {
		t.Fatalf("a two-day Retry-After became %v", long.Sub(c.Now()))
	}

	key := SourceKey("api|baidu|acct")
	if api := b.APIRateLimited(key, 0, true); api.Sub(c.Now()) != 750*time.Millisecond {
		t.Fatalf("API Retry-After 0 gave %v, want the first backoff step", api.Sub(c.Now()))
	}
	c.Advance(750 * time.Millisecond)
	if api := b.APIRateLimited(key, 0, true); api.Sub(c.Now()) != 1500*time.Millisecond {
		t.Fatalf("second API episode waits %v, want 1.5s", api.Sub(c.Now()))
	}
	c.Advance(1500 * time.Millisecond)
	b.APISucceeded(key)
	if api := b.APIRateLimited(key, 0, false); api.Sub(c.Now()) != 750*time.Millisecond {
		t.Fatalf("a success must reset the API backoff, got %v", api.Sub(c.Now()))
	}
}

// A deferrable task run gets a DeferredError instead of waiting out a long API
// cooldown, or a shorter one while other tasks queue; other calls wait, and an
// extension made meanwhile is honoured.
func TestAcquireAPIDefersOnlyDeferrableRuns(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	key := SourceKey("api|quark|acct")
	until := b.APIRateLimited(key, time.Minute, true)
	_, err := b.AcquireAPI(WithDeferral(context.Background(), nil, nil), key)
	if deferred, ok := AsDeferred(err); !ok || !deferred.Until.Equal(until) {
		t.Fatalf("a deferrable run got %v, want a deferral until %v", err, until)
	}

	// A run waiting out a short cooldown looks again when it is extended.
	short := SourceKey("api|baidu|acct")
	b.APIRateLimited(short, 3*time.Second, true)
	got := make(chan error, 1)
	go func() {
		_, err := b.AcquireAPI(WithDeferral(context.Background(), func() bool { return false }, nil), short)
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	c.Advance(time.Second)
	extended := b.APIRateLimited(short, time.Minute, true)
	select {
	case err := <-got:
		if deferred, ok := AsDeferred(err); !ok || !deferred.Until.Equal(extended) {
			t.Fatalf("after the extension the run got %v, want a deferral until %v", err, extended)
		}
	case <-time.After(time.Second):
		t.Fatal("a waiting run did not look again when the cooldown was extended")
	}
}

func TestAcquireAPIWaitsWhenNotDeferrable(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	key := SourceKey("api|quark|acct")
	b.APIRateLimited(key, 3*time.Second, true)
	// With tasks queued a short cooldown is handed back as well.
	queued := WithDeferral(context.Background(), func() bool { return true }, nil)
	if _, err := b.AcquireAPI(queued, key); err == nil {
		t.Fatal("a 3s cooldown with tasks queued must defer")
	}
	got := make(chan error, 3)
	for _, ctx := range []context.Context{
		context.Background(),
		WithDeferral(context.Background(), func() bool { return false }, nil),
		WithoutDeferral(queued),
	} {
		go func(ctx context.Context) {
			release, err := b.AcquireAPI(ctx, key)
			if release != nil {
				release()
			}
			got <- err
		}(ctx)
	}
	time.Sleep(20 * time.Millisecond)
	c.Advance(time.Second)
	b.APIRateLimited(key, 4*time.Second, true) // extended while they wait
	c.Advance(2 * time.Second)
	select {
	case err := <-got:
		t.Fatalf("a call went out before the extended cooldown ended: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	c.Advance(2 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("a waiting call did not proceed after the cooldown")
		}
	}
}

// Taken from the independent acceptance (F04): stream transports of proxy
// policies no longer in use are closed and forgotten.
func TestOldPolicyPoolsAreReclaimed(t *testing.T) {
	p := NewTransportPool()
	defer p.Close()
	now := time.Now()
	p.now = func() time.Time { return now }
	for i := 0; i < 20; i++ {
		opts := TransportOptions{ProxyID: fmt.Sprintf("policy-generation-%d", i)}
		key := keyOf(opts)
		st, err := p.lease(key, opts, "http://fixture.invalid:80")
		if err != nil {
			t.Fatal(err)
		}
		p.release(key, st, "http://fixture.invalid:80", false)
		now = now.Add(48 * time.Hour)
	}
	if n := len(p.free); n != 1 || p.Stats().Free != 1 {
		t.Fatalf("%d policy keys and %d free transports remain, want only the current one", n, p.Stats().Free)
	}
}

// Free stream transports stay bounded across keys, the oldest go first, and a
// leased transport is never touched by eviction.
func TestPoolBoundsFreeTransportsAcrossKeys(t *testing.T) {
	p := NewTransportPool()
	defer p.Close()
	now := time.Now()
	p.now = func() time.Time { return now }
	leasedOpts := TransportOptions{ProxyID: "in-use"}
	leased, err := p.lease(keyOf(leasedOpts), leasedOpts, "http://busy.invalid:80")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3*maxFreeTransports; i++ {
		opts := TransportOptions{ProxyID: fmt.Sprintf("policy-%d", i)}
		st, err := p.lease(keyOf(opts), opts, "http://fixture.invalid:80")
		if err != nil {
			t.Fatal(err)
		}
		p.release(keyOf(opts), st, "http://fixture.invalid:80", true)
		now = now.Add(time.Second)
	}
	stats := p.Stats()
	if stats.Free != maxFreeTransports || stats.Leased != 1 {
		t.Fatalf("free=%d leased=%d, want %d free and the busy one still leased", stats.Free, stats.Leased, maxFreeTransports)
	}
	if _, kept := p.free[keyOf(TransportOptions{ProxyID: "policy-0"})]; kept {
		t.Fatal("the oldest released transport was kept over newer ones")
	}
	now = now.Add(48 * time.Hour)
	p.release(keyOf(leasedOpts), leased, "http://busy.invalid:80", true)
	if stats := p.Stats(); stats.Free != 1 || stats.Leased != 0 {
		t.Fatalf("after a long lease: %+v, want only the returned transport", stats)
	}
}

// Shared API transports of unused policies expire on any pool activity, and
// Close forgets everything.
func TestPoolExpiresSharedTransportsAndCloses(t *testing.T) {
	srv := newCountingServer(t, "plain", payload)
	p := NewTransportPool()
	now := time.Now()
	p.now = func() time.Time { return now }
	if _, err := p.Shared(TransportOptions{ProxyID: "old-policy"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(sharedTransportTTL + time.Minute)
	resp := get(t, streamClient(p, "new-policy", false), srv.URL)
	drain(t, resp)
	if stats := p.Stats(); stats.Shared != 0 || stats.Free != 1 {
		t.Fatalf("after the TTL: %+v, want the old shared transport gone", stats)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := p.Stats(); stats.Free != 0 || stats.Shared != 0 {
		t.Fatalf("after Close: %+v", stats)
	}
	if _, err := p.lease(keyOf(TransportOptions{}), TransportOptions{}, "http://x.invalid:80"); err != ErrPoolClosed {
		t.Fatalf("lease after Close returned %v", err)
	}
}

func assertClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("the wake channel from the failed attempt stayed open after the %s", what)
	}
}

// Re-acceptance finding R01 at the budget level: a failed attempt hands out the
// wake channel of that same moment, so a cooldown ending, a probe succeeding
// or the budget closing right after the attempt cannot be missed.
func TestTryAcquireOrWaitSeesEveryLaterChange(t *testing.T) {
	b, c := testBudget()
	key := SourceKey("origin|https://wake.test|direct")
	m := b.Join(key, 1, false)
	m.RateLimited(429, time.Second, true)
	granted, wake, until, closed := m.TryAcquireOrWait()
	if granted || closed || !until.Equal(c.Now().Add(time.Second)) {
		t.Fatalf("attempt during a cooldown: granted=%t closed=%t until=%v", granted, closed, until)
	}
	c.Advance(time.Second)
	assertClosed(t, wake, "cooldown end")

	if granted, _, _, _ := m.TryAcquireOrWait(); !granted {
		t.Fatal("no probe permit after the cooldown")
	}
	other := b.Join(key, 1, false)
	granted, wake, _, _ = other.TryAcquireOrWait()
	if granted {
		t.Fatal("a second request went out while the recovery probe was outstanding")
	}
	m.Succeeded()
	assertClosed(t, wake, "probe success")
	if granted, _, _, _ := other.TryAcquireOrWait(); !granted {
		t.Fatal("no permit after the probe succeeded")
	}

	m.RateLimited(429, time.Hour, true)
	third := b.Join(key, 1, false)
	if granted, wake, _, _ = third.TryAcquireOrWait(); granted {
		t.Fatal("a permit was granted during a cooldown")
	}
	b.Close()
	assertClosed(t, wake, "budget closing")
	if _, _, _, closed := third.TryAcquireOrWait(); !closed {
		t.Fatal("a closed budget was not reported")
	}
}

// Re-acceptance finding R02 at the budget level: a deferrable API call waiting
// out a short cooldown with nothing queued gives its slot up once a task
// joins the queue, keeping the server's deadline; a call with deferral
// cleared keeps waiting and proceeds after the cooldown.
func TestAcquireAPIDefersWhenTasksQueueDuringWait(t *testing.T) {
	b, c := testBudget()
	defer b.Close()
	key := SourceKey("api|quark|acct")
	until := b.APIRateLimited(key, 5*time.Second, true)
	var queued atomic.Bool
	signal := make(chan struct{})
	ctx := WithDeferral(context.Background(), queued.Load, func() <-chan struct{} { return signal })
	deferrable, waiting := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := b.AcquireAPI(ctx, key)
		deferrable <- err
	}()
	go func() {
		release, err := b.AcquireAPI(WithoutDeferral(ctx), key)
		if release != nil {
			release()
		}
		waiting <- err
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-deferrable:
		t.Fatalf("the call returned %v before any task queued", err)
	default:
	}
	queued.Store(true)
	close(signal)
	select {
	case err := <-deferrable:
		if deferred, ok := AsDeferred(err); !ok || !deferred.Until.Equal(until) {
			t.Fatalf("after a task queued the call returned %v, want a deferral until %v", err, until)
		}
	case <-time.After(time.Second):
		t.Fatal("a task joining the queue did not make the waiting call give its slot up")
	}
	select {
	case err := <-waiting:
		t.Fatalf("the call with deferral cleared returned %v during the cooldown", err)
	case <-time.After(30 * time.Millisecond):
	}
	c.Advance(5 * time.Second)
	select {
	case err := <-waiting:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the call with deferral cleared did not proceed after the cooldown")
	}
}
