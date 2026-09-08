//go:build integration

// Shared fixtures for this package's own integration tests (sandbox_test.go,
// dispatch_test.go, client_test.go) -- gated behind the "integration" build
// tag (needs Docker), matching internal/app/sessionactor/integration_helpers_test.go's
// own testcontainers-Postgres-plus-embedded-migrations convention exactly.
// newTestPool (below) no longer starts its OWN container per call -- see
// sharedpool_integration_test.go's own top doc comment for the
// container/pool this now shares with every other test in this package.
package wshub_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go), not freshly per test/container as this function
// used to do itself. Kept as a thin wrapper under its own original
// name/signature so every existing call site in this package's own
// *_integration_test.go files keeps compiling unchanged. See sharedpool_
// integration_test.go's own top doc comment for the full container-reuse
// story.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// createTestSession inserts a minimal session row and returns its id.
func createTestSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()

	created, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
	})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	return created.ID
}

// createTestSandbox inserts a sandbox row for sessionID (gen 1, Pending by
// default per migrations/000006_sandboxes.up.sql) and returns it.
func createTestSandbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) sqlcgen.Sandbox {
	t.Helper()

	created, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID)
	if err != nil {
		t.Fatalf("create test sandbox: %v", err)
	}
	return created
}

// moveSandboxStatus sets sessionID's sandbox row to status via a plain
// UpdateStatus call (no last_seen_at bump -- the zero pgtype.Timestamptz
// COALESCEs to "leave unchanged", matching UpdateSandboxStatus's own doc
// comment).
func moveSandboxStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, status sqlcgen.SandboxStatus) {
	t.Helper()

	if _, err := narvipg.NewSandboxStore(pool).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID,
		Status:    status,
	}); err != nil {
		t.Fatalf("move sandbox to %s: %v", status, err)
	}
}

// setSandboxTokenHash writes sessionID's sandbox row's token_hash directly
// via raw SQL -- no sqlc query exists for this (real token MINTING is
// §9.3+'s own job, see migrations/000015_sandbox_token_hash.up.sql's own
// doc comment); this is test-fixture setup only, proving verifySandboxToken
// actually enforces a REAL stored hash when one exists.
func setSandboxTokenHash(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, hash string) {
	t.Helper()

	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET token_hash = $1 WHERE session_id = $2`, hash, sessionID); err != nil {
		t.Fatalf("set sandbox token_hash: %v", err)
	}
}

// getSandbox re-reads sessionID's current sandbox row.
func getSandbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) sqlcgen.Sandbox {
	t.Helper()

	got, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return got
}

// countEvents returns how many rows exist in events for (sessionID,
// eventType).
func countEvents(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, eventType string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = $2`,
		sessionID, eventType,
	).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

// waitUntil polls cond every 20ms until it reports true, or fails the test
// once timeout elapses -- used to observe the eventual effect of the
// session actor's own asynchronous mailbox processing without coupling the
// test to internal timing. Mirrors internal/app/sessionactor/
// integration_helpers_test.go's own identical helper.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newTestServer builds a chi router mounting NewSandboxHandler at the real
// route path, wraps it in an httptest.Server, and returns both the server
// (for Close) and the ws:// URL prefix to dial against (session id + query
// string are the caller's own job to append).
func newTestServer(registry *sessionactor.Registry, sandboxes *narvipg.SandboxStore, timeouts platform.Timeouts) (*httptest.Server, string) {
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, sandboxes, wshub.NewSandboxRegistry(timeouts), timeouts))
	server := httptest.NewServer(router)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	return server, wsURL
}

// newDispatcherTestServer builds a chi router mounting the REAL top-level
// dispatcher (wshub.NewHandler, wiring BOTH NewSandboxHandler and
// NewClientHandler at the single shared route) -- used by client_test.go
// for the client-hub's own tests and for proving the type=sandbox vs
// type=client routing itself, as opposed to newTestServer above (which
// mounts the sandbox handler alone, still used by
// this package's pre-existing sandbox-only tests).
func newDispatcherTestServer(
	registry *sessionactor.Registry,
	sessions *narvipg.SessionStore,
	turns *narvipg.TurnStore,
	sandboxes *narvipg.SandboxStore,
	events *narvipg.EventStore,
	artifacts *narvipg.ArtifactStore,
	wsTokens *narvipg.WSTokenStore,
	users *narvipg.UserStore,
	hub *wshub.Hub,
	timeouts platform.Timeouts,
) (*httptest.Server, string) {
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewHandler(
		wshub.NewSandboxHandler(registry, sandboxes, wshub.NewSandboxRegistry(timeouts), timeouts),
		wshub.NewClientHandler(registry, sessions, turns, sandboxes, events, artifacts, wsTokens, users, hub, timeouts),
	))
	server := httptest.NewServer(router)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	return server, wsURL
}

// createTestWSToken mints a fresh plaintext ws-token via the SAME
// production helpers the real mint endpoint uses
// (platform.GenerateToken/HashToken), persists only its hash with the
// given expiresAt, and returns the plaintext -- exactly what a real
// client would have received from POST /api/sessions/:id/ws-token.
func createTestWSToken(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, expiresAt time.Time) string {
	t.Helper()

	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate test ws-token: %v", err)
	}
	if _, err := narvipg.NewWSTokenStore(pool).Create(ctx, sqlcgen.CreateWSTokenParams{
		SessionID: sessionID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		t.Fatalf("create test ws-token: %v", err)
	}
	return token
}

// createTestUser inserts a minimal, real users row (§8.11 "multiplayer
// presence" needs a real user for resolveParticipants to resolve back to
// a display name) and returns its id. email is namespaced by the caller
// to avoid colliding with any other test sharing this package's own
// shared Postgres pool (users.primary_email has no uniqueness constraint
// today, but a distinct value per test keeps failures easy to attribute).
func createTestUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, email, displayName string) pgtype.UUID {
	t.Helper()

	created, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: email,
		DisplayName:  displayName,
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	return created.ID
}

// createTestWSTokenForUser mirrors createTestWSToken exactly, except the
// minted ws_tokens row carries a real user_id -- §8.11's own "multiplayer
// presence" needs a connection to authenticate as an ACTUAL user before
// Hub.Participants/resolveParticipants have anything to report; every
// OTHER existing caller of createTestWSToken deliberately leaves user_id
// NULL (a legitimate, pre-auth-v1-shaped row, migrations/000016's own
// nullable column) and is unaffected by this addition.
func createTestWSTokenForUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, userID pgtype.UUID, expiresAt time.Time) string {
	t.Helper()

	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate test ws-token: %v", err)
	}
	if _, err := narvipg.NewWSTokenStore(pool).Create(ctx, sqlcgen.CreateWSTokenParams{
		SessionID: sessionID,
		UserID:    userID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		t.Fatalf("create test ws-token for user: %v", err)
	}
	return token
}
