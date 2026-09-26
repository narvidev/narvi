package mcpauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
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
// changes the key -- IPv4-mapped addresses unmapped, IPv6 by its /48.
func TestClientAddressKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{"198.51.100.7:4321", "198.51.100.7"},
		{"[::ffff:198.51.100.7]:4321", "198.51.100.7"},
		{"[2001:db8:1:2:3:4:5:6]:443", "2001:db8:1::/48"},
		{"[2001:db8:1:ffff:ffff::9]:443", "2001:db8:1::/48"},
		{"[2001:db8:2::1]:443", "2001:db8:2::/48"},
		{"[fe80::1%eth0]:443", "fe80::/48"},
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

// TestRateLimiter_BoundedMemoryEvictsLeastRecentlyUsed: past
// maxTrackedAddresses, the network seen least recently is forgotten to
// make room -- a newcomer is admitted, never refused for the table being
// full -- while a network seen recently keeps its bucket, and the table
// never grows past the bound.
func TestRateLimiter_BoundedMemoryEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	l, _ := newTestLimiter(time.Minute, 1)
	for i := range maxTrackedAddresses {
		if ok, _ := l.Allow(fmt.Sprintf("key-%d", i)); !ok {
			t.Fatalf("key %d refused while filling", i)
		}
	}
	// key-0 is seen again (and refused: its one request is spent), so
	// key-1 is now the least recently seen.
	if ok, _ := l.Allow("key-0"); ok {
		t.Fatal("key-0's second request in the interval was allowed")
	}
	if ok, _ := l.Allow("newcomer"); !ok {
		t.Fatal("a newcomer was refused because the table was full")
	}
	if n := len(l.buckets); n != maxTrackedAddresses || l.recency.Len() != n {
		t.Fatalf("tracking %d addresses (%d in recency order), want exactly %d", n, l.recency.Len(), maxTrackedAddresses)
	}
	if ok, _ := l.Allow("key-0"); ok {
		t.Fatal("key-0, seen recently, lost its bucket to the newcomer")
	}
	if ok, _ := l.Allow("key-1"); !ok {
		t.Fatal("key-1, the least recently seen, still had its spent bucket: it was not the one forgotten")
	}
}

// TestRateLimiter_OneNetworkCannotLockOutOthers: through the middleware,
// with the shipped interval and burst, keyed as production keys it.
// (a) A flood from one IPv6 /48, every request from a different /64,
// gets that one network's burst and no more, and a registrant from
// another network -- IPv6 or IPv4 -- still gets through. (b) A flood from
// more networks than the table holds, each coming back well inside the
// refill interval so none of their buckets ever refills, still leaves a
// registrant from yet another network getting through.
func TestRateLimiter_OneNetworkCannotLockOutOthers(t *testing.T) {
	t.Parallel()
	timeouts := platform.DefaultTimeouts()
	send := func(h http.Handler, remote string) int {
		r := httptest.NewRequest(http.MethodPost, "/oauth/register", nil)
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	newLimited := func() (http.Handler, *fakeClock) {
		l, clock := newTestLimiter(timeouts.MCPRegisterRateInterval, timeouts.MCPRegisterRateBurst)
		return l.Limit(RegisterRateLimited)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})), clock
	}
	others := []string{"[2001:db8:5000::1]:443", "198.51.100.9:443"}

	t.Run("a flood from one /48", func(t *testing.T) {
		h, _ := newLimited()
		admitted := 0
		for i := range 2 * maxTrackedAddresses {
			// 2001:db8:4000:<i>::<i>, a different /64 every time.
			if send(h, fmt.Sprintf("[2001:db8:4000:%x::%x]:443", i, i+1)) == http.StatusCreated {
				admitted++
			}
		}
		if admitted != timeouts.MCPRegisterRateBurst {
			t.Fatalf("the /48's flood got %d registrations through, want its one burst of %d", admitted, timeouts.MCPRegisterRateBurst)
		}
		for _, remote := range others {
			if code := send(h, remote); code != http.StatusCreated {
				t.Errorf("a registrant from %s after the flood: status %d, want 201", remote, code)
			}
		}
	})

	t.Run("a flood from more networks than the table holds", func(t *testing.T) {
		h, clock := newLimited()
		step := timeouts.MCPRegisterRateInterval / 4
		for round := range 3 {
			for i := range maxTrackedAddresses + maxTrackedAddresses/2 {
				// One request per /48 -- 2001:db8:<i>::/48 -- each network
				// back within a quarter of the refill interval. (The
				// registrants below are in 2001:db8:5000::/48, which this
				// range never reaches.)
				_ = send(h, fmt.Sprintf("[2001:db8:%x::1]:443", i))
			}
			clock.t = clock.t.Add(step)
			for _, remote := range others {
				if code := send(h, strings.Replace(remote, "::1]", fmt.Sprintf("::%x]", round+1), 1)); code != http.StatusCreated {
					t.Errorf("round %d: a registrant from %s: status %d, want 201", round, remote, code)
				}
			}
		}
	})
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

// TestTokenRateLimited_Answer: POST /oauth/token's refusal is 429 with
// Retry-After in whole seconds rounded up, never cached, in the token
// endpoint's own JSON error shape, and its code is temporarily_unavailable --
// never invalid_grant, which an OAuth client reads as "this refresh token is
// dead" (the MCP Go SDK's client then drops it and sends its user back
// through consent).
func TestTokenRateLimited_Answer(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	TokenRateLimited(rec, httptest.NewRequest(http.MethodPost, "/oauth/token", nil), 1500*time.Millisecond)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" ||
		rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Location") != "" {
		t.Fatalf("answer: status %d headers %v", rec.Code, rec.Header())
	}
	if body := rec.Body.String(); body != `{"error":"temporarily_unavailable","error_description":"too many token requests from this network; retry later"}`+"\n" {
		t.Fatalf("body = %q", body)
	}
}

// TestAuthorizeRateLimited_Answer: GET /oauth/authorize's refusal is the
// error page -- 429, Retry-After, the page headers every page here carries
// -- and never a redirect, even for a request naming a redirect_uri: the
// brake runs before anything validated it.
func TestAuthorizeRateLimited_Answer(t *testing.T) {
	t.Parallel()
	s, err := New(Config{PublicBaseURL: "https://narvi.example", Enabled: true, Scopes: []mcpscope.Scope{mcpscope.Read}, Timeouts: platform.DefaultTimeouts()}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+url.Values{
		"client_id": {"narvi_mcp_c_x"}, "redirect_uri": {"https://tools.example/cb"}, "response_type": {"code"}, "state": {"s"},
	}.Encode(), nil)
	rec := httptest.NewRecorder()
	s.AuthorizeRateLimited(rec, req, 2*time.Second+time.Millisecond)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "3" || rec.Header().Get("Location") != "" {
		t.Fatalf("answer: status %d Retry-After %q Location %q, want 429, 3, none", rec.Code, rec.Header().Get("Retry-After"), rec.Header().Get("Location"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("page headers: %v", rec.Header())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Too many requests from your network") || strings.Contains(body, "tools.example") {
		t.Fatalf("page body = %s", body)
	}
}

// TestRateLimiter_RefusalLogCarriesNoCredential: a refused request is
// logged at WARN with its path and client network, and with nothing the
// request carried -- neither its query (an authorization request's state
// and PKCE challenge) nor its body (a code, a refresh token), which the
// brake never reads. Not parallel: it swaps the default logger, and puts
// back the standard log package's output and flags too (slog.SetDefault
// points that package at the handler it installs).
func TestRateLimiter_RefusalLogCarriesNoCredential(t *testing.T) {
	var buf bytes.Buffer
	prev, prevOutput, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	const refresh, state, challenge = "narvi_mcp_rt_secretsecretsecret", "state-secret-value", "challenge-secret-value"
	l, _ := newTestLimiter(time.Minute, 1)
	var reached int
	h := l.Limit(TokenRateLimited)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusBadRequest)
	}))
	send := func() int {
		r := httptest.NewRequest(http.MethodPost, "/oauth/token?state="+state+"&code_challenge="+challenge,
			strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "[2001:db8:77:1::5]:4000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := send(); code != http.StatusBadRequest {
		t.Fatalf("first request: status %d, want it to reach the handler", code)
	}
	if code := send(); code != http.StatusTooManyRequests || reached != 1 {
		t.Fatalf("second request: status %d (handler reached %d times), want 429 and the handler never reached", code, reached)
	}
	line := strings.TrimSpace(buf.String())
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil || strings.Count(line, "\n") != 0 {
		t.Fatalf("log = %q, want exactly one JSON line", line)
	}
	if entry["level"] != "WARN" || entry["msg"] != "mcpauth: rate limited" || entry["path"] != "/oauth/token" || entry["client_address"] != "2001:db8:77::/48" {
		t.Fatalf("log entry = %v, want a WARN naming the path and the client network", entry)
	}
	for _, secret := range []string{refresh, state, challenge, "refresh_token", "state="} {
		if strings.Contains(line, secret) {
			t.Fatalf("the refusal log carries %q: %s", secret, line)
		}
	}
}
