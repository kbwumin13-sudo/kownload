package transfer

import (
	"context"
	"encoding/json"
	"sync"
)

// RecoveryParentController is the same durable Task owner used by HTTP. A
// protocol must not substitute a new owner, episode, ledger or Budget when it
// replaces a provider session, child HTTP fetcher or Python process. Absence of
// this interface on an enrolled lease is held, never an unmetered fallback.
type RecoveryParentController interface {
	RecoveryController
	RecoveryActionController
	// Cursor observation is not an execution permit. It avoids guessing Seen
	// from an immutable older lease after a child charged another action.
	CurrentRecoveryRound(context.Context, RecoveryToken) (uint64, error)
	// Enter checks the existing funded workset and Until before actual IO.
	// It is a scope, not a new debit or a free retry. Retrying failed work uses
	// RetryRecovery; refresh/deferral first require their durable action ACK.
	EnterRecoveryWork(context.Context, RecoveryToken, string) (*RecoveryWorkScope, error)
	// A child keeps the parent token/owner accounting. Its own captured HTTP
	// request hash is bound explicitly, rather than pretending the CDN URL is
	// the parent's raw provider request. Binding supplies no new IO allowance.
	BindRecoveryChild(context.Context, RecoveryToken, string, *HTTPExecutionContext) (*RecoveryLease, error)
	// Caller has joined the OLD workset without holding Core/protocol locks.
	// The owner snapshots/checks seal outside its transaction; the callback
	// compares immutable values and full CURRENT CAS coordinates only. Known
	// ACK plus post-ACK D/token/epoch/context recheck may issue one next lease.
	// Unknown ACK returns nil; query/reopen cannot reconstruct that lease.
	CommitRecoverySource(context.Context, RecoveryToken, uint64, RecoverySourceSeal, json.RawMessage) (*RecoveryLease, error)
}

// RecoverySourceSeal is implemented by the protocol's actual joined lifecycle,
// not populated from request JSON. Snapshot/Check may take protocol locks and
// must never be called inside a DB transaction or while holding Core locks.
type RecoverySourceSeal interface {
	Snapshot(context.Context) (RecoverySourceSnapshot, error)
	Check(context.Context) error
}

type RecoverySourceSnapshot struct {
	Token      RecoveryToken
	SourceHash string
	Epoch      uint64
	Members    []string
	// Empty means object/account/validator continuity is unprovable. It stops
	// automatic coverage rewards while preserving all prior B/sequence/budget.
	// A nonempty digest alone is not evidence or permission to clear coverage.
	IdentityHash string
}

// RecoveryWorkScope tracks actual request/body/write lifetime. The owner-issued
// context carries the parent lease. Keep this scope through response body close
// or writer exit, and Leave before asking a coordinator to drain/commit source.
type RecoveryWorkScope struct {
	ctx     context.Context
	check   func(context.Context) error
	release func()
	once    sync.Once
}

// IssueRecoveryWorkScope is a trusted owner constructor, never a wire API.
func IssueRecoveryWorkScope(ctx context.Context, check func(context.Context) error, release func()) *RecoveryWorkScope {
	return &RecoveryWorkScope{ctx: ctx, check: check, release: release}
}

func (scope *RecoveryWorkScope) Context() context.Context {
	if scope == nil {
		return nil
	}
	return scope.ctx
}

func (scope *RecoveryWorkScope) Check() error {
	if scope == nil || scope.ctx == nil || scope.check == nil {
		return ErrRecoveryWork
	}
	if err := scope.ctx.Err(); err != nil {
		return err
	}
	return scope.check(scope.ctx)
}

func (scope *RecoveryWorkScope) Leave() {
	if scope != nil {
		scope.once.Do(func() {
			if scope.release != nil {
				scope.release()
			}
		})
	}
}
