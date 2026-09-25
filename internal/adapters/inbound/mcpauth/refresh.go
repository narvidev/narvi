package mcpauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// refreshGrant answers grant_type=refresh_token (technical plan §43.16):
// the presented refresh token is replaced -- rotated -- by a new one, and a
// new access token is issued beside it, both holding the presented token's
// own scopes, or the narrowing of them the request's scope parameter asks
// for (mcpscope.Covers). Never the grant's scopes: those only record the
// most recent consent, which a later approval for the same client
// overwrites, so reading them here would let a consent widen a refresh
// chain. Presenting a token that was already rotated is a replay, and so
// is presenting one to a client it was not issued to, rotated or not: both
// are handled by handleRefreshReuse.
//
// Lifetimes are checked before replays. A token past its own expiry, its
// chain's end or its grant's expiry is refused, and revokes nothing,
// rotated or not and whichever client presents it; only a token still
// inside all three can be a replay. So the answer is the same whether or
// not the expired-credential sweep has deleted the row yet -- once it has,
// the token is unknown, which revokes nothing either.
//
// The same holds for the rest of what the chain can do: its resource and
// its absolute end were fixed when the chain began (refreshChain) and are
// read from the presented token, which the rotation copies them from
// unchanged. The grant's expiry still ends the chain early if it comes
// first -- a grant past its lifetime refreshes nothing -- but the grant's
// resource is never compared, and its expiry never extends the chain.
//
// Unlike a code, a refresh token is NOT spent by a refusal: every check
// that can refuse runs before the rotation, and a refused request commits
// nothing. A client that sent a scope it may not have, or a foreign
// resource, can retry with the same token -- where spending it would turn
// that retry into a replay and revoke the client's grant.
//
// Locks follow the one order every transaction on these tables follows,
// parent before child (the top of postgres/mcpoauthgrant_store.go): the
// client this request authenticated as, then the refresh token's grant,
// both FOR KEY SHARE, and only then the refresh token itself, which the
// rotation locks. So the token is read first, without a lock. The client
// locked is the requesting one -- the grant's own client whenever tokens
// are issued, since a refresh token presented by another client is a
// replay (below). A replay found once both are locked ends this
// transaction before handleRefreshReuse starts its own.
func (s *Server) refreshGrant(w http.ResponseWriter, r *http.Request, client sqlcgen.McpOauthClient, form url.Values) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	fail := func(msg string, err error) {
		logger.Error("mcpauth: token: "+msg, "error", err)
		writeTokenError(w, errServerError, "", false)
	}

	presented := form.Get("refresh_token")
	if presented == "" {
		writeTokenError(w, errInvalidRequest, "refresh_token is required", false)
		return
	}
	// resource is optional on a refresh -- the token is already bound to
	// the resource its chain was issued for -- but one that is sent must be
	// this deployment's (RFC 8707 section 2), and so must the chain's,
	// below.
	if raw := form.Get("resource"); raw != "" && !s.ids.matchesResource(raw) {
		writeTokenError(w, errInvalidTarget, "resource must be this deployment's MCP endpoint", false)
		return
	}
	requested, narrowing, ok := s.requestedRefreshScopes(form.Get("scope"))
	if !ok {
		writeTokenError(w, errInvalidScope, "a requested scope is not offered by this deployment", false)
		return
	}

	tokenHash := platform.HashToken(presented)
	presentedRow, err := s.deps.Grants.GetRefreshTokenByHash(ctx, tokenHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		logger.Warn("mcpauth: token refused", "outcome", "unknown_refresh_token")
		writeTokenError(w, errInvalidGrant, "the refresh token is invalid", false)
		return
	case err != nil:
		fail("load refresh token failed", err)
		return
	}

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		fail("begin tx failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grants := s.deps.Grants.WithTx(tx)

	if _, err := s.deps.Clients.WithTx(tx).LockKeyShare(ctx, client.ID); errors.Is(err, pgx.ErrNoRows) {
		// Deleted since Token read it, and every token issued to it with it.
		logger.Warn("mcpauth: token refused", "outcome", "client_deleted", "client_id", client.ClientID)
		writeTokenError(w, errInvalidGrant, "the refresh token is invalid", false)
		return
	} else if err != nil {
		fail("lock client failed", err)
		return
	}
	grant, err := grants.LockGrantKeyShare(ctx, presentedRow.GrantID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Revoked since the token was read, and the token with it.
		logger.Warn("mcpauth: token refused", "outcome", "grant_revoked", "client_id", client.ClientID)
		writeTokenError(w, errInvalidGrant, "the refresh token is invalid", false)
		return
	}
	if err != nil {
		fail("lock grant failed", err)
		return
	}

	// Every refusal below returns before anything is written: the deferred
	// rollback leaves the presented token exactly as it was.
	refuse := func(errCode, description, outcome string) {
		logger.Warn("mcpauth: token refused", "outcome", outcome, "client_id", client.ClientID)
		writeTokenError(w, errCode, description, false)
	}
	now := time.Now()

	// The three lifetimes come first, before either replay check below: a
	// token outside any of them is refused, and revokes nothing, rotated or
	// not and whichever client presents it (technical plan §43.14).
	switch {
	case !presentedRow.ExpiresAt.Time.After(now):
		refuse(errInvalidGrant, "the refresh token is invalid", "refresh_token_expired")
		return
	case !presentedRow.ChainExpiresAt.Time.After(now):
		// A chain past its absolute end refreshes nothing, whatever the
		// token's own expiry says -- and however recently the user
		// consented again: a later consent renews the grant, never a chain
		// an earlier one began.
		refuse(errInvalidGrant, "the refresh token is invalid", "refresh_chain_expired")
		return
	case !grant.ExpiresAt.Time.After(now):
		// A grant past its absolute lifetime refreshes nothing, whatever
		// its refresh tokens say: the user must consent again.
		refuse(errInvalidGrant, "the refresh token is invalid", "grant_expired")
		return
	}

	if presentedRow.RotatedAt.Valid || grant.ClientID != client.ID {
		// A replay (technical plan §43.16): a token already rotated,
		// whichever client presents it, or one presented by a client it
		// was not issued to -- one that client never received, so a copy,
		// whether or not it was rotated yet. The latter is also how a
		// racing refresh from another client, which reads the token
		// unrotated while the grant's own client rotates it, still counts
		// as one use and one replay. This transaction holds the requesting
		// client and the grant FOR KEY SHARE, so it ends here, before
		// handleRefreshReuse locks the grant's client and deletes the grant
		// in a transaction of its own.
		_ = tx.Rollback(ctx)
		s.handleRefreshReuse(w, r, tokenHash, client)
		return
	}

	held := mcpscope.FromStrings(presentedRow.Scopes)
	switch {
	case presentedRow.Resource != s.ids.Resource:
		// The chain's own resource, fixed when it began -- never the
		// grant's, which a later consent rebinds in place. Only possible
		// when PublicBaseURL changed since the chain began (the code
		// exchange's own resource_mismatch case).
		refuse(errInvalidTarget, "the refresh token was issued for another resource", "resource_mismatch")
		return
	case narrowing && !mcpscope.Covers(held, requested):
		refuse(errInvalidScope, "a refresh can only narrow the scope the refresh token holds", "scope_widening")
		return
	}
	scopes := held
	if narrowing {
		scopes = requested
	}

	issued, err := s.issueTokens(ctx, grants, grant, scopes, refreshChain{
		resource:  presentedRow.Resource,
		expiresAt: presentedRow.ChainExpiresAt.Time,
	}, now)
	if err != nil {
		fail("record tokens failed", err)
		return
	}
	if _, err := grants.RotateRefreshToken(ctx, presentedRow.ID, issued.refreshID); errors.Is(err, pgx.ErrNoRows) {
		// Rotated by a concurrent refresh since it was read: this request
		// replayed it. This transaction now holds the grant and the tokens
		// it inserted, so it ends here -- undoing them -- before
		// handleRefreshReuse locks the grant's client and deletes the
		// grant in a transaction of its own.
		_ = tx.Rollback(ctx)
		s.handleRefreshReuse(w, r, tokenHash, client)
		return
	} else if err != nil {
		fail("rotate refresh token failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail("commit tokens failed", err)
		return
	}
	writeIssued(w, issued, now)
}

// requestedRefreshScopes parses a refresh request's optional scope
// parameter. Absent or blank asks for the presented refresh token's own
// scopes, unchanged (RFC 6749 section 6): narrowing is false. Otherwise
// every value must be a scope this build offers (ok false, invalid_scope,
// exactly as at the authorization endpoint), and the caller must still
// check the result against what the presented token holds.
func (s *Server) requestedRefreshScopes(raw string) (requested []mcpscope.Scope, narrowing, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false, true
	}
	requested, err := mcpscope.ParseRequested(raw, s.scopes)
	if err != nil {
		return nil, false, false
	}
	return requested, true, true
}

// handleRefreshReuse answers a replayed refresh token (OAuth 2.1 section
// 4.3.1, technical plan §43.16): one that was already rotated, whichever
// client presents it -- a client_id is public, so requiring the grant's
// own would protect nothing -- or one presented by a client other than
// the one it was issued to, rotated or not, since that client never
// received it. The grant it was issued under is deleted, so every access
// and refresh token under it stops working -- the legitimate client's
// included, which is the point: the server cannot tell the thief from the
// victim, so it ends both, and the user must consent again. There is no
// grace window. The revocation is audited (reason refresh_reuse,
// attributed to the grant's user with actor "system"). refreshGrant sends
// it only a token it found still inside its own lifetime, its chain's and
// its grant's: an expired one is refused there and revokes nothing.
//
// It runs in a transaction of its own, never the refresh's, and locks in
// the one order (the top of postgres/mcpoauthgrant_store.go): the grant's
// client FOR KEY SHARE, then the grant, whose deletion cascades. A refresh
// or code exchange still in flight under the grant holds it FOR KEY
// SHARE, so the deletion waits for it to commit and then takes what it
// issued too.
func (s *Server) handleRefreshReuse(w http.ResponseWriter, r *http.Request, tokenHash string, presenter sqlcgen.McpOauthClient) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	fail := func(msg string, err error) {
		logger.Error("mcpauth: token: "+msg, "error", err)
		writeTokenError(w, errServerError, "", false)
	}
	invalid := func(outcome string) {
		logger.Warn("mcpauth: token refused", "outcome", outcome)
		writeTokenError(w, errInvalidGrant, "the refresh token is invalid", false)
	}

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		fail("begin reuse tx failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grants := s.deps.Grants.WithTx(tx)

	row, err := grants.GetRefreshTokenByHash(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Gone since: revoked with its grant, or expired and swept.
		invalid("unknown_refresh_token")
		return
	}
	if err != nil {
		fail("load replayed refresh token failed", err)
		return
	}
	grant, err := grants.GetGrant(ctx, row.GrantID)
	if errors.Is(err, pgx.ErrNoRows) {
		invalid("grant_revoked")
		return
	}
	if err != nil {
		fail("load replayed grant failed", err)
		return
	}
	if !row.RotatedAt.Valid && grant.ClientID == presenter.ID {
		// Never reached (a token comes here only once seen rotated, and
		// rotation is never undone, or presented by another client);
		// refused without revoking, since the grant's own client presenting
		// its current token is no replay.
		invalid("refresh_token_not_rotated")
		return
	}
	client, err := s.deps.Clients.WithTx(tx).LockKeyShare(ctx, grant.ClientID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The client was deleted since, and this grant with it.
		invalid("client_deleted")
		return
	}
	if err != nil {
		fail("lock client for reuse revocation failed", err)
		return
	}
	deleted, err := grants.DeleteGrant(ctx, grant.ID)
	if err != nil {
		fail("revoke replayed grant failed", err)
		return
	}
	if deleted == 1 {
		if err := auditlog.Record(ctx, s.deps.AuditLog.WithTx(tx), grant.UserID, "mcp_authorization.revoked", "mcp_authorization", grant.ID.String(), map[string]any{
			"reason":      "refresh_reuse",
			"actor":       "system",
			"client_id":   client.ClientID,
			"client_name": client.ClientName,
		}); err != nil {
			fail("record reuse audit failed", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		fail("commit reuse revocation failed", err)
		return
	}
	logger.Warn("mcpauth: token refused; grant revoked", "outcome", "refresh_reuse", "grant_id", grant.ID.String(),
		"presented_by", presenter.ClientID, "rotated", row.RotatedAt.Valid)
	writeTokenError(w, errInvalidGrant, "the refresh token is invalid", false)
}
