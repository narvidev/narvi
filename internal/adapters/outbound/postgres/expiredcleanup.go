// This file (expiredcleanup.go) implements RunExpiredTokenCleanup: a
// small, standalone periodic loop that purges expired ws_tokens/
// user_sessions rows (audit-remediation, config/platform-hardening
// batch) and the MCP authorization server's own expired rows (technical
// plan §43.16: authorization requests, codes, access tokens, refresh
// tokens, grants). Every one of these tables
// (migrations/000016_ws_tokens.up.sql, migrations/000017_auth_v1.up.sql,
// migrations/000141_mcp_oauth.up.sql,
// migrations/000142_mcp_oauth_refresh_tokens.up.sql) has an expires_at TIMESTAMPTZ NOT NULL column that is checked only at
// read/verify time -- nothing else ever DELETEs an expired row, so left
// alone table growth is unbounded.
//
// Deliberately NOT a new "reconciler" subsystem: this mirrors
// internal/app/sessionactor/timerpump.go's own RunTimerPump precedent
// exactly -- a small, focused, ticker-driven loop is already this
// codebase's established shape for "periodically do a bounded piece of
// background work."

package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// RunExpiredTokenCleanup runs the process-wide expired-credential cleanup
// loop until ctx is done, ticking every interval (callers pass
// platform.Timeouts.ExpiredCredentialCleanupInterval). On each tick it
// deletes every ws_tokens/user_sessions row and every expired MCP
// authorization-server row (cleanupExpiredCredentialsOnce lists them)
// whose expires_at has already passed, logging the deleted row counts at
// Info level for observability.
// A single tick's failure is logged, never propagated -- exactly like
// RunTimerPump's own per-tick error handling -- so one bad tick never
// takes down the whole loop.
//
// The caller is expected to start this via its own errgroup.Go (§11: no
// naked `go` statements) exactly once per process, independent of any
// session actor.
func RunExpiredTokenCleanup(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := cleanupExpiredCredentialsOnce(ctx, pool); err != nil {
				platform.Logger(ctx).Error("postgres: expired credential cleanup tick failed", "error", err)
			}
		}
	}
}

// cleanupExpiredCredentialsOnce runs exactly one cleanup tick: deletes
// every expired ws_tokens row, then every expired user_sessions row, then
// every expired MCP authorization request, authorization code, access
// token, refresh token and grant (independent statements -- deliberately not one shared
// transaction, since no table's cleanup depends on another's outcome, and
// a failure in one must not roll back an already-successful delete in
// another), logging every deleted row count together. Unexported: PumpOnce's own
// "exported so tests can drive exactly one tick deterministically"
// precedent isn't needed here since the integration test drives cleanup
// through RunExpiredTokenCleanup's own loop instead (see
// expiredcleanup_integration_test.go).
func cleanupExpiredCredentialsOnce(ctx context.Context, pool *pgxpool.Pool) error {
	q := sqlcgen.New(pool)

	wsTokensDeleted, err := q.DeleteExpiredWSTokens(ctx)
	if err != nil {
		return err
	}

	userSessionsDeleted, err := q.DeleteExpiredUserSessions(ctx)
	if err != nil {
		return err
	}

	// The MCP authorization server's own rows (technical plan §43.16,
	// migrations/000141_mcp_oauth.up.sql and
	// 000142_mcp_oauth_refresh_tokens.up.sql) -- every one of them carries
	// an expires_at that is otherwise only checked at read time. A rotated
	// refresh token is kept until its own expiry (so a replay of it is
	// still recognised) and swept like any other. Grants go last: deleting
	// an expired grant cascades whatever codes and tokens it still has, so
	// sweeping those first only keeps the counts honest.
	mcpRequestsDeleted, err := q.DeleteExpiredMCPOAuthAuthorizationRequests(ctx)
	if err != nil {
		return err
	}
	mcpCodesDeleted, err := q.DeleteExpiredMCPOAuthAuthorizationCodes(ctx)
	if err != nil {
		return err
	}
	mcpTokensDeleted, err := q.DeleteExpiredMCPOAuthAccessTokens(ctx)
	if err != nil {
		return err
	}
	mcpRefreshTokensDeleted, err := q.DeleteExpiredMCPOAuthRefreshTokens(ctx)
	if err != nil {
		return err
	}
	mcpGrantsDeleted, err := q.DeleteExpiredMCPOAuthGrants(ctx)
	if err != nil {
		return err
	}

	platform.Logger(ctx).Info("postgres: expired credential cleanup",
		"ws_tokens_deleted", wsTokensDeleted,
		"user_sessions_deleted", userSessionsDeleted,
		"mcp_authorization_requests_deleted", mcpRequestsDeleted,
		"mcp_authorization_codes_deleted", mcpCodesDeleted,
		"mcp_access_tokens_deleted", mcpTokensDeleted,
		"mcp_refresh_tokens_deleted", mcpRefreshTokensDeleted,
		"mcp_grants_deleted", mcpGrantsDeleted,
	)
	return nil
}
