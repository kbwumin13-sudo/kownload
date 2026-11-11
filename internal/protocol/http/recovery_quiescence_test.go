package http

import (
	"context"
	"testing"
	"time"
)

type observedQuiescenceContext struct {
	context.Context
	waits chan struct{}
}

func (c observedQuiescenceContext) Done() <-chan struct{} {
	select {
	case c.waits <- struct{}{}:
	case <-c.Context.Done():
	}
	return c.Context.Done()
}

func TestRecoveryQuiescenceCapturesBothOldWriters(t *testing.T) {
	loop, prefetch := make(chan struct{}), make(chan struct{})
	f := &Fetcher{downloadLoopDone: loop, prefetchDoneCh: prefetch}
	join := f.CaptureRunQuiescence()
	// A replacement must neither satisfy nor prolong the captured old run.
	f.downloadLoopDone, f.prefetchDoneCh = make(chan struct{}), make(chan struct{})
	close(loop)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := make(chan struct{})
	ctx := observedQuiescenceContext{base, waits}
	finished := make(chan error, 1)
	go func() { finished <- join(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-waits:
		case <-time.After(time.Second):
			t.Fatal("did not join both captured writers")
		}
	}
	select {
	case err := <-finished:
		t.Fatal("ignored old prefetch writer", err)
	default:
	}
	close(prefetch)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("joined replacement instead of captured run")
	}
}

func TestRecoveryQuiescenceCancellationDoesNotWaitForNewRun(t *testing.T) {
	f := &Fetcher{downloadLoopDone: make(chan struct{})}
	join := f.CaptureRunQuiescence()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := join(ctx); err != context.Canceled {
		t.Fatal("ignored cancellation", err)
	}
}
