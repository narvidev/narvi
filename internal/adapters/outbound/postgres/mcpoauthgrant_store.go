// Lock order for the MCP authorization server's tables (technical plan
// §43.16, migrations/000141_mcp_oauth.up.sql and
// 000142_mcp_oauth_refresh_tokens.up.sql). Every transaction that touches
// them takes its row locks parent before child, in this one order:
//
//	mcp_oauth_clients
//	  -> mcp_oauth_grants
//	       -> mcp_oauth_authorization_requests (a client's children),
//	          mcp_oauth_authorization_codes, mcp_oauth_access_tokens,
//	          mcp_oauth_refresh_tokens (a grant's)
//
// Every row lock counts, however it is taken: SELECT ... FOR UPDATE or FOR
// KEY SHARE, an UPDATE or DELETE of the row, the FOR KEY SHARE an insert's
// foreign-key check takes on its parent, and an ON DELETE CASCADE reaching
// a child. A plain read locks nothing and may come in any order.
//
// Revocation locks downwards by construction: deleting a client
// (MCPOAuthClientStore.Lock first) or a grant locks that row, then its
// cascade locks the rows under it. So every transaction that issues
// something locks its parents FIRST, explicitly, before it consumes the
// child that gates the issuance -- the consent decision takes the client
// FOR KEY SHARE (MCPOAuthClientStore.LockKeyShare) before consuming its
// authorization request; the code exchange takes the client, then the
// code's grant (LockGrantKeyShare), before consuming its code; the refresh
// grant takes the client, then the refresh token's grant, before rotating
// its refresh token (RotateRefreshToken). Deleting a grant -- by its user,
// by an administrator on the user's behalf, on code reuse, on
// refresh-token reuse, or at its client's request (RFC 7009) -- takes the
// grant's client FOR KEY SHARE before the grant. A
// revocation racing an issuance therefore waits on the parent the
// issuance already holds and then deletes what it issued, or the issuance
// waits on the revocation and then finds its parent gone; neither ever
// holds a child the other is waiting for, so they cannot deadlock.
// Taking the client first in a grant's revocation also keeps a client
// deletion's audit exact: the deletion's Lock waits for any revocation
// already holding the client, so the grants it lists are exactly the ones
// its cascade removes, each audited once (never both by the revocation and
// as client_deleted).
//
// The consent decision is the one transaction that locks both a request
// and a grant (request first): both hang off the client it holds FOR KEY
// SHARE throughout, so the only transaction that could lock them the other
// way round -- the client's deletion, which needs the client FOR UPDATE --
// never interleaves with it. users(id) is a parent of grants and requests
// too; nothing here locks a users row except those foreign-key checks, and
// users rows are never deleted.
//
// The authorization endpoint stores a request in a transaction of its own
// (CreatePendingAuthorizationRequest, technical plan §43.14's
// pending-request cap): the client FOR NO KEY UPDATE, then a count of the
// client's pending requests, then the insert. It locks one existing row,
// first, and after it only the row it inserts, which no other transaction
// can know of yet -- so it waits at most once, holding nothing, and can
// never be part of a wait cycle. FOR NO KEY UPDATE conflicts with another
// authorization of the same client (so their counts and inserts never
// interleave), with a metadata document's upsert and failed re-fetch
// record, and with a client's deletion -- never with the FOR KEY SHARE an
// issuance or a grant revocation takes.
//
// Client registration (technical plan §43.15) adds four writers, each
// following the same order. A metadata document's upsert, and the record
// of its first failed re-fetch, are each ONE statement locking ONE row --
// the client, FOR NO KEY UPDATE: that never conflicts with the FOR KEY
// SHARE every issuance and grant revocation takes, only with a client's
// deletion (an administrator's or the sweep's) and with each other, and
// a statement holding a single lock cannot be part of a wait cycle. A
// dynamic registration inserts a new client row and locks
// nothing that exists. The expired-credential sweep's unused-client pass
// (MCPOAuthClientStore.DeleteUnused) deletes a client as an
// administrator's deletion does -- the client FOR UPDATE, then its
// cascade -- in two statements: it locks its candidates first (in id
// order), then deletes only those a fresh look still finds with no grant
// and no authorization request, so its cascade reaches no row at all. An
// authorization holding the client (FOR NO KEY UPDATE) or a grant whose
// insert holds it (FOR KEY SHARE, its foreign-key check) when the sweep
// arrives makes the sweep wait, and, once committed, keeps its client; one
// that arrives after the lock waits for the sweep, then finds its client
// gone. A consent in flight always
// has its request, so its client is never even a candidate
// (TestLockOrder_ClientRegistrationWriters).
//
// An administrator's disable or enable of a client
// (MCPOAuthClientStore.Disable/Enable, technical plan §43.15) is, like the
// upsert, ONE statement locking ONE row -- the client, FOR NO KEY UPDATE,
// since disabled_at is no key column -- followed only by its audit row,
// outside this schema. It waits at most once, holding nothing, so it can
// never be part of a wait cycle, and it never conflicts with the FOR KEY
// SHARE an issuance or a grant revocation takes.
//
// Refresh tokens also reference each other (superseded_by, ON DELETE SET
// NULL), which adds no cycle. Under its grant, the refresh grant locks
// only the token it inserts -- a row no other transaction can know of yet
// -- and then the unrotated token it rotates, whose foreign-key check on
// superseded_by locks the inserted one. Deleting a refresh token locks,
// besides that token, only its predecessor (to set superseded_by NULL),
// which was rotated earlier and which no refresh ever locks again.

package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// MCPOAuthGrantStore is a thin, pass-through wrapper around the
// sqlc-generated queries for everything an MCP authorization produces
// (technical plan §43.14/§43.16, migrations/000141_mcp_oauth.up.sql and
// 000142_mcp_oauth_refresh_tokens.up.sql): pending authorization
// requests, grants, authorization codes, access tokens and refresh
// tokens, plus the bearer check's one-join verification lookup. No
// caching, no business rules: minting, hashing, TTL choice, PKCE and
// every validity decision are internal/adapters/inbound/mcpauth's and
// internal/adapters/inbound/auth's job; this store only ever sees
// already-hashed secrets (platform.HashToken).
type MCPOAuthGrantStore struct {
	q *sqlcgen.Queries
	// db is what q runs on -- the pool, or the transaction WithTx was
	// given -- for the one method that needs a transaction of its own
	// (CreatePendingAuthorizationRequest; on a caller's transaction, a
	// savepoint in it).
	db txBeginner
}

// NewMCPOAuthGrantStore builds an MCPOAuthGrantStore backed by pool.
func NewMCPOAuthGrantStore(pool *pgxpool.Pool) *MCPOAuthGrantStore {
	return &MCPOAuthGrantStore{q: sqlcgen.New(pool), db: pool}
}

// WithTx returns an MCPOAuthGrantStore whose queries run on tx.
func (s *MCPOAuthGrantStore) WithTx(tx pgx.Tx) *MCPOAuthGrantStore {
	return &MCPOAuthGrantStore{q: s.q.WithTx(tx), db: tx}
}

// ErrPendingAuthorizationRequestCap is CreatePendingAuthorizationRequest's
// refusal: the client already has as many pending authorization requests
// as the cap allows, and nothing was written.
var ErrPendingAuthorizationRequestCap = errors.New("postgres: the client already has the most pending MCP authorization requests allowed")

// nonNilScopes stores an empty scope set as '{}', never NULL: pgx encodes
// a nil []string as SQL NULL, and every scopes column is NOT NULL. A
// scope-less grant is a legitimate, first-class outcome (§43.17), not an
// error to be surfaced as a constraint violation.
func nonNilScopes(scopes []string) []string {
	if scopes == nil {
		return []string{}
	}
	return scopes
}

// CreatePendingAuthorizationRequest inserts one validated GET
// /oauth/authorize request -- unless its client already has maxPending
// requests waiting for a decision (unexpired, not consumed), when it
// writes nothing and returns ErrPendingAuthorizationRequestCap (technical
// plan §43.14). One transaction, three statements, in the lock order at
// the top of this file: the client FOR NO KEY UPDATE
// (LockMCPOAuthClientForNewRequest), then the count, taken under that lock
// by a statement of its own so it sees every request an earlier holder
// committed, then the insert. Two authorizations of one client therefore
// count and insert one after the other, and however many race, the
// client never holds more than maxPending pending requests. On a store
// built WithTx, the transaction is a savepoint in the caller's, whose
// locks the caller then holds. pgx.ErrNoRows means the client is gone.
func (s *MCPOAuthGrantStore) CreatePendingAuthorizationRequest(ctx context.Context, arg sqlcgen.CreateMCPOAuthAuthorizationRequestParams, maxPending int) (sqlcgen.McpOauthAuthorizationRequest, error) {
	arg.Scopes = nonNilScopes(arg.Scopes)
	var row sqlcgen.McpOauthAuthorizationRequest
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockMCPOAuthClientForNewRequest(ctx, arg.ClientID); err != nil {
			return err
		}
		pending, err := q.CountPendingMCPOAuthAuthorizationRequests(ctx, arg.ClientID)
		if err != nil {
			return err
		}
		if pending >= int64(maxPending) {
			return ErrPendingAuthorizationRequestCap
		}
		row, err = q.CreateMCPOAuthAuthorizationRequest(ctx, arg)
		return err
	})
	return row, err
}

// GetAuthorizationRequest fetches one authorization request by id.
func (s *MCPOAuthGrantStore) GetAuthorizationRequest(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthAuthorizationRequest, error) {
	return s.q.GetMCPOAuthAuthorizationRequest(ctx, id)
}

// BindAuthorizationRequest binds a still-pending, unexpired request to
// userID (first render) or confirms it is already bound to userID, and
// replaces its CSRF nonce hash -- see the query's own doc comment.
// pgx.ErrNoRows means the request is unknown, bound to another user,
// consumed, or expired.
func (s *MCPOAuthGrantStore) BindAuthorizationRequest(ctx context.Context, id, userID pgtype.UUID, csrfNonceHash string) (sqlcgen.McpOauthAuthorizationRequest, error) {
	return s.q.BindMCPOAuthAuthorizationRequest(ctx, sqlcgen.BindMCPOAuthAuthorizationRequestParams{
		ID:            id,
		UserID:        userID,
		CsrfNonceHash: &csrfNonceHash,
	})
}

// ConsumeAuthorizationRequest marks a request decided, at most once, for
// its bound user only. pgx.ErrNoRows means it was not (any longer)
// decidable by userID. Lock the request's client first (the lock order at
// the top of this file).
func (s *MCPOAuthGrantStore) ConsumeAuthorizationRequest(ctx context.Context, id, userID pgtype.UUID) (sqlcgen.McpOauthAuthorizationRequest, error) {
	return s.q.ConsumeMCPOAuthAuthorizationRequest(ctx, sqlcgen.ConsumeMCPOAuthAuthorizationRequestParams{ID: id, UserID: userID})
}

// UpsertGrant records a consent: it creates the user's one grant for this
// client, or renews that grant's resource and expiry in place and records
// the scopes just approved -- for display only; no authorization decision
// reads a grant's scopes (queries/mcp_oauth_grants.sql's own doc comment).
func (s *MCPOAuthGrantStore) UpsertGrant(ctx context.Context, arg sqlcgen.UpsertMCPOAuthGrantParams) (sqlcgen.McpOauthGrant, error) {
	arg.Scopes = nonNilScopes(arg.Scopes)
	return s.q.UpsertMCPOAuthGrant(ctx, arg)
}

// GetGrant fetches one grant by id.
func (s *MCPOAuthGrantStore) GetGrant(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthGrant, error) {
	return s.q.GetMCPOAuthGrant(ctx, id)
}

// LockGrantKeyShare takes the grant row's FOR KEY SHARE lock for the rest
// of the transaction and returns the row as of that lock: what the code
// exchange takes before consuming a code issued under the grant, and the
// refresh grant before rotating a refresh token issued under it (the lock
// order at the top of this file). pgx.ErrNoRows means no such grant -- it
// was revoked, and its codes and tokens with it.
func (s *MCPOAuthGrantStore) LockGrantKeyShare(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthGrant, error) {
	return s.q.LockMCPOAuthGrantKeyShare(ctx, id)
}

// DeleteGrant revokes a grant (and, by cascade, every code, access token
// and refresh token issued under it), returning how many rows were deleted
// (0 or 1).
// Lock the grant's client first (the lock order at the top of this file).
func (s *MCPOAuthGrantStore) DeleteGrant(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.DeleteMCPOAuthGrant(ctx, id)
}

// DeleteGrantForUser revokes a grant only if it belongs to userID,
// returning the deleted row; pgx.ErrNoRows means no such grant for this
// user (another user's grant is indistinguishable from a missing one).
// Lock the grant's client first (the lock order at the top of this file).
func (s *MCPOAuthGrantStore) DeleteGrantForUser(ctx context.Context, id, userID pgtype.UUID) (sqlcgen.McpOauthGrant, error) {
	return s.q.DeleteMCPOAuthGrantForUser(ctx, sqlcgen.DeleteMCPOAuthGrantForUserParams{ID: id, UserID: userID})
}

// ListGrantsForUser returns userID's own unexpired grants with their
// client's display facts, newest first.
func (s *MCPOAuthGrantStore) ListGrantsForUser(ctx context.Context, userID pgtype.UUID) ([]sqlcgen.ListMCPOAuthGrantsForUserRow, error) {
	return s.q.ListMCPOAuthGrantsForUser(ctx, userID)
}

// ListGrantsForClient returns the id and user of every grant issued to
// clientID (internal id) -- read before a client is deleted, so each
// cascaded revocation can be audited.
func (s *MCPOAuthGrantStore) ListGrantsForClient(ctx context.Context, clientID pgtype.UUID) ([]sqlcgen.ListMCPOAuthGrantsForClientRow, error) {
	return s.q.ListMCPOAuthGrantsForClient(ctx, clientID)
}

// CreateAuthorizationCode inserts one (hashed) authorization code,
// carrying the scopes approved in the consent decision that issued it.
func (s *MCPOAuthGrantStore) CreateAuthorizationCode(ctx context.Context, arg sqlcgen.CreateMCPOAuthAuthorizationCodeParams) (sqlcgen.McpOauthAuthorizationCode, error) {
	arg.Scopes = nonNilScopes(arg.Scopes)
	return s.q.CreateMCPOAuthAuthorizationCode(ctx, arg)
}

// ConsumeAuthorizationCode marks the code with this hash used and returns
// it -- at most once per code. pgx.ErrNoRows means it was already used or
// never existed (GetAuthorizationCodeByHash tells the two apart). Lock the
// client, then the code's grant, first (the lock order at the top of this
// file).
func (s *MCPOAuthGrantStore) ConsumeAuthorizationCode(ctx context.Context, codeHash string) (sqlcgen.McpOauthAuthorizationCode, error) {
	return s.q.ConsumeMCPOAuthAuthorizationCode(ctx, codeHash)
}

// GetAuthorizationCodeByHash fetches a code row regardless of whether it
// was consumed.
func (s *MCPOAuthGrantStore) GetAuthorizationCodeByHash(ctx context.Context, codeHash string) (sqlcgen.McpOauthAuthorizationCode, error) {
	return s.q.GetMCPOAuthAuthorizationCodeByHash(ctx, codeHash)
}

// CreateAccessToken inserts one (hashed) access token with its scopes,
// fixed for the token's whole lifetime.
func (s *MCPOAuthGrantStore) CreateAccessToken(ctx context.Context, arg sqlcgen.CreateMCPOAuthAccessTokenParams) (sqlcgen.McpOauthAccessToken, error) {
	arg.Scopes = nonNilScopes(arg.Scopes)
	return s.q.CreateMCPOAuthAccessToken(ctx, arg)
}

// GetAccessTokenByHash fetches an access token row, expired or not --
// what POST /oauth/revoke needs to find the grant a token belongs to.
// pgx.ErrNoRows means no row carries this hash.
func (s *MCPOAuthGrantStore) GetAccessTokenByHash(ctx context.Context, tokenHash string) (sqlcgen.McpOauthAccessToken, error) {
	return s.q.GetMCPOAuthAccessTokenByHash(ctx, tokenHash)
}

// CreateRefreshToken inserts one (hashed) refresh token with its scopes,
// fixed for the token's whole lifetime.
func (s *MCPOAuthGrantStore) CreateRefreshToken(ctx context.Context, arg sqlcgen.CreateMCPOAuthRefreshTokenParams) (sqlcgen.McpOauthRefreshToken, error) {
	arg.Scopes = nonNilScopes(arg.Scopes)
	return s.q.CreateMCPOAuthRefreshToken(ctx, arg)
}

// GetRefreshTokenByHash fetches a refresh token row whether or not it was
// rotated (or has expired). pgx.ErrNoRows means no row carries this hash:
// never issued, swept after expiring, or revoked with its grant.
func (s *MCPOAuthGrantStore) GetRefreshTokenByHash(ctx context.Context, tokenHash string) (sqlcgen.McpOauthRefreshToken, error) {
	return s.q.GetMCPOAuthRefreshTokenByHash(ctx, tokenHash)
}

// RotateRefreshToken marks refresh token id replaced by successor -- at
// most once per token -- and returns it. pgx.ErrNoRows means it was
// already rotated (or is gone), so whoever presented it replayed it. Lock
// the client, then the token's grant, first (the lock order at the top of
// this file).
func (s *MCPOAuthGrantStore) RotateRefreshToken(ctx context.Context, id, successor pgtype.UUID) (sqlcgen.McpOauthRefreshToken, error) {
	return s.q.RotateMCPOAuthRefreshToken(ctx, sqlcgen.RotateMCPOAuthRefreshTokenParams{ID: id, SupersededBy: successor})
}

// MCPAccessTokenPrincipal is everything the /mcp bearer check needs to
// decide one call, read by LookupAccessToken's single join: the token's
// own expiry and scopes, its grant (audience, expiry), its client
// (disabled or not) and its user (role, email, disabled or not). Plain Go
// values, so auth.RequireMCPBearer's own unit tests can hand it a fake
// lookup.
type MCPAccessTokenPrincipal struct {
	TokenExpiresAt time.Time
	// TokenScopes are the scopes approved in the flow that issued this
	// token, fixed at issuance. The grant's own scopes column is never
	// read here: it only records the most recent consent for display.
	TokenScopes    []string
	GrantID        pgtype.UUID
	GrantResource  string
	GrantExpiresAt time.Time
	// GrantLastUsedAt is the zero time when the grant was never used.
	GrantLastUsedAt time.Time
	ClientID        string
	// ClientKind is the client's registration mechanism
	// (mcp_oauth_client_kind): the bearer check refuses a client whose
	// mechanism the deployment has switched off (mcpclient.Mechanisms).
	ClientKind     string
	ClientDisabled bool
	UserID         pgtype.UUID
	UserRole       string
	UserEmail      string
	UserDisabled   bool
}

// LookupAccessToken resolves an access token's hash into its full
// principal in one query (queries/mcp_oauth_access_tokens.sql's own doc
// comment). pgx.ErrNoRows means no live row carries this hash -- the token
// never existed, expired and was swept, or its grant was revoked.
func (s *MCPOAuthGrantStore) LookupAccessToken(ctx context.Context, tokenHash string) (MCPAccessTokenPrincipal, error) {
	row, err := s.q.LookupMCPOAuthAccessToken(ctx, tokenHash)
	if err != nil {
		return MCPAccessTokenPrincipal{}, err
	}
	p := MCPAccessTokenPrincipal{
		TokenExpiresAt: row.TokenExpiresAt.Time,
		TokenScopes:    row.TokenScopes,
		GrantID:        row.GrantID,
		GrantResource:  row.Resource,
		GrantExpiresAt: row.GrantExpiresAt.Time,
		ClientID:       row.ClientPublicID,
		ClientKind:     string(row.ClientKind),
		ClientDisabled: row.ClientDisabledAt.Valid,
		UserID:         row.UserID,
		UserRole:       string(row.UserRole),
		UserEmail:      row.PrimaryEmail,
		UserDisabled:   row.UserDisabled,
	}
	if row.LastUsedAt.Valid {
		p.GrantLastUsedAt = row.LastUsedAt.Time
	}
	return p, nil
}

// TouchGrantLastUsed stamps grantID's last_used_at with now(), unless it
// was already stamped at or after staleBefore.
func (s *MCPOAuthGrantStore) TouchGrantLastUsed(ctx context.Context, grantID pgtype.UUID, staleBefore time.Time) error {
	return s.q.TouchMCPOAuthGrantLastUsed(ctx, sqlcgen.TouchMCPOAuthGrantLastUsedParams{
		ID:          grantID,
		StaleBefore: pgtype.Timestamptz{Time: staleBefore, Valid: true},
	})
}
