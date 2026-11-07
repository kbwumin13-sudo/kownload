package download

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type contextResolveFetcher struct {
	fetcher.Fetcher
	meta    *fetcher.FetcherMeta
	resolve func(context.Context) error
	closed  atomic.Bool
}

func (f *contextResolveFetcher) Setup(*controller.Controller) {}
func (f *contextResolveFetcher) Resolve(*base.Request, *base.Options) error {
	panic("context resolution was bypassed")
}
func (f *contextResolveFetcher) ResolveContext(ctx context.Context, req *base.Request, opts *base.Options) error {
	f.meta = &fetcher.FetcherMeta{Req: req, Opts: opts, Res: &base.Resource{Files: []*base.FileInfo{{Name: "file", Size: 1}}, Size: 1}}
	return f.resolve(ctx)
}
func (f *contextResolveFetcher) Close() error               { f.closed.Store(true); return nil }
func (f *contextResolveFetcher) Meta() *fetcher.FetcherMeta { return f.meta }

type contextResolveManager struct {
	fetcher.FetcherManager
	f fetcher.Fetcher
}

func (m *contextResolveManager) Name() string           { return "context-test" }
func (m *contextResolveManager) Build() fetcher.Fetcher { return m.f }

func TestResolveContextFailureCleanupAndCache(t *testing.T) {
	for _, outcome := range []string{"failure", "canceled-after-success", "success"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantErr := errors.New("metadata unavailable")
			f := &contextResolveFetcher{resolve: func(got context.Context) error {
				if got != ctx {
					t.Fatal("request context was not passed to fetcher")
				}
				switch outcome {
				case "failure":
					return wantErr
				case "canceled-after-success":
					cancel()
				}
				return nil
			}}
			d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{&contextResolveManager{f: f}}})
			// An explicit protocol avoids extensions; a concrete path avoids storage.
			rr, err := d.ResolveContext(ctx, &base.Request{URL: "context-test:file", Protocol: "context-test"}, &base.Options{Path: t.TempDir()})
			if outcome == "success" {
				if err != nil || rr == nil || len(d.fetcherCache) != 1 || f.closed.Load() {
					t.Fatalf("successful resolve was not retained: %v", err)
				}
				cancel()
				if f.closed.Load() {
					t.Fatal("request cancellation closed a completed resolution")
				}
			} else {
				if outcome == "failure" && !errors.Is(err, wantErr) {
					t.Fatalf("got %v, want resolution failure", err)
				}
				if outcome == "canceled-after-success" && !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want cancellation", err)
				}
				if rr != nil || len(d.fetcherCache) != 0 || !f.closed.Load() {
					t.Fatal("failed resolution leaked a fetcher or cache entry")
				}
			}
		})
	}
}

type legacyResolveFetcher struct {
	fetcher.Fetcher
	resolve func() error
	closed  atomic.Bool
}

func (f *legacyResolveFetcher) Setup(*controller.Controller)               {}
func (f *legacyResolveFetcher) Resolve(*base.Request, *base.Options) error { return f.resolve() }
func (f *legacyResolveFetcher) Close() error                               { f.closed.Store(true); return nil }

func TestResolveContextLegacyResolverDoesNotCloseExistingTransfer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &legacyResolveFetcher{resolve: func() error { cancel(); return nil }}
	d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{&contextResolveManager{f: f}}})
	rr, err := d.ResolveContext(ctx, &base.Request{URL: "context-test:file", Protocol: "context-test"}, &base.Options{Path: t.TempDir()})
	if !errors.Is(err, context.Canceled) || rr != nil || len(d.fetcherCache) != 0 {
		t.Fatalf("canceled legacy resolve was retained: %v", err)
	}
	if f.closed.Load() {
		t.Fatal("closing a metadata-only resolver can delete an existing hash transfer")
	}
}
