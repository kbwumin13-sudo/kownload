package http

import (
	"context"
	"io"
	"net/http"

	"github.com/GopeedLab/gopeed/internal/benchtrace"
)

const rangeReadAheadLimit = 512 * 1024

// rangeReadAhead temporarily owns the first response during the optional
// capability probe. The next consumer sees its prefix, unread body and error
// in order, even if the probe rejects ranges.
type rangeReadAhead struct {
	body   io.ReadCloser
	ready  chan struct{}
	prefix []byte
	err    error
}

func readWhileProbing(resp *http.Response, url string, size int64, stop <-chan struct{}, scope *recoveryWaveWork) *rangeReadAhead {
	body := &rangeReadAhead{body: resp.Body, ready: make(chan struct{})}
	go func() {
		defer close(body.ready)
		defer scope.Leave()
		cancelRead := context.AfterFunc(scope.Context, func() { _ = body.body.Close() })
		defer cancelRead()
		buf := make([]byte, rangeReadAheadLimit)
		for offset := 0; offset < len(buf); {
			select {
			case <-stop:
				return
			default:
			}
			n, err := body.body.Read(buf[offset:min(offset+32*1024, len(buf))])
			if n > 0 {
				if benchtrace.Enabled {
					benchtrace.Payload(url, size, int64(offset), n)
				}
				offset += n
				body.prefix = buf[:offset]
			}
			if err != nil {
				body.err = err
				return
			}
		}
	}()
	return body
}

func (b *rangeReadAhead) Read(p []byte) (int, error) {
	<-b.ready
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		if len(b.prefix) == 0 {
			b.prefix = nil
		}
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.body.Read(p)
}

func (b *rangeReadAhead) Close() error {
	err := b.body.Close()
	<-b.ready
	return err
}
