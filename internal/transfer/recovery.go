package transfer

import (
	"context"
	"errors"
	"sync/atomic"
)

var (
	ErrRecoveryUnavailable = errors.New("recovery_state_unavailable")
	ErrRecoveryHeld        = errors.New("recovery_state_incompatible")
	ErrRecoveryStale       = errors.New("recovery_generation_obsolete")
	ErrRecoveryExhausted   = errors.New("recovery_budget_exhausted")
	ErrRecoveryPermitUsed  = errors.New("recovery_permit_consumed")
	ErrRecoveryWork        = errors.New("recovery_work_not_authorized")
)

// RecoveryToken is an execution fence, not an authentication credential. Only
// the Task owner issues it; a new generation never replenishes an episode.
type RecoveryToken struct {
	TaskID         string
	Episode        string
	Generation     uint64
	SourceRevision uint64
}

// RecoveryController is implemented by the durable Task owner. Protocols never
// construct a second budget. Members is a frozen, bounded logical work set.
type RecoveryController interface {
	CheckRecovery(RecoveryToken) error
	RetryRecovery(context.Context, RecoveryToken, uint64, string, []string) (*RecoveryPermit, error)
	FinishRecovery(context.Context, RecoveryToken, uint64, string) error
}

type RecoveryLease struct {
	SourceHash string
	Token      RecoveryToken
	Round      uint64
	Owner      RecoveryController
}

func (l *RecoveryLease) Check() error {
	if l == nil {
		return nil
	}
	if l.Owner == nil {
		return ErrRecoveryUnavailable
	}
	return l.Owner.CheckRecovery(l.Token)
}
func (l *RecoveryLease) Retry(ctx context.Context, seen uint64, work string, members []string) (*RecoveryPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || l.Owner == nil {
		return nil, ErrRecoveryUnavailable
	}
	return l.Owner.RetryRecovery(ctx, l.Token, seen, work, members)
}

// A permit authorizes one logical retry, not arbitrary requests. Reading an
// action or asking twice returns no fresh authority. A process restart cannot
// reconstruct this in-memory one-shot capability from a charged DB record.
type RecoveryPermit struct {
	Round uint64
	Work  string
	lease *RecoveryLease
	used  atomic.Bool
}

func IssueRecoveryPermit(lease *RecoveryLease, round uint64, work string) *RecoveryPermit {
	return &RecoveryPermit{Round: round, Work: work, lease: lease}
}
func (p *RecoveryPermit) Consume() error {
	if p == nil || p.lease == nil {
		return ErrRecoveryWork
	}
	if err := p.lease.Check(); err != nil {
		return err
	}
	if !p.used.CompareAndSwap(false, true) {
		return ErrRecoveryPermitUsed
	}
	return p.lease.Check()
}
func (p *RecoveryPermit) Finish(ctx context.Context) error {
	if p == nil || !p.used.Load() {
		return ErrRecoveryWork
	}
	return p.lease.Owner.FinishRecovery(ctx, p.lease.Token, p.Round, p.Work)
}

type recoveryContextKey struct{}

func WithRecoveryLease(ctx context.Context, l *RecoveryLease) context.Context {
	return context.WithValue(ctx, recoveryContextKey{}, l)
}
func RecoveryLeaseFromContext(ctx context.Context) *RecoveryLease {
	if ctx == nil {
		return nil
	}
	lease, _ := ctx.Value(recoveryContextKey{}).(*RecoveryLease)
	return lease
}
