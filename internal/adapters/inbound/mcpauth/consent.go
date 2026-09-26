package mcpauth

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// maxConsentFormBytes bounds the consent decision's form body: a request
// id, a nonce, a decision and a handful of scope checkboxes.
const maxConsentFormBytes = 16 << 10

// authorizationCodePrefix marks the code family (technical plan §43.16);
// the whole prefixed string is what gets hashed.
const authorizationCodePrefix = "narvi_mcp_ac_"

// consentScope is one checkbox on the consent page.
type consentScope struct {
	Value       string
	Description string
}

// consentPage is consent.html's own data. Every field is rendered through
// html/template's contextual escaping.
type consentPage struct {
	ClientName string
	// IdentityHost is set for a metadata-document client only: the host
	// of its client_id URL, which the page shows as the headline -- the
	// client's true identity -- with the name it gave itself second.
	IdentityHost string
	// SelfRegistered is set for a dynamically registered client only. Its
	// name is the unauthenticated registrant's free choice -- it could copy
	// the very wording the page keeps for a verified host -- so the title
	// and headline are fixed words saying the app registered itself, and
	// the name comes second, quoted, marked as the name it gave itself.
	SelfRegistered     bool
	ClientIdentity     string
	ClientHomepageHost string
	UserEmail          string
	RedirectHost       string
	RedirectLoopback   bool
	Scopes             []consentScope
	RequestID          string
	Nonce              string
}

// The identity line the consent page shows for each client kind (technical
// plan §43.14/§43.15): who vouched for the client, never only the name it
// goes by -- which, for every kind but a pre-registered one, the client
// chose itself.
const (
	// preregisteredIdentity: an administrator of this deployment
	// registered it.
	preregisteredIdentity = "Registered by an administrator of this deployment."
	// metadataDocumentIdentity follows the client_id URL's host, which the
	// page shows as its headline.
	metadataDocumentIdentity = "That name is the app's own choice. The address above is where this deployment read the app's description, over a verified connection, and it is what identifies the app."
	// dynamicIdentity: it registered itself, and nothing vouches for it.
	dynamicIdentity = "This app registered itself with this deployment, so nothing vouches for its name. Check where your browser is sent back to before you allow it."
)

// identityFor fills page's identity for client: what heads the page (the
// verified host, the fixed self-registered headline, or an administrator's
// name) and the identity line.
func identityFor(page *consentPage, client sqlcgen.McpOauthClient) bool {
	switch mcpclient.Kind(client.Kind) {
	case mcpclient.KindPreregistered:
		page.ClientIdentity = preregisteredIdentity
		if client.ClientUri != nil {
			page.ClientHomepageHost = mcpclient.ClientURIHost(*client.ClientUri)
		}
	case mcpclient.KindMetadataDocument:
		page.IdentityHost = mcpclient.IdentityHost(client.ClientID)
		page.ClientIdentity = metadataDocumentIdentity
		if page.IdentityHost == "" {
			return false
		}
	case mcpclient.KindDynamic:
		page.SelfRegistered = true
		page.ClientIdentity = dynamicIdentity
	default:
		return false
	}
	return true
}

// parseRequestID parses a consent request id.
func parseRequestID(raw string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if raw == "" || id.Scan(raw) != nil {
		return pgtype.UUID{}, false
	}
	return id, true
}

// requestUsable reports whether a stored request can still be decided.
func requestUsable(row sqlcgen.McpOauthAuthorizationRequest, now time.Time) bool {
	return !row.ConsumedAt.Valid && row.ExpiresAt.Time.After(now)
}

// formActionSource is the CSP form-action source for the page's own
// redirect target: browsers apply form-action to the redirect a form
// submission follows, so the stored redirect URI's origin must be allowed
// next to 'self'. An IPv6 literal cannot be written as a CSP host-source,
// so that case falls back to its scheme; the target itself is always the
// stored, validated URI, never anything the page's own form supplies.
func formActionSource(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "'self'"
	}
	if strings.Contains(u.Hostname(), ":") {
		return "'self' " + u.Scheme + ":"
	}
	origin, err := platform.CanonicalOrigin(redirectURI)
	if err != nil {
		return "'self'"
	}
	return "'self' " + origin
}

// ConsentPage backs GET /oauth/consent?request=<id> (technical plan
// §43.14): authenticate the cookie, bind the request to this user on its
// first render (another user is refused for good), mint a fresh nonce
// whose hash replaces the stored one, and render the page.
func (s *Server) ConsentPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	requestID, ok := parseRequestID(r.URL.Query().Get("request"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "This authorization request is not valid", "The link you followed is incomplete or malformed.")
		return
	}
	user, ok := auth.Authenticate(ctx, s.deps.UserSessions, s.deps.Users, r)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, signInURL(consentURL(requestID)), http.StatusFound)
		return
	}
	var userID pgtype.UUID
	if err := userID.Scan(user.ID); err != nil {
		logger.Error("mcpauth: consent: parse user id failed", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be processed.")
		return
	}

	row, err := s.deps.Grants.GetAuthorizationRequest(ctx, requestID)
	if err != nil || !requestUsable(row, time.Now()) {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("mcpauth: consent: load request failed", "error", err)
		}
		s.renderError(w, r, http.StatusBadRequest, "This authorization request has expired", "It was already decided, or it waited too long. Start again from the app.")
		return
	}
	if row.UserID.Valid && row.UserID != userID {
		logger.Warn("mcpauth: consent refused", "outcome", "other_user")
		s.renderError(w, r, http.StatusForbidden, "This authorization request belongs to another sign-in", "Another account opened this request first. Start again from the app while signed in as the account you want it to use.")
		return
	}
	if err := authz.Authorize(authz.Actor{UserID: user.ID, Role: authz.Role(user.Role)}, authz.ActionConnectMCPClient, authz.Resource{}); err != nil {
		logger.Warn("mcpauth: consent refused", "outcome", "forbidden", "error", err)
		s.renderError(w, r, http.StatusForbidden, "Your role cannot connect apps", "Your account is not allowed to connect MCP clients on this deployment.")
		return
	}
	client, err := s.deps.Clients.GetByID(ctx, row.ClientID)
	if err != nil || !s.clientUsable(client) {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("mcpauth: consent: load client failed", "error", err)
		}
		s.renderError(w, r, http.StatusBadRequest, "This app is not available", "The app that asked for access is no longer registered on this deployment.")
		return
	}

	scopes := make([]consentScope, 0, len(row.Scopes))
	for _, raw := range row.Scopes {
		desc, err := describeScope(mcpscope.Scope(raw))
		if err != nil {
			logger.Error("mcpauth: consent: scope without a description", "scope", raw)
			s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be displayed.")
			return
		}
		scopes = append(scopes, consentScope{Value: raw, Description: desc})
	}

	nonce, err := platform.GenerateToken()
	if err != nil {
		logger.Error("mcpauth: consent: generate nonce failed", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be displayed.")
		return
	}
	if _, err := s.deps.Grants.BindAuthorizationRequest(ctx, requestID, userID, platform.HashToken(nonce)); err != nil {
		// Lost a race: decided, expired, or bound to someone else between
		// the read above and this write.
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("mcpauth: consent: bind request failed", "error", err)
		}
		s.renderError(w, r, http.StatusBadRequest, "This authorization request has expired", "It was already decided, or it waited too long. Start again from the app.")
		return
	}

	page := consentPage{
		ClientName:       client.ClientName,
		UserEmail:        user.Email,
		RedirectHost:     mcpclient.RedirectHost(row.RedirectUri),
		RedirectLoopback: mcpclient.IsLoopbackRedirect(row.RedirectUri),
		Scopes:           scopes,
		RequestID:        requestID.String(),
		Nonce:            nonce,
	}
	if !identityFor(&page, client) || page.RedirectHost == "" {
		// Unreachable for a stored client and request: every kind has an
		// identity, a metadata-document client_id was validated before it
		// was ever stored, and so was every redirect URI a request
		// carries. A row that bypassed that validation is refused rather
		// than shown with a host a user cannot check.
		logger.Error("mcpauth: consent: client has no displayable identity", "client_id", client.ClientID, "kind", client.Kind)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be displayed.")
		return
	}
	var buf bytes.Buffer
	if err := s.consentTpl.Execute(&buf, page); err != nil {
		logger.Error("mcpauth: consent: render failed", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be displayed.")
		return
	}
	setPageHeaders(w, formActionSource(row.RedirectUri))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// sameOrigin is the consent decision's cross-site guard (technical plan
// §43.14). An Origin header that is present and not the literal "null"
// must be this deployment's own canonical origin. Otherwise -- no Origin,
// or "null", which a browser sends on the page's own form POST because
// the page is served with Referrer-Policy: no-referrer -- the browser-set
// Sec-Fetch-Site header must read "same-origin". A present Sec-Fetch-Site
// of anything else is refused regardless of Origin. A request carrying
// neither header is refused: only a browser legitimately posts here.
func (s *Server) sameOrigin(r *http.Request) bool {
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	if fetchSite != "" && fetchSite != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "null" {
		got, err := platform.CanonicalOrigin(origin)
		return err == nil && got == s.ids.Origin
	}
	return fetchSite == "same-origin"
}

// ConsentDecision backs POST /oauth/consent (technical plan §43.14).
func (s *Server) ConsentDecision(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	if !s.sameOrigin(r) {
		logger.Warn("mcpauth: consent decision refused", "outcome", "cross_site")
		s.renderError(w, r, http.StatusForbidden, "Request refused", "This decision did not come from this deployment's own consent page.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConsentFormBytes)
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "This decision could not be read", "The form was malformed.")
		return
	}
	form := r.PostForm

	user, ok := auth.Authenticate(ctx, s.deps.UserSessions, s.deps.Users, r)
	if !ok {
		s.renderError(w, r, http.StatusUnauthorized, "Your sign-in has expired", "Sign in again, then start again from the app.")
		return
	}
	var userID pgtype.UUID
	if err := userID.Scan(user.ID); err != nil {
		logger.Error("mcpauth: consent decision: parse user id failed", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The decision could not be recorded.")
		return
	}

	rawRequest, ok1 := singleParam(form, "request")
	nonce, ok2 := singleParam(form, "nonce")
	decision, ok3 := singleParam(form, "decision")
	requestID, ok4 := parseRequestID(rawRequest)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		s.renderError(w, r, http.StatusBadRequest, "This decision could not be read", "The form was malformed.")
		return
	}

	row, err := s.deps.Grants.GetAuthorizationRequest(ctx, requestID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("mcpauth: consent decision: load request failed", "error", err)
		}
		s.renderError(w, r, http.StatusBadRequest, "This authorization request has expired", "It was already decided, or it waited too long. Start again from the app.")
		return
	}
	if !row.UserID.Valid || row.UserID != userID {
		logger.Warn("mcpauth: consent decision refused", "outcome", "other_user")
		s.renderError(w, r, http.StatusForbidden, "This authorization request belongs to another sign-in", "Another account opened this request first. Start again from the app while signed in as the account you want it to use.")
		return
	}
	if row.CsrfNonceHash == nil || subtle.ConstantTimeCompare([]byte(platform.HashToken(nonce)), []byte(*row.CsrfNonceHash)) != 1 {
		logger.Warn("mcpauth: consent decision refused", "outcome", "nonce_mismatch")
		s.renderError(w, r, http.StatusForbidden, "This consent form is out of date", "Reload the consent page and decide again.")
		return
	}
	if !requestUsable(row, time.Now()) {
		s.renderError(w, r, http.StatusBadRequest, "This authorization request has expired", "It was already decided, or it waited too long. Start again from the app.")
		return
	}
	if err := authz.Authorize(authz.Actor{UserID: user.ID, Role: authz.Role(user.Role)}, authz.ActionConnectMCPClient, authz.Resource{}); err != nil {
		logger.Warn("mcpauth: consent decision refused", "outcome", "forbidden", "error", err)
		s.renderError(w, r, http.StatusForbidden, "Your role cannot connect apps", "Your account is not allowed to connect MCP clients on this deployment.")
		return
	}
	if decision != "approve" && decision != "deny" {
		s.renderError(w, r, http.StatusBadRequest, "This decision could not be read", "The form was malformed.")
		return
	}

	// Narrowing only: every selected scope must be one the request asked
	// for; an unrequested one refuses the whole decision rather than being
	// silently dropped.
	requested := make(map[string]bool, len(row.Scopes))
	for _, sc := range row.Scopes {
		requested[sc] = true
	}
	selectedSet := map[mcpscope.Scope]bool{}
	for _, sc := range form["scope"] {
		if !requested[sc] {
			logger.Warn("mcpauth: consent decision refused", "outcome", "unrequested_scope")
			s.renderError(w, r, http.StatusBadRequest, "This decision could not be read", "It named access the app did not ask for.")
			return
		}
		selectedSet[mcpscope.Scope(sc)] = true
	}
	selected := make([]mcpscope.Scope, 0, len(selectedSet))
	for _, v := range mcpscope.Vocabulary {
		if selectedSet[v] {
			selected = append(selected, v)
		}
	}

	s.decide(w, r, requestID, row.ClientID, userID, decision == "approve", selected)
}

// decide records one consent decision in a single transaction and
// redirects to the request's STORED redirect URI -- never a URI read from
// the form. clientID is the request's client (a request's client never
// changes).
func (s *Server) decide(w http.ResponseWriter, r *http.Request, requestID, clientID, userID pgtype.UUID, approve bool, selected []mcpscope.Scope) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	fail := func(msg string, err error) {
		logger.Error("mcpauth: consent decision: "+msg, "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The decision could not be recorded.")
	}

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		fail("begin tx failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grants := s.deps.Grants.WithTx(tx)

	// The client first, FOR KEY SHARE, before the request is consumed:
	// parent before child, the lock order every transaction on these
	// tables follows (the top of postgres/mcpoauthgrant_store.go,
	// technical plan §43.16). A client deletion that got here first is
	// waited out and leaves no client; one that arrives later waits for
	// this decision to commit and then deletes what it granted.
	client, err := s.deps.Clients.WithTx(tx).LockKeyShare(ctx, clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		logger.Warn("mcpauth: consent decision refused", "outcome", "client_deleted")
		s.renderError(w, r, http.StatusBadRequest, "This app is not available", "The app that asked for access is no longer registered on this deployment.")
		return
	}
	if err != nil {
		fail("lock client failed", err)
		return
	}

	consumed, err := grants.ConsumeAuthorizationRequest(ctx, requestID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderError(w, r, http.StatusBadRequest, "This authorization request has expired", "It was already decided, or it waited too long. Start again from the app.")
		return
	}
	if err != nil {
		fail("consume request failed", err)
		return
	}
	state := ""
	if consumed.State != nil {
		state = *consumed.State
	}

	if !approve {
		if err := tx.Commit(ctx); err != nil {
			fail("commit denial failed", err)
			return
		}
		s.redirectError(w, r, consumed.RedirectUri, errAccessDenied, "the user denied the request", state)
		return
	}

	if !s.clientUsable(client) {
		// Disabled -- or its registration mechanism switched off -- after
		// the page was rendered: nothing is granted. The
		// row is the one the lock above returned, and the client cannot
		// be deleted before this transaction ends. The rollback also
		// leaves the request unconsumed, but the page's own render
		// refuses a disabled client, so it can never be decided again.
		logger.Warn("mcpauth: consent decision refused", "outcome", "client_not_usable", "client_id", client.ClientID)
		s.renderError(w, r, http.StatusBadRequest, "This app is not available", "An administrator of this deployment has disabled this app.")
		return
	}
	// The grant records this approval's scopes for display only; what the
	// resulting token may do is the code's own scopes below, fixed now and
	// never re-read from the grant (technical plan §43.16).
	now := time.Now()
	grant, err := grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID:    userID,
		ClientID:  consumed.ClientID,
		Scopes:    mcpscope.Strings(selected),
		Resource:  consumed.Resource,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(s.cfg.Timeouts.MCPGrantMaxLifetime), Valid: true},
	})
	if err != nil {
		fail("record grant failed", err)
		return
	}
	token, err := platform.GenerateToken()
	if err != nil {
		fail("generate code failed", err)
		return
	}
	code := authorizationCodePrefix + token
	if _, err := grants.CreateAuthorizationCode(ctx, sqlcgen.CreateMCPOAuthAuthorizationCodeParams{
		GrantID:       grant.ID,
		CodeHash:      platform.HashToken(code),
		CodeChallenge: consumed.CodeChallenge,
		RedirectUri:   consumed.RedirectUri,
		Resource:      consumed.Resource,
		Scopes:        mcpscope.Strings(selected),
		ExpiresAt:     pgtype.Timestamptz{Time: now.Add(s.cfg.Timeouts.MCPAuthorizationCodeTTL), Valid: true},
	}); err != nil {
		fail("record code failed", err)
		return
	}
	if err := auditlog.Record(ctx, s.deps.AuditLog.WithTx(tx), userID, "mcp_authorization.granted", "mcp_authorization", grant.ID.String(), map[string]any{
		"client_id":     client.ClientID,
		"client_name":   client.ClientName,
		"scopes":        mcpscope.Strings(selected),
		"redirect_host": mcpclient.RedirectHost(consumed.RedirectUri),
	}); err != nil {
		fail("record audit failed", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail("commit approval failed", err)
		return
	}

	params := url.Values{}
	params.Set("code", code)
	if state != "" {
		params.Set("state", state)
	}
	s.redirectToClient(w, r, consumed.RedirectUri, params)
}
