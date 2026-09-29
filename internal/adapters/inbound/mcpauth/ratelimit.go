package mcpauth

import (
	"container/list"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/narvidev/narvi/internal/platform"
)

// maxTrackedKeys bounds how many keys -- client networks
// (ByClientAddress) or MCP grants (GrantKey) -- one RateLimiter keeps a
// bucket for, so a spray of source addresses cannot grow its memory without
// limit. Past it, the key seen least recently is forgotten to make room (its
// next request starts a fresh bucket) -- a newcomer is never refused
// because the table is full.
const maxTrackedKeys = 10_000

// RateLimiter is a keyed token bucket (technical plan §43.6/§43.14/§43.15;
// golang.org/x/time/rate): each key -- a client network, or an MCP grant --
// may make burst requests at once, then one per interval. It lives in
// memory, one per replica -- a brake on abuse (table growth, spam, a
// runaway client), not a correctness property, so it creates no second
// authority over any state (§5.1). One is built per route, each with its
// own interval, burst, key and table: POST /oauth/register
// (RegisterRateLimited), POST /oauth/token (TokenRateLimited) and GET
// /oauth/authorize (Server.AuthorizeRateLimited), each by client network
// (ByClientAddress); POST /mcp by grant (GrantKey, MCPCallRateLimited). It
// runs before the route's handler, so a refused request reads no body and
// spends nothing -- a refresh token it refuses is not rotated, a code it
// refuses is not consumed, and a tool call it refuses runs no tool. Allow
// alone is the brake narvi_create_session consults per grant, inside the
// MCP adapter, which may not import this package: that adapter names the
// one method it needs as an interface, which *RateLimiter satisfies.
//
// Keyed by network, its memory is bounded at maxTrackedKeys buckets, and no one
// network can use that bound to lock out another: a flood from a single
// network -- one IPv4 address (arriving as IPv4, or through a translator
// ClientAddressKey recognizes), or one IPv6 /48 however many addresses it
// sprays from -- lands in that network's one bucket, and a flood from
// more networks than the table holds only evicts the buckets seen least
// recently, so a client from any other network -- registering, refreshing
// its token, or starting an authorization -- always gets a bucket of its
// own. A network is only what ClientAddressKey reads from the connecting
// address: behind a proxy that hides client addresses, every client is
// the proxy's one network and shares its one bucket, so one sender's
// flood refuses them all. The price is stated, not hidden: a party
// rotating through more networks than the table holds gets each one's
// burst afresh. A per-address brake is beaten by enough addresses
// whatever it does when full; refusing every newcomer when full only
// turned that into a lockout of everyone else.
type RateLimiter struct {
	interval time.Duration
	limit    rate.Limit
	burst    int
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*list.Element // each a *trackedBucket in recency
	recency *list.List               // front: the network seen most recently
}

// trackedBucket is one network's bucket in RateLimiter.recency.
type trackedBucket struct {
	key     string
	limiter *rate.Limiter
}

// NewRateLimiter builds a RateLimiter refilling one request per interval
// up to burst -- both from platform.Timeouts, whose Validate refuses a
// zero interval (which would be no limit at all) and a burst below one.
func NewRateLimiter(interval time.Duration, burst int) *RateLimiter {
	return &RateLimiter{
		interval: interval,
		limit:    rate.Every(interval),
		burst:    burst,
		now:      time.Now,
		buckets:  map[string]*list.Element{},
		recency:  list.New(),
	}
}

// ClientAddressKey is the key a request is limited by: the host of
// r.RemoteAddr, the peer that actually connected -- never X-Forwarded-For
// or any other header a client can write (trusting a proxy's header needs
// a deployment decision about the proxy chain, not made yet) -- with an
// IPv6 address reduced to its /48, the block one site is usually
// assigned: a party holding a /48 holds its 65,536 /64s too, and keyed
// any finer it would spend a bucket on each.
//
// An IPv6 address that carries an IPv4 client's address is keyed as that
// IPv4 address (embeddedIPv4), exactly like an IPv4-mapped one: reduced
// to its /48, every IPv4 client behind one translator would share a
// single bucket, whatever its network. Only the forms recognizable from
// the address alone are: a translator using a network-specific prefix
// (RFC 6052 section 2.3) cannot be told from any other IPv6 network, so
// the IPv4 clients behind it share that prefix's /48 -- the same caveat as
// a proxy that hides client addresses.
func ClientAddressKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.Unmap().WithZone("")
	if v4, ok := embeddedIPv4(a); ok {
		return v4.String()
	}
	if a.Is6() {
		if p, err := a.Prefix(48); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// The IPv6 forms whose last 32 bits are an IPv4 client's address
// (embeddedIPv4).
var (
	// nat64WellKnownPrefix is RFC 6052's well-known prefix: a NAT64 or
	// SIIT translator presents the IPv4 peer a.b.c.d as 64:ff9b::a.b.c.d.
	nat64WellKnownPrefix = netip.MustParsePrefix("64:ff9b::/96")
	// ipv4CompatiblePrefix is RFC 4291's deprecated IPv4-compatible
	// form, ::a.b.c.d.
	ipv4CompatiblePrefix = netip.MustParsePrefix("::/96")
	// teredoPrefix is RFC 4380's: a Teredo client's address ends with its
	// public IPv4 address, every bit inverted.
	teredoPrefix = netip.MustParsePrefix("2001::/32")
)

// embeddedIPv4 is the IPv4 client address an IPv6 address carries, when
// it is one of the forms above. :: and ::1 (unspecified, loopback) are
// not IPv4-compatible addresses, nor is anything else in 0.0.0.0/8.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	v4 := [4]byte{b[12], b[13], b[14], b[15]}
	switch {
	case nat64WellKnownPrefix.Contains(a):
		return netip.AddrFrom4(v4), true
	case ipv4CompatiblePrefix.Contains(a) && v4[0] != 0:
		return netip.AddrFrom4(v4), true
	case teredoPrefix.Contains(a):
		return netip.AddrFrom4([4]byte{^v4[0], ^v4[1], ^v4[2], ^v4[3]}), true
	}
	return netip.Addr{}, false
}

// Allow takes one request from key's bucket. When it refuses, retryAfter
// is how long until the bucket holds a request again. The table forgets
// the key seen least recently past maxTrackedKeys.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var bucket *rate.Limiter
	if el, tracked := l.buckets[key]; tracked {
		l.recency.MoveToFront(el)
		bucket = el.Value.(*trackedBucket).limiter
	} else {
		if l.recency.Len() >= maxTrackedKeys {
			oldest := l.recency.Back()
			l.recency.Remove(oldest)
			delete(l.buckets, oldest.Value.(*trackedBucket).key)
		}
		bucket = rate.NewLimiter(l.limit, l.burst)
		l.buckets[key] = l.recency.PushFront(&trackedBucket{key: key, limiter: bucket})
	}
	res := bucket.ReserveN(now, 1)
	if !res.OK() {
		return false, l.interval
	}
	if delay := res.DelayFrom(now); delay > 0 {
		res.CancelAt(now)
		return false, delay
	}
	return true, 0
}

// KeyFunc names the bucket a request draws from, and the attributes a
// refusal of it is logged with -- never a credential, a query or a body.
// ok false lets the request through unbraked: it is not one the key
// applies to.
type KeyFunc func(r *http.Request) (key string, logAttrs []any, ok bool)

// ByClientAddress is the authorization server's key: the client network
// (ClientAddressKey), logged as client_address.
func ByClientAddress(r *http.Request) (string, []any, bool) {
	key := ClientAddressKey(r)
	return key, []any{"client_address", key}, true
}

// GrantKey is POST /mcp's key (technical plan §43.6): the MCP grant
// auth.RequireMCPBearer attached to the request, so each user's approval of
// each client has a bucket of its own -- one runaway client spends only its
// own, and a network full of users is never braked as one. It is logged as
// grant_id and client_id, never the bearer token, which that gate has
// already stripped. A request with no grant is unreachable behind that gate;
// it is let through, unbraked, to the MCP handler, which answers it with no
// tools at all (mcp.defectServer).
func GrantKey(r *http.Request) (string, []any, bool) {
	grant, ok := platform.MCPGrantFromContext(r.Context())
	if !ok {
		return "", nil, false
	}
	return grant.GrantID, []any{"grant_id", grant.GrantID, "client_id", grant.ClientID}, true
}

// LimitBy is the middleware: a request whose key (key) has an empty bucket
// is answered by refuse, with how long until it may retry, and never
// reaches next. Each refusal is logged at WARN with the route's path and
// the key's own attributes, and nothing else: never the query (an
// authorization request's state or challenge), the body (a code, a refresh
// token, a tool call's arguments) or a credential, none of which the
// limiter reads. A refusal is never audited (technical plan §43.18).
func (l *RateLimiter) LimitBy(key KeyFunc, refuse func(w http.ResponseWriter, r *http.Request, retryAfter time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k, attrs, applies := key(r)
			if !applies {
				next.ServeHTTP(w, r)
				return
			}
			if ok, retryAfter := l.Allow(k); !ok {
				platform.Logger(r.Context()).Warn("mcpauth: rate limited", append([]any{"path", r.URL.Path}, attrs...)...)
				refuse(w, r, retryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// setRetryAfter sets Retry-After to retryAfter in whole seconds, rounded
// up, and never below one.
func setRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// MCPCallRateLimited is POST /mcp's answer to a grant over its budget
// (technical plan §43.6): 429 with Retry-After in whole seconds (rounded up)
// and {"error":"rate limited"}, the plain JSON error every REST refusal
// uses -- the brake runs before the JSON-RPC message is read, so there is
// no request id to answer. No tool ran and no twin was invoked.
func MCPCallRateLimited(w http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"rate limited"}`))
}

// RegisterRateLimited is POST /oauth/register's answer to a client address
// over its budget: 429 with Retry-After in whole seconds (rounded up) and
// an OAuth-shaped JSON body.
func RegisterRateLimited(w http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	writeTokenJSON(w, http.StatusTooManyRequests, tokenError{
		Error:            errTemporarilyUnavailable,
		ErrorDescription: "too many client registrations from this address; retry later",
	})
}

// TokenRateLimited is POST /oauth/token's answer to a client network over
// its budget (technical plan §43.14): 429 with Retry-After and the token
// endpoint's own JSON error shape, never cached, carrying
// temporarily_unavailable -- and never invalid_grant, which tells an OAuth
// client its credential is dead: the MCP Go SDK's client, handed
// invalid_grant by a refresh, drops its tokens and sends its user back
// through consent, while any other error only fails the one request and
// keeps the refresh token for the next. Nothing was spent: the brake runs
// before the handler, so the refresh token is not rotated and the code
// not consumed.
func TokenRateLimited(w http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	writeTokenJSON(w, http.StatusTooManyRequests, tokenError{
		Error:            errTemporarilyUnavailable,
		ErrorDescription: "too many token requests from this network; retry later",
	})
}

// AuthorizeRateLimited is GET /oauth/authorize's answer to a client network
// over its budget (technical plan §43.14): the error page, 429 with
// Retry-After -- never a redirect. The brake runs before a single
// parameter is read, so the request's redirect_uri has not been validated,
// and an unvalidated redirect is exactly what the authorization endpoint
// never sends a browser to.
func (s *Server) AuthorizeRateLimited(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	s.renderError(w, r, http.StatusTooManyRequests, "Too many requests from your network", "Too many authorization requests came from your network just now. Wait a moment, then start again from the app.")
}
