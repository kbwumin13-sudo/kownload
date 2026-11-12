package netdisk

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	httpfetch "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

type frozenPath struct {
	owner   string
	ctx     context.Context
	storage *transfer.VerifiedLayoutStorage
}

// Core requires the guard before any Task preparation. This setter performs
// no filesystem IO, owner callback or child join under a Task lock.
func (f *Fetcher) RequirePathFreeze(owner string) error {
	if owner == "" {
		return transfer.ErrRecoveryHeld
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.running || f.resolving {
		return transfer.ErrRecoveryStale
	}
	if prior := f.pathFreeze.Load(); prior != nil && prior.owner != owner {
		return transfer.ErrRecoveryHeld
	}
	f.pathFreeze.Store(&frozenPath{owner: owner})
	return nil
}

// A netdisk Task represents exactly one prepared file. The containing batch
// owns the full tree decision; this projection never uses a CDN filename.
func (f *Fetcher) PathFreezeLayout() (pathpolicy.LayoutV1, []int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meta == nil || f.meta.Opts == nil {
		return pathpolicy.LayoutV1{}, nil, transfer.ErrRecoveryHeld
	}
	extra, err := parseExtra(clone(f.meta.Req))
	if err != nil {
		return pathpolicy.LayoutV1{}, nil, err
	}
	if f.meta.Res != nil && len(f.meta.Res.Files) != 1 {
		return pathpolicy.LayoutV1{}, nil, transfer.ErrRecoveryHeld
	}
	selected := len(f.meta.Opts.SelectFiles) == 0
	seen := false
	for _, index := range f.meta.Opts.SelectFiles {
		if index != 0 || seen {
			return pathpolicy.LayoutV1{}, nil, transfer.ErrRecoveryHeld
		}
		selected, seen = true, true
	}
	name := extra.File.Name
	if f.meta.Opts.Name != "" {
		name = f.meta.Opts.Name
	}
	relative := name
	if extra.File.Path != "" {
		relative = extra.File.Path + "/" + name
	}
	// Preserve every raw component, letting Layout reject traversal or empty
	// components instead of cleaning them into a different frozen target.
	if strings.ContainsAny(name, "/\\") || name == "" || extra.File.Size < 0 {
		return pathpolicy.LayoutV1{}, nil, transfer.ErrRecoveryHeld
	}
	layout, err := pathpolicy.BuildLayoutV1(f.meta.Opts.Path, []pathpolicy.Member{{Index: 0, RelativePath: relative, Selected: selected}})
	if err != nil {
		return pathpolicy.LayoutV1{}, nil, err
	}
	return layout, []int64{extra.File.Size}, nil
}

// Core retains storage until all run/child users join. Only its known F and
// output-identity ACKs can activate storage; a persisted layout alone cannot.
func (f *Fetcher) BindPathFreeze(ctx context.Context, layout pathpolicy.LayoutV1, storage *transfer.VerifiedLayoutStorage) error {
	if ctx == nil || storage == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	prior := f.pathFreeze.Load()
	if prior == nil {
		return transfer.ErrRecoveryHeld
	}
	actual, sizes, err := f.PathFreezeLayout()
	if err != nil {
		return err
	}
	want, err := pathpolicy.LayoutFingerprint(actual)
	if err != nil {
		return err
	}
	got, err := pathpolicy.LayoutFingerprint(layout)
	if err != nil || want != got {
		return transfer.ErrRecoveryHeld
	}
	if err := storage.MatchLayout(actual, sizes); err != nil {
		return err
	}
	if err := storage.Ready(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.running || f.resolving || f.pathFreeze.Load() != prior {
		return transfer.ErrRecoveryStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.pathFreeze.Store(&frozenPath{owner: prior.owner, ctx: ctx, storage: storage})
	return nil
}

func (f *Fetcher) checkPathFreeze() error {
	binding := f.pathFreeze.Load()
	if binding == nil {
		return nil
	}
	if binding.ctx == nil || binding.storage == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := binding.ctx.Err(); err != nil {
		return err
	}
	return binding.storage.Ready()
}

// A fresh child receives an independent descriptor for the already opened
// identity, including after source refresh. There is no pathname fallback.
func (f *Fetcher) bindChildPathFreeze(child *httpfetch.Fetcher) error {
	binding := f.pathFreeze.Load()
	if binding == nil {
		return nil
	}
	if err := f.checkPathFreeze(); err != nil {
		return err
	}
	if err := child.RequirePathFreeze(binding.owner); err != nil {
		return err
	}
	file, err := transfer.DuplicateVerifiedLayoutWriter(binding.storage, 0)
	if err != nil {
		return err
	}
	if err = child.SetVerifiedOutput(file); err != nil {
		_ = file.Close()
		return err
	}
	if err = child.BindPathFreezeGuard(binding.owner, func() error {
		if f.pathFreeze.Load() != binding {
			return transfer.ErrRecoveryStale
		}
		return f.checkPathFreeze()
	}); err != nil {
		child.ReleaseVerifiedOutput()
		return err
	}
	return nil
}

func (f *Fetcher) integrityFile() (*os.File, error) {
	if binding := f.pathFreeze.Load(); binding != nil {
		if err := f.checkPathFreeze(); err != nil {
			return nil, err
		}
		return transfer.DuplicateVerifiedLayoutWriter(binding.storage, 0)
	}
	return os.Open(f.meta.SingleFilepath())
}

// Core invokes this only for its fresh published+Done ACK, before invalidating
// the same parent run. It never runs for a query, unknown ACK or failed run.
func (f *Fetcher) PathFreezeSucceeded(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return transfer.ErrRecoveryStale
	}
	binding, lease := f.pathFreeze.Load(), f.recovery.Load()
	if binding == nil || lease == nil || lease.Context == nil || binding.owner != lease.Token.TaskID || ctx.Done() == nil || ctx.Done() != lease.Context.Done() {
		return transfer.ErrRecoveryStale
	}
	if supplied := transfer.RecoveryLeaseFromContext(ctx); supplied != nil && supplied.Token != lease.Token {
		return transfer.ErrRecoveryStale
	}
	if err := lease.Check(); err != nil {
		return err
	}
	ctx = transfer.WithRecoveryLease(ctx, lease)
	f.mu.Lock()
	if f.closed || f.running || f.resolving || f.completedOperation == 0 || f.completedOperation != f.operation || f.recovery.Load() != lease {
		f.mu.Unlock()
		return transfer.ErrRecoveryStale
	}
	req := clone(f.meta.Req)
	// Consume the one local success callback even when cleanup reports failure.
	// The durable receipt remains the existing service's retry surface.
	f.completedOperation = 0
	f.mu.Unlock()
	extra, err := parseExtra(req)
	if err != nil {
		return err
	}
	ctl, err := f.controllerForContext(ctx)
	if err != nil {
		return err
	}
	cleanctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	return f.service.Complete(cleanctx, ctl, req, extra)
}
