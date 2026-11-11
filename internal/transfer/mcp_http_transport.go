package transfer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"time"
)

var (
	ErrMCPHTTPOptions       = errors.New("invalid MCP direct HTTP options")
	ErrMCPHTTPRequest       = errors.New("invalid MCP direct HTTP request")
	ErrMCPHTTPTransport     = errors.New("MCP direct HTTP exchange failed")
	ErrMCPHTTPRedirectLimit = errors.New("MCP direct HTTP redirect limit reached")
)

// MCPHTTPOptions is trusted construction input, NOT model input. This is a
// deliberately separate direct-only profile, not a conversion of an existing
// http.Transport or GUI proxy configuration. Unsupported caller settings must
// be rejected by the future adapter, never silently discarded.
//
// Zero durations mean no additional timeout, as in net/http. Request context
// cancellation/deadlines still apply. HTTP2 explicitly opts into HTTP/2.
// RootCAPEM, when nonempty, replaces system roots with these parsed certificates;
// it does not disable verification. No caller-owned bytes or TLS hooks survive
// construction. There is no custom ServerName, insecure TLS, proxy, PAC,
// DialTLS, protocol registration, cookie jar or redirect callback interface.
// Credential/request/response policy integration remains the HTTP owner's job.
type MCPHTTPOptions struct {
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	HTTP2                 bool
	DisableCompression    bool
	RootCAPEM             []byte
}

// MCPHTTPTransport owns an isolated connection pool. It never shares the GUI
// pool, even with another instance having the same network PolicyID. The ID
// identifies the network policy only, not all TLS/settings/dependency identity;
// callers must not use it alone to introduce a shared external pool.
//
// Every RoundTrip checks the URL before even reusing a connection. Every actual
// connection goes through the fixed policy's numeric-IP DialContext, including
// net/http's own reconnects. Reusing an already-authorized connection does not
// re-resolve DNS; a new connection resolves and validates all answers again.
// This is an unwired component, not cap.create or a claim about fetcher hooks.
type MCPHTTPTransport struct {
	policy    *MCPNetworkPolicy
	transport *http.Transport
}

func NewMCPHTTPTransport(policy *MCPNetworkPolicy, opts MCPHTTPOptions) (*MCPHTTPTransport, error) {
	if !policy.valid() {
		return nil, ErrMCPNetworkPolicyInvalid
	}
	// Own the policy value as well as the pool: callers may replace their
	// original pointer's value after construction. Resolver/dialer interfaces
	// remain the explicitly trusted, concurrent-safe dependencies.
	owned := *policy
	owned.cidrs = append(owned.cidrs[:0:0], policy.cidrs...)
	owned.ports = append(owned.ports[:0:0], policy.ports...)
	policy = &owned
	if opts.DialTimeout < 0 || opts.TLSHandshakeTimeout < 0 || opts.ResponseHeaderTimeout < 0 || opts.IdleConnTimeout < 0 {
		return nil, ErrMCPHTTPOptions
	}
	var roots *x509.CertPool
	if len(opts.RootCAPEM) > 0 {
		roots = x509.NewCertPool()
		rest := bytes.TrimSpace(opts.RootCAPEM)
		count := 0
		for len(rest) > 0 {
			// pem.Decode otherwise skips leading garbage; configuration is strict.
			if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
				return nil, ErrMCPHTTPOptions
			}
			block, tail := pem.Decode(rest)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return nil, ErrMCPHTTPOptions
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, ErrMCPHTTPOptions
			}
			roots.AddCert(cert)
			count++
			rest = bytes.TrimSpace(tail)
		}
		if count == 0 {
			return nil, ErrMCPHTTPOptions
		}
	}
	dialTimeout := opts.DialTimeout
	tr := &http.Transport{
		// Never clone DefaultTransport: ProxyFromEnvironment or a caller-installed
		// DialTLS/TLSNextProto hook would bypass the direct authorization boundary.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			timeout := dialTimeout
			if v, ok := ctx.Value(dialTimeoutKey{}).(time.Duration); ok && v > 0 && (timeout == 0 || v < timeout) {
				timeout = v
			}
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return policy.DialContext(ctx, network, address)
		},
		TLSClientConfig:       &tls.Config{RootCAs: roots},
		TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ForceAttemptHTTP2:     opts.HTTP2,
		DisableCompression:    opts.DisableCompression,
	}
	return &MCPHTTPTransport{policy: policy, transport: tr}, nil
}

func (t *MCPHTTPTransport) PolicyID() string {
	if t == nil || t.transport == nil {
		return ""
	}
	return t.policy.PolicyID()
}

func (t *MCPHTTPTransport) check(req *http.Request) error {
	if t == nil || t.transport == nil || !t.policy.valid() || req == nil {
		return ErrMCPHTTPRequest
	}
	if err := t.policy.CheckURL(req.URL); err != nil {
		return err
	}
	// An alternate Host could route an approved IP to an unauthorized virtual
	// host. The original URL authority supplies both HTTP Host and TLS SNI.
	if req.Host != "" && req.Host != req.URL.Host {
		return ErrMCPHTTPRequest
	}
	return nil
}

func (t *MCPHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.check(req); err != nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.transport.RoundTrip(req)
	return resp, mcpHTTPError(err)
}

func (t *MCPHTTPTransport) CloseIdleConnections() {
	if t != nil && t.transport != nil {
		t.transport.CloseIdleConnections()
	}
}

// MCPHTTPClient hides the mutable standard client's Transport/CheckRedirect
// fields. It follows at most the standard ten-request redirect chain. Every
// redirect is checked before dispatch and each resulting RoundTrip checks
// again. It does not retry HTTP failures or change lease/recovery budgets.
// Requests must not be concurrently mutated, per the net/http contract.
type MCPHTTPClient struct {
	transport *MCPHTTPTransport
	client    *http.Client
}

func NewMCPHTTPClient(policy *MCPNetworkPolicy, opts MCPHTTPOptions) (*MCPHTTPClient, error) {
	tr, err := NewMCPHTTPTransport(policy, opts)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if err := tr.check(req); err != nil {
			return err
		}
		if len(via) >= 10 {
			return ErrMCPHTTPRedirectLimit
		}
		return nil
	}}
	return &MCPHTTPClient{transport: tr, client: client}, nil
}

func (c *MCPHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil || c.transport == nil {
		return nil, ErrMCPHTTPRequest
	}
	if err := c.transport.check(req); err != nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := c.client.Do(req)
	// net/http wraps errors with the original URL. Never return that raw wrapper
	// as a bridge error; fixed classes remain matchable with errors.Is.
	return resp, mcpHTTPError(err)
}

func (c *MCPHTTPClient) PolicyID() string {
	if c == nil {
		return ""
	}
	return c.transport.PolicyID()
}
func (c *MCPHTTPClient) CloseIdleConnections() {
	if c != nil {
		c.transport.CloseIdleConnections()
	}
}

func mcpHTTPError(err error) error {
	if err == nil {
		return nil
	}
	for _, safe := range []error{context.Canceled, context.DeadlineExceeded, ErrMCPNetworkPolicyInvalid, ErrMCPNetworkDenied, ErrMCPNetworkResolve, ErrMCPNetworkDial, ErrMCPNetworkPeer, ErrMCPHTTPRequest, ErrMCPHTTPRedirectLimit} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return ErrMCPHTTPTransport
}
