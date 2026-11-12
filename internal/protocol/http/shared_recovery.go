package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

var errSharedWaveDraining = errors.New("shared_recovery_wave_draining")

type sharedHTTPController interface {
	transfer.RecoveryActionController
	ExtendRecoveryDeferral(context.Context, transfer.RecoveryToken, uint64, string, []string, time.Time) (time.Time, error)
	AdmitRecoveryWave(context.Context, *RecoveryWaveSeal, uint64) (map[string]*transfer.RecoveryPermit, error)
}

type sharedHTTPWait struct {
	seen    uint64
	work    string
	members []string
	until   time.Time
	status  int
	done    chan struct{}
	err     error
}

type httpSharedRecovery struct {
	sequence  atomic.Uint64
	mu        sync.Mutex
	f         *Fetcher
	run       *httpRecoveryRun
	lease     *transfer.RecoveryLease
	owner     sharedHTTPController
	barrier   *recoveryWaveBarrier
	failure   error
	pending   *sharedHTTPWait
	permits   map[string]*transfer.RecoveryPermit
	completed map[string]bool
}

func newHTTPSharedRecovery(f *Fetcher, run *httpRecoveryRun, lease *transfer.RecoveryLease) *httpSharedRecovery {
	s := &httpSharedRecovery{f: f, run: run, lease: lease, permits: map[string]*transfer.RecoveryPermit{}, completed: map[string]bool{}}
	var ok bool
	s.owner, ok = lease.Owner.(sharedHTTPController)
	if !ok {
		s.failure = transfer.ErrRecoveryHeld
		return s
	}
	s.barrier, s.failure = newRecoveryWaveBarrier(lease.Context, lease)
	return s
}

func (f *Fetcher) sharedRun() *httpSharedRecovery {
	if r := f.recovery.Load(); r != nil {
		return r.shared
	}
	return nil
}

func (f *Fetcher) enterSharedIO(ctx context.Context, work string) (*recoveryWaveWork, error) {
	s := f.sharedRun()
	if s == nil {
		return &recoveryWaveWork{Context: ctx, leave: func() {}}, nil
	}
	if s.failure != nil {
		return nil, s.failure
	}
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil || lease.Token != s.lease.Token {
		return nil, transfer.ErrRecoveryStale
	}
	scope, err := s.barrier.enter(ctx, work)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	permit := s.permits[work]
	delete(s.permits, work)
	s.mu.Unlock()
	if permit != nil {
		if err = permit.Consume(); err != nil {
			scope.Leave()
			return nil, err
		}
		s.mu.Lock()
		s.completed[work] = true
		s.mu.Unlock()
		f.connMu.Lock()
		for _, conn := range f.connections {
			if f.recoveryWork(conn) == work {
				conn.recoveryInitialized = true
				conn.recoveryRound = permit.Round
				conn.recoveryPermit = nil
			}
		}
		f.connMu.Unlock()
	}
	return scope, nil
}

func sharedScopeResult(scope *recoveryWaveWork, parent context.Context, err error) error {
	if scope != nil && scope.Context.Err() != nil && parent.Err() == nil {
		return errSharedWaveDraining
	}
	return err
}

// Called after leaving the reporting request/body/write scope. The independent
// coordinator drains scopes, never the workers parked on this result channel.
func (f *Fetcher) sharedRateLimited(ctx context.Context, work string, until time.Time, status int) (bool, error) {
	s := f.sharedRun()
	if s == nil {
		return false, nil
	}
	if s.failure != nil {
		return true, s.failure
	}
	s.run.mu.Lock()
	seen := s.run.round
	f.connMu.Lock()
	members := []string{}
	unique := map[string]bool{}
	for _, c := range f.connections {
		if !c.Completed {
			w := f.recoveryWork(c)
			if !unique[w] {
				unique[w] = true
				members = append(members, w)
			}
		}
	}
	if len(members) == 0 && s.run.initialLimit > 0 {
		for id := 0; id < s.run.initialLimit; id++ {
			w := fmt.Sprintf("worker/%d", id)
			members = append(members, w)
			unique[w] = true
		}
	}
	if !unique[work] {
		members = append(members, work)
	}
	f.connMu.Unlock()
	s.run.mu.Unlock()
	s.mu.Lock()
	p := s.pending
	if p == nil {
		p = &sharedHTTPWait{seen: seen, work: work, members: members, until: until, status: status, done: make(chan struct{})}
		s.pending = p
		go s.coordinate(p)
	} else {
		included := false
		for _, member := range p.members {
			if member == work {
				included = true
				break
			}
		}
		if !included {
			s.mu.Unlock()
			return true, transfer.ErrRecoveryWork
		}
		if until.After(p.until) {
			p.until = until
		}
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-p.done:
		return true, p.err
	}
}

func (s *httpSharedRecovery) finish(p *sharedHTTPWait, err error) {
	s.mu.Lock()
	p.err = err
	if s.pending == p {
		s.pending = nil
	}
	close(p.done)
	s.mu.Unlock()
	if err != nil {
		s.barrier.close(err)
	}
}

func (s *httpSharedRecovery) coordinate(p *sharedHTTPWait) {
	ctx := s.lease.Context
	seal, err := s.barrier.drain(ctx, p.members)
	if err != nil {
		s.finish(p, err)
		return
	}
	s.mu.Lock()
	until := p.until
	s.mu.Unlock()
	permit, err := s.owner.ReserveRecoveryAction(ctx, s.lease.Token, p.seen, transfer.RecoveryActionDeferral, p.work, p.members, until)
	if err == nil {
		err = permit.Consume(ctx)
	}
	if err != nil {
		s.finish(p, err)
		return
	}
	until = permit.Until
	for {
		s.mu.Lock()
		latest := p.until
		s.mu.Unlock()
		if budget := s.f.ctl.Budget; budget != nil {
			if u := budget.Cooldown(s.f.sourceKey()); u.After(latest) {
				latest = u
			}
		}
		if latest.After(until) {
			until, err = s.owner.ExtendRecoveryDeferral(ctx, s.lease.Token, permit.ActionID, p.work, p.members, latest)
			if err != nil {
				s.finish(p, err)
				return
			}
		}
		if s.f.shouldDefer(time.Until(until)) {
			s.run.mu.Lock()
			current := s.f.recovery.Load() == s.run && ctx.Err() == nil
			if current {
				s.f.deferRun(&transfer.DeferredError{Until: until, Status: p.status})
			}
			s.run.mu.Unlock()
			if !current {
				s.finish(p, transfer.ErrRecoveryStale)
				return
			}
			s.finish(p, &transfer.DeferredError{Until: until, Status: p.status})
			return
		}
		if !until.After(time.Now()) {
			break
		}
		if member := s.f.member.Load(); member != nil {
			wake := member.Changed()
			queued := s.f.queueChanged()
			// Recheck after acquiring the wake channel so expiration cannot be missed.
			if !until.After(time.Now()) {
				continue
			}
			select {
			case <-ctx.Done():
				s.finish(p, ctx.Err())
				return
			case <-wake:
			case <-queued:
			}
		} else if !sleepContext(ctx, time.Until(until)) {
			s.finish(p, ctx.Err())
			return
		}
	}
	permits, err := s.owner.AdmitRecoveryWave(ctx, seal, permit.ActionID)
	if err != nil {
		s.finish(p, err)
		return
	}
	// Owner validation is outside run/barrier mutexes. Actual requests also
	// recheck their immutable lease, so cancellation after this check grants no IO.
	if err = s.lease.Check(); err != nil {
		s.finish(p, err)
		return
	}
	s.run.mu.Lock()
	if ctx.Err() != nil || s.f.recovery.Load() != s.run {
		s.run.mu.Unlock()
		s.finish(p, transfer.ErrRecoveryStale)
		return
	}
	err = seal.Check(ctx)
	if err == nil {
		s.mu.Lock()
		s.permits = make(map[string]*transfer.RecoveryPermit, len(permits))
		for work, permit := range permits {
			s.permits[work] = permit
		}
		s.completed = map[string]bool{}
		// Clear before opening the gate: a new refusal belongs to a NEW action.
		if s.pending == p {
			s.pending = nil
		}
		s.mu.Unlock()
		for _, next := range permits {
			s.run.round = next.Round
			break
		}
		s.run.members = append([]string{}, p.members...)
		for id := 0; id < s.run.initialLimit; id++ {
			s.run.initialClaims[id] = true
		}
		err = seal.reopen(ctx)
	}
	s.run.mu.Unlock()
	s.finish(p, err)
}

// Every actual HTTP request (including probes) registers through the existing
// guard transport. Worker scopes additionally cover read->WriteAt/truncate, so
// a response close cannot hide bytes still being written by its consumer.
func (f *Fetcher) enterSharedRequest(ctx context.Context) (*recoveryWaveWork, error) {
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil || lease.ActionVersion == 0 {
		return nil, nil
	}
	run := f.recovery.Load()
	if lease.ActionVersion != 4 || run == nil || run.lease.Token != lease.Token || run.shared == nil {
		return nil, transfer.ErrRecoveryStale
	}
	s := run.shared
	if s.failure != nil {
		return nil, s.failure
	}
	return s.barrier.enter(ctx, fmt.Sprintf("request/%d", s.sequence.Add(1)))
}

type sharedWaveBody struct {
	io.ReadCloser
	work *recoveryWaveWork
	once sync.Once
	done chan struct{}
}

func wrapSharedWaveBody(body io.ReadCloser, work *recoveryWaveWork) io.ReadCloser {
	if work == nil {
		return body
	}
	b := &sharedWaveBody{ReadCloser: body, work: work, done: make(chan struct{})}
	go func() {
		select {
		case <-work.Context.Done():
			_ = b.Close()
		case <-b.done:
		}
	}()
	return b
}
func (body *sharedWaveBody) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if err != nil {
		_ = body.Close()
	}
	return n, err
}
func (body *sharedWaveBody) Close() error {
	var err error
	body.once.Do(func() { err = body.ReadCloser.Close(); body.work.Leave(); close(body.done) })
	return err
}
func (f *Fetcher) sharedDeadline(wait time.Duration, deferred *transfer.DeferredError) time.Time {
	if deferred != nil {
		return deferred.Until
	}
	if b := f.ctl.Budget; b != nil {
		if until := b.Cooldown(f.sourceKey()); !until.IsZero() {
			return until
		}
	}
	return time.Now().Add(wait)
}
func (f *Fetcher) waitSharedWorker(conn *connection) error {
	if f.sharedRun() == nil {
		return nil
	}
	work, err := f.enterSharedIO(conn.ctx, f.recoveryWork(conn))
	if work != nil {
		work.Leave()
	}
	return err
}
