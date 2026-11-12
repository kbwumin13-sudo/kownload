package transfer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/GopeedLab/gopeed/pkg/base"
)

// ProtocolExecutionSourceHash retains the complete interpreted parent request,
// including provider/session/account/file or media context. It is deliberately
// distinct from the unchanged HTTP v1 projection and is not content identity.
func ProtocolExecutionSourceHash(protocol string, req *base.Request, defaults string) (string, error) {
	if (protocol != "netdisk" && protocol != "media") || req == nil || !ValidExecutionHash(defaults) {
		return "", ErrRecoveryHeld
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", ErrRecoveryHeld
	}
	var owned any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&owned) != nil {
		return "", ErrRecoveryHeld
	}
	return executionDigest(struct {
		Version  int
		Protocol string
		Request  any
		Defaults string
	}{1, protocol, owned, defaults})
}

// ProtocolExecutionContext owns actual inherited configuration and the frozen
// proxy handler used by this parent run. A digest without these execution values
// must not be used to certify requests that still read mutable global config.
type ProtocolExecutionContext struct {
	protocol, defaultsHash, sourceHash, proxyID string
	config                                      json.RawMessage
	proxy                                       func(*http.Request) (*url.URL, error)
}

func NewProtocolExecutionContext(protocol string, req *base.Request, config json.RawMessage, proxyID string, proxy func(*http.Request) (*url.URL, error)) (*ProtocolExecutionContext, error) {
	if proxyID == "" || !json.Valid(config) {
		return nil, ErrRecoveryHeld
	}
	owned := append(json.RawMessage(nil), config...)
	defaults, err := executionDigest(struct {
		Version  int
		Protocol string
		Config   json.RawMessage
		Proxy    string
	}{1, protocol, owned, proxyID})
	if err != nil {
		return nil, err
	}
	source, err := ProtocolExecutionSourceHash(protocol, req, defaults)
	if err != nil {
		return nil, err
	}
	return &ProtocolExecutionContext{protocol, defaults, source, proxyID, owned, proxy}, nil
}

func (c *ProtocolExecutionContext) DefaultsHash() string                         { return c.defaultsHash }
func (c *ProtocolExecutionContext) SourceHash() string                           { return c.sourceHash }
func (c *ProtocolExecutionContext) ProxyID() string                              { return c.proxyID }
func (c *ProtocolExecutionContext) Proxy() func(*http.Request) (*url.URL, error) { return c.proxy }
func (c *ProtocolExecutionContext) Config(out any) error {
	if c == nil || out == nil || json.Unmarshal(c.config, out) != nil {
		return ErrRecoveryHeld
	}
	return nil
}
