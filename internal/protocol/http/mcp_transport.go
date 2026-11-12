package http

import (
	"errors"
	"net/http"
	"strings"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

var ErrMCPHTTPConfiguration = errors.New("mcp_http_configuration_unsupported")

type mcpHTTPTransportBinding struct {
	transport http.RoundTripper
	networkID string
}

func (f *Fetcher) HasMCPHTTPClient(client *transfer.MCPHTTPClient) bool {
	binding := f.mcpTransport.Load()
	if binding == nil {
		return false
	}
	actual, ok := binding.transport.(*transfer.MCPHTTPClient)
	return ok && actual == client && binding.networkID == client.PolicyID()
}

// SetMCPHTTPTransport installs the actual trusted startup client's one-hop
// transport before any request. Request/response credential policy and the
// fetcher cookie jar remain in the existing outer HTTP chain. Network identity
// is metadata, never a selector for a shared or newly constructed pool.
func (f *Fetcher) SetMCPHTTPTransport(transport http.RoundTripper, networkPolicyID string) error {
	if transport == nil || !strings.HasPrefix(networkPolicyID, "mcp-network/v1:") || f.sourceRequestIssued.Load() {
		return ErrMCPHTTPConfiguration
	}
	binding := &mcpHTTPTransportBinding{transport: transport, networkID: networkPolicyID}
	if !f.mcpTransport.CompareAndSwap(nil, binding) {
		return ErrMCPHTTPConfiguration
	}
	return nil
}

type mcpHTTPConfiguredTransport struct {
	fetcher *Fetcher
	binding *mcpHTTPTransportBinding
}

func (t *mcpHTTPConfiguredTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	f := t.fetcher
	if f.meta == nil || f.meta.Req == nil || f.meta.Req.SkipVerifyCert || f.mcpTransport.Load() != t.binding {
		return nil, ErrMCPHTTPConfiguration
	}
	// The dedicated direct client cannot represent a configured proxy or an
	// insecure/custom TLS profile. Preserve that boundary explicitly rather
	// than dropping it when changing the mechanical transport.
	if proxy := f.clientProxySnapshot(); proxy != nil {
		target, err := proxy(request)
		if err != nil || target != nil {
			return nil, ErrMCPHTTPConfiguration
		}
	}
	return t.binding.transport.RoundTrip(request)
}
