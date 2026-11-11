package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

var (
	ErrMCPNetworkPolicyInvalid = errors.New("invalid MCP network policy or target")
	ErrMCPNetworkDenied        = errors.New("MCP network target denied")
	ErrMCPNetworkResolve       = errors.New("MCP network resolution failed")
	ErrMCPNetworkDial          = errors.New("MCP network connection failed")
	ErrMCPNetworkPeer          = errors.New("MCP network peer mismatch")
)

// MCPNetworkPolicyConfig is TRUSTED constructor input, not tool arguments or a
// JSON authorization interface. Public addresses are allowed by default;
// AllowedCIDRs additionally authorize otherwise special/non-public unicast.
// AllowedPorts applies to both sets: empty means every valid port (1..65535),
// not just 80/443. Unspecified, multicast, limited broadcast and zones are never
// authorized. The constructor owns normalized copies of both slices.
type MCPNetworkPolicyConfig struct {
	AllowedCIDRs []netip.Prefix
	AllowedPorts []uint16
}

// MCPNetworkResolver and MCPNetworkDialer are trusted, concurrent-safe direct
// networking dependencies. A custom dialer must not proxy/tunnel traffic or
// invent RemoteAddr. RemoteAddr is a socket observation, not peer authentication.
// Returned resolver slices must not be concurrently mutated during copying.
type MCPNetworkResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type MCPNetworkDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// MCPNetworkPolicy is an immutable, direct-TCP component, NOT a write capability.
// HTTP adapters must still guard each initial/redirect/retry/probe request,
// preserve Host/TLS SNI and certificate checks, and isolate connection pools by
// PolicyID plus their other transport settings. No HTTP/Task/CLI hook is wired
// here. Proxy remote DNS/PAC and actual output publication remain unsupported.
type MCPNetworkPolicy struct {
	cidrs    []netip.Prefix
	ports    []uint16
	id       string
	resolver MCPNetworkResolver
	dialer   MCPNetworkDialer
}

// Fixed classifier evidence, checked 2026-11-12 (no runtime registry fetch):
// https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml
// https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
// Both registries last updated 2025-10-09; subsets covered by a listed parent
// are folded into it. ALL listed special purposes are excluded by default,
// including globally-reachable anycast/translation entries, not only RFC1918.
// https://www.iana.org/assignments/ipv6-address-space/ipv6-address-space.xhtml
// Last updated 2025-10-23: current ordinary IPv6 unicast allocation is 2000::/3.
// https://www.rfc-editor.org/rfc/rfc6890#section-2.2.1
// https://www.rfc-editor.org/rfc/rfc8190#section-2.1
// Global reachability differs from allocation scope; Go IsGlobalUnicast alone
// includes private/ULA addresses and therefore cannot implement this policy.
const mcpNetworkRuleVersion = "mcp-direct/v1:iana-special-2025-10-09:ipv6-space-2025-10-23"
const mcpSpecialPrefixes = `
0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16
172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.31.196.0/24 192.52.193.0/24
192.88.99.0/24 192.168.0.0/16 192.175.48.0/24 198.18.0.0/15
198.51.100.0/24 203.0.113.0/24 240.0.0.0/4
::/128 ::1/128 64:ff9b::/96 64:ff9b:1::/48 100::/64 100:0:0:1::/64
2001::/23 2001:db8::/32 2002::/16 2620:4f:8000::/48 3fff::/20
5f00::/16 fc00::/7 fe80::/10`

// These unexported tables are never returned or changed. IPv4-mapped addresses
// are classified after Unmap, so ::ffff:0:0/96 is not itself a blanket denial.
var mcpSpecialNetworks = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range strings.Fields(mcpSpecialPrefixes) {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()
var mcpIPv6PublicSpace = netip.MustParsePrefix("2000::/3")

// NewMCPNetworkPolicy performs no IO. Nil dependencies select fresh standard
// resolver/dialer instances, with cancellation/deadlines supplied by the caller.
// There is no global DNS cache, new timer, HTTP retry or lease/budget mutation.
func NewMCPNetworkPolicy(cfg MCPNetworkPolicyConfig, resolver MCPNetworkResolver, dialer MCPNetworkDialer) (*MCPNetworkPolicy, error) {
	p := &MCPNetworkPolicy{}
	for _, prefix := range cfg.AllowedCIDRs {
		if !prefix.IsValid() || prefix.Addr().Zone() != "" {
			return nil, ErrMCPNetworkPolicyInvalid
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, ErrMCPNetworkPolicyInvalid
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix != prefix.Masked() {
			return nil, ErrMCPNetworkPolicyInvalid
		}
		p.cidrs = append(p.cidrs, prefix)
	}
	slices.SortFunc(p.cidrs, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	p.cidrs = slices.Compact(p.cidrs)
	p.ports = append([]uint16(nil), cfg.AllowedPorts...)
	for _, port := range p.ports {
		if port == 0 {
			return nil, ErrMCPNetworkPolicyInvalid
		}
	}
	slices.Sort(p.ports)
	p.ports = slices.Compact(p.ports)
	var identity strings.Builder
	identity.WriteString(mcpNetworkRuleVersion)
	identity.WriteString(mcpSpecialPrefixes)
	identity.WriteString(mcpIPv6PublicSpace.String())
	for _, prefix := range p.cidrs {
		identity.WriteString("\ncidr:")
		identity.WriteString(prefix.String())
	}
	for _, port := range p.ports {
		identity.WriteString("\nport:")
		identity.WriteString(strconv.Itoa(int(port)))
	}
	sum := sha256.Sum256([]byte(identity.String()))
	p.id = "mcp-network/v1:" + hex.EncodeToString(sum[:])
	if resolver == nil {
		resolver = &net.Resolver{}
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	p.resolver, p.dialer = resolver, dialer
	return p, nil
}
func (p *MCPNetworkPolicy) PolicyID() string {
	if p == nil {
		return ""
	}
	return p.id
}
func (p *MCPNetworkPolicy) valid() bool {
	return p != nil && p.id != "" && p.resolver != nil && p.dialer != nil
}
func (p *MCPNetworkPolicy) portAllowed(port uint16) bool {
	return port != 0 && (len(p.ports) == 0 || slices.Contains(p.ports, port))
}
func (p *MCPNetworkPolicy) addressAllowed(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if addr.IsUnspecified() || addr.IsMulticast() || addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return false
	}
	for _, prefix := range p.cidrs {
		if prefix.Contains(addr) {
			return true
		}
	}
	if addr.Is6() && !mcpIPv6PublicSpace.Contains(addr) {
		return false
	}
	for _, prefix := range mcpSpecialNetworks {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// CheckURL is pure. A syntactically valid DNS name is NOT a resolved-address
// authorization: the mandatory DialContext check performs that part later.
func (p *MCPNetworkPolicy) CheckURL(u *url.URL) error {
	if !p.valid() || u == nil || u.User != nil || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrMCPNetworkPolicyInvalid
	}
	// Parse the authority independently, including callers constructing URL by
	// hand: no hidden userinfo/path/query/fragment or malformed bracket/port.
	authority, err := url.Parse(u.Scheme + "://" + u.Host)
	if err != nil || authority.Host != u.Host || authority.User != nil || authority.Path != "" || authority.RawQuery != "" || authority.Fragment != "" {
		return ErrMCPNetworkPolicyInvalid
	}
	host, ip, err := mcpTargetHost(u.Hostname())
	if err != nil || host == "" {
		return ErrMCPNetworkPolicyInvalid
	}
	portText := u.Port()
	if portText == "" {
		if strings.HasSuffix(u.Host, ":") {
			return ErrMCPNetworkPolicyInvalid
		}
		if u.Scheme == "http" {
			portText = "80"
		} else {
			portText = "443"
		}
	}
	port, err := mcpTargetPort(portText)
	if err != nil {
		return err
	}
	if !p.portAllowed(port) || (ip.IsValid() && !p.addressAllowed(ip)) {
		return ErrMCPNetworkDenied
	}
	return nil
}

func mcpTargetPort(s string) (uint16, error) {
	if s == "" {
		return 0, ErrMCPNetworkPolicyInvalid
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, ErrMCPNetworkPolicyInvalid
		}
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, ErrMCPNetworkPolicyInvalid
	}
	return uint16(n), nil
}
func mcpTargetHost(host string) (string, netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return "", netip.Addr{}, ErrMCPNetworkPolicyInvalid
		}
		addr = addr.Unmap()
		return addr.String(), addr, nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", netip.Addr{}, ErrMCPNetworkPolicyInvalid
	}
	ascii = strings.ToLower(ascii)
	if addr, err := netip.ParseAddr(ascii); err == nil {
		addr = addr.Unmap()
		return addr.String(), addr, nil
	}
	// Preserve an explicit final dot so an absolute DNS name stays absolute.
	labels := strings.TrimSuffix(ascii, ".")
	if labels == "" || len(labels) > 253 {
		return "", netip.Addr{}, ErrMCPNetworkPolicyInvalid
	}
	for _, label := range strings.Split(labels, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", netip.Addr{}, ErrMCPNetworkPolicyInvalid
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", netip.Addr{}, ErrMCPNetworkPolicyInvalid
			}
		}
	}
	return ascii, netip.Addr{}, nil
}

// DialContext resolves once, checks EVERY answer before any TCP dial, then dials
// only numeric addresses. Candidate fallback is within this one connect call;
// it never retries HTTP or changes recovery budgets. A new call resolves anew.
// The actual socket peer must match the chosen numeric address AND port. This
// is a direct-socket boundary, not proof about transparent OS tunnels or remote
// proxies; a future adapter must refuse unsupported routing capabilities.
func (p *MCPNetworkPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !p.valid() || ctx == nil || (network != "tcp" && network != "tcp4" && network != "tcp6") {
		return nil, ErrMCPNetworkPolicyInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrMCPNetworkPolicyInvalid
	}
	port, err := mcpTargetPort(portText)
	if err != nil {
		return nil, err
	}
	if !p.portAllowed(port) {
		return nil, ErrMCPNetworkDenied
	}
	host, literal, err := mcpTargetHost(host)
	if err != nil {
		return nil, err
	}
	var answers []netip.Addr
	if literal.IsValid() {
		answers = []netip.Addr{literal}
	} else {
		resolved, e := p.resolver.LookupNetIP(ctx, "ip", host)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if e != nil {
			return nil, ErrMCPNetworkResolve
		}
		answers = append([]netip.Addr(nil), resolved...)
	}
	if len(answers) == 0 {
		return nil, ErrMCPNetworkResolve
	}
	candidates := make([]netip.Addr, 0, len(answers))
	for _, ip := range answers {
		if !p.addressAllowed(ip) {
			return nil, ErrMCPNetworkDenied
		}
		ip = ip.Unmap()
		if network == "tcp4" && !ip.Is4() || network == "tcp6" && !ip.Is6() {
			continue
		}
		if !slices.Contains(candidates, ip) {
			candidates = append(candidates, ip)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrMCPNetworkDenied
	}
	for _, ip := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, e := p.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
		if ctx.Err() != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return nil, ctx.Err()
		}
		if e != nil {
			if conn != nil {
				_ = conn.Close()
			}
			continue
		}
		if conn == nil {
			return nil, ErrMCPNetworkDial
		}
		peer, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok || peer == nil || peer.Zone != "" || peer.Port != int(port) {
			_ = conn.Close()
			return nil, ErrMCPNetworkPeer
		}
		observed, ok := netip.AddrFromSlice(peer.IP)
		if !ok || observed.Unmap() != ip {
			_ = conn.Close()
			return nil, ErrMCPNetworkPeer
		}
		return conn, nil
	}
	return nil, ErrMCPNetworkDial
}
