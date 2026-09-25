package mcpauth

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// maxTokenRequestBytes bounds a token request body: a handful of short
// form parameters.
const maxTokenRequestBytes = 16 << 10

// accessTokenPrefix marks the access-token family (technical plan
// §43.16); the whole prefixed string is what gets hashed.
const accessTokenPrefix = "narvi_mcp_at_"

// tokenResponse is a successful token response (RFC 6749 section 5.1). scope is
// always present: the user may have narrowed what the client asked for,
// and a scope-less approval answers "". It is the token's own scopes --
// exactly what the user approved in the flow that produced the code --
// which is all the token will ever be able to do.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

// Token backs POST /oauth/token (technical plan §43.14). It is never
// behind the cookie middleware and reads no cookie: the only client
// authentication is the public-client form (client_id in the body, or
// HTTP Basic with an empty secret), and the code itself, bound by PKCE,
// is the credential.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeTokenError(w, errInvalidRequest, "the body must be application/x-www-form-urlencoded", false)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, errInvalidRequest, "the body could not be parsed", false)
		return
	}
	form := r.PostForm
	for _, vals := range form {
		if len(vals) > 1 {
			writeTokenError(w, errInvalidRequest, "a parameter was repeated", false)
			return
		}
	}

	clientID, basicAttempted, ok := tokenClientID(r, form)
	if !ok {
		logger.Warn("mcpauth: token refused", "outcome", "client_authentication")
		writeTokenError(w, errInvalidClient, "public clients authenticate with client_id alone", basicAttempted)
		return
	}
	if !storableText(clientID) {
		// No registered client_id carries such bytes: refused exactly
		// like an unknown one (storableText's own doc comment).
		logger.Warn("mcpauth: token refused", "outcome", "unknown_client")
		writeTokenError(w, errInvalidClient, "unknown client", basicAttempted)
		return
	}
	client, err := s.deps.Clients.GetByClientID(ctx, clientID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		logger.Warn("mcpauth: token refused", "outcome", "unknown_client")
		writeTokenError(w, errInvalidClient, "unknown client", basicAttempted)
		return
	case err != nil:
		logger.Error("mcpauth: token: load client failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	if client.DisabledAt.Valid || client.Kind != sqlcgen.McpOauthClientKindPreregistered {
		logger.Warn("mcpauth: token refused", "outcome", "client_not_usable", "client_id", client.ClientID)
		writeTokenError(w, errInvalidClient, "this client is not available", basicAttempted)
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
	case "":
		writeTokenError(w, errInvalidRequest, "grant_type is required", false)
		return
	default:
		writeTokenError(w, errUnsupportedGrantType, "only the authorization_code grant is supported", false)
		return
	}

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
// holds, issues one access token -- in one transaction. A code that
// reaches this point is spent by the attempt even when a later check
// fails (single use is not "single successful use"), and a code that was
// already spent is a replay: the
// grant it produced is deleted, so every token issued under it stops
// working on its next use (technical plan §43.16).
func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, client sqlcgen.McpOauthClient, code, verifier, redirectURI string) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	fail := func(msg string, err error) {
		logger.Error("mcpauth: token: "+msg, "error", err)
		writeTokenError(w, errServerError, "", false)
	}

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		fail("begin tx failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grants := s.deps.Grants.WithTx(tx)
	codeHash := platform.HashToken(code)

	row, err := grants.ConsumeAuthorizationCode(ctx, codeHash)
	if errors.Is(err, pgx.ErrNoRows) {
		s.handleUnusableCode(w, r, tx, codeHash)
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
	grant, err := grants.GetGrant(ctx, row.GrantID)
	if errors.Is(err, pgx.ErrNoRows) {
		refuse("grant_revoked")
		return
	}
	if err != nil {
		fail("load grant failed", err)
		return
	}
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

	token, err := platform.GenerateToken()
	if err != nil {
		fail("generate token failed", err)
		return
	}
	accessToken := accessTokenPrefix + token
	expiresAt := now.Add(s.cfg.Timeouts.MCPAccessTokenTTL)
	if grant.ExpiresAt.Time.Before(expiresAt) {
		expiresAt = grant.ExpiresAt.Time
	}
	// The token's scopes are the code's -- what the user approved in the
	// consent decision that issued it -- never the grant's, which a later
	// consent for the same client overwrites (technical plan §43.16).
	if _, err := grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID:   grant.ID,
		TokenHash: platform.HashToken(accessToken),
		Scopes:    row.Scopes,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		fail("record token failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail("commit token failed", err)
		return
	}
	writeTokenJSON(w, http.StatusOK, tokenResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(platform.DurationToSeconds(expiresAt.Sub(now))),
		Scope:       mcpscope.Join(mcpscope.FromStrings(row.Scopes)),
	})
}

// handleUnusableCode answers a code ConsumeAuthorizationCode would not
// hand out: never issued (invalid_grant) or already spent -- a replay,
// which deletes the grant it produced and records why.
func (s *Server) handleUnusableCode(w http.ResponseWriter, r *http.Request, tx pgx.Tx, codeHash string) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
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
		client, cerr := s.deps.Clients.WithTx(tx).GetByID(ctx, grant.ClientID)
		if cerr != nil {
			logger.Error("mcpauth: token: load client for replay audit failed", "error", cerr)
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
