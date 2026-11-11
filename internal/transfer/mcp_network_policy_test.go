package transfer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mcpResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f mcpResolverFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}

type mcpDialerFunc func(context.Context, string, string) (net.Conn, error)

func (f mcpDialerFunc) DialContext(c context.Context, n, a string) (net.Conn, error) {
	return f(c, n, a)
}

type mcpTestConn struct {
	peer   net.Addr
	closed atomic.Bool
}

func (c *mcpTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *mcpTestConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *mcpTestConn) Close() error                     { c.closed.Store(true); return nil }
func (c *mcpTestConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }
func (c *mcpTestConn) RemoteAddr() net.Addr             { return c.peer }
func (c *mcpTestConn) SetDeadline(time.Time) error      { return nil }
func (c *mcpTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mcpTestConn) SetWriteDeadline(time.Time) error { return nil }
func mcpNumericConn(t *testing.T, address string) *mcpTestConn {
	t.Helper()
	a, e := netip.ParseAddrPort(address)
	if e != nil {
		t.Fatal("dial was not numeric")
	}
	return &mcpTestConn{peer: &net.TCPAddr{IP: net.IP(a.Addr().AsSlice()), Port: int(a.Port())}}
}
func mcpPolicy(t *testing.T, c MCPNetworkPolicyConfig, r MCPNetworkResolver, d MCPNetworkDialer) *MCPNetworkPolicy {
	t.Helper()
	p, e := NewMCPNetworkPolicy(c, r, d)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func mcpURL(s string) *url.URL {
	u, e := url.Parse(s)
	if e != nil {
		panic(e)
	}
	return u
}
func mcpIPURL(ip string) *url.URL {
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, "8080")}
}

func TestMCPNetworkFixedPublicClassifier(t *testing.T) {
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, nil)
	// Independently list representatives for every registry block (including
	// special entries marked globally reachable), plus multicast/reserved space.
	denied := []string{"0.0.0.0", "0.255.255.255", "10.2.3.4", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.169.254", "172.16.0.1", "172.31.255.255", "192.0.0.9", "192.0.0.170", "192.0.2.1", "192.31.196.1", "192.52.193.1", "192.88.99.2", "192.168.1.1", "192.175.48.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1", "224.0.0.1", "239.255.255.255", "240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::808:808", "64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001::1", "2001:1::3", "2001:2::1", "2001:3::1", "2001:4:112::1", "2001:20::1", "2001:30::1", "2001:db8::1", "2002:808:808::1", "2620:4f:8000::1", "3fff::1", "5f00::1", "fc00::1", "fdff::1", "fe80::1", "fec0::1", "ff02::1", "4000::1", "::2"}
	for _, ip := range denied {
		t.Run(ip, func(t *testing.T) {
			if e := p.CheckURL(mcpIPURL(ip)); e != ErrMCPNetworkDenied {
				t.Fatalf("special target not denied: %v", e)
			}
		})
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1", "198.17.255.255", "198.20.0.1", "223.255.255.254", "2001:4860:4860::8888", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		t.Run("public_"+ip, func(t *testing.T) {
			if e := p.CheckURL(mcpIPURL(ip)); e != nil {
				t.Fatal(e)
			}
		})
	}
	// Confirm the key counterexample motivating the fixed registry policy.
	if !netip.MustParseAddr("10.0.0.1").IsGlobalUnicast() {
		t.Fatal("unexpected stdlib semantics")
	}
}

func TestMCPNetworkURLAndPorts(t *testing.T) {
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, nil)
	for _, s := range []string{"http://example.test", "https://example.test/a?x=1#fragment", "http://example.test:1", "https://example.test:65535", "http://example.test:9000", "http://EXAMPLE.test./", "https://bücher.example/", "http://[2606:4700::1111]:8080/"} {
		t.Run(s, func(t *testing.T) {
			u := mcpURL(s)
			before := *u
			if e := p.CheckURL(u); e != nil {
				t.Fatal(e)
			}
			if *u != before {
				t.Fatal("URL/Host/SNI input mutated")
			}
		})
	}
	invalid := []*url.URL{nil, {Scheme: "ftp", Host: "example.test"}, {Scheme: "http", Opaque: "example.test"}, {Scheme: "http"}, {Scheme: "http", Host: "example.test", User: url.UserPassword("SYNTHETIC", "SECRET")}, {Scheme: "http", Host: "example.test:"}, {Scheme: "http", Host: "example.test:0"}, {Scheme: "http", Host: "example.test:65536"}, {Scheme: "http", Host: "example.test:-1"}, {Scheme: "http", Host: "example.test:80x"}, {Scheme: "http", Host: "example.test/path"}, {Scheme: "http", Host: "example.test?x=1"}, {Scheme: "http", Host: "secret@example.test"}, {Scheme: "http", Host: "[fe80::1%en0]"}, {Scheme: "http", Host: "bad_name.test"}, {Scheme: "http", Host: "a..test"}, {Scheme: "http", Host: "-a.test"}, {Scheme: "http", Host: "example.test\n"}}
	for i, u := range invalid {
		if e := p.CheckURL(u); e != ErrMCPNetworkPolicyInvalid {
			t.Fatalf("invalid URL %d: %v", i, e)
		}
	}
	if e := p.CheckURL(&url.URL{Scheme: "http", Host: "１２７.０.０.１"}); e != ErrMCPNetworkDenied {
		t.Fatal("IDNA numeric address escaped literal classification")
	}
	limited := mcpPolicy(t, MCPNetworkPolicyConfig{AllowedPorts: []uint16{9443}}, nil, nil)
	if limited.CheckURL(mcpURL("https://example.test:9443")) != nil || limited.CheckURL(mcpURL("https://example.test")) != ErrMCPNetworkDenied {
		t.Fatal("trusted port restriction not enforced")
	}
	// CheckURL performs no resolver call, even for an allowed DNS name.
	noIO := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("pure URL check did DNS")
		return nil, nil
	}), nil)
	if noIO.CheckURL(mcpURL("http://example.test:12345")) != nil {
		t.Fatal("custom public port rejected")
	}
}

func TestMCPNetworkTrustedConfigOwned(t *testing.T) {
	cfg := MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::ffff:127.0.0.1/128")}, AllowedPorts: []uint16{8080, 9000}}
	p := mcpPolicy(t, cfg, nil, nil)
	id := p.PolicyID()
	cfg.AllowedCIDRs[0] = netip.MustParsePrefix("192.168.0.0/16")
	cfg.AllowedPorts[0] = 22
	if p.CheckURL(mcpURL("http://10.0.0.1:8080")) != nil || p.CheckURL(mcpURL("http://127.0.0.1:8080")) != nil || p.CheckURL(mcpURL("http://192.168.0.1:8080")) != ErrMCPNetworkDenied || p.CheckURL(mcpURL("http://10.0.0.1:22")) != ErrMCPNetworkDenied || p.PolicyID() != id {
		t.Fatal("caller mutation changed policy")
	}
	equiv := mcpPolicy(t, MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8")}, AllowedPorts: []uint16{9000, 8080, 8080}}, nil, nil)
	if equiv.PolicyID() != id {
		t.Fatal("equivalent normalized policies differ")
	}
	if mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, nil).PolicyID() == id {
		t.Fatal("transport policies share ID")
	}
	for _, bad := range []MCPNetworkPolicyConfig{{AllowedPorts: []uint16{0}}, {AllowedCIDRs: []netip.Prefix{{}}}, {AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.1.2.3/8")}}, {AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("::ffff:0:0/80")}}} {
		if v, e := NewMCPNetworkPolicy(bad, nil, nil); v != nil || e != ErrMCPNetworkPolicyInvalid {
			t.Fatal("invalid trusted config accepted")
		}
	}
	broad := mcpPolicy(t, MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}, nil, nil)
	for _, ip := range []string{"0.0.0.0", "::", "224.0.0.1", "ff02::1", "255.255.255.255"} {
		if broad.CheckURL(mcpIPURL(ip)) != ErrMCPNetworkDenied {
			t.Fatal("nondestination authorized")
		}
	}
	var zero MCPNetworkPolicy
	var absent *MCPNetworkPolicy
	for _, v := range []*MCPNetworkPolicy{&zero, absent} {
		if v.CheckURL(mcpURL("http://example.test")) != ErrMCPNetworkPolicyInvalid {
			t.Fatal("zero policy usable")
		}
	}
}

func TestMCPNetworkDNSAllAnswersAndRebinding(t *testing.T) {
	public := netip.MustParseAddr("8.8.8.8")
	private := netip.MustParseAddr("127.0.0.1")
	for _, answers := range [][]netip.Addr{{public, private}, {private, public}, {public, {}}, {public, netip.MustParseAddr("::ffff:127.0.0.1")}, {public, netip.MustParseAddr("fe80::1%en0")}} {
		var dialed int
		p := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return answers, nil }), mcpDialerFunc(func(context.Context, string, string) (net.Conn, error) { dialed++; return nil, nil }))
		if c, e := p.DialContext(context.Background(), "tcp4", "mixed.test:8080"); c != nil || e != ErrMCPNetworkDenied || dialed != 0 {
			t.Fatal("dial occurred before all candidates passed")
		}
	}
	var lookups, dials int
	answers := []netip.Addr{public, netip.MustParseAddr("::ffff:8.8.8.8")}
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(_ context.Context, n, h string) ([]netip.Addr, error) {
		lookups++
		if n != "ip" || h != "rebind.test" {
			t.Fatal("wrong lookup")
		}
		if lookups > 1 {
			return []netip.Addr{private}, nil
		}
		return answers, nil
	}), mcpDialerFunc(func(_ context.Context, n, a string) (net.Conn, error) {
		dials++
		if a != "8.8.8.8:8080" {
			t.Fatal("host or unapproved IP passed to dialer")
		}
		answers[0] = private
		return mcpNumericConn(t, a), nil
	}))
	c, e := p.DialContext(context.Background(), "tcp", "rebind.test:8080")
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	if lookups != 1 || dials != 1 {
		t.Fatal("resolved twice or retried duplicate")
	}
	if c, e = p.DialContext(context.Background(), "tcp", "rebind.test:8080"); c != nil || e != ErrMCPNetworkDenied || lookups != 2 || dials != 1 {
		t.Fatal("new lookup rebinding bypassed validation")
	}
	literal := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("numeric address resolved again")
		return nil, nil
	}), mcpDialerFunc(func(_ context.Context, _ string, a string) (net.Conn, error) { return mcpNumericConn(t, a), nil }))
	c, e = literal.DialContext(context.Background(), "tcp", "[::ffff:8.8.8.8]:65535")
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
}

func TestMCPNetworkWrongPeerAndRedaction(t *testing.T) {
	for _, peer := range []net.Addr{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80}, &net.TCPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 81}, &net.TCPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 80, Zone: "en0"}, &net.TCPAddr{IP: net.IP{1}, Port: 80}, &net.UnixAddr{Name: "SYNTHETIC_SECRET", Net: "unix"}, (*net.TCPAddr)(nil)} {
		conn := &mcpTestConn{peer: peer}
		p := mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, mcpDialerFunc(func(context.Context, string, string) (net.Conn, error) { return conn, nil }))
		if c, e := p.DialContext(context.Background(), "tcp", "8.8.8.8:80"); c != nil || e != ErrMCPNetworkPeer || !conn.closed.Load() {
			t.Fatal("wrong peer accepted or not closed")
		}
	}
	leak := errors.New("SYNTHETIC_SECRET_URL_TOKEN")
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, leak }), nil)
	if _, e := p.DialContext(context.Background(), "tcp", "secret.test:80"); e != ErrMCPNetworkResolve || strings.Contains(e.Error(), "SECRET") {
		t.Fatal("resolver details escaped")
	}
	for _, nilSuccess := range []bool{false, true} {
		p = mcpPolicy(t, MCPNetworkPolicyConfig{}, nil, mcpDialerFunc(func(context.Context, string, string) (net.Conn, error) {
			if nilSuccess {
				return nil, nil
			}
			return nil, leak
		}))
		if _, e := p.DialContext(context.Background(), "tcp", "8.8.8.8:80"); e != ErrMCPNetworkDial {
			t.Fatal("dial details escaped")
		}
	}
}

func TestMCPNetworkCancellationAndFamilies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failResolver := mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("cancelled call resolved")
		return nil, nil
	})
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, failResolver, nil)
	if _, e := p.DialContext(ctx, "tcp", "example.test:80"); e != context.Canceled {
		t.Fatal("pre-cancel ignored")
	}
	for _, phase := range []string{"resolve", "dial", "returnedConn"} {
		t.Run(phase, func(t *testing.T) {
			cctx, stop := context.WithCancel(context.Background())
			defer stop()
			conn := mcpNumericConn(t, "8.8.8.8:80")
			r := mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				if phase == "resolve" {
					stop()
					return nil, errors.New("SECRET")
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			})
			d := mcpDialerFunc(func(context.Context, string, string) (net.Conn, error) {
				stop()
				if phase == "returnedConn" {
					return conn, nil
				}
				return nil, errors.New("SECRET")
			})
			p := mcpPolicy(t, MCPNetworkPolicyConfig{}, r, d)
			if c, e := p.DialContext(cctx, "tcp", "example.test:80"); c != nil || e != context.Canceled {
				t.Fatal("cancel lost")
			}
			if phase == "returnedConn" && !conn.closed.Load() {
				t.Fatal("cancelled connection leaked")
			}
		})
	}
	for _, network := range []string{"udp", "unix", "tcp7"} {
		if _, e := p.DialContext(context.Background(), network, "8.8.8.8:80"); e != ErrMCPNetworkPolicyInvalid {
			t.Fatal("unsupported network")
		}
	}
	for _, address := range []string{"example.test", "example.test:0", "example.test:http", "example.test:65536", "[fe80::1%en0]:80"} {
		if _, e := p.DialContext(context.Background(), "tcp", address); e != ErrMCPNetworkPolicyInvalid {
			t.Fatal("invalid target")
		}
	}
	var dials int
	r := mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2606:4700::1111"), netip.MustParseAddr("8.8.8.8")}, nil
	})
	d := mcpDialerFunc(func(_ context.Context, n, a string) (net.Conn, error) {
		dials++
		if n != "tcp4" || a != "8.8.8.8:80" {
			t.Fatal("family mismatch")
		}
		return mcpNumericConn(t, a), nil
	})
	p = mcpPolicy(t, MCPNetworkPolicyConfig{}, r, d)
	c, e := p.DialContext(context.Background(), "tcp4", "example.test:80")
	if e != nil || dials != 1 {
		t.Fatal("family selection failed")
	}
	c.Close()
}

func TestMCPNetworkRealLocalSocket(t *testing.T) {
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	end := make(chan error, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			end <- e
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 4)
		_, e = io.ReadFull(c, b)
		if e == nil && string(b) != "ping" {
			e = errors.New("unexpected input")
		}
		if e == nil {
			_, e = c.Write([]byte("pong"))
		}
		end <- e
	}()
	address := listener.Addr().(*net.TCPAddr)
	var lookups atomic.Int32
	resolver := mcpResolverFunc(func(_ context.Context, n, h string) ([]netip.Addr, error) {
		lookups.Add(1)
		if n != "ip" || h != "owned.test" {
			t.Error("unexpected DNS input")
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	cfg := MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, AllowedPorts: []uint16{uint16(address.Port)}}
	p := mcpPolicy(t, cfg, resolver, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, e := p.DialContext(ctx, "tcp4", net.JoinHostPort("owned.test", strconv.Itoa(address.Port)))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, e = conn.Write([]byte("ping")); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 4)
	if _, e = io.ReadFull(conn, b); e != nil || string(b) != "pong" {
		t.Fatal("real TCP exchange failed")
	}
	conn.Close()
	if e = <-end; e != nil {
		t.Fatal(e)
	}
	if lookups.Load() != 1 {
		t.Fatal("unexpected DNS count")
	}
}

func TestMCPNetworkConcurrentDecisions(t *testing.T) {
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}), mcpDialerFunc(func(_ context.Context, _ string, a string) (net.Conn, error) { return mcpNumericConn(t, a), nil }))
	id := p.PolicyID()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if p.PolicyID() != id || p.CheckURL(mcpURL("https://example.test:8443")) != nil {
					t.Error("immutable policy changed")
				}
				c, e := p.DialContext(context.Background(), "tcp", "example.test:8443")
				if e != nil {
					t.Error(e)
					return
				}
				c.Close()
			}
		}()
	}
	wg.Wait()
}

func TestMCPNetworkConnectFailureBoundaries(t *testing.T) {
	empty := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil }), mcpDialerFunc(func(context.Context, string, string) (net.Conn, error) { t.Fatal("empty DNS dialed"); return nil, nil }))
	if c, e := empty.DialContext(context.Background(), "tcp", "empty.test:80"); c != nil || e != ErrMCPNetworkResolve {
		t.Fatal("empty answers accepted")
	}
	var calls, lookups int
	abandoned := mcpNumericConn(t, "8.8.8.8:8080")
	p := mcpPolicy(t, MCPNetworkPolicyConfig{}, mcpResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
	}), mcpDialerFunc(func(_ context.Context, _ string, address string) (net.Conn, error) {
		calls++
		if calls == 1 {
			if address != "8.8.8.8:8080" {
				t.Fatal("first numeric candidate")
			}
			return abandoned, errors.New("SECRET")
		}
		if address != "1.1.1.1:8080" {
			t.Fatal("second numeric candidate")
		}
		return mcpNumericConn(t, address), nil
	}))
	c, e := p.DialContext(context.Background(), "tcp", "fallback.test:8080")
	if e != nil || calls != 2 || lookups != 1 || !abandoned.closed.Load() {
		t.Fatal("candidate fallback or failed connection ownership")
	}
	c.Close()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if c, e := p.DialContext(ctx, "tcp", "fallback.test:8080"); c != nil || e != context.DeadlineExceeded || calls != 2 || lookups != 1 {
		t.Fatal("expired deadline not respected")
	}
}
