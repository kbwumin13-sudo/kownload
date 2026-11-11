package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Version 1 canonicalizes only fields HTTP interprets. It deliberately does
// not rewrite URL query/escape, Body or authentication values, and is not a
// content identity. Once activated, changing this projection requires a version.
type httpExecutionProjection struct {
	Version    int         `json:"version"`
	URL        string      `json:"url"`
	Method     string      `json:"method"`
	Body       string      `json:"body"`
	Header     http.Header `json:"header"`
	Proxy      string      `json:"proxy"`
	SkipVerify bool        `json:"skipVerify"`
}

func httpProjection(req *base.Request) (httpExecutionProjection, error) {
	if req == nil {
		return httpExecutionProjection{}, ErrRecoveryHeld
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return httpExecutionProjection{}, ErrRecoveryHeld
	}
	var copied base.Request
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&copied) != nil || base.ParseReqExtra[httpModel.ReqExtra](&copied) != nil {
		return httpExecutionProjection{}, ErrRecoveryHeld
	}
	extra, _ := copied.Extra.(*httpModel.ReqExtra)
	if extra == nil {
		extra = &httpModel.ReqExtra{}
	}
	headers, err := CredentialHeaders(extra.Header)
	if err != nil {
		return httpExecutionProjection{}, err
	}
	DropProxyHeaders(headers)
	method := extra.Method
	if method == "" {
		method = "GET"
	}
	proxy := "follow"
	if copied.Proxy != nil {
		if id := copied.Proxy.PolicyID(); id != "" {
			proxy = id
		}
	}
	return httpExecutionProjection{1, copied.URL, method, extra.Body, headers, proxy, copied.SkipVerifyCert}, nil
}
func executionDigest(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", ErrRecoveryHeld
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func ValidExecutionHash(value string) bool {
	raw, e := hex.DecodeString(value)
	return e == nil && len(raw) == 32 && value == strings.ToLower(value)
}
func HTTPExecutionDefaultsHash(req *base.Request, userAgent, proxyID string) (string, error) {
	p, err := httpProjection(req)
	if err != nil {
		return "", err
	}
	defaults := struct {
		Version   int     `json:"version"`
		UserAgent *string `json:"inheritedUserAgent,omitempty"`
		ProxyID   *string `json:"inheritedProxyPolicyId,omitempty"`
	}{Version: 1}
	if _, explicit := p.Header["User-Agent"]; !explicit {
		value := strings.TrimSpace(userAgent)
		defaults.UserAgent = &value
	}
	if p.Proxy == "follow" {
		if proxyID == "" {
			return "", ErrRecoveryHeld
		}
		defaults.ProxyID = &proxyID
	}
	return executionDigest(defaults)
}
func HTTPExecutionSourceHash(req *base.Request, defaultsHash string) (string, error) {
	if !ValidExecutionHash(defaultsHash) {
		return "", ErrRecoveryHeld
	}
	projection, err := httpProjection(req)
	if err != nil {
		return "", err
	}
	return executionDigest(struct {
		Version  int                     `json:"version"`
		Request  httpExecutionProjection `json:"request"`
		Defaults string                  `json:"executionDefaultsHash"`
	}{1, projection, defaultsHash})
}

// Private fields prevent mutation after capture. This is only an in-memory Go
// capability: it is never a new Request field, JSON authority or credential log.
type HTTPExecutionContext struct {
	defaultsHash string
	sourceHash   string
	userAgent    string
	proxyID      string
	proxy        func(*http.Request) (*url.URL, error)
}

func NewHTTPExecutionContext(req *base.Request, userAgent, proxyID string, proxy func(*http.Request) (*url.URL, error)) (*HTTPExecutionContext, error) {
	if proxyID == "" {
		return nil, ErrRecoveryHeld
	}
	defaults, err := HTTPExecutionDefaultsHash(req, userAgent, proxyID)
	if err != nil {
		return nil, err
	}
	source, err := HTTPExecutionSourceHash(req, defaults)
	if err != nil {
		return nil, err
	}
	return &HTTPExecutionContext{defaults, source, strings.TrimSpace(userAgent), proxyID, proxy}, nil
}
func (c *HTTPExecutionContext) DefaultsHash() string                         { return c.defaultsHash }
func (c *HTTPExecutionContext) SourceHash() string                           { return c.sourceHash }
func (c *HTTPExecutionContext) UserAgent() string                            { return c.userAgent }
func (c *HTTPExecutionContext) ProxyID() string                              { return c.proxyID }
func (c *HTTPExecutionContext) Proxy() func(*http.Request) (*url.URL, error) { return c.proxy }
func (c *HTTPExecutionContext) Validate(req *base.Request, sourceHash string) error {
	hash, err := HTTPExecutionSourceHash(req, c.defaultsHash)
	if err != nil || hash != c.sourceHash || hash != sourceHash {
		return ErrRecoveryHeld
	}
	return nil
}
