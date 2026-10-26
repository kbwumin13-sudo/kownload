package http

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// TimeoutReader fails a read that makes no progress within timeout.
//
// For response bodies (readers that can be closed) one timer per reader
// watches every read; when it fires it closes the body, which unblocks the
// pending read in the caller's goroutine. No goroutine is started per read and
// nothing can write into the caller's buffer after Read returns. Closing an
// HTTP/2 body only resets that stream. Plain readers keep the original
// goroutine-per-read behaviour.
type TimeoutReader struct {
	reader  io.Reader
	closer  io.Closer
	timeout time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func NewTimeoutReader(r io.Reader, timeout time.Duration) *TimeoutReader {
	tr := &TimeoutReader{reader: r, timeout: timeout}
	if closer, ok := r.(io.Closer); ok {
		tr.closer = closer
	}
	return tr
}

func (tr *TimeoutReader) Read(p []byte) (n int, err error) {
	if tr.closer == nil {
		return tr.readInGoroutine(p)
	}
	if tr.expired.Load() {
		return 0, context.DeadlineExceeded
	}
	if tr.timer == nil {
		tr.timer = time.AfterFunc(tr.timeout, tr.expire)
	} else {
		tr.timer.Reset(tr.timeout)
	}
	n, err = tr.reader.Read(p)
	tr.timer.Stop()
	if tr.expired.Load() {
		// The watchdog closed the body: report the stall, not the close.
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (tr *TimeoutReader) expire() {
	tr.expired.Store(true)
	tr.closer.Close()
}

func (tr *TimeoutReader) readInGoroutine(p []byte) (n int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), tr.timeout)
	defer cancel()

	done := make(chan struct{})
	var readErr error
	var bytesRead int

	go func() {
		bytesRead, readErr = tr.reader.Read(p)
		close(done)
	}()

	select {
	case <-done:
		return bytesRead, readErr
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
