package transfer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPHTTPClientOneHopPoolAndGuard(t *testing.T) {
	var hits, dials atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "http://127.0.0.2/private")
			w.WriteHeader(302)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer s.Close()
	dialer := mcpDialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, a)
	})
	p := mcpHTTPLocalPolicy(t, s.URL, nil, dialer)
	c := mcpHTTPClient(t, p, MCPHTTPOptions{})
	var rt http.RoundTripper = c
	req, _ := http.NewRequest("GET", s.URL+"/start", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 302 || hits.Load() != 1 {
		t.Fatal("single hop followed redirect")
	}
	forbidden, _ := http.NewRequest("GET", resp.Header.Get("Location"), nil)
	if _, err = rt.RoundTrip(forbidden); !errors.Is(err, ErrMCPNetworkDenied) {
		t.Fatal("redirect target bypassed guard", err)
	}
	if got := mcpHTTPRead(t, c, s.URL+"/end"); got != "ok" {
		t.Fatal(got)
	}
	if hits.Load() != 2 || dials.Load() != 1 || c.PolicyID() != p.PolicyID() {
		t.Fatal("RoundTrip did not use client's pool/identity", hits.Load(), dials.Load())
	}
	wrong, _ := http.NewRequest("GET", s.URL+"/end", nil)
	wrong.Host = "other.invalid"
	if _, err = rt.RoundTrip(wrong); !errors.Is(err, ErrMCPHTTPRequest) {
		t.Fatal("Host bypass", err)
	}
	if hits.Load() != 2 {
		t.Fatal("rejected request reached source")
	}
}
func TestMCPHTTPClientOneHopCancellation(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer s.Close()
	c := mcpHTTPClient(t, mcpHTTPLocalPolicy(t, s.URL, nil, nil), MCPHTTPOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	if _, err := c.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatal("cancelled request reached source")
	}
}

type mcpOneHopBody struct {
	io.Reader
	closed bool
}

func (b *mcpOneHopBody) Close() error { b.closed = true; return nil }
func TestMCPHTTPClientOneHopZeroClosesBody(t *testing.T) {
	for _, c := range []*MCPHTTPClient{nil, {}} {
		body := &mcpOneHopBody{Reader: strings.NewReader("private")}
		req, _ := http.NewRequest("POST", "http://example.invalid", body)
		if _, err := c.RoundTrip(req); !errors.Is(err, ErrMCPHTTPRequest) || !body.closed {
			t.Fatal("zero client did not reject/close")
		}
	}
}
