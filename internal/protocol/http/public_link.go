package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/netdisk/mailru"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

const (
	// A Mail.ru link takes new requests for a few minutes. A run refreshes a
	// refused link at most this often: streams refused together share one
	// refresh, and a fresh link refused anyway is not replaced at once.
	publicRefreshInterval = 2 * time.Minute
	// Refreshes in one run, against a refresh storm.
	maxPublicRefreshes = 30
)

func (f *Fetcher) configurePublicSource(ctx context.Context) error {
	if err := base.ParseReqExtra[fhttp.ReqExtra](f.meta.Req); err != nil {
		return err
	}
	extra, _ := f.meta.Req.Extra.(*fhttp.ReqExtra)
	if extra == nil {
		extra = &fhttp.ReqExtra{}
	}
	if extra.Body != "" || extra.Method != "" && !strings.EqualFold(extra.Method, http.MethodGet) {
		return nil
	}
	referer := http.Header{}
	for k, v := range extra.Header {
		referer.Set(k, v)
	}
	source, ok := mailru.Match(f.meta.Req.URL, referer.Get("Referer"))
	if !ok {
		return nil
	}
	f.publicSource.Store(&source)
	if !mailru.Direct(f.meta.Req.URL) {
		return f.refreshPublicLink(ctx, "")
	}
	return nil
}

func (f *Fetcher) publicSourceSnapshot() *mailru.Source {
	source := f.publicSource.Load()
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

// Use the existing request fallback, not a second task or scheduler. A run
// refreshes a refused link at most once per publicRefreshInterval and
// maxPublicRefreshes times in all; concurrent refused streams share the
// result. A refusal beyond that keeps its status, so that a run whose link
// cannot be renewed still ends with status=403.
func (f *Fetcher) refreshPublicLink(ctx context.Context, refusedURL string) error {
	f.publicMu.Lock()
	defer f.publicMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	source := f.publicSourceSnapshot()
	if source == nil {
		return fmt.Errorf("Mail.ru public share page is required to refresh this link")
	}
	f.redirectLock.Lock()
	current := f.redirectURL
	f.redirectLock.Unlock()
	if refusedURL != "" && current != "" && current != refusedURL {
		return nil // another stream renewed the refused link meanwhile
	}
	gap, limit := publicRefreshInterval, maxPublicRefreshes
	if f.publicRefreshGap > 0 {
		gap = f.publicRefreshGap
	}
	if f.publicRefreshLimit > 0 {
		limit = f.publicRefreshLimit
	}
	if f.publicRefreshes >= limit {
		return fmt.Errorf("Mail.ru public link refresh exhausted")
	}
	if f.publicRefreshes > 0 && time.Since(f.publicRefreshedAt) < gap {
		return fmt.Errorf("Mail.ru public link was renewed less than %v ago", gap)
	}
	f.publicRefreshes++
	f.publicRefreshedAt = time.Now()
	client := f.buildClient()
	if policy, ok := client.Transport.(*policyTransport); ok {
		client.Transport = policy.transport
	}
	client.Timeout = 25 * time.Second
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	link, err := source.Refresh(ctx, client)
	if err != nil {
		return err
	}
	f.updateRedirectURL(link)
	return nil
}

func (f *Fetcher) refreshResponse(ctx context.Context, client *http.Client, req *http.Request, resp *http.Response) (*http.Response, error) {
	if !mailru.ClassifyError(resp.StatusCode) || f.publicSourceSnapshot() == nil {
		return resp, nil
	}
	status := resp.StatusCode
	resp.Body.Close()
	if err := f.refreshPublicLink(ctx, req.URL.String()); err != nil {
		return nil, fmt.Errorf("public link refresh: %w", errors.Join(NewRequestError(status), err))
	}
	replacement, err := f.buildRequest(ctx, f.meta.Req)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Range", "If-Range", "Accept-Encoding"} {
		if value := req.Header.Get(name); value != "" {
			replacement.Header.Set(name, value)
		}
	}
	return client.Do(replacement)
}
