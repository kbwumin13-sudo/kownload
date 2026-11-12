package http

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type mcpTestRoundTripper func(*http.Request) (*http.Response, error)

func (round mcpTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return round(request)
}

func TestMCPHTTPTransportKeepsCredentialAndResponsePolicies(t *testing.T) {
	f := newPooledFetcher(controller.NewController())
	defer f.Close()
	f.meta.Req = &base.Request{URL: "http://example.test/file"}
	var calls, responseChecks int
	transport := mcpTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("X-Policy") != "retained" {
			t.Fatal("request policy was bypassed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("owned")), Request: request}, nil
	})
	if err := f.SetMCPHTTPTransport(transport, "mcp-network/v1:"+strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	f.SetRequestPolicy(func(request *http.Request) { request.Header.Set("X-Policy", "retained") })
	f.SetResponsePolicy(func(*http.Request, *http.Response) error { responseChecks++; return nil })
	request := httptest.NewRequest("GET", f.meta.Req.URL, nil)
	response, err := f.buildClient().Transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 || responseChecks != 1 {
		t.Fatal("one-hop policy chain changed", calls, responseChecks)
	}
}

func TestMCPHTTPTransportRejectsUnsupportedConfiguration(t *testing.T) {
	for _, mode := range []string{"insecure", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			ctl := controller.NewController()
			if mode == "proxy" {
				ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) {
					return func(*http.Request) (*url.URL, error) { return url.Parse("http://127.0.0.1:1234") }
				}
			}
			f := newPooledFetcher(ctl)
			defer f.Close()
			f.meta.Req = &base.Request{URL: "http://example.test/file", SkipVerifyCert: mode == "insecure"}
			calls := 0
			rt := mcpTestRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })
			if err := f.SetMCPHTTPTransport(rt, "mcp-network/v1:"+strings.Repeat("b", 64)); err != nil {
				t.Fatal(err)
			}
			_, err := f.buildClient().Transport.RoundTrip(httptest.NewRequest("GET", f.meta.Req.URL, nil))
			if !errors.Is(err, ErrMCPHTTPConfiguration) || calls != 0 {
				t.Fatal("unsupported setting silently discarded", err, calls)
			}
		})
	}
}
