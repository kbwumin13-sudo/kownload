package netdisk

import (
	"context"
	"encoding/json"
	"time"

	httpfetch "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func (f *Fetcher) checkOperationLocked(ctx context.Context, operation uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.closed || f.operation != operation {
		return transfer.ErrRecoveryStale
	}
	return nil
}

func (f *Fetcher) checkOperation(ctx context.Context, operation uint64) error {
	f.mu.Lock()
	err := f.checkOperationLocked(ctx, operation)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if lease := transfer.RecoveryLeaseFromContext(ctx); lease != nil {
		return lease.Check()
	}
	return nil
}

// Core supplies the only parent authority. Installing it performs no owner
// callback or join while Core may hold a Task lock.
func (f *Fetcher) SetRecoveryLease(lease *transfer.RecoveryLease) {
	if lease == nil {
		f.recovery.Store(nil)
		return
	}
	copy := *lease
	copy.InitialMembers = append([]string(nil), lease.InitialMembers...)
	f.recovery.Store(&copy)
}

func (f *Fetcher) SetParentRecoveryContext(ctx context.Context) error {
	if ctx == nil {
		return transfer.ErrRecoveryHeld
	}
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil || lease.Owner == nil {
		return transfer.ErrRecoveryHeld
	}
	if _, ok := lease.Owner.(transfer.RecoveryParentController); !ok {
		return transfer.ErrRecoveryHeld
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lease.Check(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.resolving || f.closed {
		return transfer.ErrRecoveryStale
	}
	copy := *lease
	copy.Context = ctx
	f.SetRecoveryLease(&copy)
	return nil
}

// Core captures this before replacement, then joins it after releasing Task
// locks. Captured channels cannot silently switch to a newer run.
func (f *Fetcher) CaptureRunQuiescence() func(context.Context) error {
	f.mu.Lock()
	run, resolve, child := f.runDone, f.resolveDone, f.child
	f.mu.Unlock()
	var joinChild func(context.Context) error
	if child != nil {
		joinChild = child.CaptureRunQuiescence()
	}
	return func(ctx context.Context) error {
		for _, done := range []<-chan struct{}{run, resolve} {
			if done != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-done:
				}
			}
		}
		if joinChild != nil {
			return joinChild(ctx)
		}
		return ctx.Err()
	}
}

func (f *Fetcher) BindRunWait(ctx context.Context) func() error {
	f.mu.Lock()
	f.done = make(chan error, 1)
	done := f.done
	f.mu.Unlock()
	return func() error {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Managed Task preparation publishes only the provider's selected file. It
// cannot perform provider IO or call back into Core under Task.lock.
func (f *Fetcher) ResolveTask(ctx context.Context, req *base.Request, opts *base.Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req = clone(req)
	extra, err := parseExtra(req)
	if err != nil {
		return err
	}
	if opts == nil {
		opts = &base.Options{}
	} else {
		opts = opts.Clone()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.running || f.resolving {
		return transfer.ErrRecoveryStale
	}
	f.meta.Req, f.meta.Opts = req, opts
	if f.meta.Res == nil {
		f.meta.Res = &base.Resource{Size: extra.File.Size, Files: []*base.FileInfo{{Name: extra.File.Name, Path: extra.File.Path, Size: extra.File.Size}}}
	}
	f.refresh = true
	f.publishViewLocked()
	return nil
}

func (f *Fetcher) PrepareContext(ctx context.Context, req *base.Request, opts *base.Options) error {
	return f.ResolveTask(ctx, req, opts)
}

func (f *Fetcher) bindHTTPChild(ctx context.Context, child *httpfetch.Fetcher, req *base.Request) (context.Context, error) {
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil {
		return ctx, nil
	}
	owner, ok := lease.Owner.(transfer.RecoveryParentController)
	if !ok || lease.Context == nil {
		return nil, transfer.ErrRecoveryHeld
	}
	snapshot, supported, err := child.CaptureRecoveryContext(req)
	if err != nil {
		return nil, err
	}
	if !supported {
		return nil, transfer.ErrRecoveryHeld
	}
	bound, err := owner.BindRecoveryChild(ctx, lease.Token, "netdisk/http", snapshot)
	if err != nil {
		return nil, err
	}
	if bound == nil || bound.Token != lease.Token || bound.SourceHash != snapshot.SourceHash() || bound.Context == nil {
		return nil, transfer.ErrRecoveryStale
	}
	if err = f.checkOperationContext(ctx); err != nil {
		return nil, err
	}
	child.SetRecoveryContext(snapshot)
	child.SetRecoveryLease(bound)
	child.BindRunWait(transfer.WithRecoveryLease(lease.Context, bound))
	return transfer.WithRecoveryLease(ctx, bound), nil
}

func (f *Fetcher) checkOperationContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil {
		return nil
	}
	current := f.recovery.Load()
	if current == nil || current.Token != lease.Token {
		return transfer.ErrRecoveryStale
	}
	return lease.Check()
}

func snapshotChild(child *httpfetch.Fetcher) (*saved, error) {
	data, err := (&httpfetch.FetcherManager{}).Store(child)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var clean map[string]json.RawMessage
	if err = json.Unmarshal(raw, &clean); err != nil {
		return nil, err
	}
	delete(clean, "RedirectURL")
	raw, err = json.Marshal(clean)
	if err != nil {
		return nil, err
	}
	return &saved{Child: raw, Resource: clone(child.Meta().Res), Downloaded: child.Progress().TotalDownloaded()}, nil
}

type netdiskRefreshPlan struct {
	lease *transfer.RecoveryLease
	owner transfer.RecoveryParentController
	action uint64
	seal *netdiskSourceSeal
	checkpoint *saved
}

// This seal exists only after the real old child and its writers have joined.
// It carries no account digest: the current provider contract cannot prove a
// stable UID/object/validator chain, so identity remains unprovable.
type netdiskSourceSeal struct {
	f *Fetcher
	operation uint64
	lease *transfer.RecoveryLease
}

func (seal *netdiskSourceSeal) Check(ctx context.Context) error {
	seal.f.mu.Lock()
	err := seal.f.checkOperationLocked(ctx, seal.operation)
	seal.f.mu.Unlock()
	if err != nil {
		return err
	}
	lease := seal.f.recovery.Load()
	if lease == nil || lease.Token != seal.lease.Token {
		return transfer.ErrRecoveryStale
	}
	return nil
}

func (seal *netdiskSourceSeal) Snapshot(ctx context.Context) (transfer.RecoverySourceSnapshot, error) {
	if err := seal.Check(ctx); err != nil {
		return transfer.RecoverySourceSnapshot{}, err
	}
	return transfer.RecoverySourceSnapshot{Token: seal.lease.Token, SourceHash: seal.lease.SourceHash,
		Epoch: seal.operation, Members: []string{"netdisk/api", "netdisk/http"}}, nil
}

func (f *Fetcher) prepareRefresh(ctx context.Context, operation uint64) (*netdiskRefreshPlan, error) {
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil {
		return nil, nil // Interactive/legacy behavior has no managed ledger.
	}
	owner, ok := lease.Owner.(transfer.RecoveryParentController)
	if !ok {
		return nil, transfer.ErrRecoveryHeld
	}
	// Read only the trusted same-run RAM cursor. A durable query, an old lease
	// Round or a locally incremented number cannot reconstruct authorization.
	cursor, ok := lease.Owner.(interface {
		CurrentRecoveryRound(context.Context, transfer.RecoveryToken) (uint64, error)
	})
	if !ok {
		return nil, transfer.ErrRecoveryHeld
	}
	f.mu.Lock()
	child := f.child
	f.mu.Unlock()
	checkpoint := &saved{}
	if child != nil {
		if err := child.Pause(); err != nil {
			return nil, err
		}
		if err := child.CaptureRunQuiescence()(ctx); err != nil {
			return nil, err
		}
		var err error
		checkpoint, err = snapshotChild(child)
		if err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	checkpoint.Resource = clone(f.meta.Res)
	f.mu.Unlock()
	if err := f.checkOperation(ctx, operation); err != nil {
		return nil, err
	}
	seen, err := cursor.CurrentRecoveryRound(ctx, lease.Token)
	if err != nil {
		return nil, err
	}
	permit, err := owner.ReserveRecoveryAction(ctx, lease.Token, seen, transfer.RecoveryActionRefresh,
		"netdisk/api", []string{"netdisk/api", "netdisk/http"}, time.Time{})
	if err != nil {
		return nil, err
	}
	if permit == nil || permit.Token != lease.Token || permit.Kind != transfer.RecoveryActionRefresh {
		return nil, transfer.ErrRecoveryWork
	}
	if err = permit.Consume(ctx); err != nil {
		return nil, err
	}
	if err = f.checkOperation(ctx, operation); err != nil {
		return nil, err
	}
	return &netdiskRefreshPlan{lease: lease, owner: owner, action: permit.ActionID, checkpoint: checkpoint,
		seal: &netdiskSourceSeal{f: f, operation: operation, lease: lease}}, nil
}

func (f *Fetcher) commitRefresh(ctx context.Context, operation uint64, plan *netdiskRefreshPlan) (context.Context, error) {
	checkpoint, err := json.Marshal(plan.checkpoint)
	if err != nil {
		return ctx, err
	}
	lease, err := plan.owner.CommitRecoverySource(ctx, plan.lease.Token, plan.action, plan.seal, checkpoint)
	if err != nil {
		return ctx, err
	}
	if lease == nil || lease.Context == nil || lease.Token.TaskID != plan.lease.Token.TaskID ||
		lease.Token.Episode != plan.lease.Token.Episode || lease.Token.Generation != plan.lease.Token.Generation {
		return ctx, transfer.ErrRecoveryStale
	}
	f.mu.Lock()
	if err = f.checkOperationLocked(ctx, operation); err == nil {
		f.SetRecoveryLease(lease)
	}
	f.mu.Unlock()
	if err != nil {
		return ctx, err
	}
	ctx = transfer.WithRecoveryLease(ctx, f.recovery.Load())
	return ctx, lease.Check()
}
