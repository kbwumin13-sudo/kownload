package nativehost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
)

type UnixClient struct{ Socket string }

func (c UnixClient) Available(ctx context.Context) bool {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
func (c UnixClient) Post(ctx context.Context, path string, request Request) (int, []byte, error) {
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+path, bytes.NewReader(request.Params))
	if err != nil {
		return 0, nil, err
	}
	// GetBody=nil prevents a transport replay even if implementation defaults change.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	if request.OperationID != "" {
		req.Header.Set("X-Kownload-Operation-Id", request.OperationID)
	}
	if request.Meta != nil {
		meta, _ := json.Marshal(request.Meta)
		req.Header.Set("X-Kownload-Host-Meta", string(meta))
		req.Header.Set("X-Gopeed-Host-Meta", string(meta))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxOutput+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(body) > MaxOutput {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, body, nil
}
