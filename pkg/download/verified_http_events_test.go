package download

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func doneEventFixture() VerifiedHTTPDoneEvent {
	return VerifiedHTTPDoneEvent{1, strings.Repeat("a", 32), "example-task", "example-completion"}
}

func requireDoneEventClosed(t *testing.T, events <-chan VerifiedHTTPDoneEvent) {
	t.Helper()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("closed subscription retained a queued event")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not close")
	}
}

func TestVerifiedHTTPDoneBrokerLiveOnlyAndDomain(t *testing.T) {
	b := newVerifiedHTTPDoneBroker()
	defer b.close()
	event := doneEventFixture()
	b.publish(event) // No subscriber: there is no catch-up history.
	ch, release, err := b.subscribe(context.Background(), event.DatasetID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, otherRelease, err := b.subscribe(context.Background(), strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer otherRelease()
	select {
	case <-ch:
		t.Fatal("historical event replayed")
	default:
	}
	b.publish(event)
	if got := <-ch; !reflect.DeepEqual(got, event) {
		t.Fatalf("event changed: %+v", got)
	}
	select {
	case <-other:
		t.Fatal("cross-domain delivery")
	default:
	}
	release()
	release()
	requireDoneEventClosed(t, ch)
	b.publish(event)
	fresh, freshRelease, err := b.subscribe(context.Background(), event.DatasetID)
	if err != nil {
		t.Fatal(err)
	}
	defer freshRelease()
	select {
	case <-fresh:
		t.Fatal("reconnect replayed history")
	default:
	}
	for _, mutate := range []func(*VerifiedHTTPDoneEvent){
		func(e *VerifiedHTTPDoneEvent) { e.Version = 2 },
		func(e *VerifiedHTTPDoneEvent) { e.TaskID = "" },
		func(e *VerifiedHTTPDoneEvent) { e.CompletionID = "" },
		func(e *VerifiedHTTPDoneEvent) { e.TaskID = strings.Repeat("x", 129) },
		func(e *VerifiedHTTPDoneEvent) { e.CompletionID = string([]byte{0xff}) },
	} {
		invalid := event
		mutate(&invalid)
		b.publish(invalid)
	}
	select {
	case <-fresh:
		t.Fatal("invalid wire shape delivered")
	default:
	}
}

func TestVerifiedHTTPDoneBrokerBoundsOverflowAndCancellation(t *testing.T) {
	b := newVerifiedHTTPDoneBroker()
	defer b.close()
	event := doneEventFixture()
	var channels []<-chan VerifiedHTTPDoneEvent
	var releases []func()
	for i := 0; i < 4; i++ {
		ch, release, err := b.subscribe(context.Background(), event.DatasetID)
		if err != nil {
			t.Fatal(err)
		}
		channels = append(channels, ch)
		releases = append(releases, release)
	}
	if ch, release, err := b.subscribe(context.Background(), event.DatasetID); !errors.Is(err, ErrVerifiedHTTPEventsCapacity) || ch != nil || release != nil {
		t.Fatal("unbounded subscriptions")
	}
	for i := 0; i < 16; i++ {
		b.publish(event)
		// Keep three readers healthy, while the first connection stalls.
		for _, ch := range channels[1:] {
			<-ch
		}
	}
	b.publish(event)
	requireDoneEventClosed(t, channels[0])
	for _, ch := range channels[1:] {
		if <-ch != event {
			t.Fatal("healthy connection lost")
		}
	}
	releases[0]() // Already evicted, still idempotent.
	ctx, cancel := context.WithCancel(context.Background())
	ch, release, err := b.subscribe(ctx, event.DatasetID)
	if err != nil {
		t.Fatal("overflow did not free capacity", err)
	}
	cancel()
	requireDoneEventClosed(t, ch)
	release()
	for _, release := range releases {
		release()
	}
	b.close()
	b.close()
	if ch, _, err := b.subscribe(context.Background(), event.DatasetID); !errors.Is(err, ErrVerifiedHTTPEventsClosed) || ch != nil {
		t.Fatal("closed broker reopened")
	}
	b.publish(event)
}

func TestVerifiedHTTPDoneBrokerConcurrentReleaseClosePublish(t *testing.T) {
	b := newVerifiedHTTPDoneBroker()
	event := doneEventFixture()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch, release, err := b.subscribe(ctx, event.DatasetID)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.publish(event)
			}
			cancel()
			release()
			release()
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); b.close() }()
	wg.Wait()
	b.mu.Lock()
	count := len(b.subscribers)
	b.mu.Unlock()
	if count != 0 {
		t.Fatal("subscribers retained after close", count)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := newVerifiedHTTPDoneBroker().subscribe(ctx, event.DatasetID); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled context accepted")
	}
	if _, _, err := newVerifiedHTTPDoneBroker().subscribe(context.Background(), "bad"); !errors.Is(err, ErrDatasetInvalid) {
		t.Fatal("invalid domain accepted")
	}
}
