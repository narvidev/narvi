package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/platform"
)

// OIDCCallbackOutcome names each distinct branch NewOIDCCallbackHandler's
// own flow can take -- mirrors CallbackOutcome's own role for the GitHub
// flow (callback.go) exactly: used only in server-side log lines (the
// HTTP response body never distinguishes why a request was rejected, same
// security note as doc.go's own GitHub-flow discipline) and by this
// package's own tests.
type OIDCCallbackOutcome string

// The full outcome table (see NewOIDCCallbackHandler's own doc comment
// for the complete branch-by-branch writeup).
const (
	OIDCOutcomeReturningUser    OIDCCallbackOutcome = "oidc_returning_user"
	OIDCOutcomeAutoLinked       OIDCCallbackOutcome = "oidc_auto_linked"
	OIDCOutcomeFirstTimeAllowed OIDCCallbackOutcome = "oidc_first_time_allowed"
	OIDCOutcomeFirstTimeDenied  OIDCCallbackOutcome = "oidc_first_time_denied"
	OIDCOutcomeAmbiguousMatch   OIDCCallbackOutcome = "oidc_ambiguous_match"
	OIDCOutcomeStateMismatch    OIDCCallbackOutcome = "oidc_state_mismatch"
	OIDCOutcomeExchangeFailed   OIDCCallbackOutcome = "oidc_exchange_failed"
	OIDCOutcomeMissingIDToken   OIDCCallbackOutcome = "oidc_missing_id_token"
	OIDCOutcomeVerifyFailed     OIDCCallbackOutcome = "oidc_verify_failed"
	OIDCOutcomeNonceMismatch    OIDCCallbackOutcome = "oidc_nonce_mismatch"
	OIDCOutcomeAudienceMismatch OIDCCallbackOutcome = "oidc_audience_mismatch"
	OIDCOutcomeEmailNotVerified OIDCCallbackOutcome = "oidc_email_not_verified"
)

// NewOIDCCallbackHandler backs GET /auth/oidc/callback (§41.3).
//
// # The outcome table
//
//   - Missing/mismatched `state` query param vs. the narvi_oidc_state
//     cookie -> 400 (OIDCOutcomeStateMismatch). The token exchange is
//     NEVER attempted -- mirrors NewCallbackHandler's own identical GitHub
//     check exactly.
//   - oauth2Config.Exchange failure (bad/reused code, wrong or missing
//     PKCE code_verifier, a genuine backend/network problem) -> 401
//     (OIDCOutcomeExchangeFailed) -- same "not distinguished" simplification
//     NewCallbackHandler's own doc comment already makes for GitHub.
//   - No id_token in the token response -> 502 (OIDCOutcomeMissingIDToken):
//     a malformed/non-compliant IdP, not a caller mistake.
//   - verifier.Verify failure (bad signature, wrong issuer, expired token)
//     -> 401 (OIDCOutcomeVerifyFailed) -- go-oidc's own job; this package
//     never re-implements it.
//   - ID token's own `nonce` claim != the narvi_oidc_nonce cookie -> 401
//     (OIDCOutcomeNonceMismatch).
//   - ID token's own `aud` does not contain the configured client id -> 401
//     (OIDCOutcomeAudienceMismatch).
//   - `email_verified` claim absent, false, or not literally the JSON
//     boolean true (e.g. a string "true") -> 403
//     (OIDCOutcomeEmailNotVerified) -- §41.3: "Never fall back to an
//     unverified email."
//   - Identity already linked (GetByProviderAndExternalID hits) ->
//     "returning user" (OIDCOutcomeReturningUser) -- a fresh session is
//     minted, 302 to "/". The allowlist is skipped entirely, exactly like
//     GitHub's own returning-user branch.
//   - Identity not linked, but the verified email matches EXACTLY ONE
//     existing user (by primary_email or another verified identity's own
//     email) -> auto-linked onto that SAME user (OIDCOutcomeAutoLinked,
//     identitylink.AutoLink) -- §41.3: "the graph merges on verified email
//     exactly as §13.2 step 3 does for Slack and Linear." The allowlist is
//     skipped here too: this user already passed it once, under whichever
//     identity first created their account.
//   - Identity not linked, zero existing users match the verified email,
//     allowlist passes -> a BRAND NEW user+identity is created
//     (OIDCOutcomeFirstTimeAllowed, createOIDCUserAndIdentity) -- the same
//     users row model, default-role assignment, and allowlist gate as
//     GitHub's own first-time-sign-in branch.
//   - Identity not linked, zero existing users match, allowlist fails ->
//     403 (OIDCOutcomeFirstTimeDenied); no user/identity/session row is
//     ever created.
//   - Identity not linked, MORE THAN ONE existing user matches the
//     verified email -> 403 (OIDCOutcomeAmbiguousMatch), audited, no
//     row ever created -- §13.2's own "never guess" rule: unlike Slack/
//     Linear's own async ingress (which can defer to a magic-link prompt
//     while the action proceeds under bot attribution), a live sign-in
//     has no "proceed under bot attribution" fallback to defer to -- the
//     honest response is to refuse this sign-in outright, loudly enough
//     to page whoever owns identity hygiene for this deployment, rather
//     than silently guess which of several accounts just proved control
//     of one email address.
//
// pool/users/identities/auditLog/userSessions/allowlist/initialAdminEmails/
// timeouts/secureCookies mirror NewCallbackHandler's own identical
// parameters one-for-one (callback.go) -- the SAME stores, the SAME
// allowlist config, reused rather than a second, independently-wired
// copy of any of them. linkPrompts is the SAME identity_link_prompts
// store the Slack/Linear auto-link algorithm already uses
// (internal/app/identitylink.Deps.LinkPrompts) -- threaded through here
// because identitylink.AutoLink (called from resolveOIDCUser's own graph
// -merge branch) unconditionally deletes any still-pending prompt for the
// identity it just linked, exactly like it does for Slack/Linear.
func NewOIDCCallbackHandler(
	pool *pgxpool.Pool,
	cache *OIDCProviderCache,
	users *postgres.UserStore,
	identities *postgres.IdentityStore,
	auditLog *postgres.AuditLogStore,
	userSessions *postgres.UserSessionStore,
	linkPrompts *postgres.IdentityLinkPromptStore,
	allowlist AllowlistConfig,
	initialAdminEmails []string,
	timeouts platform.Timeouts,
	secureCookies bool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		// This route is mounted UNCONDITIONALLY (controlplane/serve.go's
		// own doc comment on why) -- mirrors NewOIDCLoginHandler's own
		// identical guard.
		if !cache.cfg.Configured() {
			http.Error(w, "oidc sign-in is not configured for this deployment", http.StatusServiceUnavailable)
			return
		}

		// a. State check: exact-match against the short-lived cookie set
		// by NewOIDCLoginHandler -- consumed (cleared) regardless of the
		// outcome, so it can never be replayed. Mirrors
		// NewCallbackHandler's own identical GitHub check exactly.
		stateCookie, stateErr := r.Cookie(oidcStateCookieName)
		queryState := r.URL.Query().Get("state")
		http.SetCookie(w, expiredCookie(oidcStateCookieName, secureCookies))
		if stateErr != nil || stateCookie.Value == "" || stateCookie.Value != queryState {
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeStateMismatch)
			http.Error(w, "invalid or missing oidc state", http.StatusBadRequest)
			return
		}

		// nonce/verifier cookies are read+cleared here too, alongside
		// state, rather than lazily later -- every pre-auth cookie this
		// flow minted is consumed in exactly one place, at the top of
		// this handler, so no early-return path below can ever leave one
		// of them un-cleared.
		nonceCookie, nonceErr := r.Cookie(oidcNonceCookieName)
		http.SetCookie(w, expiredCookie(oidcNonceCookieName, secureCookies))
		verifierCookie, verifierErr := r.Cookie(oidcVerifierCookieName)
		http.SetCookie(w, expiredCookie(oidcVerifierCookieName, secureCookies))
		if nonceErr != nil || nonceCookie.Value == "" || verifierErr != nil || verifierCookie.Value == "" {
			// Belt and suspenders: state already matched above (state and
			// nonce/verifier are minted and cleared together by
			// NewOIDCLoginHandler, so this should be unreachable in
			// practice), but a request missing either is exactly as
			// unusable as a state mismatch -- refuse the same way rather
			// than proceed with an empty expected-nonce/verifier value.
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeStateMismatch)
			http.Error(w, "invalid or missing oidc state", http.StatusBadRequest)
			return
		}

		rt, err := cache.get(ctx)
		if err != nil {
			logger.Error("auth: oidc callback discovery failed", "error", err)
			http.Error(w, "oidc provider unavailable", http.StatusServiceUnavailable)
			return
		}

		// b. Exchange the code for a token, presenting the PKCE code
		// verifier this same flow's own login handler minted -- a wrong
		// or missing verifier is refused by the IdP's own token endpoint,
		// surfacing here as an ordinary Exchange error (§41.3's own
		// "missing/incorrect PKCE verifier ... refused").
		code := r.URL.Query().Get("code")
		oauth2Token, err := rt.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(verifierCookie.Value))
		if err != nil {
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeExchangeFailed, "error", err)
			http.Error(w, "oidc exchange failed", http.StatusUnauthorized)
			return
		}

		rawIDToken, ok := oauth2Token.Extra("id_token").(string)
		if !ok || rawIDToken == "" {
			logger.Error("auth: oidc callback rejected", "outcome", OIDCOutcomeMissingIDToken)
			http.Error(w, "oidc provider returned no id_token", http.StatusBadGateway)
			return
		}

		// c. Verify the ID token: signature, issuer, and expiry are
		// go-oidc's own job (rt.verifier was built with
		// SkipClientIDCheck: true -- OIDCProviderCache.get's own doc
		// comment explains why the audience check below is this
		// package's own, explicit responsibility instead).
		idToken, err := rt.verifier.Verify(ctx, rawIDToken)
		if err != nil {
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeVerifyFailed, "error", err)
			http.Error(w, "oidc id token verification failed", http.StatusUnauthorized)
			return
		}

		// d. Nonce check: the ID token's own nonce claim (go-oidc reads it
		// off the token, IDToken.Nonce) must equal the value THIS
		// browser's own login request minted -- binds the ID token to
		// this specific browser's own pre-auth cookie, exactly like state
		// binds the authorization code to it.
		if idToken.Nonce == "" || idToken.Nonce != nonceCookie.Value {
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeNonceMismatch)
			http.Error(w, "oidc nonce mismatch", http.StatusUnauthorized)
			return
		}

		// e. Audience check, explicit (see rt.verifier's own construction
		// comment for why this is not delegated to go-oidc).
		if !audienceContains(idToken.Audience, cache.cfg.ClientID) {
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeAudienceMismatch)
			http.Error(w, "oidc audience mismatch", http.StatusUnauthorized)
			return
		}

		// f. email_verified: ONLY the `email` claim with `email_verified`
		// === the JSON boolean true is accepted -- §41.3: absent, false,
		// or any non-boolean value (e.g. a string "true") is refused, with
		// an audited reason (this log line -- the same audit-worthy-log
		// path callback.go's own OutcomeNoVerifiedEmail refusal uses for
		// the identical GitHub-side gap).
		var claims map[string]any
		if err := idToken.Claims(&claims); err != nil {
			logger.Error("auth: oidc callback: decode id token claims failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		email, emailVerified := verifiedOIDCEmail(claims)
		if !emailVerified {
			logger.Warn("auth: oidc callback rejected", "outcome", OIDCOutcomeEmailNotVerified)
			http.Error(w, "no verified email", http.StatusForbidden)
			return
		}

		externalID := oidcExternalID(idToken.Issuer, idToken.Subject)

		userID, outcome, httpStatus, publicMsg := resolveOIDCUser(ctx, oidcResolveDeps{
			pool:               pool,
			users:              users,
			identities:         identities,
			auditLog:           auditLog,
			linkPrompts:        linkPrompts,
			allowlist:          allowlist,
			initialAdminEmails: initialAdminEmails,
		}, externalID, email, claims)
		if httpStatus != 0 {
			logger.Warn("auth: oidc callback rejected", "outcome", outcome)
			http.Error(w, publicMsg, httpStatus)
			return
		}
		logger.Info("auth: oidc callback", "outcome", outcome)

		// g. Mint a fresh user-session -- identical shape to
		// NewCallbackHandler's own final step (callback.go).
		sessionToken, err := platform.GenerateToken()
		if err != nil {
			logger.Error("auth: generate user-session token failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		expiresAt := time.Now().Add(timeouts.UserSessionTTL)
		if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
			UserID:    userID,
			TokenHash: platform.HashToken(sessionToken),
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		}); err != nil {
			logger.Error("auth: create user session failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, platform.WithAuthSessionCookie(sessionToken, expiresAt, secureCookies))
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// oidcExternalID renders §41.3's own "external_id = {issuer}|{sub}"
// format -- issuer-qualified, since two different IdPs can legitimately
// issue the same sub value for two different real people.
func oidcExternalID(issuer, sub string) string {
	return issuer + "|" + sub
}

// audienceContains reports whether aud (an ID token's own, possibly
// multi-valued, audience claim) contains clientID.
func audienceContains(aud []string, clientID string) bool {
	for _, a := range aud {
		if a == clientID {
			return true
		}
	}
	return false
}

// verifiedOIDCEmail extracts claims["email"]/claims["email_verified"] and
// reports whether both are present in the ONE shape §41.3 accepts: email
// a non-empty string, email_verified the literal JSON boolean true --
// never "truthy" in any looser sense. A type assertion on an interface{}
// decoded from encoding/json is exactly the right tool here: json.
// Unmarshal into `any` decodes a JSON boolean as Go bool and nothing
// else, so `v, ok := claims["email_verified"].(bool)` is false for every
// one of §41.3's named rejection shapes in one uniform check --
// email_verified ABSENT (map lookup itself returns the zero value/ok=false
// before the type assertion is even reached), FALSE (ok=true, v=false --
// rejected by `!v`), and a STRING "true" (the type assertion itself fails,
// ok=false) all take the same `return "", false` path, with no special
// case required for any of the three.
func verifiedOIDCEmail(claims map[string]any) (string, bool) {
	email, emailOK := claims["email"].(string)
	if !emailOK || email == "" {
		return "", false
	}
	verified, verifiedOK := claims["email_verified"].(bool)
	if !verifiedOK || !verified {
		return "", false
	}
	return email, true
}

// oidcResolveDeps bundles resolveOIDCUser's own store/config dependencies.
type oidcResolveDeps struct {
	pool               *pgxpool.Pool
	users              *postgres.UserStore
	identities         *postgres.IdentityStore
	auditLog           *postgres.AuditLogStore
	linkPrompts        *postgres.IdentityLinkPromptStore
	allowlist          AllowlistConfig
	initialAdminEmails []string
}

// auditActionOIDCAmbiguousMatch is the audit_log action recorded when a
// first-time OIDC identity's verified email matches more than one
// existing user -- kept as its own named constant, and passed explicitly
// to resolveFirstTimeIdentity (firsttimeidentity.go), so this exact
// string survives byte-for-byte across the review-round-1 refactor that
// made the ambiguous-match branch itself shared with callback.go's own
// GitHub callback (findings O1/O2/O9) -- this package's own pre-existing
// tests (e.g. TestOIDCCallback_AmbiguousMatch_Refused) assert this exact
// value.
const auditActionOIDCAmbiguousMatch = "identity.oidc_ambiguous_match"

// resolveOIDCUser implements this file's own steps after the ID token is
// fully verified: returning-user fast path, then -- for a genuinely new
// identity -- resolveFirstTimeIdentity's own shared §13.2 step 3 graph
// merge (firsttimeidentity.go), the SAME function callback.go's own
// GitHub callback now shares this logic with (review round 1, findings
// O1/O2/O9: before that share existed, only THIS file ran the merge, so
// an OIDC-only user's later GitHub sign-in could never merge onto their
// existing account).
//
// Returns (userID, outcome, 0, "") on success (httpStatus==0 is the
// caller's own "proceed to mint a session" signal), or (invalid,
// outcome, httpStatus, publicMsg) on any refusal -- mirrors this file's
// own outcome-table doc comment on NewOIDCCallbackHandler.
func resolveOIDCUser(ctx context.Context, deps oidcResolveDeps, externalID, email string, claims map[string]any) (pgtype.UUID, OIDCCallbackOutcome, int, string) {
	existing, err := deps.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, externalID)
	if err == nil {
		return existing.UserID, OIDCOutcomeReturningUser, 0, ""
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, "", http.StatusInternalServerError, "internal error"
	}

	firstTimeDeps := firstTimeIdentityDeps{
		pool:        deps.pool,
		users:       deps.users,
		identities:  deps.identities,
		auditLog:    deps.auditLog,
		linkPrompts: deps.linkPrompts,
	}
	// checkAllowed mirrors this file's own pre-existing zero-match check
	// exactly: OIDC has no org-membership mechanism to check (that is a
	// GitHub-specific concept), so this is the email/domain allowlist
	// alone -- deliberately generic on refusal, matching callback.go's own
	// identical "does not say which mechanism almost matched" discipline
	// (enumeration hardening).
	checkAllowed := func() bool { return deps.allowlist.EmailAllowed(email) }
	createFirstTime := func(ctx context.Context) (pgtype.UUID, error) {
		return createOIDCUserAndIdentity(ctx, deps.pool, deps.users, deps.identities, deps.auditLog, oidcUserAndIdentityParams{
			verifiedEmail:      email,
			displayName:        oidcDisplayName(claims, email),
			externalID:         externalID,
			initialAdminEmails: deps.initialAdminEmails,
		})
	}

	userID, outcome, err := resolveFirstTimeIdentity(ctx, firstTimeDeps, sqlcgen.IdentityProviderOidc, externalID, email, nil, auditActionOIDCAmbiguousMatch, checkAllowed, createFirstTime)
	if err != nil {
		return pgtype.UUID{}, "", http.StatusInternalServerError, "internal error"
	}

	switch outcome {
	case firstTimeAutoLinked:
		return userID, OIDCOutcomeAutoLinked, 0, ""
	case firstTimeCreated:
		return userID, OIDCOutcomeFirstTimeAllowed, 0, ""
	case firstTimeDenied:
		return pgtype.UUID{}, OIDCOutcomeFirstTimeDenied, http.StatusForbidden, "not authorized to sign up"
	default: // firstTimeAmbiguous
		// §13.2's own "never guess" rule, and (unlike Slack/Linear) this
		// live sign-in has no bot-attribution fallback to defer to; see
		// NewOIDCCallbackHandler's own outcome-table doc comment for the
		// full reasoning.
		return pgtype.UUID{}, OIDCOutcomeAmbiguousMatch, http.StatusForbidden, "not authorized to sign up"
	}
}

// oidcDisplayName prefers the standard OIDC `name` claim when present and
// non-empty, falling back to the verified email -- mirrors
// createUserAndIdentityParams' own githubLogin/githubName fallback
// (callback.go) one level simpler: a generic OIDC provider is not
// guaranteed to send anything beyond the standard claims this package
// already reads.
func oidcDisplayName(claims map[string]any, verifiedEmail string) string {
	if name, ok := claims["name"].(string); ok && name != "" {
		return name
	}
	return verifiedEmail
}

// oidcUserAndIdentityParams bundles createOIDCUserAndIdentity's inputs --
// mirrors createUserAndIdentityParams' own role one function over
// (callback.go), narrowed to what a generic OIDC identity actually has:
// no encryptedToken (this package never stores an OIDC provider token --
// §41.3 names no downstream use for one, unlike GitHub's own stored token,
// which §8.11's PR-attribution flow reads).
type oidcUserAndIdentityParams struct {
	verifiedEmail      string
	displayName        string
	externalID         string
	initialAdminEmails []string
}

// createOIDCUserAndIdentity runs the OIDC first-time-sign-in write path --
// the SAME shape as callback.go's own createUserAndIdentity (a users row
// then an identities row then a "user.created" audit_log row, in ONE
// Postgres transaction, §13.1's own explicit requirement), applying the
// SAME resolveInitialRole rule GitHub sign-in uses. Kept as a distinct
// function rather than a literal call to createUserAndIdentity: the two
// callers' own input shapes differ enough (no GitHub login/name pair, no
// encrypted provider token, a different identities.provider value) that
// forcing both through one signature would mean threading unused
// GitHub-only fields through this caller for no benefit -- see
// resolveInitialRole's own doc comment for the piece that IS shared
// rather than duplicated.
func createOIDCUserAndIdentity(ctx context.Context, pool *pgxpool.Pool, users *postgres.UserStore, identities *postgres.IdentityStore, auditLog *postgres.AuditLogStore, p oidcUserAndIdentityParams) (pgtype.UUID, error) {
	role := resolveInitialRole(p.verifiedEmail, p.initialAdminEmails)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: begin oidc user-creation tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	createdUser, err := users.WithTx(tx).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: p.verifiedEmail,
		DisplayName:  p.displayName,
		Role:         role,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: create oidc user: %w", err)
	}

	// linked_via="admin" mirrors createUserAndIdentity's own identical,
	// deliberate overload -- see that function's own doc comment for the
	// full "least-wrong of the 3 existing enum values" reasoning; it
	// applies identically here.
	if _, err := identities.WithTx(tx).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        createdUser.ID,
		Provider:      sqlcgen.IdentityProviderOidc,
		ExternalID:    p.externalID,
		Email:         &p.verifiedEmail,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: create oidc identity: %w", err)
	}

	if err := auditlog.Record(ctx, auditLog.WithTx(tx), createdUser.ID, "user.created", "user", createdUser.ID.String(), map[string]any{
		"role":     string(role),
		"provider": string(sqlcgen.IdentityProviderOidc),
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: record oidc user-creation audit log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: commit oidc user-creation tx: %w", err)
	}

	return createdUser.ID, nil
}
