package mcpauth

import (
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

// maxTrackedAddresses bounds how many client addresses one RateLimiter
// keeps a bucket for, so a spray of source addresses cannot grow its
// memory without limit. Past it, buckets that have refilled completely
// (their address has been idle long enough to be forgotten) are dropped;
// if none has, a new address is refused until one does -- failing closed,
// since this limiter is a brake on an unauthenticated write.
const maxTrackedAddresses = 10_000

// RateLimiter is a per-client-address token bucket (technical plan §43.15;
// golang.org/x/time/rate): each address may make burst requests at once,
// then one per interval. It lives in memory, one per replica -- a brake on
// abuse (table growth, spam), not a correctness property, so it creates no
// second authority over any state (§5.1). It is built for every
// unauthenticated MCP authorization-server route that needs one: POST
// /oauth/register uses it now.
type RateLimiter struct {
	interval time.Duration
	limit    rate.Limit
	burst    int
	now      func() time.Time

	mu        sync.Mutex
	buckets   map[string]*rate.Limiter
	lastPrune time.Time
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
		buckets:  map[string]*rate.Limiter{},
	}
}

// ClientAddressKey is the key a request is limited by: the host of
// r.RemoteAddr, the peer that actually connected -- never X-Forwarded-For
// or any other header a client can write (trusting a proxy's header needs
// a deployment decision about the proxy chain, not made yet) -- with an
// IPv6 address reduced to its /64, the smallest block one party usually
// holds.
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
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// Allow takes one request from key's bucket. When it refuses, retryAfter
// is how long until the bucket holds a request again.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, tracked := l.buckets[key]
	if !tracked {
		if len(l.buckets) >= maxTrackedAddresses {
			l.prune(now)
		}
		if len(l.buckets) >= maxTrackedAddresses {
			return false, l.interval
		}
		bucket = rate.NewLimiter(l.limit, l.burst)
		l.buckets[key] = bucket
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

// prune drops every bucket that has refilled completely -- an address idle
// for burst intervals, indistinguishable from one never seen -- at most
// once per interval.
func (l *RateLimiter) prune(now time.Time) {
	if !l.lastPrune.IsZero() && now.Sub(l.lastPrune) < l.interval {
		return
	}
	l.lastPrune = now
	for key, bucket := range l.buckets {
		if bucket.TokensAt(now) >= float64(l.burst) {
			delete(l.buckets, key)
		}
	}
}

// Limit is the middleware: a request whose address (ClientAddressKey) has
// an empty bucket is answered by refuse, with how long until it may retry,
// and never reaches next.
func (l *RateLimiter) Limit(refuse func(w http.ResponseWriter, r *http.Request, retryAfter time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retryAfter := l.Allow(ClientAddressKey(r)); !ok {
				platform.Logger(r.Context()).Warn("mcpauth: rate limited", "path", r.URL.Path, "client_address", ClientAddressKey(r))
				refuse(w, r, retryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RegisterRateLimited is POST /oauth/register's answer to a client address
// over its budget: 429 with Retry-After in whole seconds (rounded up) and
// an OAuth-shaped JSON body.
func RegisterRateLimited(w http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeTokenJSON(w, http.StatusTooManyRequests, tokenError{
		Error:            "temporarily_unavailable",
		ErrorDescription: "too many client registrations from this address; retry later",
	})
}
