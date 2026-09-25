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
		err := narvipg.RunExpiredTokenCleanup(cleanupCtx, pool, 20*time.Millisecond)
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
// authorization request, code, access token and grant are deleted, while
// a live row of each survives untouched.
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

	cleanupCtx, cancel := context.WithCancel(ctx)
	var eg errgroup.Group
	eg.Go(func() error {
		err := narvipg.RunExpiredTokenCleanup(cleanupCtx, pool, 20*time.Millisecond)
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
}
