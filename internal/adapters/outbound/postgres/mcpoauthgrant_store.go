package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// MCPOAuthGrantStore is a thin, pass-through wrapper around the
// sqlc-generated queries for everything an MCP authorization produces
// (technical plan §43.14/§43.16, migrations/000141_mcp_oauth.up.sql):
// pending authorization requests, grants, authorization codes and access
// tokens, plus the bearer check's one-join verification lookup. No
// caching, no business rules: minting, hashing, TTL choice, PKCE and
// every validity decision are internal/adapters/inbound/mcpauth's and
// internal/adapters/inbound/auth's job; this store only ever sees
// already-hashed secrets (platform.HashToken).
type MCPOAuthGrantStore struct {
	q *sqlcgen.Queries
}

// NewMCPOAuthGrantStore builds an MCPOAuthGrantStore backed by pool.
func NewMCPOAuthGrantStore(pool *pgxpool.Pool) *MCPOAuthGrantStore {
	return &MCPOAuthGrantStore{q: sqlcgen.New(pool)}
}

// WithTx returns an MCPOAuthGrantStore whose queries run on tx.
func (s *MCPOAuthGrantStore) WithTx(tx pgx.Tx) *MCPOAuthGrantStore {
	return &MCPOAuthGrantStore{q: s.q.WithTx(tx)}
}

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

// CreateAuthorizationRequest inserts one validated GET /oauth/authorize
// request.
func (s *MCPOAuthGrantStore) CreateAuthorizationRequest(ctx context.Context, arg sqlcgen.CreateMCPOAuthAuthorizationRequestParams) (sqlcgen.McpOauthAuthorizationRequest, error) {
	arg.Scopes = nonNilScopes(arg.Scopes)
	return s.q.CreateMCPOAuthAuthorizationRequest(ctx, arg)
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
// decidable by userID.
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

// DeleteGrant revokes a grant (and, by cascade, every code and access
// token issued under it), returning how many rows were deleted (0 or 1).
func (s *MCPOAuthGrantStore) DeleteGrant(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.DeleteMCPOAuthGrant(ctx, id)
}

// DeleteGrantForUser revokes a grant only if it belongs to userID,
// returning the deleted row; pgx.ErrNoRows means no such grant for this
// user (another user's grant is indistinguishable from a missing one).
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
// never existed (GetAuthorizationCodeByHash tells the two apart).
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
	ClientDisabled  bool
	UserID          pgtype.UUID
	UserRole        string
	UserEmail       string
	UserDisabled    bool
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
