package netdisk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/GopeedLab/gopeed/internal/controller"
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
}

func NewAPIClient(ctl *controller.Controller, req *base.Request, info model.ProviderInfo, s *Session) *APIClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if ctl != nil && ctl.GetProxy != nil {
		tr.Proxy = ctl.GetProxy(req.Proxy)
	}
	hosts := append(append([]string{}, info.Hosts...), info.APIHosts...)
	jar, _ := cookiejar.New(nil)
	if s != nil {
		SeedCookies(jar, s.Capture.Cookies, hosts)
	}
	c := &APIClient{Hosts: hosts, Client: &http.Client{Transport: tr, Jar: jar, Timeout: 35 * time.Second}}
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
	res, err := c.Client.Do(r)
	if err != nil {
		return nil, Fail("network_error", "网盘接口请求失败，请检查网络或代理")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		// Providers return login/quota codes in JSON even with HTTP 400/403.
		// Read only the status code, never surface raw bodies containing tokens.
		var payload map[string]any
		decoder := json.NewDecoder(io.LimitReader(res.Body, 64<<10))
		decoder.UseNumber()
		if decoder.Decode(&payload) == nil {
			for _, key := range []string{"code", "errno", "errorCode"} {
				if value, ok := payload[key]; ok && str(value) != "0" {
					return nil, responseError(str(value))
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
