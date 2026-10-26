package transfer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	backoffBase = time.Second
	backoffCap  = 30 * time.Second
	// A server-requested wait longer than this releases the task's running slot
	// instead of keeping it while idle.
	DeferThreshold = 30 * time.Second
	// While other tasks queue for a slot, waits from this long are given up too.
	QueuedDeferThreshold = 2 * time.Second
	// A run asked to wait longer than this fails instead of waiting: the server
	// is not retried early, and the task does not sit queued for days.
	MaxRetryAfter = 24 * time.Hour
	// Longest representable wait; larger Retry-After values mean "do not retry".
	maxDuration = time.Duration(1<<63 - 1)
)

// ParseRetryAfter reads the delay-seconds or HTTP-date form of Retry-After.
// ok is false when the header is absent or malformed. Past dates mean "now".
// Waits are returned in full, however long; callers decide what a wait beyond
// MaxRetryAfter means rather than shortening it.
func ParseRetryAfter(value string, now time.Time) (wait time.Duration, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds > int64(maxDuration/time.Second) {
			// Digits only but too large to represent: never sooner than the limit.
			return maxDuration, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	wait = at.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

// Backoff returns the wait before retry number attempt (1-based) when the
// server gave no Retry-After: 1s, 2s, 4s ... capped at 30s, with equal jitter
// so parallel streams do not retry in lockstep.
func Backoff(attempt int, random func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := backoffCap
	if attempt <= 6 {
		wait = backoffBase << (attempt - 1)
		if wait > backoffCap {
			wait = backoffCap
		}
	}
	if random == nil {
		random = rand.Float64
	}
	half := wait / 2
	return half + time.Duration(random()*float64(half))
}

// IsRateLimitStatus reports responses that ask the client to slow down.
func IsRateLimitStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// DeferredError ends a run that must wait for a source cooldown longer than
// DeferThreshold. The scheduler requeues the task at Until without failing it.
type DeferredError struct {
	Until  time.Time
	Status int
}

func (e *DeferredError) Error() string {
	return fmt.Sprintf("source rate limited (HTTP %d), retry after %s", e.Status, e.Until.Format(time.RFC3339))
}

func AsDeferred(err error) (*DeferredError, bool) {
	var deferred *DeferredError
	if errors.As(err, &deferred) {
		return deferred, true
	}
	return nil, false
}

type deferralKey struct{}

// deferral marks a context as belonging to a task run that may give its
// running slot back when a request would first have to wait out a cooldown.
type deferral struct {
	queueWaiting func() bool
	queueChanged func() <-chan struct{}
}

// WithDeferral marks ctx as a task run that may be deferred: a request that
// would wait for a cooldown before it is sent returns a DeferredError instead
// when ShouldDefer says so. queueWaiting reports whether other tasks wait for
// a running slot; nil means none ever do. queueChanged, when not nil, returns
// a channel closed once tasks may have joined the queue, so a request already
// waiting decides again.
func WithDeferral(ctx context.Context, queueWaiting func() bool, queueChanged func() <-chan struct{}) context.Context {
	return context.WithValue(ctx, deferralKey{}, deferral{queueWaiting: queueWaiting, queueChanged: queueChanged})
}

// queueSignal returns the queue-change channel of a deferrable ctx, or nil
// (never ready) when there is none. Take it before ShouldDefer looks at the
// queue, so a task queued in between is not missed.
func queueSignal(ctx context.Context) <-chan struct{} {
	if d, ok := ctx.Value(deferralKey{}).(deferral); ok && d.queueChanged != nil {
		return d.queueChanged()
	}
	return nil
}

// WithoutDeferral clears the mark for requests that must not be split from
// work already done, such as a side-effecting operation under way.
func WithoutDeferral(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferralKey{}, nil)
}

// ShouldDefer reports whether a run made with ctx should give its slot back
// instead of waiting this long: always beyond DeferThreshold, and from
// QueuedDeferThreshold while other tasks wait for a slot. Contexts without
// WithDeferral always wait.
func ShouldDefer(ctx context.Context, wait time.Duration) bool {
	d, ok := ctx.Value(deferralKey{}).(deferral)
	if !ok {
		return false
	}
	if wait > DeferThreshold {
		return true
	}
	return wait >= QueuedDeferThreshold && d.queueWaiting != nil && d.queueWaiting()
}
