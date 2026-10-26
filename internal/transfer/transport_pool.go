// Package transfer holds process-wide HTTP transfer resources that protocol
// fetchers share: reusable transports, per-source stream budgets and retry
// timing. It depends only on the standard library so that protocols can use it
// without importing the downloader or each other.
package transfer

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Matches net/http.DefaultTransport; idle keep-alive connections close after it.
	idleConnTimeout = 90 * time.Second
	// A stream transport carries one request at a time; a few idle connections
	// cover the hosts it served most recently.
	streamIdleConns = 4
	// Released stream transports kept across all compatibility keys. When more
	// are released, the ones idle longest are closed.
	maxFreeTransports = 64
	// A released stream transport idle this long has no live connection left,
	// so it is closed and forgotten, whatever proxy policy or TLS setting it
	// belonged to.
	freeTransportTTL = idleConnTimeout
	// Shared transports unused for this long are closed and forgotten.
	sharedTransportTTL = 10 * time.Minute
)

var ErrPoolClosed = errors.New("transport pool closed")

// TransportOptions are the connection-compatibility settings of a transport.
// Requests reuse connections only when every field except DialTimeout
// matches; a request can shorten its own TCP connect with WithDialTimeout.
type TransportOptions struct {
	// ProxyID identifies the proxy policy, including a snapshot of the system
	// proxy configuration. Proxy must implement exactly that policy.
	ProxyID             string
	Proxy               func(*http.Request) (*url.URL, error)
	InsecureSkipVerify  bool
	DialTimeout         time.Duration // used when the request sets none
	TLSHandshakeTimeout time.Duration
	HTTP2               bool
}

type transportKey struct {
	proxyID  string
	insecure bool
	tls      time.Duration
	http2    bool
}

func keyOf(opts TransportOptions) transportKey {
	return transportKey{opts.ProxyID, opts.InsecureSkipVerify, opts.TLSHandshakeTimeout, opts.HTTP2}
}

type dialTimeoutKey struct{}

// WithDialTimeout bounds the TCP connect of requests made with ctx. Dials keep
// the request's context values even though net/http detaches them from its
// cancellation, so the bound applies per request on a shared transport.
func WithDialTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, dialTimeoutKey{}, timeout)
}

// PoolStats is a point-in-time view used by diagnostics and tests.
type PoolStats struct {
	StreamTransports int64 `json:"streamTransports"` // stream transports ever created
	Leases           int64 `json:"leases"`           // stream round trips served
	WarmLeases       int64 `json:"warmLeases"`       // leases given a transport that recently served the host
	Leased           int   `json:"leased"`           // transports currently carrying a request
	Free             int   `json:"free"`             // released transports kept for reuse
	Shared           int   `json:"shared"`           // multiplexed transports for API traffic
}

// TransportPool hands out transports that keep TCP/TLS connections alive across
// requests and tasks. Stream transports are leased exclusively for one request,
// so parallel download streams never share a connection (HTTP/2 included) and
// the stream count stays equal to the physical connection count. Shared
// transports multiplex small API requests.
type TransportPool struct {
	mu     sync.Mutex
	free   map[transportKey][]*streamTransport
	shared map[transportKey]*sharedTransport
	leased int
	closed bool
	now    func() time.Time

	created atomic.Int64
	leases  atomic.Int64
	warm    atomic.Int64
}

type streamTransport struct {
	transport *http.Transport
	hosts     map[string]time.Time // host -> when an idle connection to it was last left behind
	released  time.Time
}

type sharedTransport struct {
	transport *http.Transport
	lastUsed  time.Time
}

func NewTransportPool() *TransportPool {
	return &TransportPool{
		free:   make(map[transportKey][]*streamTransport),
		shared: make(map[transportKey]*sharedTransport),
		now:    time.Now,
	}
}

// Streams returns a RoundTripper that runs every round trip on a transport
// leased exclusively until the response body is closed or fully read.
func (p *TransportPool) Streams(opts TransportOptions) http.RoundTripper {
	return &streamRoundTripper{pool: p, opts: opts, key: keyOf(opts)}
}

// Shared returns one multiplexing transport per compatibility key, for short
// API requests whose concurrency is bounded elsewhere.
func (p *TransportPool) Shared(opts TransportOptions) (http.RoundTripper, error) {
	key := keyOf(opts)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	p.sweepLocked(now, key)
	st := p.shared[key]
	if st == nil {
		t := newTransport(opts)
		t.ForceAttemptHTTP2 = true
		t.MaxIdleConns = 64
		t.MaxIdleConnsPerHost = 8
		st = &sharedTransport{transport: t}
		p.shared[key] = st
	}
	st.lastUsed = now
	return st.transport, nil
}

func (p *TransportPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	free := 0
	for _, list := range p.free {
		free += len(list)
	}
	return PoolStats{
		StreamTransports: p.created.Load(),
		Leases:           p.leases.Load(),
		WarmLeases:       p.warm.Load(),
		Leased:           p.leased,
		Free:             free,
		Shared:           len(p.shared),
	}
}

// Close drops all idle connections. Transports still leased finish their
// current request and close their idle connections when released.
func (p *TransportPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for key, list := range p.free {
		for _, st := range list {
			st.transport.CloseIdleConnections()
		}
		delete(p.free, key)
	}
	for key, st := range p.shared {
		st.transport.CloseIdleConnections()
		delete(p.shared, key)
	}
	return nil
}

// sweepLocked closes and forgets free stream transports whose connections have
// all expired, under every compatibility key, and shared transports unused for
// sharedTransportTTL except the one for keep. Leased transports are not in
// these maps and are never touched. Caller holds p.mu.
func (p *TransportPool) sweepLocked(now time.Time, keep transportKey) {
	for key, list := range p.free {
		kept := list[:0]
		for _, st := range list {
			if now.Sub(st.released) >= freeTransportTTL {
				st.transport.CloseIdleConnections()
				continue
			}
			kept = append(kept, st)
		}
		clear(list[len(kept):])
		if len(kept) == 0 {
			delete(p.free, key)
		} else {
			p.free[key] = kept
		}
	}
	for key, st := range p.shared {
		if key != keep && now.Sub(st.lastUsed) > sharedTransportTTL {
			st.transport.CloseIdleConnections()
			delete(p.shared, key)
		}
	}
}

// trimLocked keeps at most maxFreeTransports free stream transports in total by
// closing the ones idle longest. Caller holds p.mu.
func (p *TransportPool) trimLocked() {
	total := 0
	for _, list := range p.free {
		total += len(list)
	}
	for ; total > maxFreeTransports; total-- {
		var oldestKey transportKey
		oldest := -1
		for key, list := range p.free {
			// Each list is in release order, so its first entry is its oldest.
			if len(list) > 0 && (oldest < 0 || list[0].released.Before(p.free[oldestKey][0].released)) {
				oldestKey, oldest = key, 0
			}
		}
		list := p.free[oldestKey]
		list[0].transport.CloseIdleConnections()
		list[0] = nil
		if len(list) == 1 {
			delete(p.free, oldestKey)
		} else {
			p.free[oldestKey] = list[1:]
		}
	}
}

func (p *TransportPool) lease(key transportKey, opts TransportOptions, host string) (*streamTransport, error) {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	p.sweepLocked(now, key)
	list := p.free[key]
	pick, warm := -1, false
	for i := len(list) - 1; i >= 0; i-- {
		if at, ok := list[i].hosts[host]; ok && now.Sub(at) < idleConnTimeout {
			pick, warm = i, true
			break
		}
	}
	if pick < 0 {
		// A transport whose idle connections all expired is as good as new.
		for i := len(list) - 1; i >= 0; i-- {
			if !list[i].hasLiveHost(now) {
				pick = i
				break
			}
		}
	}
	var st *streamTransport
	if pick >= 0 {
		st = list[pick]
		p.free[key] = append(list[:pick], list[pick+1:]...)
	} else {
		t := newTransport(opts)
		// One request at a time: a second dial would only race the connection
		// that is about to become idle.
		t.MaxConnsPerHost = 1
		t.MaxIdleConnsPerHost = 1
		t.MaxIdleConns = streamIdleConns
		st = &streamTransport{transport: t, hosts: make(map[string]time.Time)}
		p.created.Add(1)
	}
	p.leased++
	p.leases.Add(1)
	if warm {
		p.warm.Add(1)
	}
	return st, nil
}

func (p *TransportPool) release(key transportKey, st *streamTransport, host string, reusable bool) {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.leased--
	if p.closed {
		st.transport.CloseIdleConnections()
		return
	}
	for h, at := range st.hosts {
		if now.Sub(at) >= idleConnTimeout {
			delete(st.hosts, h)
		}
	}
	if reusable {
		st.hosts[host] = now
	}
	st.released = now
	p.sweepLocked(now, key)
	p.free[key] = append(p.free[key], st)
	p.trimLocked()
}

func (st *streamTransport) hasLiveHost(now time.Time) bool {
	for _, at := range st.hosts {
		if now.Sub(at) < idleConnTimeout {
			return true
		}
	}
	return false
}

func newTransport(opts TransportOptions) *http.Transport {
	dialer := &net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: opts.Proxy,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if timeout, ok := ctx.Value(dialTimeoutKey{}).(time.Duration); ok && timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify},
		TLSHandshakeTimeout: opts.TLSHandshakeTimeout,
		// Custom dialers and TLS settings disable HTTP/2 unless forced.
		ForceAttemptHTTP2:     opts.HTTP2,
		IdleConnTimeout:       idleConnTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

type streamRoundTripper struct {
	pool *TransportPool
	opts TransportOptions
	key  transportKey
}

func (s *streamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := hostKey(req.URL)
	st, err := s.pool.lease(s.key, s.opts, host)
	if err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	resp, err := st.transport.RoundTrip(req)
	if err != nil {
		s.pool.release(s.key, st, host, false)
		return nil, err
	}
	// Closing an HTTP/2 stream early resets only that stream; the connection
	// stays usable. HTTP/1.1 connections survive only a body read to EOF.
	multiplexed := resp.ProtoMajor == 2
	resp.Body = &leasedBody{ReadCloser: resp.Body, release: func(eof bool) {
		s.pool.release(s.key, st, host, eof || multiplexed)
	}}
	return resp, nil
}

// leasedBody returns its transport to the pool once the stream ends.
type leasedBody struct {
	io.ReadCloser
	once    sync.Once
	release func(eof bool)
}

func (b *leasedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(func() { b.release(true) })
	}
	return n, err
}

func (b *leasedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.release(false) })
	return err
}

func hostKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	if u.Port() == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			host += ":443"
		case "http":
			host += ":80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + host
}
