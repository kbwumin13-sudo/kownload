package transfer

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// SourceKey groups requests that share a server-side limit: an origin plus
// proxy policy for ordinary downloads, or a provider account for netdisk.
type SourceKey string

// Clock lets tests drive cooldowns and throughput windows with virtual time.
type Clock interface {
	Now() time.Time
	// AfterFunc runs f once after d and returns a function that cancels it.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

var ErrLeft = errors.New("budget member left")

const (
	// Concurrent API requests per provider account or API origin.
	apiConcurrency = 4
	// Interval between adaptive throughput evaluations.
	adaptiveTick = time.Second
	// After a cooldown one request probes the source; the others follow when
	// it succeeds, or after this long if it neither succeeds nor is refused.
	recoveryProbeTimeout = 2 * time.Second
	// A refusal this soon after a cooldown ended still escalates the backoff,
	// so an unused source or API gate is kept that long when it has strikes.
	strikeMemory = backoffCap
)

// wakeup is the one pending timer of a source or API gate. Arming an earlier
// time replaces a later one; a replaced or cancelled callback does nothing.
type wakeup struct {
	at   time.Time
	stop func() bool
	gen  uint64
}

func (w *wakeup) pending() bool { return w.stop != nil }

// set arms the timer for at unless it fires no later already. Caller holds
// b.mu; f runs with b.mu held.
func (w *wakeup) set(b *Budget, at time.Time, f func()) {
	if b.closed {
		return
	}
	if w.stop != nil {
		if !w.at.After(at) {
			return
		}
		w.stop()
	}
	w.gen++
	gen := w.gen
	w.at = at
	w.stop = b.clock.AfterFunc(at.Sub(b.clock.Now()), func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if w.gen != gen {
			return
		}
		w.stop = nil
		f()
	})
}

func (w *wakeup) cancel() {
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.gen++
}

// Budget coordinates download streams per source across tasks. Every task
// stays within its own stream limit; a server rate limit pauses new requests to
// the whole source; when an adaptive target is below the tasks' combined
// limits, capacity is shared so that each task first gets one stream. Without
// rate limits or adaptive targets it grants exactly what the tasks' own limits
// allow, so the default behaviour matches independent tasks.
type Budget struct {
	mu       sync.Mutex
	clock    Clock
	random   func() float64
	sources  map[SourceKey]*source
	apis     map[SourceKey]*apiGate
	stopTick func() bool
	closed   bool
}

type source struct {
	key       SourceKey
	members   []*Member // join order
	active    int
	shedding  int
	cooldown  time.Time
	status    int
	strikes   int
	wake      chan struct{}
	timer     wakeup
	bytes     atomic.Int64
	adapt     *adaptiveState
	lastLimit time.Time // last 429/503, for diagnostics
	// After a cooldown (recoverPending) the source recovers: one probe permit
	// goes out (probing) and the rest wait for its outcome, so waiters do not
	// all retry at the moment the cooldown ends.
	recoverPending bool
	recovering     bool
	probing        bool
	stopProbe      func() bool
}

// Member is one running task's view of its source budget.
type Member struct {
	b         *Budget
	src       *source
	limit     int
	active    int
	shedding  int
	waiting   bool
	left      bool
	remaining atomic.Int64
}

func NewBudget() *Budget {
	return newBudget(systemClock{}, nil)
}

func newBudget(clock Clock, random func() float64) *Budget {
	if random == nil {
		random = rand.Float64
	}
	return &Budget{
		clock:   clock,
		random:  random,
		sources: make(map[SourceKey]*source),
		apis:    make(map[SourceKey]*apiGate),
	}
}

// Close stops background evaluation and every timer, wakes every waiter and
// forgets all sources and API gates.
func (b *Budget) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.stopTick != nil {
		b.stopTick()
		b.stopTick = nil
	}
	for key, s := range b.sources {
		s.timer.cancel()
		s.endRecovery()
		s.broadcast()
		delete(b.sources, key)
	}
	for key, g := range b.apis {
		g.timer.cancel()
		g.broadcast()
		delete(b.apis, key)
	}
	return nil
}

// Join registers a running task. limit is the task's own stream limit.
// adaptive enables throughput-validated stream targets for the source.
func (b *Budget) Join(key SourceKey, limit int, adaptive bool) *Member {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sources[key]
	if s == nil {
		s = &source{key: key, wake: make(chan struct{})}
		b.sources[key] = s
	}
	m := &Member{b: b, src: s, limit: max(1, limit)}
	s.members = append(s.members, m)
	if adaptive && s.adapt == nil {
		s.adapt = &adaptiveState{}
		b.ensureTickerLocked()
	}
	if s.adapt != nil {
		s.adapt.reset(b.clock.Now(), s)
	}
	s.broadcast()
	return m
}

// Leave drops the member and any streams it still counts.
func (m *Member) Leave() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.left {
		return
	}
	m.left = true
	s := m.src
	s.active -= m.active
	s.shedding -= m.shedding
	m.active, m.shedding, m.waiting = 0, 0, false
	for i, o := range s.members {
		if o == m {
			s.members = append(s.members[:i], s.members[i+1:]...)
			break
		}
	}
	if s.adapt != nil {
		s.adapt.reset(b.clock.Now(), s)
	}
	s.broadcast()
	// The last member may leave during a cooldown; the source is forgotten
	// when the cooldown ends unless someone joins again before.
	b.armCooldownLocked(s)
	b.forgetIdleLocked(s)
}

// SetLimit updates the task's own stream limit.
func (m *Member) SetLimit(limit int) {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	m.limit = max(1, limit)
	m.src.broadcast()
}

// TryAcquire grants a stream permit if one is available now.
func (m *Member) TryAcquire() bool {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.grantLocked(m)
}

// Acquire waits until a stream permit is granted or ctx ends.
func (m *Member) Acquire(ctx context.Context) error {
	b := m.b
	for {
		b.mu.Lock()
		if m.left || b.closed {
			b.mu.Unlock()
			return ErrLeft
		}
		if b.grantLocked(m) {
			b.mu.Unlock()
			return nil
		}
		wake := m.src.wake
		b.armCooldownLocked(m.src)
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

// Hold counts a stream that is already open, such as a reused first response.
func (m *Member) Hold() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.left {
		return
	}
	m.active++
	m.src.active++
}

// Release returns a stream permit.
func (m *Member) Release() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.left || m.active == 0 {
		return
	}
	m.active--
	m.src.active--
	m.src.broadcast()
}

// ShedOne reports whether this member should hand back one stream to meet a
// lowered adaptive target, and reserves that reduction. The stream calls
// ReleaseShed once it has finished its retained range.
func (m *Member) ShedOne() bool {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	s := m.src
	if m.left || s.adapt == nil || s.active-s.shedding <= s.capacity() || m.active-m.shedding <= 1 {
		return false
	}
	// Shed from the first member that holds the most effective streams.
	for _, o := range s.members {
		if o.active-o.shedding > m.active-m.shedding {
			return false
		}
		if o.active-o.shedding == m.active-m.shedding {
			if o != m {
				return false
			}
			break
		}
	}
	m.shedding++
	s.shedding++
	return true
}

// CancelShed withdraws a ShedOne reservation when no stream could be shed.
func (m *Member) CancelShed() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if !m.left && m.shedding > 0 {
		m.shedding--
		m.src.shedding--
	}
}

// Contended reports whether another member is waiting while holding at least
// two fewer streams, so this member should give one back at a chunk boundary.
func (m *Member) Contended() bool {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.left || m.active-m.shedding <= 1 {
		return false
	}
	for _, o := range m.src.members {
		if o != m && o.waiting && o.active+1 < m.active-m.shedding {
			return true
		}
	}
	return false
}

// ReleaseShed returns a permit reserved by ShedOne.
func (m *Member) ReleaseShed() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.left {
		return
	}
	if m.shedding > 0 {
		m.shedding--
		m.src.shedding--
	}
	if m.active > 0 {
		m.active--
		m.src.active--
	}
	m.src.broadcast()
}

// NoDemand tells the budget this member does not want more streams for now,
// so it no longer takes priority over members holding more streams.
func (m *Member) NoDemand() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	m.waiting = false
}

// Changed returns a channel closed when capacity may have grown.
func (m *Member) Changed() <-chan struct{} {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armCooldownLocked(m.src)
	return m.src.wake
}

// TryAcquireOrWait is TryAcquire for a member waiting for a permit. When none
// is granted it returns, from the same critical section, the channel closed on
// the next change and the source cooldown (zero when none), so a change right
// after the attempt, such as the cooldown ending, cannot be missed. closed
// reports a member that left or a closed budget, where no change will come.
func (m *Member) TryAcquireOrWait() (granted bool, wake <-chan struct{}, cooldown time.Time, closed bool) {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.left || b.closed {
		return false, nil, time.Time{}, true
	}
	if b.grantLocked(m) {
		return true, nil, time.Time{}, false
	}
	b.armCooldownLocked(m.src)
	if b.clock.Now().Before(m.src.cooldown) {
		cooldown = m.src.cooldown
	}
	return false, m.src.wake, cooldown, false
}

// RateLimited records a 429/503 and returns when the source may be retried.
// See RateLimitedEpisode.
func (m *Member) RateLimited(status int, retryAfter time.Duration, hasHeader bool) time.Time {
	until, _ := m.RateLimitedEpisode(status, retryAfter, hasHeader)
	return until
}

// RateLimitedEpisode records a 429/503 and returns when the source may be
// retried, and whether this response started a new cooldown episode rather
// than arriving during one. A positive Retry-After is honoured in full. A
// missing, malformed, zero or past one gives no usable wait, so the source
// backs off exponentially from half a second per consecutive episode: a
// server that keeps refusing at once must not be retried in a loop.
func (m *Member) RateLimitedEpisode(status int, retryAfter time.Duration, hasHeader bool) (time.Time, bool) {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rateLimitedLocked(m.src, status, retryAfter, hasHeader)
}

// RateLimited records a rate-limit response for a source without a member,
// such as the first request of a download. See RateLimitedEpisode.
func (b *Budget) RateLimited(key SourceKey, status int, retryAfter time.Duration, hasHeader bool) time.Time {
	until, _ := b.RateLimitedEpisode(key, status, retryAfter, hasHeader)
	return until
}

// RateLimitedEpisode is Member.RateLimitedEpisode for a source without a member.
// A source nobody joins is forgotten once its cooldown ends.
func (b *Budget) RateLimitedEpisode(key SourceKey, status int, retryAfter time.Duration, hasHeader bool) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sources[key]
	if s == nil {
		s = &source{key: key, wake: make(chan struct{})}
		b.sources[key] = s
	}
	return b.rateLimitedLocked(s, status, retryAfter, hasHeader)
}

func (b *Budget) rateLimitedLocked(s *source, status int, retryAfter time.Duration, hasHeader bool) (time.Time, bool) {
	now := b.clock.Now()
	// Responses already in flight when a cooldown began belong to its episode:
	// they neither count a strike nor restart the backoff.
	fresh := !now.Before(s.cooldown)
	if fresh {
		s.strikes++
	}
	var until time.Time
	if hasHeader && retryAfter > 0 {
		until = addWait(now, retryAfter)
	} else if fresh {
		until = now.Add(Backoff(max(1, s.strikes), b.random))
	}
	if until.After(s.cooldown) {
		// A pending wake-up re-arms itself for the extended end; waiters look
		// again in case the longer wait should be deferred.
		s.cooldown = until
		s.status = status
		s.broadcast()
	}
	s.endRecovery()
	s.recoverPending = true
	s.lastLimit = now
	if s.adapt != nil {
		s.adapt.rateLimited(now)
	}
	b.armCooldownLocked(s)
	return s.cooldown, fresh
}

// addWait adds a server-requested wait to now without overflowing.
func addWait(now time.Time, wait time.Duration) time.Time {
	if until := now.Add(wait); until.After(now) {
		return until
	}
	return now.Add(maxDuration)
}

// Succeeded clears the backoff escalation after a request completes outside
// any cooldown, and reopens a recovering source to every waiting stream.
func (m *Member) Succeeded() {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	s := m.src
	if !b.clock.Now().Before(s.cooldown) {
		s.strikes = 0
	}
	if s.recovering {
		s.endRecovery()
		s.broadcast()
	}
}

// beginRecovery enters the probe phase once a cooldown has ended. Caller
// holds b.mu and has checked that the cooldown is over.
func (s *source) beginRecovery() {
	if s.recoverPending {
		s.recoverPending = false
		s.recovering, s.probing = true, false
	}
}

// endRecovery stops a recovery probe phase. Caller holds b.mu.
func (s *source) endRecovery() {
	s.recovering, s.probing = false, false
	if s.stopProbe != nil {
		s.stopProbe()
		s.stopProbe = nil
	}
}

// CoolingDown reports whether the member's source asked clients to wait.
func (m *Member) CoolingDown() bool {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock.Now().Before(m.src.cooldown)
}

// Cooldown returns the end of the source cooldown, or zero when none applies.
func (b *Budget) Cooldown(key SourceKey) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.sources[key]; s != nil && b.clock.Now().Before(s.cooldown) {
		return s.cooldown
	}
	return time.Time{}
}

// Probe reports whether an optional extra request (such as a capability
// probe) may be sent to the source right now without waiting.
func (b *Budget) Probe(key SourceKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sources[key]
	if s == nil {
		return true
	}
	if b.clock.Now().Before(s.cooldown) {
		return false
	}
	return s.adapt == nil || len(s.members) == 0 || s.active < s.capacity()
}

// Transferred records payload bytes for the source's throughput windows.
func (m *Member) Transferred(n int64) {
	m.src.bytes.Add(n)
}

// SetRemaining reports bytes this member still has to download, letting the
// adaptive controller skip experiments that a short transfer cannot repay.
func (m *Member) SetRemaining(n int64) {
	m.remaining.Store(n)
}

// MemberSnapshot is a diagnostic view of a member and its source.
type MemberSnapshot struct {
	Active         int
	Limit          int
	SourceActive   int
	SourceCapacity int
	Members        int
	CooldownUntil  time.Time
	CooldownStatus int
	Adaptive       *AdaptiveSnapshot
}

func (m *Member) Snapshot() MemberSnapshot {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	s := m.src
	out := MemberSnapshot{Active: m.active, Limit: m.limit, SourceActive: s.active,
		SourceCapacity: s.capacity(), Members: len(s.members)}
	if b.clock.Now().Before(s.cooldown) {
		out.CooldownUntil, out.CooldownStatus = s.cooldown, s.status
	}
	if s.adapt != nil {
		snap := s.adapt.snapshot()
		out.Adaptive = &snap
	}
	return out
}

func (s *source) limitSum() int {
	sum := 0
	for _, m := range s.members {
		sum += m.limit
	}
	return sum
}

// capacity is the number of streams the source may run: the members' combined
// limits, lowered by an adaptive target but never below one stream per member.
func (s *source) capacity() int {
	sum := s.limitSum()
	if s.adapt != nil && s.adapt.target > 0 && s.adapt.target < sum {
		return max(s.adapt.target, len(s.members))
	}
	return sum
}

// broadcast wakes everyone waiting for a capacity change. Caller holds b.mu.
func (s *source) broadcast() {
	close(s.wake)
	s.wake = make(chan struct{})
}

func (b *Budget) grantLocked(m *Member) bool {
	s := m.src
	if m.left || b.closed || m.active >= m.limit {
		m.waiting = false
		return false
	}
	if b.clock.Now().Before(s.cooldown) {
		return false
	}
	s.beginRecovery()
	if s.recovering && s.probing {
		return false
	}
	if s.active >= s.capacity() || b.unfairLocked(m) {
		m.waiting = true
		return false
	}
	if s.recovering {
		// This permit probes the source; the rest wait for its outcome.
		b.startProbeLocked(s)
	}
	m.active++
	s.active++
	m.waiting = false
	return true
}

// startProbeLocked lets one request probe a recovering source. The others wait
// for its outcome, or for recoveryProbeTimeout if it neither succeeds nor is
// refused.
func (b *Budget) startProbeLocked(s *source) {
	s.probing = true
	s.stopProbe = b.clock.AfterFunc(recoveryProbeTimeout, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if s.recovering {
			s.recovering, s.probing, s.stopProbe = false, false, nil
			s.broadcast()
		}
	})
}

// unfairLocked keeps an extra stream from a member while another waiting
// member holds fewer, so every task gets its first stream before extras.
func (b *Budget) unfairLocked(m *Member) bool {
	for _, o := range m.src.members {
		if o != m && o.waiting && o.active < m.active {
			return true
		}
	}
	return false
}

// armCooldownLocked schedules a wake-up at the end of the source cooldown.
func (b *Budget) armCooldownLocked(s *source) {
	if b.clock.Now().Before(s.cooldown) {
		s.timer.set(b, s.cooldown, func() { b.sourceWakeLocked(s) })
	}
}

func (b *Budget) sourceWakeLocked(s *source) {
	if b.clock.Now().Before(s.cooldown) {
		b.armCooldownLocked(s)
		return
	}
	s.beginRecovery()
	s.broadcast()
	b.forgetIdleLocked(s)
}

// forgetIdleLocked drops a source nobody uses once its cooldown, and the
// strikeMemory after it when it has strikes, have passed.
func (b *Budget) forgetIdleLocked(s *source) {
	if len(s.members) != 0 || s.active != 0 || s.timer.pending() || b.sources[s.key] != s {
		return
	}
	now := b.clock.Now()
	if now.Before(s.cooldown) {
		b.armCooldownLocked(s)
		return
	}
	if forget := s.cooldown.Add(strikeMemory); s.strikes > 0 && now.Before(forget) {
		s.timer.set(b, forget, func() { b.sourceWakeLocked(s) })
		return
	}
	s.endRecovery()
	delete(b.sources, s.key)
}

func (b *Budget) ensureTickerLocked() {
	if b.stopTick != nil || b.closed {
		return
	}
	var tick func()
	tick = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed {
			return
		}
		if !b.tickLocked() {
			b.stopTick = nil
			return
		}
		b.stopTick = b.clock.AfterFunc(adaptiveTick, tick)
	}
	b.stopTick = b.clock.AfterFunc(adaptiveTick, tick)
}

// tickLocked advances every adaptive source; false means none is left.
func (b *Budget) tickLocked() bool {
	now := b.clock.Now()
	any := false
	for _, s := range b.sources {
		if s.adapt == nil {
			continue
		}
		if len(s.members) == 0 {
			s.adapt = nil
			b.forgetIdleLocked(s)
			continue
		}
		any = true
		before := s.capacity()
		s.adapt.tick(now, s)
		if s.capacity() > before {
			s.broadcast()
		}
	}
	return any
}

// apiGate bounds concurrent API calls for one provider account or API origin.
type apiGate struct {
	key      SourceKey
	active   int
	cooldown time.Time
	strikes  int
	wake     chan struct{}
	timer    wakeup
}

func (g *apiGate) broadcast() {
	close(g.wake)
	g.wake = make(chan struct{})
}

// apiGateLocked returns the gate for key, creating it. Caller holds b.mu.
func (b *Budget) apiGateLocked(key SourceKey) *apiGate {
	g := b.apis[key]
	if g == nil {
		g = &apiGate{key: key, wake: make(chan struct{})}
		b.apis[key] = g
	}
	return g
}

// armAPILocked schedules a wake-up at the end of the gate's cooldown. The
// wake-up re-arms itself if the cooldown was extended, and forgets the gate
// once it is over and unused.
func (b *Budget) armAPILocked(g *apiGate) {
	if b.clock.Now().Before(g.cooldown) {
		g.timer.set(b, g.cooldown, func() { b.apiWakeLocked(g) })
	}
}

func (b *Budget) apiWakeLocked(g *apiGate) {
	if b.clock.Now().Before(g.cooldown) {
		b.armAPILocked(g)
		return
	}
	g.broadcast()
	b.forgetAPILocked(g)
}

// forgetAPILocked drops a gate with no call in flight once its cooldown, and
// the strikeMemory after it when it has strikes, have passed. Waiters were
// woken already and look the gate up again.
func (b *Budget) forgetAPILocked(g *apiGate) {
	if g.active != 0 || g.timer.pending() || b.apis[g.key] != g {
		return
	}
	now := b.clock.Now()
	if now.Before(g.cooldown) {
		b.armAPILocked(g)
		return
	}
	if forget := g.cooldown.Add(strikeMemory); g.strikes > 0 && now.Before(forget) {
		g.timer.set(b, forget, func() { b.apiWakeLocked(g) })
		return
	}
	delete(b.apis, g.key)
}

// AcquireAPI waits for an API slot and any cooldown on key. The returned
// function releases the slot. When ctx is a deferrable task run (see
// WithDeferral) and the cooldown should not be waited out, it returns a
// DeferredError before anything is sent. Every wake-up, including a task
// joining the queue, checks again, so an extension made meanwhile is
// honoured and a newly queued task can still get the slot.
func (b *Budget) AcquireAPI(ctx context.Context, key SourceKey) (func(), error) {
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return nil, ErrLeft
		}
		g := b.apiGateLocked(key)
		now := b.clock.Now()
		if !now.Before(g.cooldown) && g.active < apiConcurrency {
			g.active++
			b.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					b.mu.Lock()
					defer b.mu.Unlock()
					g.active--
					g.broadcast()
					b.armAPILocked(g)
					b.forgetAPILocked(g)
				})
			}, nil
		}
		wake, until := g.wake, g.cooldown
		b.armAPILocked(g)
		b.mu.Unlock()
		// The queue is read outside b.mu, so the budget's lock is never held
		// while taking one of the downloader's.
		queued := queueSignal(ctx)
		if until.After(now) && ShouldDefer(ctx, until.Sub(now)) {
			return nil, &DeferredError{Until: until}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		case <-queued:
		}
	}
}

// APICooldown returns the end of the cooldown on key, or zero when none applies.
func (b *Budget) APICooldown(key SourceKey) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if g := b.apis[key]; g != nil && b.clock.Now().Before(g.cooldown) {
		return g.cooldown
	}
	return time.Time{}
}

// APIRateLimited records a 429/503 from an API and returns its cooldown end.
// Waits follow RateLimitedEpisode: a positive Retry-After in full, otherwise
// an exponential backoff per consecutive episode. Side-effecting calls are not
// replayed; callers report the failure.
func (b *Budget) APIRateLimited(key SourceKey, retryAfter time.Duration, hasHeader bool) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.apiGateLocked(key)
	now := b.clock.Now()
	fresh := !now.Before(g.cooldown)
	if fresh {
		g.strikes++
	}
	var until time.Time
	if hasHeader && retryAfter > 0 {
		until = addWait(now, retryAfter)
	} else if fresh {
		until = now.Add(Backoff(max(1, g.strikes), b.random))
	}
	if until.After(g.cooldown) {
		// Waiters look again in case the longer wait should be deferred.
		g.cooldown = until
		g.broadcast()
	}
	b.armAPILocked(g)
	return g.cooldown
}

// APISucceeded clears the backoff escalation of key after a call completes
// outside any cooldown.
func (b *Budget) APISucceeded(key SourceKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if g := b.apis[key]; g != nil && !b.clock.Now().Before(g.cooldown) {
		g.strikes = 0
	}
}
