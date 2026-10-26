package transfer

import (
	"context"
	"sync"
	"testing"
	"time"
)

func testBudget() (*Budget, *fakeClock) {
	clock := newFakeClock()
	return newBudget(clock, func() float64 { return 0.5 }), clock
}

func acquireAll(m *Member) int {
	n := 0
	for m.TryAcquire() {
		n++
	}
	return n
}

func TestBudgetDefaultGrantsEachTaskItsOwnLimit(t *testing.T) {
	b, _ := testBudget()
	var members []*Member
	total := 0
	for i := 0; i < 5; i++ {
		m := b.Join("origin|https://example.test|direct", 4, false)
		members = append(members, m)
		got := acquireAll(m)
		if got != 4 {
			t.Fatalf("task %d got %d streams, want its own limit 4", i, got)
		}
		total += got
	}
	if total != 20 {
		t.Fatalf("five same-source tasks got %d streams; the default must not shrink 5x4 to a smaller pool", total)
	}
	snap := members[0].Snapshot()
	if snap.SourceActive != 20 || snap.SourceCapacity != 20 || snap.Members != 5 {
		t.Fatalf("snapshot %+v", snap)
	}
	members[0].Release()
	if !members[0].TryAcquire() {
		t.Fatal("a released permit must be reusable")
	}
	for _, m := range members {
		m.Leave()
	}
	if len(b.sources) != 0 {
		t.Fatalf("idle sources must be forgotten, have %d", len(b.sources))
	}
}

func TestBudgetCooldownHonoursRetryAfterPerSource(t *testing.T) {
	b, clock := testBudget()
	limited := b.Join("origin|https://limited.test|direct", 4, false)
	other := b.Join("origin|https://other.test|direct", 4, false)
	if !limited.TryAcquire() {
		t.Fatal("first stream")
	}
	until := limited.RateLimited(429, 10*time.Second, true)
	if want := clock.Now().Add(10 * time.Second); !until.Equal(want) {
		t.Fatalf("cooldown until %v, want %v", until, want)
	}
	limited.Release()
	if limited.TryAcquire() {
		t.Fatal("no request may start during the server-requested wait")
	}
	if !other.TryAcquire() {
		t.Fatal("another source must not be cooled down")
	}
	done := make(chan error, 1)
	go func() { done <- limited.Acquire(context.Background()) }()
	clock.Advance(9 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("Acquire returned %v before Retry-After elapsed", err)
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not resume when the cooldown ended")
	}
	// A long Retry-After is honoured in full rather than clipped to the local cap.
	until = limited.RateLimited(503, 5*time.Minute, true)
	if got := until.Sub(clock.Now()); got != 5*time.Minute {
		t.Fatalf("Retry-After shortened to %v", got)
	}
	if b.Cooldown("origin|https://limited.test|direct").IsZero() {
		t.Fatal("cooldown must be visible for scheduling")
	}
}

func TestBudgetBackoffEscalatesOncePerEpisode(t *testing.T) {
	b, clock := testBudget()
	m := b.Join("api|quark|acct", 4, false)
	first := m.RateLimited(429, 0, false).Sub(clock.Now())
	// Parallel in-flight requests hitting the same limit do not escalate it.
	again := m.RateLimited(429, 0, false).Sub(clock.Now())
	if first != 750*time.Millisecond || again != first {
		t.Fatalf("first episode waits %v then %v", first, again)
	}
	clock.Advance(first)
	second := m.RateLimited(429, 0, false).Sub(clock.Now())
	if second != 1500*time.Millisecond {
		t.Fatalf("second episode waits %v", second)
	}
	clock.Advance(second)
	m.Succeeded()
	if reset := m.RateLimited(503, 0, false).Sub(clock.Now()); reset != first {
		t.Fatalf("success must reset backoff, got %v", reset)
	}
}

func TestBudgetAcquireCancellationReleasesNothing(t *testing.T) {
	b, _ := testBudget()
	m := b.Join("origin|https://example.test|direct", 1, false)
	if !m.TryAcquire() {
		t.Fatal("first stream")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Acquire(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Acquire = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not end the wait within one second")
	}
	if snap := m.Snapshot(); snap.Active != 1 || snap.SourceActive != 1 {
		t.Fatalf("cancelled wait changed accounting: %+v", snap)
	}
	m.Leave()
	if err := m.Acquire(context.Background()); err != ErrLeft {
		t.Fatalf("Acquire after Leave = %v", err)
	}
}

func TestBudgetFairnessGivesFirstStreamsBeforeExtras(t *testing.T) {
	b, _ := testBudget()
	key := SourceKey("netdisk|quark|account")
	a := b.Join(key, 4, true)
	c := b.Join(key, 4, true)
	s := b.sources[key]
	s.adapt.target = 3
	if !a.TryAcquire() || !a.TryAcquire() || !a.TryAcquire() {
		t.Fatal("capacity 3 for the first task")
	}
	if c.TryAcquire() {
		t.Fatal("capacity is exhausted")
	}
	// c is waiting with zero streams, so a may not take an extra one back.
	a.Release()
	if a.TryAcquire() {
		t.Fatal("a task holding streams must yield to one waiting for its first")
	}
	if !c.TryAcquire() {
		t.Fatal("the waiting task gets the released permit")
	}
	c.NoDemand()
	a.Release()
	if !a.TryAcquire() {
		t.Fatal("without other demand the permit returns")
	}
}

func TestBudgetShedsFromLargestHolderOnly(t *testing.T) {
	b, _ := testBudget()
	key := SourceKey("origin|https://capped.test|direct")
	a := b.Join(key, 4, true)
	c := b.Join(key, 4, true)
	for i := 0; i < 3; i++ {
		a.TryAcquire()
	}
	c.TryAcquire()
	b.sources[key].adapt.target = 2
	if c.ShedOne() {
		t.Fatal("the smaller holder must keep its only stream")
	}
	if !a.ShedOne() || !a.ShedOne() {
		t.Fatal("the largest holder sheds down to the target")
	}
	if a.ShedOne() {
		t.Fatal("reservations count toward the target; no over-shedding")
	}
	a.ReleaseShed()
	a.ReleaseShed()
	if snap := a.Snapshot(); snap.Active != 1 || snap.SourceActive != 2 {
		t.Fatalf("after shedding: %+v", snap)
	}
}

func TestBudgetAPIGateBoundsConcurrencyAndCooldown(t *testing.T) {
	b, clock := testBudget()
	key := SourceKey("api|baidu|acct")
	var releases []func()
	for i := 0; i < apiConcurrency; i++ {
		release, err := b.AcquireAPI(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := b.AcquireAPI(ctx, key); err != context.DeadlineExceeded {
		t.Fatalf("fifth concurrent API call = %v", err)
	}
	for _, release := range releases {
		release()
		release() // idempotent
	}
	b.APIRateLimited(key, 3*time.Second, true)
	got := make(chan error, 1)
	go func() {
		release, err := b.AcquireAPI(context.Background(), key)
		if release != nil {
			release()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("API call started during cooldown")
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(3 * time.Second)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("API call did not resume after cooldown")
	}
}

func TestBudgetConcurrentUseIsConsistent(t *testing.T) {
	b, _ := testBudget()
	key := SourceKey("origin|https://race.test|direct")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := b.Join(key, 3, i%2 == 0)
			for j := 0; j < 200; j++ {
				if m.TryAcquire() {
					m.Transferred(1024)
					m.Release()
				}
				if j%50 == 0 {
					_ = m.Snapshot()
				}
			}
			m.Leave()
		}()
	}
	wg.Wait()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if s := b.sources[key]; s != nil && (s.active != 0 || len(s.members) != 0) {
		t.Fatalf("leaked accounting: active=%d members=%d", s.active, len(s.members))
	}
}

func TestBudgetExtendedCooldownStillWakesWaiters(t *testing.T) {
	b, clock := testBudget()
	m := b.Join("origin|https://ext.test|direct", 2, false)
	m.RateLimited(429, 300*time.Millisecond, true)
	done := make(chan error, 1)
	go func() { done <- m.Acquire(context.Background()) }()
	time.Sleep(20 * time.Millisecond) // let the waiter arm the wake-up
	m.RateLimited(429, 600*time.Millisecond, true)
	clock.Advance(300 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("waiter woke before the extended cooldown ended")
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(300 * time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter slept past the end of an extended cooldown")
	}
}

func TestBudgetRecoveryProbesBeforeReopening(t *testing.T) {
	b, clock := testBudget()
	a := b.Join("origin|https://probe.test|direct", 4, false)
	c := b.Join("origin|https://probe.test|direct", 4, false)
	a.RateLimited(429, time.Second, true)
	if a.TryAcquire() || c.TryAcquire() {
		t.Fatal("cooldown")
	}
	clock.Advance(time.Second)
	if !a.TryAcquire() {
		t.Fatal("one request probes the source after the cooldown")
	}
	if a.TryAcquire() || c.TryAcquire() {
		t.Fatal("the others wait for the probe's outcome instead of retrying at once")
	}
	a.Succeeded()
	if !c.TryAcquire() || !a.TryAcquire() {
		t.Fatal("a successful probe reopens the source")
	}
	// A probe that neither succeeds nor is refused reopens after its timeout.
	a.RateLimited(503, time.Second, true)
	clock.Advance(time.Second)
	if !c.TryAcquire() || c.TryAcquire() {
		t.Fatal("exactly one probe after the second cooldown")
	}
	clock.Advance(recoveryProbeTimeout)
	if !c.TryAcquire() {
		t.Fatal("the probe timeout reopens the source")
	}
	// A refused probe starts a new cooldown with no probe outstanding.
	a.RateLimited(429, time.Second, true)
	clock.Advance(time.Second)
	if !a.TryAcquire() {
		t.Fatal("probe after third cooldown")
	}
	a.RateLimited(429, time.Second, true)
	if a.TryAcquire() || c.TryAcquire() {
		t.Fatal("refused probe restarts the cooldown")
	}
}
