package transfer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mcpHTTPLocalPolicy(t *testing.T, raw string, resolver MCPNetworkResolver, dialer MCPNetworkDialer) *MCPNetworkPolicy {
	t.Helper()
	u := mcpURL(raw)
	port, e := strconv.ParseUint(u.Port(), 10, 16)
	if e != nil {
		t.Fatal(e)
	}
	return mcpPolicy(t, MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, AllowedPorts: []uint16{uint16(port)}}, resolver, dialer)
}
func mcpHTTPClient(t *testing.T, p *MCPNetworkPolicy, o MCPHTTPOptions) *MCPHTTPClient {
	t.Helper()
	c, e := NewMCPHTTPClient(p, o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}
func mcpHTTPRead(t *testing.T, c *MCPHTTPClient, raw string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		t.Fatal(e)
	}
	resp, e := c.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func TestMCPHTTPDirectRedirectAndHost(t *testing.T) {
	var hits atomic.Int32
	seen := make(chan string, 8)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seen <- r.Host
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/next", http.StatusFound)
		case "/next":
			http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
		default:
			io.WriteString(w, "ok")
		}
	}))
	defer s.Close()
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHits.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	// Environment proxy settings are not consulted by this explicit direct
	// component. A GUI proxy adapter must not select it as a silent fallback.
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	var dials atomic.Int32
	host := net.JoinHostPort("approved.test", mcpURL(s.URL).Port())
	resolver := mcpResolverFunc(func(ctx context.Context, n, h string) ([]netip.Addr, error) {
		if h != "approved.test" {
			t.Errorf("unexpected hostname %s", h)
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	dialer := mcpDialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		if a != strings.TrimPrefix(s.URL, "http://") {
			t.Errorf("nonnumeric/unexpected dial %s", a)
		}
		return (&net.Dialer{}).DialContext(ctx, n, a)
	})
	p := mcpHTTPLocalPolicy(t, s.URL, resolver, dialer)
	c := mcpHTTPClient(t, p, MCPHTTPOptions{DialTimeout: time.Second})
	req, _ := http.NewRequest("GET", "http://"+host+"/start", nil)
	before := req.URL.String()
	resp, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || string(b) != "ok" {
		t.Fatal("redirect failed", e)
	}
	if req.URL.String() != before || req.Host != host || hits.Load() != 3 || proxyHits.Load() != 0 || dials.Load() != 1 {
		t.Fatal("host/pool/proxy boundary failed")
	}
	for range 3 {
		if <-seen != host {
			t.Fatal("Host rewritten to numeric IP")
		}
	}
	if c.PolicyID() != p.PolicyID() {
		t.Fatal("policy identity lost")
	}
}
func TestMCPHTTPRedirectDenials(t *testing.T) {
	var target atomic.Value
	target.Store("http://127.0.0.2/secret")
	var hits, dials atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, target.Load().(string), 302)
	}))
	defer s.Close()
	resolver := mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	})
	dialer := mcpDialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, a)
	})
	p := mcpHTTPLocalPolicy(t, s.URL, resolver, dialer)
	c := mcpHTTPClient(t, p, MCPHTTPOptions{})
	for _, tc := range []struct {
		name, target string
		want         error
	}{
		{"literal", "http://127.0.0.2:" + mcpURL(s.URL).Port() + "/secret", ErrMCPNetworkDenied},
		{"DNS", "http://blocked.test:" + mcpURL(s.URL).Port() + "/secret", ErrMCPNetworkDenied},
		{"userinfo", "http://secret:password@127.0.0.1:" + mcpURL(s.URL).Port(), ErrMCPNetworkPolicyInvalid},
		{"scheme", "file:///private/secret", ErrMCPNetworkPolicyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target.Store(tc.target)
			req, _ := http.NewRequest("GET", s.URL, nil)
			resp, e := c.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if e != tc.want || strings.Contains(e.Error(), "secret") {
				t.Fatalf("unexpected error %v", e)
			}
		})
	}
	if hits.Load() != 4 || dials.Load() != 1 {
		t.Fatalf("denied redirect reached dial: hits=%d dials=%d", hits.Load(), dials.Load())
	}
	target.Store(s.URL)
	req, _ := http.NewRequest("GET", s.URL, nil)
	resp, e := c.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if e != ErrMCPHTTPRedirectLimit || hits.Load() != 14 {
		t.Fatal("redirect limit", e, hits.Load())
	}
}

type mcpHTTPBody struct{ closed bool }

func (b *mcpHTTPBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *mcpHTTPBody) Close() error             { b.closed = true; return nil }
func TestMCPHTTPEveryRequestAndPoolIsolation(t *testing.T) {
	var hits, dials, lookups atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "ok") }))
	defer s.Close()
	var rebound atomic.Bool
	resolver := mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups.Add(1)
		if rebound.Load() {
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	dialer := mcpDialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, a)
	})
	p := mcpHTTPLocalPolicy(t, s.URL, resolver, dialer)
	c := mcpHTTPClient(t, p, MCPHTTPOptions{})
	target := "http://" + net.JoinHostPort("approved.test", mcpURL(s.URL).Port())
	mcpHTTPRead(t, c, target)
	rebound.Store(true)
	mcpHTTPRead(t, c, target)
	if lookups.Load() != 1 || dials.Load() != 1 {
		t.Fatal("warm connection unexpectedly redialed")
	}
	// Warm pool must not bypass per-request authority validation.
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Host = "other.test" },
		func(r *http.Request) { r.URL.User = url.UserPassword("secret", "secret") },
		func(r *http.Request) { r.URL.Scheme = "file" },
		func(r *http.Request) { r.URL.Host = "127.0.0.2:" + mcpURL(s.URL).Port(); r.Host = r.URL.Host },
	} {
		r, _ := http.NewRequest("GET", target, nil)
		b := &mcpHTTPBody{}
		r.Body = b
		mutate(r)
		resp, e := c.transport.RoundTrip(r)
		if e == nil || resp != nil || !b.closed {
			t.Fatal("unguarded pooled request")
		}
	}
	if hits.Load() != 2 || dials.Load() != 1 {
		t.Fatal("rejected request reached network")
	}
	c.CloseIdleConnections()
	r, _ := http.NewRequest("GET", target, nil)
	_, e := c.Do(r)
	if e != ErrMCPNetworkDenied || lookups.Load() != 2 || dials.Load() != 1 {
		t.Fatal("rebind not denied", e)
	}
	rebound.Store(false)
	other := mcpHTTPClient(t, p, MCPHTTPOptions{})
	mcpHTTPRead(t, other, target)
	mcpHTTPRead(t, c, target)
	if dials.Load() != 3 {
		t.Fatal("separate instance reused external pool")
	}
	denied := mcpHTTPClient(t, mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, nil), MCPHTTPOptions{})
	r, _ = http.NewRequest("GET", s.URL, nil)
	_, e = denied.Do(r)
	if e != ErrMCPNetworkDenied || denied.PolicyID() == c.PolicyID() || dials.Load() != 3 {
		t.Fatal("cross-policy pool")
	}
}

func mcpHTTPCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "approved.test"}, DNSNames: []string{"approved.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	keyDER, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, e := tls.X509KeyPair(ca, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if e != nil {
		t.Fatal(e)
	}
	return pair, ca
}
func TestMCPHTTPTLSHostSNIAndOwnedOptions(t *testing.T) {
	pair, ca := mcpHTTPCertificate(t)
	sni := make(chan string, 8)
	hosts := make(chan string, 8)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts <- r.Host
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		io.WriteString(w, strconv.Itoa(r.ProtoMajor))
	}))
	s.EnableHTTP2 = true
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) { sni <- h.ServerName; return nil, nil }}
	s.StartTLS()
	defer s.Close()
	resolver := mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	var dials atomic.Int32
	dialer := mcpDialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		if a != strings.TrimPrefix(s.URL, "https://") {
			t.Error("TLS did not use approved numeric dial")
		}
		return (&net.Dialer{}).DialContext(ctx, n, a)
	})
	p := mcpHTTPLocalPolicy(t, s.URL, resolver, dialer)
	authority := net.JoinHostPort("approved.test", mcpURL(s.URL).Port())
	for _, h2 := range []bool{false, true} {
		t.Run(strconv.FormatBool(h2), func(t *testing.T) {
			roots := bytes.Clone(ca)
			opts := MCPHTTPOptions{RootCAPEM: roots, HTTP2: h2, TLSHandshakeTimeout: time.Second}
			c := mcpHTTPClient(t, p, opts)
			for i := range roots {
				roots[i] = 'x'
			}
			opts.HTTP2 = !h2
			want := "1"
			if h2 {
				want = "2"
			}
			if mcpHTTPRead(t, c, "https://"+authority+"/redirect") != want {
				t.Fatal("protocol selection changed")
			}
			if <-sni != "approved.test" || <-hosts != authority || <-hosts != authority {
				t.Fatal("URL Host/SNI not retained through TLS redirect")
			}
		})
	}
	c := mcpHTTPClient(t, p, MCPHTTPOptions{RootCAPEM: ca})
	req, _ := http.NewRequest("GET", "https://"+net.JoinHostPort("wrong.test", mcpURL(s.URL).Port())+"/private", nil)
	_, e := c.Do(req)
	if e != ErrMCPHTTPTransport || strings.Contains(e.Error(), "wrong.test") {
		t.Fatal("certificate name accepted or leaked", e)
	}
	if <-sni != "wrong.test" {
		t.Fatal("SNI overridden")
	}
	if dials.Load() != 3 {
		t.Fatal("TLS bypassed policy DialContext")
	}
}

type mcpHTTPWrongPeer struct {
	net.Conn
	closed *atomic.Bool
}

func (c *mcpHTTPWrongPeer) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 443}
}
func (c *mcpHTTPWrongPeer) Close() error { c.closed.Store(true); return c.Conn.Close() }
func TestMCPHTTPWrongPeer(t *testing.T) {
	var hits atomic.Int32
	var closed atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer s.Close()
	dialer := mcpDialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		c, e := (&net.Dialer{}).DialContext(ctx, n, a)
		if e != nil {
			return nil, e
		}
		return &mcpHTTPWrongPeer{Conn: c, closed: &closed}, nil
	})
	c := mcpHTTPClient(t, mcpHTTPLocalPolicy(t, s.URL, nil, dialer), MCPHTTPOptions{})
	req, _ := http.NewRequest("GET", s.URL, nil)
	_, e := c.Do(req)
	if e != ErrMCPNetworkPeer || !closed.Load() || hits.Load() != 0 {
		t.Fatal("wrong peer used", e)
	}
}
func TestMCPHTTPCancellationAndDialTimeout(t *testing.T) {
	entered := make(chan struct{})
	ended := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(ended) }))
	defer s.Close()
	c := mcpHTTPClient(t, mcpHTTPLocalPolicy(t, s.URL, nil, nil), MCPHTTPOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	result := make(chan error, 1)
	go func() { _, e := c.Do(req); result <- e }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not arrive")
	}
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request ignored cancellation")
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("server connection not cancelled")
	}
	var dialed atomic.Int32
	resolver := mcpResolverFunc(func(ctx context.Context, n, h string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() })
	dialer := mcpDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("unexpected")
	})
	bounded := mcpHTTPClient(t, mcpPolicy(t, MCPNetworkPolicyConfig{}, resolver, dialer), MCPHTTPOptions{DialTimeout: time.Second})
	outer, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	req, _ = http.NewRequestWithContext(WithDialTimeout(outer, 20*time.Millisecond), "GET", "http://approved.test", nil)
	start := time.Now()
	_, e := bounded.Do(req)
	if e != context.DeadlineExceeded || time.Since(start) > time.Second || dialed.Load() != 0 {
		t.Fatal("dial bound lost", e)
	}
}
func TestMCPHTTPConfigurationAndZeroValues(t *testing.T) {
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, nil)
	for _, o := range []MCPHTTPOptions{{DialTimeout: -1}, {TLSHandshakeTimeout: -1}, {ResponseHeaderTimeout: -1}, {IdleConnTimeout: -1}, {RootCAPEM: []byte(" ")}, {RootCAPEM: []byte("garbage")}} {
		if _, e := NewMCPHTTPTransport(p, o); e != ErrMCPHTTPOptions {
			t.Fatal("accepted bad options", e)
		}
	}
	_, ca := mcpHTTPCertificate(t)
	for _, v := range [][]byte{append([]byte("garbage\n"), ca...), append(bytes.Clone(ca), []byte("garbage")...)} {
		if _, e := NewMCPHTTPTransport(p, MCPHTTPOptions{RootCAPEM: v}); e != ErrMCPHTTPOptions {
			t.Fatal("ignored invalid PEM data")
		}
	}
	if _, e := NewMCPHTTPTransport(nil, MCPHTTPOptions{}); e != ErrMCPNetworkPolicyInvalid {
		t.Fatal(e)
	}
	var tr *MCPHTTPTransport
	var c *MCPHTTPClient
	if tr.PolicyID() != "" || c.PolicyID() != "" {
		t.Fatal("zero ID")
	}
	tr.CloseIdleConnections()
	c.CloseIdleConnections()
	b := &mcpHTTPBody{}
	r := &http.Request{Body: b}
	if _, e := tr.RoundTrip(r); e != ErrMCPHTTPRequest || !b.closed {
		t.Fatal("zero transport")
	}
	if _, e := c.Do(nil); e != ErrMCPHTTPRequest {
		t.Fatal("zero client")
	}
}
func TestMCPHTTPConcurrentRequests(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer s.Close()
	c := mcpHTTPClient(t, mcpHTTPLocalPolicy(t, s.URL, nil, nil), MCPHTTPOptions{DialTimeout: time.Second})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if mcpHTTPRead(t, c, s.URL) != "ok" {
					t.Error("bad body")
				}
			}
		}()
	}
	wg.Wait()
}

func TestMCPHTTPPolicyOwnership(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer s.Close()
	p := mcpHTTPLocalPolicy(t, s.URL, nil, nil)
	originalID := p.PolicyID()
	c := mcpHTTPClient(t, p, MCPHTTPOptions{})
	// Replacing the caller's entire policy variable must not alter a constructed
	// transport's authorization or pool identity. Dependencies remain trusted.
	*p = *mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, nil)
	if c.PolicyID() != originalID {
		t.Fatal("caller replaced transport policy")
	}
	if mcpHTTPRead(t, c, s.URL) != "ok" {
		t.Fatal("owned policy lost")
	}
}
