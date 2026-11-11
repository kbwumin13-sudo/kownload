package download

import (
	"context"
	"errors"
	"sync"
	"unicode/utf8"
)

var (
	ErrVerifiedHTTPEventsClosed   = errors.New("completion_events_unavailable")
	ErrVerifiedHTTPEventsCapacity = errors.New("completion_events_capacity")
)

const (
	verifiedHTTPDoneSubscriberLimit = 4
	verifiedHTTPDoneBufferSize      = 16
)

// VerifiedHTTPDoneEvent is a live value, not a stored receipt or fresh permit.
// Only the lifecycle owner may publish it after fresh C ACK and API publication.
type VerifiedHTTPDoneEvent struct {
	Version      int    `json:"version"`
	DatasetID    string `json:"datasetId"`
	TaskID       string `json:"taskId"`
	CompletionID string `json:"completionId"`
}

// ValidForDataset checks wire shape only; it cannot prove a commit or authorize
// a success side effect. No Task/record is consulted or mutated here.
func (e VerifiedHTTPDoneEvent) ValidForDataset(dataset string) bool {
	return e.Version == 1 && e.DatasetID == dataset && datasetIDPattern.MatchString(dataset) &&
		e.TaskID != "" && len(e.TaskID) <= 128 && utf8.ValidString(e.TaskID) &&
		e.CompletionID != "" && len(e.CompletionID) <= 128 && utf8.ValidString(e.CompletionID)
}

type verifiedHTTPDoneSubscription struct {
	dataset string
	ctx     context.Context
	events  chan VerifiedHTTPDoneEvent
	done    chan struct{}
}

type verifiedHTTPDoneBroker struct {
	mu          sync.Mutex
	closed      bool
	subscribers map[*verifiedHTTPDoneSubscription]struct{}
}

func newVerifiedHTTPDoneBroker() *verifiedHTTPDoneBroker {
	return &verifiedHTTPDoneBroker{subscribers: make(map[*verifiedHTTPDoneSubscription]struct{})}
}

func (b *verifiedHTTPDoneBroker) subscribe(ctx context.Context, dataset string) (<-chan VerifiedHTTPDoneEvent, func(), error) {
	if b == nil {
		return nil, nil, ErrVerifiedHTTPEventsClosed
	}
	if !datasetIDPattern.MatchString(dataset) {
		return nil, nil, ErrDatasetInvalid
	}
	if ctx == nil {
		return nil, nil, context.Canceled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if b.closed {
		return nil, nil, ErrVerifiedHTTPEventsClosed
	}
	if len(b.subscribers) >= verifiedHTTPDoneSubscriberLimit {
		return nil, nil, ErrVerifiedHTTPEventsCapacity
	}
	if b.subscribers == nil {
		b.subscribers = make(map[*verifiedHTTPDoneSubscription]struct{})
	}
	sub := &verifiedHTTPDoneSubscription{dataset, ctx, make(chan VerifiedHTTPDoneEvent, verifiedHTTPDoneBufferSize), make(chan struct{})}
	b.subscribers[sub] = struct{}{}
	release := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.removeLocked(sub)
	}
	go func() {
		select {
		case <-ctx.Done():
			release()
		case <-sub.done:
		}
	}()
	return sub.events, release, nil
}

// Caller holds only the broker lock. Discard queued values on eviction so the
// closed connection cannot turn a backlog into post-cancellation notifications.
func (b *verifiedHTTPDoneBroker) removeLocked(sub *verifiedHTTPDoneSubscription) {
	if _, exists := b.subscribers[sub]; !exists {
		return
	}
	delete(b.subscribers, sub)
	for {
		select {
		case <-sub.events:
		default:
			close(sub.events)
			close(sub.done)
			return
		}
	}
}

func (b *verifiedHTTPDoneBroker) publish(event VerifiedHTTPDoneEvent) {
	if b == nil || !event.ValidForDataset(event.DatasetID) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for sub := range b.subscribers {
		if sub.ctx.Err() != nil {
			b.removeLocked(sub)
			continue
		}
		if sub.dataset != event.DatasetID {
			continue
		}
		select {
		case sub.events <- event:
		default:
			b.removeLocked(sub)
		}
	}
}

func (b *verifiedHTTPDoneBroker) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for sub := range b.subscribers {
		b.removeLocked(sub)
	}
}
