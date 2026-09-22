package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Small, fast timeouts scoped to THESE tests only -- see liveness_test.go's
// own doc comment on livenessSSEInactivityTimeout for why a short,
// fake-server-backed value is both safe and desirable here.
const (
	clientRequestTimeout = 100 * time.Millisecond
	// clientSlowHandlerDelay sleeps well past clientRequestTimeout, so a
	// correctly-implemented per-request timeout fires long before this
	// delay elapses; if it did NOT fire, the test would instead take this
	// full duration (still bounded, not infinite, but clearly distinguishable
	// from "fired quickly").
	clientSlowHandlerDelay = 2 * time.Second
)

// TestDoJSON_PerRequestTimeoutFires proves Finding 3's own fix: doJSON now
// wraps ctx in a context.WithTimeout(a.requestTimeout) before ever building
// the request, so a deliberately-slow handler (sleeping well past
// requestTimeout before responding) makes the call fail within roughly
// requestTimeout -- NOT the full handler delay.
//
// The fake handler below only sleeps for the specific "/slow" path this
// test's own doJSON call hits; every other path (in particular, the
// adapter's own background persistent GET /event connection, started
// immediately by New) responds immediately, and the "/slow" handler itself
// respects r.Context().Done() -- both deliberately, so this test (and its
// own deferred srv.Close()/a.Close() teardown, which otherwise blocks on
// any still-in-flight handler) completes quickly rather than tying up the
// full clientSlowHandlerDelay regardless of the timeout fix under test.
func TestDoJSON_PerRequestTimeoutFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case <-time.After(clientSlowHandlerDelay):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			// The client gave up (doJSON's own new per-request timeout, or
			// this test's own teardown) -- a well-behaved real handler
			// would stop working too; this fake one does the same.
		}
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, clientRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	start := time.Now()
	err := a.doJSON(context.Background(), http.MethodGet, "/slow", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("doJSON() error = nil, want a timeout error from the deliberately-slow handler")
	}
	// Generous upper bound: comfortably above clientRequestTimeout (allows
	// scheduling jitter under -race) but far below clientSlowHandlerDelay --
	// proving the call returned because of the NEW per-request timeout, not
	// because the slow handler itself eventually responded.
	if want := 10 * clientRequestTimeout; elapsed > want {
		t.Errorf("doJSON() took %v to fail, want under %v (clientSlowHandlerDelay is %v -- "+
			"the per-request timeout must fire well before the handler itself would ever respond)",
			elapsed, want, clientSlowHandlerDelay)
	}
}

// TestConnectAndConsume_UnaffectedByRequestTimeout proves the OTHER half of
// Finding 3's own constraint: a.requestTimeout must NEVER be applied as a
// client-wide http.Client.Timeout, because connectAndConsume's own GET
// /event call (sse.go) uses this SAME a.httpClient for the intentionally
// long-lived persistent SSE stream. A fake /event handler that stays open
// and keeps writing well past a.requestTimeout must NOT be torn down.
func TestConnectAndConsume_UnaffectedByRequestTimeout(t *testing.T) {
	const streamDuration = 3 * clientRequestTimeout // several multiples of requestTimeout

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		deadline := time.Now().Add(streamDuration)
		ticker := time.NewTicker(clientRequestTimeout / 4)
		defer ticker.Stop()
		for time.Now().Before(deadline) {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(sseDataPrefix + `{"type":"server.heartbeat","properties":{}}` + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, clientRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	ctx, cancel := context.WithTimeout(context.Background(), streamDuration+testWait)
	defer cancel()

	start := time.Now()
	err := a.connectAndConsume(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("connectAndConsume() error = nil, want the fake server's own handler to eventually return (ending the stream) -- this is not itself the property under test")
	}
	if elapsed < streamDuration {
		t.Errorf("connectAndConsume() returned after only %v, want it to run for roughly the fake server's own %v stream duration "+
			"(several multiples of a.requestTimeout, %v) -- a per-request timeout must never cut off this persistent connection",
			elapsed, streamDuration, clientRequestTimeout)
	}
}
