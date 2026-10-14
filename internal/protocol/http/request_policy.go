package http

import "net/http"

// SetRequestPolicy installs an in-memory policy for protocols that delegate
// their byte transfer to HTTP. Call only while the fetcher is inactive.
func (f *Fetcher) SetRequestPolicy(policy func(*http.Request)) { f.requestPolicy = policy }
func (f *Fetcher) SetResponsePolicy(policy func(*http.Request, *http.Response) error) {
	f.responsePolicy = policy
}

type policyTransport struct {
	transport      http.RoundTripper
	policy         func(*http.Request)
	responsePolicy func(*http.Request, *http.Response) error
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	if t.policy != nil {
		t.policy(r)
	}
	resp, err := t.transport.RoundTrip(r)
	if err == nil && t.responsePolicy != nil {
		err = t.responsePolicy(r, resp)
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
	}
	return resp, err
}
