package netdisk

import (
	"context"
	"encoding/json"

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
