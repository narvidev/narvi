//go:build integration

package wshub_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// TestSandboxHandler_ActorUnavailable_503WithRetryAfter (T6) proves the
// sandbox handshake answers a replica that cannot hydrate the session's
// actor in time -- its query pool saturated for the whole
// ActorHydrateTimeout -- with 503 and Retry-After set to that bound in
// whole seconds, rounded up: a status the agent's reconnect loop already
// retries, never the 500 it reserves for a real failure. The Registry runs
// on its own one-connection pool, held by the test; the sandbox row is read
// through the shared pool, so every step before the actor's is reached.
func TestSandboxHandler_ActorUnavailable_503WithRetryAfter(t *testing.T) {
	ctx := context.Background()
	shared, connStr := IntegrationTestPoolAndConnStr(t)
	sessionID := createTestSession(ctx, t, shared)
	createTestSandbox(ctx, t, shared, sessionID)

	saturated, err := narvipg.NewPoolWithMaxConns(ctx, connStr, 1)
	if err != nil {
		t.Fatalf("NewPoolWithMaxConns: %v", err)
	}
	t.Cleanup(saturated.Close)
	held, err := saturated.Acquire(ctx)
	if err != nil {
		t.Fatalf("hold the registry pool's only connection: %v", err)
	}
	t.Cleanup(held.Release)

	timeouts := platform.DefaultTimeouts()
	timeouts.ActorHydrateTimeout = 1100 * time.Millisecond // 1.1s: Retry-After rounds up to 2
	registry, err := sessionactor.NewRegistry(ctx, saturated, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	server, wsURL := newTestServer(registry, narvipg.NewSandboxStore(shared), timeouts)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http"+wsURL[len("ws"):]+"/sessions/"+sessionID.String()+"/ws?type=sandbox", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range baseHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (the actor could not be hydrated within ActorHydrateTimeout)", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := resp.Header.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want \"2\" (ActorHydrateTimeout 1.1s in whole seconds, rounded up)", got)
	}
}
