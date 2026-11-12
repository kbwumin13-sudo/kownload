package http

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// AfterFunc callbacks are independently scheduled by context. Hold them so a
// request can observe wave cancellation before its containing worker does.
type heldWaveCallbacks struct {
	context.Context
	mu        sync.Mutex
	callbacks []func()
}

// Do not expose the embedded cancelCtx through its private Value key: the
// context package must use this context's AfterFunc scheduling contract.
func (*heldWaveCallbacks) Value(any) any { return nil }

func (wave *heldWaveCallbacks) AfterFunc(callback func()) func() bool {
	var started atomic.Bool
	wave.mu.Lock()
	wave.callbacks = append(wave.callbacks, func() {
		if started.CompareAndSwap(false, true) {
			callback()
		}
	})
	wave.mu.Unlock()
	return func() bool { return started.CompareAndSwap(false, true) }
}

func TestSharedRecoveryWaveRequestCancelBeforeWorkerCallback(t *testing.T) {
	ctx, _, barrier := sharedWaveFixture(t)
	wave := &heldWaveCallbacks{Context: barrier.wave}
	barrier.wave = wave
	worker, err := barrier.enter(ctx, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Leave()
	request, err := barrier.enter(worker.Context, "request/1")
	if err != nil {
		t.Fatal(err)
	}
	defer request.Leave()
	wave.mu.Lock()
	callbacks := append([]func(){}, wave.callbacks...)
	wave.mu.Unlock()
	if len(callbacks) != 2 {
		t.Fatalf("worker/request cancellation registrations: %d", len(callbacks))
	}
	defer callbacks[0]()
	defer callbacks[1]()
	barrier.cancel()
	callbacks[1]() // The request's callback wins the scheduling race.
	select {
	case <-request.Context.Done():
	case <-ctx.Done():
		t.Fatal("request cancellation was not delivered")
	}
	if worker.Context.Err() != nil || ctx.Err() != nil || wave.Err() == nil {
		t.Fatal("test did not hold only the worker's wave callback")
	}
	mapped := sharedScopeResult(worker, ctx, request.Context.Err())
	worker.Leave() // Real downloadChunkOnce also leaves after classifying.
	if !errors.Is(mapped, errSharedWaveDraining) {
		t.Fatalf("live worker would exit with its owned range unfinished: %v", mapped)
	}
	if ctx.Err() != nil {
		t.Fatal("draining a wave cancelled the parent run")
	}

	parent, cancelParent, liveBarrier := sharedWaveFixture(t)
	left, err := liveBarrier.enter(parent, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	left.Leave()
	if errors.Is(sharedScopeResult(left, parent, context.Canceled), errSharedWaveDraining) {
		t.Fatal("a scope's own Leave was classified as a wave drain")
	}
	cancelled, err := liveBarrier.enter(parent, "worker/1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelled.Leave()
	cancelParent()
	if errors.Is(sharedScopeResult(cancelled, parent, context.Canceled), errSharedWaveDraining) {
		t.Fatal("parent cancellation was classified as a recoverable wave drain")
	}
}
