//go:build integration

// Integration tests for RunExpiredTokenCleanup (expiredcleanup.go,
// audit-remediation config/platform-hardening batch) -- gated behind the
// "integration" build tag, matching this package's own
// postgres_integration_test.go / event_artifact_wstoken_integration_test.go
// conventions (testcontainers Postgres, embedded migrations via
// golang-migrate's iofs source driver, newTestPool/createTestSession
// shared helpers from the latter file).
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestRunExpiredTokenCleanup_DeletesExpiredLeavesLive proves
// RunExpiredTokenCleanup's own periodic tick actually deletes an expired
// ws_tokens row AND an expired user_sessions row, while leaving a
// non-expired row of each untouched -- run against a real Postgres
// instance, no mocking.
func TestRunExpiredTokenCleanup_DeletesExpiredLeavesLive(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	userStore := narvipg.NewUserStore(pool)
	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "expired-cleanup-test@example.com",
		DisplayName:  "Expired Cleanup Test",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	wsTokens := narvipg.NewWSTokenStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)

	past := time.Now().Add(-1 * time.Hour).Truncate(time.Microsecond)
	future := time.Now().Add(1 * time.Hour).Truncate(time.Microsecond)

	if _, err := wsTokens.Create(ctx, sqlcgen.CreateWSTokenParams{
		SessionID: sessionID,
		UserID:    pgtype.UUID{}, // NULL -- no auth mechanism populates this yet
		TokenHash: "expired-ws-token",
		ExpiresAt: pgtype.Timestamptz{Time: past, Valid: true},
	}); err != nil {
		t.Fatalf("create expired ws token: %v", err)
	}
	if _, err := wsTokens.Create(ctx, sqlcgen.CreateWSTokenParams{
		SessionID: sessionID,
		UserID:    pgtype.UUID{},
		TokenHash: "live-ws-token",
		ExpiresAt: pgtype.Timestamptz{Time: future, Valid: true},
	}); err != nil {
		t.Fatalf("create live ws token: %v", err)
	}

	if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: "expired-user-session",
		ExpiresAt: pgtype.Timestamptz{Time: past, Valid: true},
	}); err != nil {
		t.Fatalf("create expired user session: %v", err)
	}
	if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: "live-user-session",
		ExpiresAt: pgtype.Timestamptz{Time: future, Valid: true},
	}); err != nil {
		t.Fatalf("create live user session: %v", err)
	}

	// Drive RunExpiredTokenCleanup's own loop for at least one real tick
	// via errgroup.Group.Go -- never a bare `go` statement, per §11/
	// nakedgoroutine (no test exemption for that rule, see
	// tools/lint/narvichecks/nakedgoroutine's own doc comment).
	cleanupCtx, cancel := context.WithCancel(ctx)
	var eg errgroup.Group
	eg.Go(func() error {
		err := narvipg.RunExpiredTokenCleanup(cleanupCtx, pool, 20*time.Millisecond, platform.DefaultTimeouts().MCPDynamicClientUnusedTTL)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})

	// Give the ticker time to fire at least once, then stop the loop.
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := eg.Wait(); err != nil {
		t.Fatalf("RunExpiredTokenCleanup: %v", err)
	}

	// The expired rows must be gone.
	if _, err := wsTokens.GetByHash(ctx, "expired-ws-token"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByHash(expired-ws-token) error = %v, want pgx.ErrNoRows (cleanup should have deleted it)", err)
	}
	if _, err := userSessions.GetByHash(ctx, "expired-user-session"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByHash(expired-user-session) error = %v, want pgx.ErrNoRows (cleanup should have deleted it)", err)
	}

	// The live rows must remain untouched.
	if _, err := wsTokens.GetByHash(ctx, "live-ws-token"); err != nil {
		t.Errorf("GetByHash(live-ws-token) error = %v, want nil (cleanup must leave a non-expired row alone)", err)
	}
	if _, err := userSessions.GetByHash(ctx, "live-user-session"); err != nil {
		t.Errorf("GetByHash(live-user-session) error = %v, want nil (cleanup must leave a non-expired row alone)", err)
	}
}

// TestExpiredCleanup_SweepsMCPRows proves the same tick also purges the MCP
// authorization server's expired rows (technical plan §43.16): an expired
// authorization request, code, access token, refresh token and grant are
// deleted -- the refresh tokens rotated or not: an expired rotated one is
// swept like any other, from in front of its live successor (the §43.19
// "Table growth" row) -- while a live row of each survives untouched,
// including a live, already-rotated refresh token whose expired successor
// is swept from under it (its superseded_by is cleared, the row itself
// kept so a replay of it is still recognised).
func TestExpiredCleanup_SweepsMCPRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "mcp-sweep@example.com",
		DisplayName:  "MCP Sweep",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	client, err := narvipg.NewMCPOAuthClientStore(pool).Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     "narvi_mcp_c_sweep",
		Kind:         sqlcgen.McpOauthClientKindPreregistered,
		ClientName:   "Sweep",
		RedirectUris: []string{"http://127.0.0.1/cb"},
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	// A second client for the live grant: a user holds at most one grant
	// per client (UpsertGrant), so the expired and the live grant need
	// different clients to coexist.
	liveClient, err := narvipg.NewMCPOAuthClientStore(pool).Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     "narvi_mcp_c_sweep_live",
		Kind:         sqlcgen.McpOauthClientKindPreregistered,
		ClientName:   "Sweep live",
		RedirectUris: []string{"http://127.0.0.1/cb"},
	})
	if err != nil {
		t.Fatalf("create live client: %v", err)
	}
	grants := narvipg.NewMCPOAuthGrantStore(pool)

	past := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	newRequest := func(expires pgtype.Timestamptz) pgtype.UUID {
		t.Helper()
		r, err := grants.CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
			ClientID: client.ID, RedirectUri: "http://127.0.0.1/cb", CodeChallenge: "c", CodeChallengeMethod: "S256",
			Resource: "http://127.0.0.1:9/mcp", ExpiresAt: expires,
		})
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		return r.ID
	}
	newGrant := func(clientID pgtype.UUID, expires pgtype.Timestamptz) pgtype.UUID {
		t.Helper()
		g, err := grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
			UserID: user.ID, ClientID: clientID, Scopes: []string{"mcp:read"}, Resource: "http://127.0.0.1:9/mcp", ExpiresAt: expires,
		})
		if err != nil {
			t.Fatalf("create grant: %v", err)
		}
		return g.ID
	}

	expiredRequest, liveRequest := newRequest(past), newRequest(future)
	expiredGrant, liveGrant := newGrant(client.ID, past), newGrant(liveClient.ID, future)
	for _, c := range []struct {
		hash    string
		expires pgtype.Timestamptz
	}{{"expired-code", past}, {"live-code", future}} {
		if _, err := grants.CreateAuthorizationCode(ctx, sqlcgen.CreateMCPOAuthAuthorizationCodeParams{
			GrantID: liveGrant, CodeHash: c.hash, CodeChallenge: "c", RedirectUri: "http://127.0.0.1/cb",
			Resource: "http://127.0.0.1:9/mcp", ExpiresAt: c.expires,
		}); err != nil {
			t.Fatalf("create code %s: %v", c.hash, err)
		}
	}
	for _, tok := range []struct {
		hash    string
		expires pgtype.Timestamptz
	}{{"expired-token", past}, {"live-token", future}} {
		if _, err := grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
			GrantID: liveGrant, TokenHash: tok.hash, ExpiresAt: tok.expires,
		}); err != nil {
			t.Fatalf("create token %s: %v", tok.hash, err)
		}
	}
	for _, rt := range []struct {
		hash    string
		expires pgtype.Timestamptz
	}{{"expired-refresh", past}, {"live-refresh", future}, {"live-rotated-refresh", future}, {"expired-rotated-refresh", past}} {
		if _, err := grants.CreateRefreshToken(ctx, sqlcgen.CreateMCPOAuthRefreshTokenParams{
			GrantID: liveGrant, TokenHash: rt.hash, Resource: "http://127.0.0.1:9/mcp", ExpiresAt: rt.expires, ChainExpiresAt: rt.expires,
		}); err != nil {
			t.Fatalf("create refresh token %s: %v", rt.hash, err)
		}
	}
	// rotate rotates the refresh token hashed from into the one hashed to.
	rotate := func(from, to string) {
		t.Helper()
		f, err := grants.GetRefreshTokenByHash(ctx, from)
		if err != nil {
			t.Fatal(err)
		}
		successor, err := grants.GetRefreshTokenByHash(ctx, to)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := grants.RotateRefreshToken(ctx, f.ID, successor.ID); err != nil {
			t.Fatalf("rotate %s into %s: %v", from, to, err)
		}
	}
	rotate("live-rotated-refresh", "expired-refresh")
	rotate("expired-rotated-refresh", "live-refresh")

	cleanupCtx, cancel := context.WithCancel(ctx)
	var eg errgroup.Group
	eg.Go(func() error {
		err := narvipg.RunExpiredTokenCleanup(cleanupCtx, pool, 20*time.Millisecond, platform.DefaultTimeouts().MCPDynamicClientUnusedTTL)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := eg.Wait(); err != nil {
		t.Fatalf("RunExpiredTokenCleanup: %v", err)
	}

	if _, err := grants.GetAuthorizationRequest(ctx, expiredRequest); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expired authorization request: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := grants.GetAuthorizationRequest(ctx, liveRequest); err != nil {
		t.Errorf("live authorization request: err = %v, want nil", err)
	}
	if _, err := grants.GetGrant(ctx, expiredGrant); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expired grant: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := grants.GetGrant(ctx, liveGrant); err != nil {
		t.Errorf("live grant: err = %v, want nil", err)
	}
	if _, err := grants.GetAuthorizationCodeByHash(ctx, "expired-code"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expired code: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := grants.GetAuthorizationCodeByHash(ctx, "live-code"); err != nil {
		t.Errorf("live code: err = %v, want nil", err)
	}
	if _, err := grants.LookupAccessToken(ctx, "expired-token"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expired token: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := grants.LookupAccessToken(ctx, "live-token"); err != nil {
		t.Errorf("live token: err = %v, want nil", err)
	}
	if _, err := grants.GetRefreshTokenByHash(ctx, "expired-refresh"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expired refresh token: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := grants.GetRefreshTokenByHash(ctx, "expired-rotated-refresh"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expired rotated refresh token: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := grants.GetRefreshTokenByHash(ctx, "live-refresh"); err != nil {
		t.Errorf("live refresh token: err = %v, want nil", err)
	}
	if row, err := grants.GetRefreshTokenByHash(ctx, "live-rotated-refresh"); err != nil || !row.RotatedAt.Valid || row.SupersededBy.Valid {
		t.Errorf("live rotated refresh token = %+v (err %v), want kept, still rotated, superseded_by cleared", row, err)
	}
}

// TestExpiredCleanup_SweepsUnusedMCPClients proves the sweep's
// unused-client pass (technical plan §43.15): a dynamically registered or
// metadata-document client with no grant and no authorization request,
// last used more than MCPDynamicClientUnusedTTL ago -- registered, last
// fetched, or last usable from its cache -- is deleted, including one
// whose only grant expired on the same tick; a younger one, one with a
// live grant, one with a pending request, a metadata-document client
// fetched again recently, one fetched long ago but still served from its
// cache through a failed re-fetch's grace (on a deployment whose cache
// lifetime is long enough to allow that), a disabled client of either
// kind however old (the disabled row IS the operator's block), and every
// pre-registered client, however old and unused, survive.
func TestExpiredCleanup_SweepsUnusedMCPClients(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	clients := narvipg.NewMCPOAuthClientStore(pool)
	grants := narvipg.NewMCPOAuthGrantStore(pool)
	ttl := platform.DefaultTimeouts().MCPDynamicClientUnusedTTL
	old := time.Now().Add(-ttl - 2*time.Hour)
	recent := time.Now().Add(-time.Hour)

	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "mcp-client-sweep@example.com", DisplayName: "MCP Client Sweep", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	newClient := func(clientID string, kind sqlcgen.McpOauthClientKind, createdAt time.Time) sqlcgen.McpOauthClient {
		t.Helper()
		c, err := clients.Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
			ClientID: clientID, Kind: kind, ClientName: clientID, RedirectUris: []string{"http://127.0.0.1/cb"},
		})
		if err != nil {
			t.Fatalf("create client %s: %v", clientID, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE mcp_oauth_clients SET created_at = $2 WHERE id = $1`, c.ID, createdAt); err != nil {
			t.Fatal(err)
		}
		return c
	}
	newDocument := func(clientID string, createdAt, fetchedAt time.Time) sqlcgen.McpOauthClient {
		t.Helper()
		c, err := clients.UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
			ClientID: clientID, ClientName: "Doc", RedirectUris: []string{"http://127.0.0.1/cb"},
			MetadataFetchedAt: pgtype.Timestamptz{Time: fetchedAt, Valid: true},
			MetadataStaleAt:   pgtype.Timestamptz{Time: fetchedAt.Add(time.Hour), Valid: true},
		})
		if err != nil {
			t.Fatalf("upsert metadata-document client %s: %v", clientID, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE mcp_oauth_clients SET created_at = $2 WHERE id = $1`, c.ID, createdAt); err != nil {
			t.Fatal(err)
		}
		return c
	}
	grant := func(clientID pgtype.UUID, expiresAt time.Time) {
		t.Helper()
		if _, err := grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
			UserID: user.ID, ClientID: clientID, Scopes: []string{"mcp:read"}, Resource: "http://127.0.0.1:9/mcp",
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		}); err != nil {
			t.Fatalf("create grant: %v", err)
		}
	}

	swept := []sqlcgen.McpOauthClient{
		newClient("narvi_mcp_d_unused", sqlcgen.McpOauthClientKindDynamic, old),
		newDocument("https://unused.example/client.json", old, old),
	}
	lapsed := newClient("narvi_mcp_d_lapsed", sqlcgen.McpOauthClientKindDynamic, old)
	grant(lapsed.ID, time.Now().Add(-time.Minute))
	swept = append(swept, lapsed)

	young := newClient("narvi_mcp_d_young", sqlcgen.McpOauthClientKindDynamic, recent)
	granted := newClient("narvi_mcp_d_granted", sqlcgen.McpOauthClientKindDynamic, old)
	grant(granted.ID, time.Now().Add(time.Hour))
	pending := newClient("narvi_mcp_d_pending", sqlcgen.McpOauthClientKindDynamic, old)
	if _, err := grants.CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
		ClientID: pending.ID, RedirectUri: "http://127.0.0.1/cb", CodeChallenge: "c", CodeChallengeMethod: "S256",
		Resource: "http://127.0.0.1:9/mcp", ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}
	refetched := newDocument("https://refetched.example/client.json", old, recent)
	// Fetched successfully long ago, its re-fetch failing since a moment
	// ago, on a deployment whose MCPClientMetadataCacheTTL is over half
	// MCPDynamicClientUnusedTTL (a valid setting: 14 hours against 24).
	// That fetch is then less than two cache lifetimes old, so the
	// authorization endpoint still serves the cached document until the
	// end of the grace mcpauth's refetchGraceEnd gives -- the failure plus
	// one TTL, never past the last successful fetch plus two -- and the
	// client is not unused. With the default hour-long lifetime, no
	// document read this long ago gets any grace.
	const cacheTTL = 14 * time.Hour
	failedAt := time.Now()
	graceEnd := failedAt.Add(cacheTTL)
	if bound := old.Add(2 * cacheTTL); bound.Before(graceEnd) {
		graceEnd = bound
	}
	inGrace := newDocument("https://in-grace.example/client.json", old, old)
	if _, err := clients.MarkMetadataRefetchFailed(ctx, inGrace.ID, old, failedAt, graceEnd); err != nil {
		t.Fatalf("record the failed re-fetch: %v", err)
	}
	disabledDynamic := newClient("narvi_mcp_d_disabled", sqlcgen.McpOauthClientKindDynamic, old)
	disabledDocument := newDocument("https://disabled.example/client.json", old, old)
	for _, c := range []sqlcgen.McpOauthClient{disabledDynamic, disabledDocument} {
		if _, err := pool.Exec(ctx, `UPDATE mcp_oauth_clients SET disabled_at = $2 WHERE id = $1`, c.ID, old); err != nil {
			t.Fatal(err)
		}
	}
	preregistered := newClient("narvi_mcp_c_old", sqlcgen.McpOauthClientKindPreregistered, old)
	kept := []sqlcgen.McpOauthClient{young, granted, pending, refetched, inGrace, disabledDynamic, disabledDocument, preregistered}

	cleanupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var eg errgroup.Group
	eg.Go(func() error {
		err := narvipg.RunExpiredTokenCleanup(cleanupCtx, pool, 20*time.Millisecond, ttl)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	// Wait for the observed outcome, not for a fixed time: every client
	// that must go is gone (a failure deadline, not a pacing delay).
	deadline := time.Now().Add(10 * time.Second)
	for {
		remaining := 0
		for _, c := range swept {
			if _, err := clients.GetByID(ctx, c.ID); err == nil {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d unused clients still present after the sweep ran", remaining)
		}
	}
	cancel()
	if err := eg.Wait(); err != nil {
		t.Fatalf("RunExpiredTokenCleanup: %v", err)
	}
	for _, c := range kept {
		if _, err := clients.GetByID(ctx, c.ID); err != nil {
			t.Errorf("client %s was swept (err %v), want kept", c.ClientID, err)
		}
	}
}
