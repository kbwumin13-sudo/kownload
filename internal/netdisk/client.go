package netdisk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

type APIClient struct {
	Client *http.Client
	Hosts  []string
	// API calls share a small per-account budget and its rate-limit cooldown.
	budget *transfer.Budget
	source transfer.SourceKey
}

// apiTransport reuses the downloader's multiplexed API transport for the
// request's proxy policy, or returns nil to build a private one.
func apiTransport(ctl *controller.Controller, req *base.Request) http.RoundTripper {
	if ctl == nil || ctl.Transports == nil || ctl.GetProxyPolicy == nil || req == nil {
		return nil
	}
	proxy, proxyID := ctl.GetProxyPolicy(req.Proxy)
	rt, err := ctl.Transports.Shared(transfer.TransportOptions{ProxyID: proxyID, Proxy: proxy,
		DialTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second, HTTP2: true})
	if err != nil {
		return nil
	}
	return rt
}

// apiSource keys the API budget by provider and known account; sessions
// without an account ID stay separate rather than guessing a shared limit.
func apiSource(info model.ProviderInfo, s *Session) transfer.SourceKey {
	account := ""
	if s != nil {
		account = s.Capture.AccountID
		if account == "" {
			account = "session:" + s.ID
		}
	}
	return transfer.SourceKey("api|" + info.ID + "|" + account)
}

func NewAPIClient(ctl *controller.Controller, req *base.Request, info model.ProviderInfo, s *Session) *APIClient {
	var tr http.RoundTripper = apiTransport(ctl, req)
	if tr == nil {
		private := http.DefaultTransport.(*http.Transport).Clone()
		if ctl != nil && ctl.GetProxy != nil {
			private.Proxy = ctl.GetProxy(req.Proxy)
		}
		tr = private
	}
	hosts := append(append([]string{}, info.Hosts...), info.APIHosts...)
	jar, _ := cookiejar.New(nil)
	if s != nil {
		seedHosts := append([]string{}, hosts...)
		providerAllowed := &APIClient{Hosts: hosts}
		pages := []string{s.Capture.SourcePage}
		if req != nil {
			pages = append(pages, req.URL)
		}
		for _, raw := range pages {
			if u, err := url.Parse(raw); err == nil && providerAllowed.Allowed(u) {
				seedHosts = append(seedHosts, u.Hostname())
			}
		}
		for _, cookie := range s.Capture.Cookies {
			if strings.HasPrefix(cookie.Domain, ".") {
				SeedCookies(jar, []model.Cookie{cookie}, seedHosts)
				continue
			}
			// Chrome encodes host-only cookies without a leading dot. Omitting
			// Domain in the jar preserves that boundary, including child hosts.
			for _, h := range seedHosts {
				if cookie.Name == "" || cookie.Domain == "" || !strings.EqualFold(h, cookie.Domain) {
					continue
				}
				p := cookie.Path
				if p == "" {
					p = "/"
				}
				n := &http.Cookie{Name: cookie.Name, Value: cookie.Value, Path: p, Secure: cookie.Secure}
				if cookie.Expires > 0 {
					n.Expires = time.Unix(int64(cookie.Expires), 0)
				}
				jar.SetCookies(&url.URL{Scheme: "https", Host: h, Path: "/"}, []*http.Cookie{n})
			}
		}
	}
	c := &APIClient{Hosts: hosts, Client: &http.Client{Transport: tr, Jar: jar, Timeout: 35 * time.Second}}
	if ctl != nil && ctl.Budget != nil {
		c.budget, c.source = ctl.Budget, apiSource(info, s)
	}
	c.Client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return fmt.Errorf("网盘接口重定向次数过多")
		}
		if !c.Allowed(r.URL) {
			return fmt.Errorf("网盘接口跳转到不支持的域名")
		}
		if len(via) > 0 && !strings.EqualFold(r.URL.Hostname(), via[0].URL.Hostname()) {
			for _, k := range []string{"Authorization", "Cookie", "X-Captcha-Token", "AccessToken", "X-Device-Id"} {
				r.Header.Del(k)
			}
		}
		return nil
	}
	return c
}

func HostMatches(host, domain string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(domain, "."), "."))
	return host == domain || strings.HasSuffix(host, "."+domain)
}
func (c *APIClient) Allowed(u *url.URL) bool {
	if u == nil || u.User != nil || u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	for _, h := range c.Hosts {
		if HostMatches(u.Hostname(), h) {
			return true
		}
	}
	return false
}

func SeedCookies(jar http.CookieJar, cookies []model.Cookie, hosts []string) {
	for _, h := range hosts {
		u := &url.URL{Scheme: "https", Host: h, Path: "/"}
		for _, c := range cookies {
			if c.Domain == "" || !HostMatches(h, c.Domain) || c.Name == "" {
				continue
			}
			p := c.Path
			if p == "" {
				p = "/"
			}
			n := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: p, Secure: c.Secure}
			if c.Expires > 0 {
				n.Expires = time.Unix(int64(c.Expires), 0)
			}
			jar.SetCookies(u, []*http.Cookie{n})
		}
	}
}

func (c *APIClient) Do(ctx context.Context, method, raw string, body []byte, headers map[string]string) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil || !c.Allowed(u) {
		return nil, fmt.Errorf("不支持的网盘接口地址")
	}
	r, err := http.NewRequestWithContext(ctx, method, raw, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if !strings.EqualFold(k, "Cookie") {
			r.Header.Set(k, v)
		}
	}
	if c.budget != nil {
		// The slot covers the request until response headers, so nested calls
		// made while a body is read cannot wait on their own slot.
		release, err := c.budget.AcquireAPI(ctx, c.source)
		if err != nil {
			rateLimited := &Error{Code: "rate_limited", Message: "网盘接口请求过于频繁，请稍后重试"}
			// Nothing was sent: a task run waiting out a long cooldown hands its
			// running slot back and resolves again once the cooldown ends.
			if deferred, ok := transfer.AsDeferred(err); ok {
				rateLimited.cause = deferred
			}
			return nil, rateLimited
		}
		defer release()
	}
	res, err := c.Client.Do(r)
	if err != nil {
		return nil, Fail("network_error", "网盘接口请求失败，请检查网络或代理")
	}
	if c.budget != nil {
		if transfer.IsRateLimitStatus(res.StatusCode) {
			// Later calls wait for the server's cooldown; this call is not replayed
			// because it may have side effects such as a transfer.
			wait, ok := transfer.ParseRetryAfter(res.Header.Get("Retry-After"), time.Now())
			c.budget.APIRateLimited(c.source, wait, ok)
		} else {
			c.budget.APISucceeded(c.source)
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		// Providers return login/quota codes in JSON even with HTTP 400/403.
		// Read only fixed code fields; unknown values and response text may contain secrets.
		var payload map[string]any
		decoder := json.NewDecoder(io.LimitReader(res.Body, 64<<10))
		decoder.UseNumber()
		if decoder.Decode(&payload) == nil {
			for _, key := range []string{"error", "code", "errno", "errorCode", "error_code"} {
				code := strings.ToLower(str(payload[key]))
				switch code {
				case "captcha_required", "captcha_invalid", "captcha_expired", "invalid_captcha", "invalid_captcha_token":
					code = "132"
				case "unauthenticated":
					code = "unauthorized"
				}
				// responseError's fallback echoes its input. Only return its known,
				// fixed messages, and keep looking if another field has a known code.
				if err, ok := responseError(code).(*Error); ok && err.Code != "api_error" {
					return nil, err
				}
			}
		}
		return nil, Fail("api_error", fmt.Sprintf("网盘接口返回 HTTP %d", res.StatusCode))
	}
	return res, nil
}
func (c *APIClient) JSON(ctx context.Context, method, raw string, body any, headers map[string]string, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	h := map[string]string{"Accept": "application/json", "Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	r, err := c.Do(ctx, method, raw, data, h)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if err = json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(out); err != nil {
		return Fail("invalid_response", "网盘接口返回了无法识别的数据")
	}
	return nil
}
func (c *APIClient) Form(ctx context.Context, raw string, body url.Values, headers map[string]string, out any) error {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range headers {
		h[k] = v
	}
	r, err := c.Do(ctx, "POST", raw, []byte(body.Encode()), h)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(out)
}
func (c *APIClient) Text(ctx context.Context, method, raw string, body []byte, headers map[string]string) (string, error) {
	r, err := c.Do(ctx, method, raw, body, headers)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	return string(b), err
}
