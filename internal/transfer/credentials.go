package transfer

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/idna"
)

var (
	ErrScopedContext     = errors.New("needs_scoped_context")
	ErrCredentialContext = errors.New("invalid_request_context")
)

type HeaderUpdateMode string

const (
	HeadersRetain  HeaderUpdateMode = "retain"
	HeadersReplace HeaderUpdateMode = "replace"
	HeadersClear   HeaderUpdateMode = "clear"
)

func CredentialOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrCredentialContext
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" || u.Hostname() == "" {
		return "", ErrCredentialContext
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", ErrCredentialContext
		}
		host = strings.ToLower(host)
	}
	port := u.Port()
	if port == "" {
		if scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", ErrCredentialContext
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
}
func CredentialHeaders(input map[string]string) (http.Header, error) {
	out := http.Header{}
	for k, v := range input {
		// Preserve the existing request builder's boundary-whitespace cleanup.
		// Validate afterwards so embedded control characters stay forbidden.
		v = strings.TrimSpace(v)
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return nil, ErrCredentialContext
		}
		key := http.CanonicalHeaderKey(k)
		if old, exists := out[key]; exists && old[0] != v {
			return nil, ErrCredentialContext
		}
		out[key] = []string{v}
	}
	return out, nil
}
func HeaderMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = strings.Join(v, ", ")
		}
	}
	return out
}
func inheritedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept", "Accept-Language", "User-Agent", "Range", "If-Range", "Accept-Encoding":
		return true
	}
	return false
}

// InheritedHeaders is a closed allowlist. Unknown private header names can carry
// credentials even when their spelling contains no token/auth keyword.
func InheritedHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, v := range h {
		if inheritedHeader(k) {
			out[k] = append([]string(nil), v...)
		}
	}
	if ref := h.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err == nil && u.User == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			u.Path = ""
			u.RawPath = ""
			u.RawQuery = ""
			u.Fragment = ""
			out.Set("Referer", u.String())
		}
	}
	return out
}
func HasUnscopedCredentials(h http.Header) bool {
	for key := range h {
		if !inheritedHeader(key) && !strings.EqualFold(key, "Referer") {
			return true
		}
	}
	return false
}
func DropProxyHeaders(h http.Header) {
	for _, name := range []string{"Proxy-Authorization", "Proxy-Authenticate", "ProxyAuthorization"} {
		h.Del(name)
	}
}

// TargetCredentials is constructed by trusted local code from an explicitly
// supplied target context, never by decoding a payload's trusted=true flag.
// headers are detached; jar ownership is transferred and remains target-bound.
type TargetCredentials struct {
	origin  string
	headers http.Header
	user    *url.Userinfo
	jar     http.CookieJar
}

func NewTargetCredentials(raw string, headers map[string]string, jar http.CookieJar) (*TargetCredentials, error) {
	origin, err := CredentialOrigin(raw)
	if err != nil {
		return nil, err
	}
	h, err := CredentialHeaders(headers)
	if err != nil {
		return nil, err
	}
	DropProxyHeaders(h)
	u, _ := url.Parse(raw)
	return &TargetCredentials{origin: origin, headers: h.Clone(), user: u.User, jar: jar}, nil
}
func (c *TargetCredentials) Matches(raw string) bool {
	if c == nil {
		return false
	}
	o, err := CredentialOrigin(raw)
	return err == nil && o == c.origin
}
func (c *TargetCredentials) Apply(r *http.Request) error {
	if !c.Matches(r.URL.String()) {
		return ErrScopedContext
	}
	for k, v := range c.headers {
		r.Header[k] = append([]string(nil), v...)
	}
	if host := c.headers.Get("Host"); host != "" {
		r.Host = host
		r.Header.Del("Host")
	}
	if c.user != nil {
		r.URL.User = c.user
		if r.Header.Get("Authorization") == "" {
			password, _ := c.user.Password()
			r.SetBasicAuth(c.user.Username(), password)
		}
	}
	if c.jar != nil {
		for _, cookie := range c.jar.Cookies(r.URL) {
			r.AddCookie(cookie)
		}
	}
	DropProxyHeaders(r.Header)
	return nil
}
func (c *TargetCredentials) RequestHeaders(raw string) (http.Header, *url.Userinfo, error) {
	if !c.Matches(raw) {
		return nil, nil, ErrScopedContext
	}
	return c.headers.Clone(), c.user, nil
}

func (c *TargetCredentials) WithHeaders(raw string, headers map[string]string) (*TargetCredentials, error) {
	if !c.Matches(raw) {
		return nil, ErrScopedContext
	}
	return NewTargetCredentials(raw, headers, c.jar)
}
