package transfer

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingServer struct {
	*httptest.Server
	opened atomic.Int64
	closed atomic.Int64
}

func newCountingServer(t *testing.T, tlsMode string, handler http.HandlerFunc) *countingServer {
	t.Helper()
	cs := &countingServer{}
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			cs.opened.Add(1)
		case http.StateClosed, http.StateHijacked:
			cs.closed.Add(1)
		}
	}
	switch tlsMode {
	case "h2":
		srv.EnableHTTP2 = true
		srv.StartTLS()
	case "h1tls":
		srv.StartTLS()
	default:
		srv.Start()
	}
	cs.Server = srv
	t.Cleanup(srv.Close)
	return cs
}

func payload(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "4096")
	w.Write([]byte(strings.Repeat("k", 4096)))
}

func streamClient(pool *TransportPool, proxyID string, http2 bool) *http.Client {
	return &http.Client{Transport: pool.Streams(TransportOptions{
		ProxyID: proxyID, InsecureSkipVerify: true, DialTimeout: 5 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, HTTP2: http2,
	})}
}

func get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func drain(t *testing.T, resp *http.Response) {
	t.Helper()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestPoolReusesOneConnectionForSequentialSmallFiles(t *testing.T) {
	srv := newCountingServer(t, "plain", payload)
	pool := NewTransportPool()
	defer pool.Close()
	client := streamClient(pool, "direct", false)
	for i := 0; i < 100; i++ {
		drain(t, get(t, client, srv.URL+"/small"))
	}
	if got := srv.opened.Load(); got != 1 {
		t.Fatalf("100 sequential same-origin files opened %d connections, want 1", got)
	}
	stats := pool.Stats()
	if stats.Leases != 100 || stats.WarmLeases != 99 || stats.StreamTransports != 1 || stats.Leased != 0 {
		t.Fatalf("pool stats %+v", stats)
	}
}

func TestPoolNegotiatesHTTP2AndKeepsParallelStreamsOnSeparateConnections(t *testing.T) {
	release := make(chan struct{})
	var hold atomic.Bool
	srv := newCountingServer(t, "h2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.Write([]byte("kownload"[:4]))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hold.Load() {
			<-release
		}
		w.Write([]byte("kownload"[4:]))
	})
	pool := NewTransportPool()
	defer pool.Close()
	client := streamClient(pool, "direct", true)
	for i := 0; i < 20; i++ {
		resp := get(t, client, srv.URL)
		if resp.ProtoMajor != 2 {
			t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
		}
		drain(t, resp)
	}
	if got := srv.opened.Load(); got != 1 {
		t.Fatalf("sequential HTTP/2 requests opened %d connections, want 1", got)
	}
	// Four streams in flight at once must use four connections, as download
	// segments do today; a shared HTTP/2 connection would merge their limits.
	hold.Store(true)
	var responses []*http.Response
	for i := 0; i < 4; i++ {
		responses = append(responses, get(t, client, srv.URL))
	}
	if got := srv.opened.Load(); got != 4 {
		t.Fatalf("four parallel streams used %d connections, want 4", got)
	}
	// Cancelling one stream leaves the others intact.
	responses[0].Body.Close()
	close(release)
	for _, resp := range responses[1:] {
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != "kownload" {
			t.Fatalf("parallel stream damaged: %q %v", body, err)
		}
	}
	if pool.Stats().Leased != 0 {
		t.Fatal("all leases returned")
	}
}

func TestPoolFallsBackToHTTP1WhenServerLacksHTTP2(t *testing.T) {
	srv := newCountingServer(t, "h1tls", payload)
	pool := NewTransportPool()
	defer pool.Close()
	client := streamClient(pool, "direct", true)
	for i := 0; i < 5; i++ {
		resp := get(t, client, srv.URL)
		if resp.ProtoMajor != 1 {
			t.Fatalf("HTTP/1.1-only server negotiated %s", resp.Proto)
		}
		drain(t, resp)
	}
	if got := srv.opened.Load(); got != 1 {
		t.Fatalf("HTTP/1.1 TLS reuse opened %d connections", got)
	}
}

func TestPoolSeparatesIncompatibleProxyPolicies(t *testing.T) {
	srv := newCountingServer(t, "plain", payload)
	pool := NewTransportPool()
	defer pool.Close()
	first := streamClient(pool, "system:aaa", false)
	second := streamClient(pool, "system:bbb", false)
	for i := 0; i < 3; i++ {
		drain(t, get(t, first, srv.URL))
		drain(t, get(t, second, srv.URL))
	}
	if got := srv.opened.Load(); got != 2 {
		t.Fatalf("two proxy generations share connections: opened %d, want 2", got)
	}
}

func TestPoolDropsConnectionsClosedBeforeEOF(t *testing.T) {
	srv := newCountingServer(t, "plain", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.Write(make([]byte, 1<<20))
	})
	pool := NewTransportPool()
	defer pool.Close()
	client := streamClient(pool, "direct", false)
	resp := get(t, client, srv.URL)
	buf := make([]byte, 10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	drain(t, get(t, client, srv.URL))
	if got := srv.opened.Load(); got != 2 {
		t.Fatalf("an abandoned HTTP/1.1 body must not be reused, opened %d", got)
	}
}

func TestPoolCloseRejectsNewLeasesAndClosesIdle(t *testing.T) {
	srv := newCountingServer(t, "plain", payload)
	pool := NewTransportPool()
	client := streamClient(pool, "direct", false)
	drain(t, get(t, client, srv.URL))
	inflight := get(t, client, srv.URL+"/again")
	pool.Close()
	if _, err := client.Get(srv.URL); err == nil || !strings.Contains(err.Error(), ErrPoolClosed.Error()) {
		t.Fatalf("lease after Close = %v", err)
	}
	// The response already granted completes normally.
	drain(t, inflight)
	deadline := time.Now().Add(2 * time.Second)
	for srv.closed.Load() < srv.opened.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.closed.Load() != srv.opened.Load() {
		t.Fatalf("idle connections left open after Close: opened %d closed %d", srv.opened.Load(), srv.closed.Load())
	}
}

func TestPoolStaysBoundedAcrossRepeatedBatches(t *testing.T) {
	srv := newCountingServer(t, "plain", payload)
	pool := NewTransportPool()
	defer pool.Close()
	client := streamClient(pool, "direct", false)
	for batch := 0; batch < 20; batch++ {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get(srv.URL)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		wg.Wait()
	}
	stats := pool.Stats()
	if stats.StreamTransports > 10 || stats.Free > 10 || stats.Leased != 0 {
		t.Fatalf("pool grew with batches: %+v", stats)
	}
	if opened := srv.opened.Load(); opened > 10 {
		t.Fatalf("20 batches of 10 opened %d connections, want at most 10", opened)
	}
}

func TestSharedTransportIsReusedPerKeyAndExpires(t *testing.T) {
	pool := NewTransportPool()
	defer pool.Close()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	pool.now = func() time.Time { return now }
	opts := TransportOptions{ProxyID: "direct", DialTimeout: time.Second, TLSHandshakeTimeout: time.Second}
	a, err := pool.Shared(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pool.Shared(opts)
	if a != b {
		t.Fatal("same key must share one transport")
	}
	now = now.Add(sharedTransportTTL + time.Minute)
	other := opts
	other.ProxyID = "custom:x"
	if _, err := pool.Shared(other); err != nil {
		t.Fatal(err)
	}
	if stats := pool.Stats(); stats.Shared != 1 {
		t.Fatalf("stale shared transport kept: %+v", stats)
	}
	pool.Close()
	if _, err := pool.Shared(opts); err != ErrPoolClosed {
		t.Fatalf("Shared after Close = %v", err)
	}
}

func TestLeaseHonoursRequestCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := newCountingServer(t, "plain", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	defer close(block)
	pool := NewTransportPool()
	defer pool.Close()
	client := streamClient(pool, "direct", false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		_, err := client.Do(req)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not end the request within one second")
	}
	if pool.Stats().Leased != 0 {
		t.Fatal("cancelled request kept its lease")
	}
}

func TestRequestDialTimeoutOverridesTransportDefault(t *testing.T) {
	transport := newTransport(TransportOptions{DialTimeout: 15 * time.Second})
	ctx := WithDialTimeout(context.Background(), 100*time.Millisecond)
	start := time.Now()
	// A non-routable address: the connect either hangs until the timeout or
	// fails at once, but never waits for the transport's 15 second default.
	conn, err := transport.DialContext(ctx, "tcp", "10.255.255.1:81")
	if err == nil {
		conn.Close()
		t.Skip("network routed the test address")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("per-request dial timeout ignored: took %v", took)
	}
}
