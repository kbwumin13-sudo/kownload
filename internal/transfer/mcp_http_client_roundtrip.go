package transfer

import "net/http"

// RoundTrip sends exactly one HTTP hop through this client's existing private
// policy transport and pool. It never follows redirects. The caller owns the
// redirect/credential policy and must call this method again for every next
// hop, probe, range or retry; every hop retains the transport's URL/Host checks
// and numeric-IP dial guard. It does not convert GUI proxy settings to direct.
func (c *MCPHTTPClient) RoundTrip(req *http.Request) (*http.Response, error) {
	if c == nil || c.transport == nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrMCPHTTPRequest
	}
	return c.transport.RoundTrip(req)
}
