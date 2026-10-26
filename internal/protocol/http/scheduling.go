package http

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Consecutive deferrals without progress before a rate limit becomes an error.
const maxDeferrals = 10

// sourceKey groups this download with others that share server-side limits:
// a known scope (such as a netdisk account) or the origin plus proxy policy.
func (f *Fetcher) sourceKey() transfer.SourceKey {
	if f.ctl.SourceScope != "" {
		return transfer.SourceKey("scope|" + f.ctl.SourceScope)
	}
	raw := f.meta.Req.URL
	f.redirectLock.Lock()
	if f.redirectURL != "" {
		raw = f.redirectURL
	}
	f.redirectLock.Unlock()
	proxyID := "direct"
	if f.ctl.GetProxyPolicy != nil {
		_, proxyID = f.ctl.GetProxyPolicy(f.meta.Req.Proxy)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return transfer.SourceKey("origin|invalid|" + proxyID)
	}
	return transfer.SourceKey("origin|" + strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + "|" + proxyID)
}

func (f *Fetcher) streamLimit() int {
	if f.meta.Opts != nil {
		if extra, ok := f.meta.Opts.Extra.(*fhttp.OptsExtra); ok && extra.Connections > 0 {
			return extra.Connections
		}
	}
	return 1
}

// joinBudget registers the run with its source budget. Without a budget every
// permit helper is a no-op and streams behave as before.
func (f *Fetcher) joinBudget() {
	if f.ctl.Budget == nil || f.member.Load() != nil {
		return
	}
	m := f.ctl.Budget.Join(f.sourceKey(), f.streamLimit(), f.tuning().adaptive)
	m.SetRemaining(f.remainingBytes())
	f.member.Store(m)
}

func (f *Fetcher) leaveBudget() {
	if m := f.member.Swap(nil); m != nil {
		m.Leave()
	}
}

func (f *Fetcher) tryPermit() bool {
	m := f.member.Load()
	return m == nil || m.TryAcquire()
}

// waitPermit blocks until the budget grants the connection a stream again.
func (f *Fetcher) waitPermit(conn *connection) error {
	m := f.member.Load()
	if m == nil {
		conn.permit = true
		return conn.ctx.Err()
	}
	start := time.Now()
	if err := m.Acquire(conn.ctx); err != nil {
		return err
	}
	if waited := time.Since(start); waited > 10*time.Millisecond {
		f.diag.waited(waited)
	}
	conn.permit = true
	return nil
}

func (f *Fetcher) releasePermit(conn *connection) {
	if !conn.permit {
		return
	}
	conn.permit = false
	m := f.member.Load()
	if m == nil {
		return
	}
	// A shed reservation is returned once; a stream that later gets a new
	// permit runs as an ordinary stream.
	f.connMu.Lock()
	shedding := conn.shedding
	conn.shedding = false
	f.connMu.Unlock()
	if shedding {
		m.ReleaseShed()
	} else {
		m.Release()
	}
}

// rateLimit applies a 429/503 to the whole source and returns how long the
// stream must wait, or a deferral when the wait is too long to hold a slot.
// Every refusal episode and every deferral counts until a request succeeds;
// past maxDeferrals, or when the server asks for more than a day, the stream
// gives up so the task reports the rate limit as an error instead of retrying.
func (f *Fetcher) rateLimit(re *RequestError, attempt int) (time.Duration, *transfer.DeferredError, bool) {
	f.diag.rateLimited()
	var until time.Time
	fresh := true
	if m := f.member.Load(); m != nil {
		until, fresh = m.RateLimitedEpisode(re.Code, re.RetryAfter, re.HasRetryAfter)
	} else if f.ctl.Budget != nil {
		until, fresh = f.ctl.Budget.RateLimitedEpisode(f.sourceKey(), re.Code, re.RetryAfter, re.HasRetryAfter)
	} else if re.HasRetryAfter && re.RetryAfter > 0 {
		until = time.Now().Add(re.RetryAfter)
	} else {
		// Zero, past or missing waits still back off between refusals.
		until = time.Now().Add(transfer.Backoff(attempt, nil))
	}
	wait := time.Until(until)
	if wait > transfer.MaxRetryAfter {
		return wait, nil, true
	}
	deferring := f.shouldDefer(wait)
	if (fresh || deferring) && f.deferrals.Add(1) > maxDeferrals {
		return wait, nil, true
	}
	if deferring {
		return wait, &transfer.DeferredError{Until: until, Status: re.Code}, false
	}
	return wait, nil, false
}

// admitFirstRequest waits, as a member of the source budget, until a
// download's first request may be sent: no cooldown, and during recovery
// either this request is the probe or the probe has succeeded. Every wake-up
// rechecks the latest cooldown, so one extended meanwhile is honoured in full.
// A wait that should not hold the task's running slot returns a deferral
// before anything is sent, also when other tasks queue up during the wait.
// The caller reports the outcome to the member and then leaves it. A budget
// closed with its downloader limits nothing any more: the request goes out
// without a member, as it would without a budget.
func (f *Fetcher) admitFirstRequest(ctx context.Context) (*transfer.Member, error) {
	m := f.ctl.Budget.Join(f.sourceKey(), 1, false)
	for {
		// The wake channel comes from the same critical section as the failed
		// attempt; the queue signal is taken before the queue is looked at.
		granted, wake, until, closed := m.TryAcquireOrWait()
		if granted {
			return m, nil
		}
		if closed {
			m.Leave()
			return nil, nil
		}
		queued := f.queueChanged()
		if !until.IsZero() && f.shouldDefer(time.Until(until)) {
			m.Leave()
			return nil, &transfer.DeferredError{Until: until}
		}
		select {
		case <-ctx.Done():
			m.Leave()
			return nil, ctx.Err()
		case <-wake:
		case <-queued:
		}
	}
}

// queueChanged returns the channel closed when tasks may have joined the
// download queue, or nil (never ready) without a downloader.
func (f *Fetcher) queueChanged() <-chan struct{} {
	if f.ctl.QueueChanged == nil {
		return nil
	}
	return f.ctl.QueueChanged()
}

// shouldDefer reports whether waiting this long for a source cooldown should
// release the task's running slot: always beyond DeferThreshold, and from
// QueuedDeferThreshold when queued tasks could use the slot.
func (f *Fetcher) shouldDefer(wait time.Duration) bool {
	if f.ctl.Budget == nil {
		return false
	}
	queued := f.ctl.QueueWaiting != nil && wait >= transfer.QueuedDeferThreshold && f.ctl.QueueWaiting()
	return wait > transfer.DeferThreshold || queued
}

// deferIfCooling ends a run whose source is in a cooldown that should not hold
// a running slot: before a resumed or restarted task starts, and when another
// task joins the queue while the run waits.
func (f *Fetcher) deferIfCooling() bool {
	m := f.member.Load()
	if m == nil {
		return false
	}
	until := m.Snapshot().CooldownUntil
	if until.IsZero() || !f.shouldDefer(time.Until(until)) {
		return false
	}
	f.deferRun(&transfer.DeferredError{Until: until})
	return true
}

// deferRun stops every stream so the scheduler can release this task's slot
// until the source cooldown ends. Progress and ranges stay as they are.
func (f *Fetcher) deferRun(deferred *transfer.DeferredError) {
	if f.deferred.CompareAndSwap(nil, deferred) && f.cancel != nil {
		f.cancel()
	}
}

// sleepContext waits for d unless ctx ends first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (f *Fetcher) remainingBytes() int64 {
	if f.meta.Res == nil || f.meta.Res.Size <= 0 {
		return 0
	}
	f.connMu.Lock()
	defer f.connMu.Unlock()
	done := int64(0)
	if f.resolveConn != nil {
		done += f.resolveConn.Downloaded
	}
	for _, conn := range f.connections {
		done += conn.Downloaded
	}
	if done >= f.meta.Res.Size {
		return 0
	}
	return f.meta.Res.Size - done
}

// startStream launches a connection goroutine that already holds a permit.
func (f *Fetcher) startStream(conn *connection) {
	f.wg.Add(1)
	f.liveStreams.Add(1)
	go f.runConnection(conn)
}

func (f *Fetcher) streamExited() {
	f.liveStreams.Add(-1)
	select {
	case f.streamExit <- struct{}{}:
	default:
	}
}

// shedMargin is how far past its current position a shed stream keeps
// downloading, comfortably beyond any read already in flight.
func (f *Fetcher) shedMargin() int64 {
	return max(int64(256*1024), 4*int64(f.tuning().readBuffer))
}

// govern applies a lowered adaptive target: the stream with the most work left
// finishes a short margin and hands the rest back as a parked range.
func (f *Fetcher) govern() {
	m := f.member.Load()
	if m == nil {
		return
	}
	m.SetRemaining(f.remainingBytes())
	for m.ShedOne() {
		if !f.parkOne() {
			m.CancelShed()
			return
		}
	}
}

func (f *Fetcher) parkOne() bool {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	var victim *connection
	for _, c := range f.connections {
		if c.parked || c.shedding || c.Completed || c.State != connDownloading {
			continue
		}
		if victim == nil || c.Chunk.remain() > victim.Chunk.remain() {
			victim = c
		}
	}
	if victim == nil {
		return false
	}
	victim.shedding = true
	cut := victim.Chunk.Begin + victim.Chunk.Downloaded + f.shedMargin()
	if cut <= victim.Chunk.End {
		parked := &connection{ID: len(f.connections), Role: roleWorker, State: connNotStarted,
			Chunk: newChunk(cut, victim.Chunk.End), parked: true}
		parked.ctx, parked.cancel = context.WithCancel(f.ctx)
		victim.Chunk.End = cut - 1
		f.connections = append(f.connections, parked)
	}
	return true
}

// launchParked gives parked ranges a stream while the budget allows. Ranges
// handed off after a 403 wait for a running stream to take them, unless no
// stream is left, so a connection-limited server is not sent more connections.
func (f *Fetcher) launchParked() int {
	launched := 0
	for f.ctx.Err() == nil {
		live := f.liveStreams.Load()
		f.connMu.Lock()
		var next *connection
		for _, c := range f.connections {
			if c.parked && !c.Completed && c.Chunk.remain() > 0 && (c.handoffs == 0 || live == 0) {
				next = c
				break
			}
		}
		f.connMu.Unlock()
		if next == nil || !f.tryPermit() {
			break
		}
		// A finishing stream may have taken the range meanwhile; claim it only
		// if it is still parked and unfinished.
		f.connMu.Lock()
		claimed := next.parked && !next.Completed && next.Chunk.remain() > 0
		if claimed {
			next.parked = false
			next.permit = true
			next.State = connNotStarted
			next.failed = false
		}
		f.connMu.Unlock()
		if !claimed {
			if m := f.member.Load(); m != nil {
				m.Release()
			}
			continue
		}
		f.startStream(next)
		launched++
	}
	return launched
}

func (f *Fetcher) hasParkedWork() bool {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	for _, c := range f.connections {
		if c.parked && !c.Completed && c.Chunk.remain() > 0 {
			return true
		}
	}
	return false
}

// takeParked hands a whole parked range to a helper. Caller holds connMu.
func (f *Fetcher) takeParked(helper *connection) bool {
	for _, p := range f.connections {
		if !p.parked || p.Completed || p.Chunk.remain() <= 0 {
			continue
		}
		helper.Chunk.Begin = p.Chunk.Begin + p.Chunk.Downloaded
		helper.Chunk.End = p.Chunk.End
		helper.Chunk.Downloaded = 0
		helper.handoffs = p.handoffs
		p.Chunk.End = p.Chunk.Begin + p.Chunk.Downloaded - 1
		p.parked = false
		p.failed = false
		p.Completed = true
		p.State = connCompleted
		return true
	}
	return false
}

// yieldStream lets a task holding several streams give one up at a chunk
// boundary while another task at the same source waits for its first.
func (f *Fetcher) yieldStream() bool {
	m := f.member.Load()
	return m != nil && m.Contended()
}
