package http

import (
	"context"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

type sharedWaveTestOwner struct{}

func (sharedWaveTestOwner) CheckRecovery(transfer.RecoveryToken) error { return nil }
func (sharedWaveTestOwner) RetryRecovery(context.Context, transfer.RecoveryToken, uint64, string, []string) (*transfer.RecoveryPermit, error) {
	return nil, transfer.ErrRecoveryWork
}
func (sharedWaveTestOwner) FinishRecovery(context.Context, transfer.RecoveryToken, uint64, string) error {
	return nil
}

func sharedWaveFixture(t *testing.T) (context.Context, context.CancelFunc, *recoveryWaveBarrier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	lease := &transfer.RecoveryLease{Owner: sharedWaveTestOwner{}, SourceHash: "frozen-source",
		Token: transfer.RecoveryToken{TaskID: "task", Episode: "episode", Generation: 2, SourceRevision: 3}}
	barrier, err := newRecoveryWaveBarrier(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { barrier.close(transfer.ErrRecoveryStale) })
	return ctx, cancel, barrier
}

func TestSharedRecoveryWaveDrainCountsIOWithoutJoiningParkedWorker(t *testing.T) {
	ctx, cancel, barrier := sharedWaveFixture(t)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	worker, err := barrier.enter(ctx, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Leave()
	prefetch, err := barrier.enter(ctx, "prefetch")
	if err != nil {
		t.Fatal(err)
	}
	defer prefetch.Leave()
	type result struct {
		seal *RecoveryWaveSeal
		err  error
	}
	drained := make(chan result, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		seal, err := barrier.drain(ctx, []string{"worker/0"})
		drained <- result{seal, err}
	}()
	select {
	case <-worker.Context.Done():
	case <-ctx.Done():
		t.Fatal("wave IO was not cancelled")
	}
	if ctx.Err() != nil {
		t.Fatal("short wave cancelled its parent run")
	}
	worker.Leave()
	worker.Leave() // A repeated cleanup cannot underflow the drain.
	select {
	case <-drained:
		t.Fatal("seal issued while prefetch/write scope was still active")
	default:
	}

	parked := make(chan *recoveryWaveWork, 1)
	parkErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		work, err := barrier.enter(ctx, "worker/0")
		if err != nil {
			parkErr <- err
			return
		}
		parked <- work
	}()
	prefetch.Leave()
	var sealed result
	select {
	case sealed = <-drained:
	case <-ctx.Done():
		t.Fatal("parked worker was included in the drain")
	}
	if sealed.err != nil || sealed.seal.Check(ctx) != nil {
		t.Fatalf("sealed wave: %v", sealed.err)
	}
	members := sealed.seal.Members()
	members[0] = "mutated"
	if sealed.seal.Members()[0] != "worker/0" {
		t.Fatal("seal exposed its frozen member storage")
	}
	if sealed.seal.Token() != barrier.token || sealed.seal.SourceHash() != barrier.sourceHash {
		t.Fatal("seal changed run/source identity")
	}
	snapshot, err := sealed.seal.Snapshot(ctx)
	if err != nil || snapshot.Token != barrier.token || snapshot.SourceHash != barrier.sourceHash || snapshot.Epoch != 1 {
		t.Fatalf("owned pre-transaction snapshot: %v", err)
	}
	snapshot.Members[0] = "changed-owned-copy"
	if sealed.seal.Members()[0] != "worker/0" {
		t.Fatal("owned transaction projection mutated the live barrier")
	}
	select {
	case work := <-parked:
		work.Leave()
		t.Fatal("parked work escaped before admission ACK")
	case err := <-parkErr:
		t.Fatal(err)
	default:
	}
	// Only the coordinator's confirmed-admission path is allowed to do this.
	// This unit check models that call; it is not an actual owner/DB ACK test.
	if err := sealed.seal.reopen(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(sealed.seal.Check(ctx), transfer.ErrRecoveryStale) ||
		!errors.Is(sealed.seal.reopen(ctx), transfer.ErrRecoveryStale) {
		t.Fatal("old seal remained usable after the next wave opened")
	}
	select {
	case work := <-parked:
		if work.Context.Err() != nil {
			t.Error("new wave inherited its predecessor's cancellation")
		}
		work.Leave()
	case err := <-parkErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("confirmed next wave did not admit parked work")
	}
}

func TestSharedRecoveryWaveCancellationCannotReopen(t *testing.T) {
	ctx, cancel, barrier := sharedWaveFixture(t)
	seal, err := barrier.drain(ctx, []string{"worker/0"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if seal.Check(ctx) == nil || seal.reopen(ctx) == nil {
		t.Fatal("cancelled run accepted an old wave seal")
	}
	if work, err := barrier.enter(ctx, "worker/0"); err == nil || work != nil {
		t.Fatal("cancelled run admitted IO")
	}
}

func TestSharedRecoveryWaveFailureReleasesWaiters(t *testing.T) {
	ctx, cancel, barrier := sharedWaveFixture(t)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	seal, err := barrier.drain(ctx, []string{"worker/0"})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		work, err := barrier.enter(ctx, "worker/0")
		if work != nil {
			work.Leave()
		}
		finished <- err
	}()
	barrier.close(transfer.ErrRecoveryUnavailable)
	barrier.close(transfer.ErrRecoveryStale)
	select {
	case err := <-finished:
		if !errors.Is(err, transfer.ErrRecoveryUnavailable) {
			t.Fatalf("waiter lost the original admission failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("failed admission left a parked worker blocked")
	}
	if seal.Check(ctx) == nil || seal.reopen(ctx) == nil {
		t.Fatal("failed wave reopened")
	}
}

// This is a barrier/real HTTP component intersection, not a v4 owner/kernel
// activation test: the ACK/open call below is deliberately test-controlled.
func TestSharedRecoveryWaveRealHTTPBodyCancellationLeavesRunAlive(t *testing.T) {
	ctx, cancel, barrier := sharedWaveFixture(t)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	var hits atomic.Int32
	bodyReady := make(chan struct{})
	bodyStopped := make(chan struct{})
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "prefix")
			w.(stdhttp.Flusher).Flush()
			close(bodyReady)
			<-r.Context().Done()
			close(bodyStopped)
			return
		}
		_, _ = io.WriteString(w, "whole-response")
	}))
	defer server.Close()
	transport := &stdhttp.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &stdhttp.Client{Transport: transport}
	work, err := barrier.enter(ctx, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer work.Leave()
		req, err := stdhttp.NewRequestWithContext(work.Context, stdhttp.MethodGet, server.URL, nil)
		if err != nil {
			first <- err
			return
		}
		response, err := client.Do(req)
		if err == nil {
			_, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}
		first <- err
	}()
	select {
	case <-bodyReady:
	case <-ctx.Done():
		t.Fatal("real body was not entered")
	}
	seal, err := barrier.drain(ctx, []string{"worker/0"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("old partial response unexpectedly succeeded")
		}
	case <-ctx.Done():
		t.Fatal("old IO scope was not joined")
	}
	select {
	case <-bodyStopped:
	case <-ctx.Done():
		t.Fatal("old HTTP request survived wave cancellation")
	}
	if ctx.Err() != nil || hits.Load() != 1 {
		t.Fatal("short wave lost parent run or sent before admission")
	}
	if err := seal.reopen(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := barrier.enter(ctx, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Leave()
	// A repeated/late old Leave cannot remove this epoch's active worker.
	work.Leave()
	barrier.mu.Lock()
	active := len(barrier.active)
	barrier.mu.Unlock()
	if active != 1 {
		t.Fatal("old wave cleanup removed the new epoch's worker")
	}
	req, err := stdhttp.NewRequestWithContext(next.Context, stdhttp.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || strings.TrimSpace(string(body)) != "whole-response" || hits.Load() != 2 || ctx.Err() != nil {
		t.Fatalf("next wave: body=%q hits=%d err=%v", body, hits.Load(), readErr)
	}
}

func TestSharedRecoveryWaveDrainCancellationClosesWaiters(t *testing.T) {
	parent, cancelParent, barrier := sharedWaveFixture(t)
	work, err := barrier.enter(parent, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	drainCtx, cancelDrain := context.WithCancel(parent)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancelDrain(); cancelParent(); work.Leave(); wg.Wait() })
	drainResult := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		seal, err := barrier.drain(drainCtx, []string{"worker/0"})
		if seal != nil {
			drainResult <- errors.New("cancelled drain issued a seal")
			return
		}
		drainResult <- err
	}()
	select {
	case <-work.Context.Done(): // drain has closed the gate and cancelled IO
	case <-parent.Done():
		t.Fatal("drain did not close its wave")
	}
	waiter := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		other, err := barrier.enter(parent, "worker/1")
		if other != nil {
			other.Leave()
		}
		waiter <- err
	}()
	cancelDrain()
	select {
	case err := <-drainResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("drain cancellation: %v", err)
		}
	case <-parent.Done():
		t.Fatal("cancelled drain did not return")
	}
	barrier.mu.Lock()
	active, failed, sealed := len(barrier.active), barrier.failure, barrier.sealed
	barrier.mu.Unlock()
	if active != 1 || sealed {
		t.Fatalf("cancellation forged quiescence: active=%d sealed=%v", active, sealed)
	}
	if !errors.Is(failed, context.Canceled) {
		t.Errorf("cancelled drain left no terminal failure: %v", failed)
	}
	work.Leave()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("parked work did not observe the original drain failure: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("parked worker remained blocked after drain cancellation and IO Leave")
	}
	if parent.Err() != nil {
		t.Fatal("drain failure cancelled the parent run")
	}
	if seal, err := barrier.drain(parent, []string{"worker/1"}); seal != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("failed barrier allowed a new drain or lost its error: seal=%v err=%v", seal != nil, err)
	}
}

func TestSharedRecoveryWaveCloseInterruptsDrain(t *testing.T) {
	parent, cancel, barrier := sharedWaveFixture(t)
	work, err := barrier.enter(parent, "worker/0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); work.Leave(); wg.Wait() })
	drainResult := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		seal, err := barrier.drain(parent, []string{"worker/0"})
		if seal != nil {
			drainResult <- errors.New("closed drain issued a seal")
			return
		}
		drainResult <- err
	}()
	select {
	case <-work.Context.Done():
	case <-parent.Done():
		t.Fatal("drain did not cancel its IO")
	}
	barrier.close(transfer.ErrRecoveryUnavailable)
	select {
	case err := <-drainResult:
		if !errors.Is(err, transfer.ErrRecoveryUnavailable) {
			t.Fatalf("drain did not return the explicit close failure: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("close did not wake a drain waiting for a live IO scope")
	}
	barrier.mu.Lock()
	active, sealed := len(barrier.active), barrier.sealed
	barrier.mu.Unlock()
	if active != 1 || sealed {
		t.Fatalf("close forged quiescence: active=%d sealed=%v", active, sealed)
	}
	if parent.Err() != nil {
		t.Fatal("wave close cancelled its parent")
	}
	work.Leave()
}
