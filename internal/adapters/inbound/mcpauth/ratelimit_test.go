package mcpauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeClock is a RateLimiter's own clock, advanced by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestLimiter(interval time.Duration, burst int) (*RateLimiter, *fakeClock) {
	l := NewRateLimiter(interval, burst)
	c := &fakeClock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	l.now = c.now
	return l, c
}

// TestRateLimiter_BurstThenOnePerInterval: an address gets its burst,
// then one request per interval, told when to retry; another address is
// unaffected.
func TestRateLimiter_BurstThenOnePerInterval(t *testing.T) {
	t.Parallel()
	l, clock := newTestLimiter(12*time.Minute, 5)
	for i := range 5 {
		if ok, _ := l.Allow("198.51.100.7"); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, retry := l.Allow("198.51.100.7")
	if ok || retry != 12*time.Minute {
		t.Fatalf("request past the burst: ok %v retry %v, want refused, retry after 12m", ok, retry)
	}
	if ok, _ := l.Allow("198.51.100.8"); !ok {
		t.Fatal("another address was refused")
	}
	clock.t = clock.t.Add(11 * time.Minute)
	// x/time/rate computes in float64 seconds: the delay is a minute to
	// within a microsecond (the 429's Retry-After rounds it up anyway).
	if ok, retry := l.Allow("198.51.100.7"); ok || retry < time.Minute-time.Microsecond || retry > time.Minute {
		t.Fatalf("after 11m: ok %v retry %v, want refused, retry after 1m", ok, retry)
	}
	clock.t = clock.t.Add(time.Minute)
	if ok, _ := l.Allow("198.51.100.7"); !ok {
		t.Fatal("after one full interval: refused, want one more request")
	}
	if ok, _ := l.Allow("198.51.100.7"); ok {
		t.Fatal("a second request in the same interval was allowed")
	}
}

// TestClientAddressKey: RemoteAddr only -- a forwarded header never
// changes the key -- IPv4-mapped addresses unmapped, IPv6 by its /64.
func TestClientAddressKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{"198.51.100.7:4321", "198.51.100.7"},
		{"[::ffff:198.51.100.7]:4321", "198.51.100.7"},
		{"[2001:db8:1:2:3:4:5:6]:443", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:ffff::9]:443", "2001:db8:1:2::/64"},
		{"[fe80::1%eth0]:443", "fe80::/64"},
		{"not-an-address", "not-an-address"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/oauth/register", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", "203.0.113.99")
		r.Header.Set("X-Real-IP", "203.0.113.98")
		if got := ClientAddressKey(r); got != tc.want {
			t.Errorf("ClientAddressKey(RemoteAddr %q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// TestRateLimiter_BoundedMemoryFailsClosed: past maxTrackedAddresses, an
// address that has refilled completely is forgotten to make room; with
// none to forget, a new address is refused -- never tracked without
// bound, never let through unbraked.
func TestRateLimiter_BoundedMemoryFailsClosed(t *testing.T) {
	t.Parallel()
	l, clock := newTestLimiter(time.Minute, 1)
	for i := range maxTrackedAddresses {
		if ok, _ := l.Allow(fmt.Sprintf("key-%d", i)); !ok {
			t.Fatalf("key %d refused while filling", i)
		}
	}
	if ok, _ := l.Allow("one-too-many"); ok {
		t.Fatal("a new address was tracked past the bound while no bucket had refilled")
	}
	clock.t = clock.t.Add(time.Minute)
	if ok, _ := l.Allow("one-too-many"); !ok {
		t.Fatal("once every bucket had refilled, a new address was still refused")
	}
	if n := len(l.buckets); n > maxTrackedAddresses {
		t.Fatalf("tracking %d addresses, want at most %d", n, maxTrackedAddresses)
	}
}

// TestRegisterRateLimited_Answer: 429, Retry-After in whole seconds
// rounded up, a JSON OAuth-shaped body, never cached.
func TestRegisterRateLimited_Answer(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	RegisterRateLimited(rec, httptest.NewRequest(http.MethodPost, "/oauth/register", nil), 90*time.Second+time.Millisecond)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "91" ||
		rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("answer: status %d headers %v", rec.Code, rec.Header())
	}
	if body := rec.Body.String(); body != `{"error":"temporarily_unavailable","error_description":"too many client registrations from this address; retry later"}`+"\n" {
		t.Fatalf("body = %q", body)
	}
}
