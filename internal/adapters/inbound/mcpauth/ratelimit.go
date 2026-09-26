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

// maxTrackedAddresses bounds how many client networks (ClientAddressKey)
// one RateLimiter keeps a bucket for, so a spray of source addresses
// cannot grow its memory without limit. Past it, the network seen least
// recently is forgotten to make room (its next request starts a fresh
// bucket) -- a newcomer is never refused because the table is full.
const maxTrackedAddresses = 10_000

// RateLimiter is a per-client-network token bucket (technical plan
// §43.14/§43.15; golang.org/x/time/rate): each network may make burst
// requests at once, then one per interval. It lives in memory, one per
// replica -- a brake on abuse (table growth, spam), not a correctness
// property, so it creates no second authority over any state (§5.1). One
// is built per route, each with its own interval, burst and table: POST
// /oauth/register (RegisterRateLimited), POST /oauth/token
// (TokenRateLimited) and GET /oauth/authorize (Server.AuthorizeRateLimited).
// It runs before the route's handler, so a refused request reads no body
// and spends nothing -- a refresh token it refuses is not rotated, and a
// code it refuses is not consumed.
//
// Its memory is bounded at maxTrackedAddresses buckets, and no one
// network can use that bound to lock out another: a flood from a single
// network -- one IPv4 address (arriving as IPv4, or through a translator
// ClientAddressKey recognizes), or one IPv6 /48 however many addresses it
// sprays from -- lands in that network's one bucket, and a flood from
// more networks than the table holds only evicts the buckets seen least
// recently, so a client from any other network -- registering, refreshing
// its token, or starting an authorization -- always gets a bucket of its
// own. The price is stated, not hidden: a party rotating through
// more networks than the table holds gets each one's burst afresh. A
// per-address brake is beaten by enough addresses whatever it does when
// full; refusing every newcomer when full only turned that into a lockout
// of everyone else.
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
// is how long until the bucket holds a request again.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var bucket *rate.Limiter
	if el, tracked := l.buckets[key]; tracked {
		l.recency.MoveToFront(el)
		bucket = el.Value.(*trackedBucket).limiter
	} else {
		if l.recency.Len() >= maxTrackedAddresses {
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

// Limit is the middleware: a request whose address (ClientAddressKey) has
// an empty bucket is answered by refuse, with how long until it may retry,
// and never reaches next. Each refusal is logged at WARN with the route's
// path and the client network, and nothing else: never the query (an
// authorization request's state or challenge) nor the body (a code, a
// refresh token), which the limiter never reads.
func (l *RateLimiter) Limit(refuse func(w http.ResponseWriter, r *http.Request, retryAfter time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := ClientAddressKey(r)
			if ok, retryAfter := l.Allow(key); !ok {
				platform.Logger(r.Context()).Warn("mcpauth: rate limited", "path", r.URL.Path, "client_address", key)
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
