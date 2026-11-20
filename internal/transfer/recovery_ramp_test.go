package transfer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func awaitRampPermit(t *testing.T, done <-chan *HTTPRecoveryPermit) *HTTPRecoveryPermit {
	t.Helper()
	select {
	case permit := <-done:
		if permit == nil {
			t.Fatal("recovering request lost its permit")
		}
		return permit
	case <-time.After(time.Second):
		t.Fatal("eligible request remained blocked")
		return nil
	}
}

func assertRampBlocked(t *testing.T, done <-chan *HTTPRecoveryPermit) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("a request started before its pacing window")
	default:
	}
}

func TestRecoveryRampKeepsSourceCooldownAndStagesContenders(t *testing.T) {
	b, clock := testBudget()
	defer b.Close()
	const key SourceKey = "origin|http://limited.test|direct"
	m := b.Join(key, 8, false)
	defer m.Leave()
	m.RateLimited(429, time.Second, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	permits := make(chan *HTTPRecoveryPermit, 3)
	for range 3 {
		go func() {
			if p, err := b.AdmitHTTPRequest(ctx, key); err == nil {
				permits <- p
			}
		}()
	}
	assertRampBlocked(t, permits)
	if other, err := b.AdmitHTTPRequest(ctx, "origin|http://peer.test|direct"); other != nil || err != nil {
		t.Fatalf("unrelated source should have no pacing: %+v, %v", other, err)
	}

	clock.Advance(time.Second)
	probe := awaitRampPermit(t, permits)
	if !probe.probe {
		t.Fatal("first network admission must be the probe")
	}
	// Success in the old stream gate reopens stream permits only.
	m.Succeeded()
	assertRampBlocked(t, permits)
	probe.Finish(true)
	assertRampBlocked(t, permits)
	clock.Advance(time.Second)
	first := awaitRampPermit(t, permits)
	if first.probe {
		t.Fatal("confirmed probe should allow paced requests")
	}
	assertRampBlocked(t, permits)
	first.Finish(false)
	b.ObserveHTTPRefusal(key, 503, 2*time.Second, false)
	m.RateLimited(503, 2*time.Second, true)
	clock.Advance(time.Second)
	assertRampBlocked(t, permits)
	clock.Advance(time.Second)
	if next := awaitRampPermit(t, permits); !next.probe {
		t.Fatal("a fresh refusal restarts probing")
	}
}

func TestRecoveryRampCancelledWaitAndStaleProbe(t *testing.T) {
	b, clock := testBudget()
	defer b.Close()
	const key SourceKey = "origin|http://limited.test|direct"
	m := b.Join(key, 2, false)
	defer m.Leave()
	m.RateLimited(429, time.Second, true)
	clock.Advance(time.Second)
	old, err := b.AdmitHTTPRequest(context.Background(), key)
	if err != nil || old == nil || !old.probe {
		t.Fatalf("expected first probe: %+v %v", old, err)
	}
	// A timed-out or cancelled probe cannot permanently block this source.
	clock.Advance(recoveryProbeTimeout)
	probe, err := b.AdmitHTTPRequest(context.Background(), key)
	if err != nil || probe == nil || probe == old || !probe.probe {
		t.Fatalf("expected next single probe: %+v %v", probe, err)
	}
	old.Finish(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.AdmitHTTPRequest(ctx, key)
		done <- err
	}()
	clock.Advance(time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled admission remained blocked")
	}
	probe.Finish(true)
}

func TestRecoveryRampReturnsToNormalAfterSustainedSuccess(t *testing.T) {
	b, clock := testBudget()
	defer b.Close()
	const key SourceKey = "origin|http://limited.test|direct"
	m := b.Join(key, 2, false)
	defer m.Leave()
	m.RateLimited(429, time.Second, true)
	clock.Advance(time.Second)
	for i := 0; i < 80; i++ {
		p, err := b.AdmitHTTPRequest(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if p == nil {
			if i < rampSuccessesPerStep {
				t.Fatalf("source reopened early after %d requests", i)
			}
			return
		}
		p.Finish(true)
		clock.Advance(time.Second)
	}
	t.Fatal("sustained valid responses should restore normal admission")
}

func TestRecoveryRampKeepsLateRetryAfterAndCanDefer(t *testing.T) {
	b, clock := testBudget()
	defer b.Close()
	const key SourceKey = "origin|http://limited.test|direct"
	m := b.Join(key, 2, false)
	defer m.Leave()
	m.RateLimited(429, time.Second, true)
	clock.Advance(time.Second)
	old, err := b.AdmitHTTPRequest(context.Background(), key)
	if err != nil || old == nil {
		t.Fatalf("expected original probe: %+v %v", old, err)
	}

	m.RateLimited(429, time.Second, true)
	clock.Advance(time.Second)
	b.ObserveHTTPRefusal(key, 503, 2*time.Second, true)
	old.Finish(true)
	clock.Advance(time.Millisecond)
	want := clock.Now().Add(1999 * time.Millisecond)
	if got := m.Snapshot().CooldownUntil; !got.Equal(want) {
		t.Fatalf("late refusal cutoff = %s, want %s", got, want)
	}
	ctx := WithDeferral(context.Background(), func() bool { return true }, nil)
	if _, err := b.AdmitHTTPRequest(ctx, key); err == nil {
		t.Fatal("near-two-second cooldown should release the slot")
	} else if deferred, ok := AsDeferred(err); !ok || !deferred.Until.Equal(want) || deferred.Status != 503 {
		t.Fatalf("deferral changed the Retry-After deadline: %v", err)
	}
	clock.Advance(1999 * time.Millisecond)
	next, err := b.AdmitHTTPRequest(context.Background(), key)
	if err != nil || next == nil || !next.probe {
		t.Fatalf("old success must not release the new probe: %+v, %v", next, err)
	}
	next.Finish(true)
}

func TestRecoveryRampSurvivesResolveMemberLeaving(t *testing.T) {
	b, clock := testBudget()
	defer b.Close()
	const key SourceKey = "origin|http://limited.test|direct"
	resolve := b.Join(key, 2, false)
	resolve.RateLimited(429, time.Second, true)
	clock.Advance(time.Second)
	if !resolve.TryAcquire() {
		t.Fatal("Resolve did not get the stream probe")
	}
	probe, err := b.AdmitHTTPRequest(context.Background(), key)
	if err != nil || probe == nil || !probe.probe {
		t.Fatalf("Resolve did not get the network probe: %+v %v", probe, err)
	}
	probe.Finish(true)
	resolve.Succeeded()
	resolve.Release()
	resolve.Leave()

	run := b.Join(key, 4, false)
	defer run.Leave()
	clock.Advance(time.Second)
	next, err := b.AdmitHTTPRequest(context.Background(), key)
	if err != nil || next == nil || next.probe {
		t.Fatalf("download should continue paced recovery after Resolve: %+v %v", next, err)
	}
	next.Finish(true)
	run.Leave()
	clock.Advance(strikeMemory)
	if b.sources[key] != nil || pendingTimers(clock) != 0 {
		t.Fatal("unused pacing state must expire")
	}
}
