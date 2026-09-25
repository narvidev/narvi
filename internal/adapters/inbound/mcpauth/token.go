package mcpauth

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// maxTokenRequestBytes bounds a token request body: a handful of short
// form parameters.
const maxTokenRequestBytes = 16 << 10

// accessTokenPrefix and refreshTokenPrefix mark the two token families
// (technical plan §43.16) for a secret scanner or a reviewer reading a
// log; the whole prefixed string is what gets hashed.
const (
	accessTokenPrefix  = "narvi_mcp_at_"
	refreshTokenPrefix = "narvi_mcp_rt_"
)

// tokenResponse is a successful token response (RFC 6749 section 5.1). scope is
// always present: the user may have narrowed what the client asked for,
// and a scope-less approval answers "". It is the tokens' own scopes --
// exactly what the user approved in the flow that produced the code, or
// a narrowing of them a refresh asked for -- which is all they will ever
// be able to do. refresh_token is the token the client presents at the
// next refresh (every successful response carries a new one).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// Token backs POST /oauth/token (technical plan §43.14/§43.16): the
// authorization_code grant (codeGrant) and the refresh_token grant
// (refreshGrant). It is never behind the cookie middleware and reads no
// cookie: the only client authentication is the public-client form
// (client_id in the body, or HTTP Basic with an empty secret), and the
// code -- bound by PKCE -- or the refresh token is the credential.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	logger := platform.Logger(r.Context())

	form, ok := parseOAuthForm(w, r)
	if !ok {
		return
	}
	client, basicAttempted, ok := s.authenticateClient(w, r, form, "token")
	if !ok {
		return
	}
	if client.DisabledAt.Valid || client.Kind != sqlcgen.McpOauthClientKindPreregistered {
		logger.Warn("mcpauth: token refused", "outcome", "client_not_usable", "client_id", client.ClientID)
		writeTokenError(w, errInvalidClient, "this client is not available", basicAttempted)
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		s.codeGrant(w, r, client, form)
	case "refresh_token":
		s.refreshGrant(w, r, client, form)
	case "":
		writeTokenError(w, errInvalidRequest, "grant_type is required", false)
	default:
		writeTokenError(w, errUnsupportedGrantType, "only the authorization_code and refresh_token grants are supported", false)
	}
}

// parseOAuthForm reads the application/x-www-form-urlencoded body POST
// /oauth/token and POST /oauth/revoke both take -- the body only, never
// the query string -- refusing any other media type, an oversized body,
// or a repeated parameter (RFC 6749 section 3.2) with invalid_request.
func parseOAuthForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeTokenError(w, errInvalidRequest, "the body must be application/x-www-form-urlencoded", false)
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, errInvalidRequest, "the body could not be parsed", false)
		return nil, false
	}
	form := r.PostForm
	for _, vals := range form {
		if len(vals) > 1 {
			writeTokenError(w, errInvalidRequest, "a parameter was repeated", false)
			return nil, false
		}
	}
	return form, true
}

// authenticateClient resolves the public client a token or revocation
// request identifies itself as (tokenClientID), answering invalid_client
// itself when it cannot: no usable client_id, a secret, a Basic/body
// disagreement, or a client_id no registered client carries. It says
// nothing about whether the client may still obtain tokens -- that is the
// caller's rule. endpoint names the caller in logs.
func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request, form url.Values, endpoint string) (client sqlcgen.McpOauthClient, basicAttempted, ok bool) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	clientID, basicAttempted, ok := tokenClientID(r, form)
	if !ok {
		logger.Warn("mcpauth: "+endpoint+" refused", "outcome", "client_authentication")
		writeTokenError(w, errInvalidClient, "public clients authenticate with client_id alone", basicAttempted)
		return sqlcgen.McpOauthClient{}, basicAttempted, false
	}
	if !storableText(clientID) {
		// No registered client_id carries such bytes: refused exactly
		// like an unknown one (storableText's own doc comment).
		logger.Warn("mcpauth: "+endpoint+" refused", "outcome", "unknown_client")
		writeTokenError(w, errInvalidClient, "unknown client", basicAttempted)
		return sqlcgen.McpOauthClient{}, basicAttempted, false
	}
	client, err := s.deps.Clients.GetByClientID(ctx, clientID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		logger.Warn("mcpauth: "+endpoint+" refused", "outcome", "unknown_client")
		writeTokenError(w, errInvalidClient, "unknown client", basicAttempted)
		return sqlcgen.McpOauthClient{}, basicAttempted, false
	case err != nil:
		logger.Error("mcpauth: "+endpoint+": load client failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return sqlcgen.McpOauthClient{}, basicAttempted, false
	}
	return client, basicAttempted, true
}

// codeGrant answers grant_type=authorization_code: every refusal it can
// decide from the request alone comes first and leaves the code unspent;
// exchangeCode does the rest.
func (s *Server) codeGrant(w http.ResponseWriter, r *http.Request, client sqlcgen.McpOauthClient, form url.Values) {
	code := form.Get("code")
	verifier := form.Get("code_verifier")
	if code == "" {
		writeTokenError(w, errInvalidRequest, "code is required", false)
		return
	}
	if !validPKCEValue(verifier) {
		writeTokenError(w, errInvalidRequest, "code_verifier is required (PKCE)", false)
		return
	}
	// Checked before the code is looked up: a missing or foreign resource
	// is invalid_target (RFC 8707 section 2) and leaves the code unspent,
	// like every refusal decided from the request alone -- the code is
	// still bound by PKCE and expires in MCPAuthorizationCodeTTL.
	if !s.ids.matchesResource(form.Get("resource")) {
		writeTokenError(w, errInvalidTarget, "resource must be this deployment's MCP endpoint", false)
		return
	}

	s.exchangeCode(w, r, client, code, verifier, form.Get("redirect_uri"))
}

// tokenClientID extracts the public client's id: from HTTP Basic (whose
// credentials are form-encoded, RFC 6749 section 2.3.1), from the body, or both
// -- which must then agree. Any client secret, in either place, refuses
// the request: every client here is public. basicAttempted reports
// whether the request tried HTTP Basic (an invalid_client answer then
// carries a 401 and a Basic challenge).
func tokenClientID(r *http.Request, form url.Values) (clientID string, basicAttempted, ok bool) {
	bodyID := form.Get("client_id")
	if form.Get("client_secret") != "" {
		return "", false, false
	}
	if r.Header.Get("Authorization") == "" {
		return bodyID, false, bodyID != ""
	}
	user, pass, hasBasic := r.BasicAuth()
	if !hasBasic {
		return "", true, false
	}
	basicID, err1 := url.QueryUnescape(user)
	secret, err2 := url.QueryUnescape(pass)
	if err1 != nil || err2 != nil || basicID == "" || secret != "" {
		return "", true, false
	}
	if bodyID != "" && bodyID != basicID {
		return "", true, false
	}
	return basicID, true, true
}

// exchangeCode consumes the authorization code and, if every binding
// holds, issues one access token and one refresh token -- in one
// transaction. A code that
// reaches this point is spent by the attempt even when a later check
// fails (single use is not "single successful use"), and a code that was
// already spent is a replay: the
// grant it produced is deleted, so every token issued under it stops
// working on its next use (technical plan §43.16).
//
// Locks follow the one order every transaction on these tables follows,
// parent before child (the top of postgres/mcpoauthgrant_store.go): the
// client this request authenticated as, then the code's grant, both FOR
// KEY SHARE, and only then the code itself. So the code's grant is read
// first, without a lock. The client locked is the requesting one -- the
// grant's own client whenever a token is issued, since a code issued to
// another client is refused below.
func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, client sqlcgen.McpOauthClient, code, verifier, redirectURI string) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	fail := func(msg string, err error) {
		logger.Error("mcpauth: token: "+msg, "error", err)
		writeTokenError(w, errServerError, "", false)
	}
	codeHash := platform.HashToken(code)

	presented, err := s.deps.Grants.GetAuthorizationCodeByHash(ctx, codeHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		logger.Warn("mcpauth: token refused", "outcome", "unknown_code")
		writeTokenError(w, errInvalidGrant, "the authorization code is invalid", false)
		return
	case err != nil:
		fail("load code failed", err)
		return
	case presented.ConsumedAt.Valid:
		s.handleUnusableCode(w, r, codeHash)
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
		// Deleted since Token read it, and every code issued to it with it.
		logger.Warn("mcpauth: token refused", "outcome", "client_deleted", "client_id", client.ClientID)
		writeTokenError(w, errInvalidGrant, "the authorization code is invalid", false)
		return
	} else if err != nil {
		fail("lock client failed", err)
		return
	}
	grant, err := grants.LockGrantKeyShare(ctx, presented.GrantID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Revoked since the code was read, and the code with it.
		logger.Warn("mcpauth: token refused", "outcome", "grant_revoked", "client_id", client.ClientID)
		writeTokenError(w, errInvalidGrant, "the authorization code is invalid", false)
		return
	}
	if err != nil {
		fail("lock grant failed", err)
		return
	}

	row, err := grants.ConsumeAuthorizationCode(ctx, codeHash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Spent by a concurrent exchange since it was read (or swept).
		// This transaction may now hold the code row's lock -- a consume
		// that waited keeps it -- so it ends here, before
		// handleUnusableCode locks the grant's client and deletes the
		// grant in a transaction of its own.
		_ = tx.Rollback(ctx)
		s.handleUnusableCode(w, r, codeHash)
		return
	}
	if err != nil {
		fail("consume code failed", err)
		return
	}

	// refuseAs commits the code's consumption, then answers errCode;
	// refuse is the invalid_grant case every binding but the resource
	// answers.
	refuseAs := func(errCode, description, outcome string) {
		if err := tx.Commit(ctx); err != nil {
			fail("commit spent code failed", err)
			return
		}
		logger.Warn("mcpauth: token refused", "outcome", outcome, "client_id", client.ClientID)
		writeTokenError(w, errCode, description, false)
	}
	refuse := func(outcome string) {
		refuseAs(errInvalidGrant, "the authorization code is invalid", outcome)
	}

	now := time.Now()
	switch {
	case !row.ExpiresAt.Time.After(now):
		refuse("code_expired")
		return
	case grant.ClientID != client.ID:
		refuse("code_for_other_client")
		return
	case redirectURI != "" && redirectURI != row.RedirectUri:
		refuse("redirect_uri_mismatch")
		return
	case row.Resource != s.ids.Resource:
		// The request's resource matched this deployment above, so this is
		// a code granted for another resource -- only possible when
		// PublicBaseURL changed between authorization and exchange. RFC
		// 8707 section 2 answers a resource outside what was granted with
		// invalid_target.
		refuseAs(errInvalidTarget, "the authorization code was issued for another resource", "resource_mismatch")
		return
	case !verifyPKCES256(verifier, row.CodeChallenge):
		refuse("pkce_mismatch")
		return
	case !grant.ExpiresAt.Time.After(now):
		refuse("grant_expired")
		return
	}

	// The tokens' scopes are the code's -- what the user approved in the
	// consent decision that issued it -- never the grant's, which a later
	// consent for the same client overwrites (technical plan §43.16).
	issued, err := s.issueTokens(ctx, grants, grant, mcpscope.FromStrings(row.Scopes), now)
	if err != nil {
		fail("record tokens failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail("commit tokens failed", err)
		return
	}
	writeIssued(w, issued, now)
}

// issuedTokens is one successful token response's credentials, stored and
// not yet committed: the plaintexts exist here and in the response only.
type issuedTokens struct {
	accessToken     string
	accessExpiresAt time.Time
	refreshToken    string
	// refreshID is the new refresh token's row: what a refresh rotates the
	// presented token into.
	refreshID pgtype.UUID
	scopes    []mcpscope.Scope
}

// issueTokens mints and stores, under grant and inside grants' own
// transaction, one access token and one refresh token, both holding
// exactly scopes -- fixed for their whole lifetime -- and each expiring
// after its own TTL (MCPAccessTokenTTL, MCPRefreshTokenTTL) or at the
// grant's own expiry, whichever comes first (technical plan §43.16).
func (s *Server) issueTokens(ctx context.Context, grants *postgres.MCPOAuthGrantStore, grant sqlcgen.McpOauthGrant, scopes []mcpscope.Scope, now time.Time) (issuedTokens, error) {
	capAtGrant := func(ttl time.Duration) time.Time {
		at := now.Add(ttl)
		if grant.ExpiresAt.Time.Before(at) {
			return grant.ExpiresAt.Time
		}
		return at
	}
	access, err := platform.GenerateToken()
	if err != nil {
		return issuedTokens{}, err
	}
	refresh, err := platform.GenerateToken()
	if err != nil {
		return issuedTokens{}, err
	}
	out := issuedTokens{
		accessToken:     accessTokenPrefix + access,
		accessExpiresAt: capAtGrant(s.cfg.Timeouts.MCPAccessTokenTTL),
		refreshToken:    refreshTokenPrefix + refresh,
		scopes:          scopes,
	}
	stored := mcpscope.Strings(scopes)
	if _, err := grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID:   grant.ID,
		TokenHash: platform.HashToken(out.accessToken),
		Scopes:    stored,
		ExpiresAt: pgtype.Timestamptz{Time: out.accessExpiresAt, Valid: true},
	}); err != nil {
		return issuedTokens{}, err
	}
	rt, err := grants.CreateRefreshToken(ctx, sqlcgen.CreateMCPOAuthRefreshTokenParams{
		GrantID:   grant.ID,
		TokenHash: platform.HashToken(out.refreshToken),
		Scopes:    stored,
		ExpiresAt: pgtype.Timestamptz{Time: capAtGrant(s.cfg.Timeouts.MCPRefreshTokenTTL), Valid: true},
	})
	if err != nil {
		return issuedTokens{}, err
	}
	out.refreshID = rt.ID
	return out, nil
}

// writeIssued answers a successful token request with issued, committed.
func writeIssued(w http.ResponseWriter, issued issuedTokens, now time.Time) {
	writeTokenJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  issued.accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(platform.DurationToSeconds(issued.accessExpiresAt.Sub(now))),
		Scope:        mcpscope.Join(issued.scopes),
		RefreshToken: issued.refreshToken,
	})
}

// handleUnusableCode answers a code ConsumeAuthorizationCode would not
// hand out: never issued or gone (invalid_grant), or already spent -- a
// replay, which deletes the grant it produced and records why. It runs in
// a transaction of its own, never the exchange's, and locks in the one
// order (the top of postgres/mcpoauthgrant_store.go): the grant's client
// FOR KEY SHARE, then the grant, whose deletion cascades to its codes and
// tokens. An exchange still in flight under the grant holds it FOR KEY
// SHARE, so the deletion waits for that exchange to commit and then takes
// its token too.
func (s *Server) handleUnusableCode(w http.ResponseWriter, r *http.Request, codeHash string) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		logger.Error("mcpauth: token: begin replay tx failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grants := s.deps.Grants.WithTx(tx)

	spent, err := grants.GetAuthorizationCodeByHash(ctx, codeHash)
	if errors.Is(err, pgx.ErrNoRows) {
		logger.Warn("mcpauth: token refused", "outcome", "unknown_code")
		writeTokenError(w, errInvalidGrant, "the authorization code is invalid", false)
		return
	}
	if err != nil {
		logger.Error("mcpauth: token: load spent code failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	grant, err := grants.GetGrant(ctx, spent.GrantID)
	if err == nil {
		client, cerr := s.deps.Clients.WithTx(tx).LockKeyShare(ctx, grant.ClientID)
		if errors.Is(cerr, pgx.ErrNoRows) {
			// The client was deleted since, and this grant with it.
			writeTokenError(w, errInvalidGrant, "the authorization code is invalid", false)
			return
		}
		if cerr != nil {
			logger.Error("mcpauth: token: lock client for replay revocation failed", "error", cerr)
			writeTokenError(w, errServerError, "", false)
			return
		}
		deleted, derr := grants.DeleteGrant(ctx, grant.ID)
		if derr != nil {
			logger.Error("mcpauth: token: revoke replayed grant failed", "error", derr)
			writeTokenError(w, errServerError, "", false)
			return
		}
		if deleted == 1 {
			if aerr := auditlog.Record(ctx, s.deps.AuditLog.WithTx(tx), grant.UserID, "mcp_authorization.revoked", "mcp_authorization", grant.ID.String(), map[string]any{
				"reason":      "code_reuse",
				"actor":       "system",
				"client_id":   client.ClientID,
				"client_name": client.ClientName,
			}); aerr != nil {
				logger.Error("mcpauth: token: record replay audit failed", "error", aerr)
				writeTokenError(w, errServerError, "", false)
				return
			}
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			logger.Error("mcpauth: token: commit replay revocation failed", "error", cerr)
			writeTokenError(w, errServerError, "", false)
			return
		}
		logger.Warn("mcpauth: token refused; grant revoked", "outcome", "code_reuse", "grant_id", grant.ID.String())
	} else if !errors.Is(err, pgx.ErrNoRows) {
		logger.Error("mcpauth: token: load replayed grant failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	writeTokenError(w, errInvalidGrant, "the authorization code is invalid", false)
}
