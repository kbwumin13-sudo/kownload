package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeReadAheadPreservesOriginalBody(t *testing.T) {
	payload := bytes.Repeat([]byte("read ahead must preserve every byte\n"), rangeReadAheadLimit/16)
	response := &http.Response{Body: io.NopCloser(bytes.NewReader(payload))}
	scope := &recoveryWaveWork{Context: context.Background(), leave: func() {}}
	ahead := readWhileProbing(response, "", int64(len(payload)), make(chan struct{}), scope)
	<-ahead.ready
	if got := len(ahead.prefix); got != rangeReadAheadLimit {
		t.Fatalf("staged %d bytes, want %d", got, rangeReadAheadLimit)
	}
	got, err := io.ReadAll(ahead)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("expected the original body, read %d bytes: %v", len(got), err)
	}
	if err := ahead.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStealCostAndRate(t *testing.T) {
	const mib = 1 << 20
	large, _ := profitableSteal(4*mib, mib/2, 4*mib, 128<<10, 100*time.Millisecond)
	if large <= 3*mib {
		t.Fatalf("fast helper should take most of the slow stream's tail, got %d bytes", large)
	}
	if size, _ := profitableSteal(2*mib, 4*mib, 4*mib, 128<<10, 300*time.Millisecond); size != 0 {
		t.Fatalf("request setup would consume the expected saving, got %d bytes", size)
	}
}

func TestProbeReadAheadClosesBlockedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scope := &recoveryWaveWork{Context: ctx, leave: func() {}}
	ahead := readWhileProbing(resp, "", resp.ContentLength, make(chan struct{}), scope)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = ahead.Close()
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the original body must join its blocked read")
	}
}
