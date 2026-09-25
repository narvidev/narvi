package cimdfetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
)

// ErrForbiddenAddress is wrapped by every refusal to connect: the address
// a connection was about to reach is not a public one.
var ErrForbiddenAddress = errors.New("cimdfetch: refused to connect to a non-public address")

// ErrNotHTTPS is wrapped by every refusal of a URL, first or redirected
// to, whose scheme is not https.
var ErrNotHTTPS = errors.New("cimdfetch: only https URLs are fetched")

// ErrTooManyRedirects is wrapped when a fetch would follow more than
// MaxRedirects redirects.
var ErrTooManyRedirects = errors.New("cimdfetch: too many redirects")

// MaxRedirects is how many redirects one fetch follows at most.
const MaxRedirects = 3

// maxResponseHeaderBytes bounds a document response's headers.
const maxResponseHeaderBytes = 16 << 10

// Resolver resolves a host name to its addresses: net.DefaultResolver in
// production, a fixed table in tests (so no test ever needs the network).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// GuardConfig configures NewGuardedClient. Production passes the zero
// value. Every field is a TEST seam: none widens what the production
// client can reach, and controlplane's own wiring sets none of them.
type GuardConfig struct {
	// Resolver replaces net.DefaultResolver, so a test can make a host
	// name resolve to whatever it needs without DNS.
	Resolver Resolver
	// AllowAddrPorts are exact address:port pairs the guard lets a
	// connection reach although its policy refuses them -- the only way an
	// in-test server on loopback can be fetched from. Nothing else about
	// the guard changes: every other address, including another port on
	// the same loopback address, is still refused.
	AllowAddrPorts []netip.AddrPort
	// RootCAs replaces the system roots, so a test server's own
	// certificate verifies. Verification itself is never turned off.
	RootCAs *x509.CertPool
}

// GuardedClient is an HTTP client every connection of which passes the
// SSRF guard (this package's own doc comment). The only way to obtain one
// is NewGuardedClient; a Fetcher accepts nothing else.
type GuardedClient struct {
	http *http.Client
}

// NewGuardedClient builds the one kind of HTTP client a Fetcher can use.
func NewGuardedClient(cfg GuardConfig) *GuardedClient {
	g := &guard{resolver: cfg.Resolver, allowed: map[netip.AddrPort]bool{}}
	if g.resolver == nil {
		g.resolver = net.DefaultResolver
	}
	for _, ap := range cfg.AllowAddrPorts {
		g.allowed[normalize(ap)] = true
	}
	g.dialer = &net.Dialer{Control: g.control}
	transport := &http.Transport{
		// Never a proxy, from the environment or otherwise: a proxy would
		// make the connection on the guard's behalf, where the dial-time
		// check cannot see it.
		Proxy:       nil,
		DialContext: g.dialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    cfg.RootCAs,
		},
		// Every request dials -- and so resolves and is checked -- afresh:
		// no connection is ever reused for a later request or redirect.
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	return &GuardedClient{http: &http.Client{
		Transport:     httpsOnly{next: transport},
		CheckRedirect: checkRedirect,
	}}
}

// guard is the dial-time SSRF check.
type guard struct {
	resolver Resolver
	dialer   *net.Dialer
	allowed  map[netip.AddrPort]bool
}

// dialContext is the transport's only way to open a connection: it
// resolves the host itself (through the Resolver seam) and dials each
// resulting address as a literal, so the Control hook sees -- and checks
// -- exactly the address each socket is about to connect to.
func (g *guard) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%w: network %q", ErrForbiddenAddress, network)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("cimdfetch: dial %q: %w", address, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("cimdfetch: dial %q: invalid port", address)
	}
	var addrs []netip.Addr
	if literal, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{literal}
	} else {
		addrs, err = g.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("cimdfetch: resolve %q: %w", host, err)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cimdfetch: %q resolved to no address", host)
	}
	var errs []error
	for _, a := range addrs {
		conn, err := g.dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// control is the net.Dialer Control hook: called after the socket is
// created and before it connects, with the address it is about to connect
// to. Returning an error aborts the connection before a single packet is
// sent to that address.
func (g *guard) control(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" {
		return fmt.Errorf("%w: network %q", ErrForbiddenAddress, network)
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable address %q", ErrForbiddenAddress, address)
	}
	if g.allowed[normalize(ap)] {
		return nil
	}
	if err := checkAddr(ap.Addr()); err != nil {
		return fmt.Errorf("%w: %s", err, ap)
	}
	return nil
}

// normalize unmaps an IPv4-mapped IPv6 address, so an allowed pair
// matches however the socket spells it.
func normalize(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// refusedIPv4 is every IPv4 block a fetch may never reach: not globally
// reachable per the IANA special-purpose registry, plus the ranges an
// internal service typically lives on.
var refusedIPv4 = mustPrefixes(
	"0.0.0.0/8",       // "this network", including the unspecified address
	"10.0.0.0/8",      // private (RFC 1918)
	"100.64.0.0/10",   // shared address space (CGNAT, RFC 6598)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, including 169.254.169.254 (cloud metadata)
	"172.16.0.0/12",   // private (RFC 1918)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"192.168.0.0/16",  // private (RFC 1918)
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, including the limited broadcast address
)

// globalIPv6 is the only IPv6 space a fetch may reach (global unicast).
// Everything outside it -- unspecified, loopback, IPv4-compatible, ULA
// fc00::/7, link-local fe80::/10, site-local, multicast ff00::/8,
// discard-only 100::/64 -- is refused by not being inside it.
var globalIPv6 = netip.MustParsePrefix("2000::/3")

// refusedIPv6 is every block inside globalIPv6 a fetch may still never
// reach.
var refusedIPv6 = mustPrefixes(
	"2001::/23",     // IETF protocol assignments (Teredo, benchmarking, ORCHID, ...)
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4: embeds an IPv4 address, possibly a private one
	"3fff::/20",     // documentation
	"5f00::/16",     // SRv6 segment identifiers
)

// nat64 is the well-known NAT64 prefix: its last 32 bits are an IPv4
// address a NAT64 gateway connects to, which is what gets checked.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// checkAddr is the address policy: nil only for a public unicast address.
func checkAddr(a netip.Addr) error {
	if !a.IsValid() || a.Zone() != "" {
		return ErrForbiddenAddress
	}
	a = a.Unmap()
	if a.Is4() {
		for _, p := range refusedIPv4 {
			if p.Contains(a) {
				return ErrForbiddenAddress
			}
		}
		return nil
	}
	if nat64.Contains(a) {
		b := a.As16()
		return checkAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if !globalIPv6.Contains(a) {
		return ErrForbiddenAddress
	}
	for _, p := range refusedIPv6 {
		if p.Contains(a) {
			return ErrForbiddenAddress
		}
	}
	return nil
}

func mustPrefixes(blocks ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, netip.MustParsePrefix(b))
	}
	return out
}

// requireHTTPS is the scheme rule, applied to the first URL and to every
// redirect target.
func requireHTTPS(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("%w: %q", ErrNotHTTPS, u.Scheme)
	}
	return nil
}

// checkRedirect is the client's redirect policy: at most MaxRedirects,
// each to an https URL (never a downgrade, never another scheme). The
// target's address is checked when it is dialed, like any other.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return fmt.Errorf("%w: more than %d", ErrTooManyRedirects, MaxRedirects)
	}
	return requireHTTPS(req.URL)
}

// httpsOnly refuses any request that is not https before it reaches the
// transport -- a second, independent layer under the redirect policy and
// Fetch's own check of the first URL.
type httpsOnly struct {
	next http.RoundTripper
}

func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := requireHTTPS(req.URL); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.next.RoundTrip(req)
}
