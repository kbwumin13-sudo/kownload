package http

import (
	"context"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type httpFrozenPath struct { owner string; check func() error }

// RequirePathFreeze is called before metadata resolution. The response may be
// retained, but target prefetch, pending autostart and pathname opens are denied
// until Core binds the acknowledged layout and actual writable descriptor.
func (f *Fetcher) RequirePathFreeze(owner string) error {
	if owner == "" { return transfer.ErrRecoveryHeld }
	if previous := f.pathFreeze.Load();previous != nil && previous.owner != owner { return transfer.ErrRecoveryHeld }
	f.pathFreeze.Store(&httpFrozenPath{owner:owner})
	f.metadataOnly.Store(true)
	f.RequireMCPOutput() // same-fd join/Sync; this does not create a checksum record.
	return nil
}

func (f *Fetcher) ResolveMetadataOnly(ctx context.Context, req *base.Request, opts *base.Options) error {
	f.metadataOnly.Store(true)
	f.RequireVerifiedOutput()
	return f.ResolveContext(ctx,req,opts)
}

// guard is Core's pure RAM same-run/F+identity-ACK guard, never a DB/Task lock.
// It grants no pathname fallback: doStart must still receive SetVerifiedOutput.
func (f *Fetcher) BindPathFreezeGuard(owner string, guard func() error) error {
	previous := f.pathFreeze.Load()
	if previous == nil || previous.owner != owner || guard == nil { return transfer.ErrRecoveryHeld }
	if err := guard();err != nil { return err }
	f.pathFreeze.Store(&httpFrozenPath{owner:owner,check:guard})
	f.metadataOnly.Store(false)
	return nil
}

func (f *Fetcher) checkFrozenPathWrite() error {
	binding := f.pathFreeze.Load()
	if binding == nil {
		if f.metadataOnly.Load() { return transfer.ErrRecoveryWork }
		return nil
	}
	if f.metadataOnly.Load() || binding.check == nil { return transfer.ErrRecoveryWork }
	return binding.check()
}
