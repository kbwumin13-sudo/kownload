package transfer

import (
	"context"
	"time"

	"github.com/GopeedLab/gopeed/internal/benchtrace"
)

const (
	// Space post-cooldown starts independently from the source's earliest
	// permitted retry. Header hints are coarse (whole seconds). Admit at most
	// two requests per second initially, then gather samples before speeding up.
	rampMinInitialSpacing = 500 * time.Millisecond
	rampMaxInitialSpacing = time.Second
	rampReleaseSpacing    = 125 * time.Millisecond
	rampSuccessesPerStep  = 6
)

type recoveryRamp struct {
	spacing   time.Duration
	next      time.Time
	confirmed bool
	successes int
	probe     *HTTPRecoveryPermit
}

// HTTPRecoveryPermit tracks one request start in a recovering source. A reply
// from a replaced recovery episode has no authority to advance its pacing.
type HTTPRecoveryPermit struct {
	budget *Budget
	src    *source
	ramp   *recoveryRamp
	probe  bool
	expiry time.Time
}

func (s *source) resetHTTPRamp(wait time.Duration) {
	spacing := min(rampMaxInitialSpacing, max(rampMinInitialSpacing, wait))
	if s.ramp != nil {
		spacing = max(spacing, s.ramp.spacing)
	}
	s.ramp = &recoveryRamp{spacing: spacing}
}

// AdmitHTTPRequest gates actual HTTP requests after a rate limit. Existing
// stream permits count active connections and can survive an interrupted
// recovery wave. Gate real starts so those permits cannot bypass recovery.
func (b *Budget) AdmitHTTPRequest(ctx context.Context, key SourceKey) (*HTTPRecoveryPermit, error) {
	waiting := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		queueChanged := queueSignal(ctx)
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			if waiting {
				return nil, ErrLeft
			}
			return nil, nil
		}
		s := b.sources[key]
		if s == nil {
			b.mu.Unlock()
			return nil, nil
		}
		ramp := s.ramp
		now := b.clock.Now()
		cooldown := s.effectiveCooldown()
		at := cooldown
		if ramp != nil {
			at = maxTime(at, ramp.next)
			if ramp.probe != nil {
				at = maxTime(at, ramp.probe.expiry)
			}
		}
		if !now.Before(at) {
			if ramp == nil {
				b.mu.Unlock()
				return nil, nil
			}
			p := &HTTPRecoveryPermit{budget: b, src: s, ramp: ramp, probe: !ramp.confirmed}
			ramp.next = now.Add(ramp.spacing)
			if p.probe {
				p.expiry = now.Add(recoveryProbeTimeout)
				ramp.probe = p
				if benchtrace.Enabled {
					benchtrace.Budget(string(key), "rampProbe", now, time.Time{})
				}
			}
			b.mu.Unlock()
			return p, nil
		}
		s.httpWake.set(b, at, s.broadcast)
		wake, status := s.wake, s.status
		b.mu.Unlock()
		wait := cooldown.Sub(now)
		if wait > 0 && ShouldDefer(ctx, max(wait, wait.Round(time.Second))) {
			return nil, &DeferredError{Until: cooldown, Status: status}
		}
		waiting = true
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		case <-queueChanged:
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ObserveHTTPRefusal preserves an actually received positive Retry-After
// before the response is handed to its (possibly obsolete) worker. The worker
// still owns episode accounting. An obsolete refusal without a usable hint
// uses the existing fallback only if there is no current cooldown to honor.
func (b *Budget) ObserveHTTPRefusal(key SourceKey, wait time.Duration, obsolete bool) {
	if wait <= 0 && !obsolete {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sources[key]
	if b.closed || s == nil {
		return
	}
	now := b.clock.Now()
	fresh := !now.Before(s.effectiveCooldown())
	if wait <= 0 {
		if !fresh {
			return
		}
		wait = Backoff(max(1, s.strikes+1), b.random)
	}
	if until := addWait(now, wait); until.After(s.httpNotBefore) {
		s.httpNotBefore = until
		if obsolete && fresh {
			s.resetHTTPRamp(wait)
		}
		s.broadcast()
		b.armCooldownLocked(s)
	}
}

// Finish observes admission at the server from response headers. Payload
// validity and authorization to write remain the caller's responsibility.
func (p *HTTPRecoveryPermit) Finish(accepted bool) {
	if p == nil {
		return
	}
	b, s, ramp := p.budget, p.src, p.ramp
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.sources[s.key] != s || s.ramp != ramp || p.probe && ramp.probe != p {
		return
	}
	if p.probe {
		ramp.probe = nil
		ramp.confirmed = accepted
		s.broadcast()
	}
	if !accepted {
		ramp.successes = 0
		return
	}
	ramp.successes++
	if ramp.successes < rampSuccessesPerStep {
		return
	}
	ramp.successes = 0
	ramp.spacing = ramp.spacing * 3 / 4
	if ramp.spacing <= rampReleaseSpacing {
		s.ramp = nil
		s.broadcast()
	}
}
