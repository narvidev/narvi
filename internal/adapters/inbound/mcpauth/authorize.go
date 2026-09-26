package mcpauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// maxStateLength bounds the client's opaque state value, stored verbatim
// and echoed back.
const maxStateLength = 512

// signInPath is the SPA sign-in route; its ?next= carries the consent
// path through GitHub or OIDC sign-in and back (technical plan §43.14).
const signInPath = "/sign-in"

// singleParam returns q[name]'s one value: ok is false when the parameter
// is repeated (OAuth 2.1 section 3.1: request parameters MUST NOT be included
// more than once). An absent parameter is ("", true).
func singleParam(q url.Values, name string) (string, bool) {
	vals := q[name]
	if len(vals) > 1 {
		return "", false
	}
	if len(vals) == 0 {
		return "", true
	}
	return vals[0], true
}

// foreignKeyViolation is Postgres's SQLSTATE for an insert whose parent
// row is gone.
const foreignKeyViolation = "23503"

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation
}

// storableText reports whether s can be sent to Postgres as TEXT: valid
// UTF-8 with no NUL byte. Postgres rejects anything else (SQLSTATE
// 22021), which is the caller's malformed input, not a server fault --
// so an unauthenticated client_id or state is checked here first and
// refused as a client error, never answered 500 or logged at ERROR.
func storableText(s string) bool {
	return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}

// Authorize backs GET /oauth/authorize (technical plan §43.14). Errors in
// client_id or redirect_uri render a page and never redirect; every later
// error redirects to the validated redirect_uri with error, state and
// iss. A valid request is stored and the browser continues to the consent
// page -- through sign-in first when it carries no valid session.
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	q := r.URL.Query()

	clientIDParam, ok := singleParam(q, "client_id")
	if !ok || clientIDParam == "" {
		logger.Warn("mcpauth: authorize refused", "outcome", "missing_client_id")
		s.renderError(w, r, http.StatusBadRequest, "This app could not be identified", "The request did not carry a valid client identifier.")
		return
	}
	if !storableText(clientIDParam) {
		// No registered client_id carries such bytes: refused exactly
		// like an unknown one.
		logger.Warn("mcpauth: authorize refused", "outcome", "unknown_client")
		s.renderError(w, r, http.StatusBadRequest, "This app is not registered", "This deployment does not know the app that sent you here.")
		return
	}
	client, ok := s.resolveClient(w, r, clientIDParam)
	if !ok {
		return
	}
	if !s.clientUsable(client) {
		logger.Warn("mcpauth: authorize refused", "outcome", "client_not_usable", "client_id", client.ClientID)
		s.renderError(w, r, http.StatusBadRequest, "This app is not available", "This deployment has disabled this app, or no longer accepts the way it was registered.")
		return
	}

	redirectURI, ok := singleParam(q, "redirect_uri")
	if !ok || !mcpclient.MatchRedirectURI(client.RedirectUris, redirectURI) {
		// Never redirected: an unregistered redirect_uri is exactly where
		// an attacker would want the browser sent.
		logger.Warn("mcpauth: authorize refused", "outcome", "redirect_uri_mismatch", "client_id", client.ClientID)
		s.renderError(w, r, http.StatusBadRequest, "This app's return address is not registered", "The app asked to send you back somewhere this deployment has not registered for it, so nothing was sent anywhere.")
		return
	}

	// From here on every error goes back to the validated redirect URI.
	state, ok := singleParam(q, "state")
	if !ok {
		s.redirectError(w, r, redirectURI, errInvalidRequest, "state was repeated", "")
		return
	}
	if len(state) > maxStateLength {
		s.redirectError(w, r, redirectURI, errInvalidRequest, "state is too long", "")
		return
	}
	if !storableText(state) {
		s.redirectError(w, r, redirectURI, errInvalidRequest, "state must be valid UTF-8 text", "")
		return
	}
	for _, name := range []string{"response_type", "code_challenge", "code_challenge_method", "resource", "scope"} {
		if _, ok := singleParam(q, name); !ok {
			s.redirectError(w, r, redirectURI, errInvalidRequest, name+" was repeated", state)
			return
		}
	}
	if q.Get("response_type") != "code" {
		s.redirectError(w, r, redirectURI, errUnsupportedResponseType, "only response_type=code is supported", state)
		return
	}
	challenge := q.Get("code_challenge")
	if !validPKCEValue(challenge) {
		s.redirectError(w, r, redirectURI, errInvalidRequest, "code_challenge is required (PKCE)", state)
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		s.redirectError(w, r, redirectURI, errInvalidRequest, "code_challenge_method must be S256", state)
		return
	}
	if !s.ids.matchesResource(q.Get("resource")) {
		s.redirectError(w, r, redirectURI, errInvalidTarget, "resource must be this deployment's MCP endpoint", state)
		return
	}
	scopes, err := mcpscope.ParseRequested(q.Get("scope"), s.scopes)
	if err != nil {
		s.redirectError(w, r, redirectURI, errInvalidScope, "a requested scope is not offered by this deployment", state)
		return
	}

	var statePtr *string
	if state != "" {
		statePtr = &state
	}
	row, err := s.deps.Grants.CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
		ClientID:            client.ID,
		RedirectUri:         redirectURI,
		Scopes:              mcpscope.Strings(scopes),
		State:               statePtr,
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
		Resource:            s.ids.Resource,
		ExpiresAt:           pgtype.Timestamptz{Time: time.Now().Add(s.cfg.Timeouts.MCPAuthorizationRequestTTL), Valid: true},
	})
	if isForeignKeyViolation(err) {
		// The client was deleted -- by an administrator, or by the unused-
		// client sweep -- between the read above and this insert. Its
		// redirect URI is no longer anyone's: an error page, never a
		// redirect.
		logger.Warn("mcpauth: authorize refused", "outcome", "client_deleted", "client_id", client.ClientID)
		s.renderError(w, r, http.StatusBadRequest, "This app is not registered", "This deployment does not know the app that sent you here.")
		return
	}
	if err != nil {
		logger.Error("mcpauth: authorize: store request failed", "error", err)
		s.redirectError(w, r, redirectURI, errServerError, "the request could not be stored", state)
		return
	}

	target := consentURL(row.ID)
	if _, ok := auth.Authenticate(ctx, s.deps.UserSessions, s.deps.Users, r); !ok {
		target = signInURL(target)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusFound)
}

// consentURL is the same-origin path of one request's consent page.
func consentURL(requestID pgtype.UUID) string {
	return consentPath + "?request=" + requestID.String()
}

// signInURL sends a signed-out browser to sign in first and come back to
// next. Only the request id travels through sign-in, never the OAuth
// query itself.
func signInURL(next string) string {
	return signInPath + "?next=" + url.QueryEscape(next)
}
