package transfer

import (
	"context"
	"sync/atomic"
	"time"
)

// RecoveryActionKind is an internal Go protocol boundary, not a public REST
// capability or a new persisted action format. The durable owner validates it.
type RecoveryActionKind string

const (
	RecoveryActionTransport RecoveryActionKind = "transport"
	RecoveryActionRefresh   RecoveryActionKind = "refresh"
	RecoveryActionDeferral  RecoveryActionKind = "deferral"
)

// RecoveryActionController is optional for legacy transport-only owners. A
// shared managed lease requires this interface; absence must never downgrade
// an enrolled task to unaccounted source retries.
type RecoveryActionController interface {
	ReserveRecoveryAction(context.Context, RecoveryToken, uint64, RecoveryActionKind, string, []string, time.Time) (*RecoveryActionPermit, error)
}

// RecoveryActionPermit is a one-use process-local transition capability. In
// particular a deferral permit allows only its waiting/queue transition. It
// NEVER authorizes a request: Until and the owner's atomic next-wave admission
// must still succeed, with a separate bounded set of IO permits.
type RecoveryActionPermit struct {
	ActionID uint64
	Token    RecoveryToken
	Kind     RecoveryActionKind
	Until    time.Time
	validate func(context.Context) error
	used     atomic.Bool
}

// IssueRecoveryActionPermit is used only by the trusted durable owner after a
// confirmed fresh ACK. The validator is bound to immutable D/E/G/S/action and
// the owner's current lifetime, not to caller-provided authentication data.
func IssueRecoveryActionPermit(token RecoveryToken, id uint64, kind RecoveryActionKind, until time.Time, validate func(context.Context) error) *RecoveryActionPermit {
	return &RecoveryActionPermit{ActionID: id, Token: token, Kind: kind, Until: until, validate: validate}
}

func (permit *RecoveryActionPermit) Consume(ctx context.Context) error {
	if permit == nil || permit.ActionID == 0 || permit.validate == nil || ctx == nil {
		return ErrRecoveryWork
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := permit.validate(ctx); err != nil {
		return err
	}
	if !permit.used.CompareAndSwap(false, true) {
		return ErrRecoveryPermitUsed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return permit.validate(ctx)
}
