package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// githubUser is the subset of GET /user's response body this Step reads
// (verified live against docs.github.com during this Step's design
// phase): id (int64) and login (string). The response also carries a
// top-level, nullable "email" field -- deliberately NEVER read here: it
// may be unverified, so githubEmail (below), fetched from the SEPARATE
// /user/emails endpoint, is the ONLY source of the verified primary email
// this package trusts.
type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

// githubEmail is one entry of GET /user/emails's response array (verified
// live against docs.github.com). Only an entry with Primary && Verified is
// ever trusted as the user's real email -- see fetchVerifiedPrimaryEmail.
type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// CallbackOutcome names each distinct branch NewCallbackHandler's own flow
// can take. Used only in server-side log lines (see this package's own
// security note: the HTTP response body never distinguishes WHY a request
// was rejected, so an attacker probing the endpoint gets no differential
// signal beyond the status code) and by this package's own tests.
type CallbackOutcome string

// The full outcome table (see doc.go for the complete branch-by-branch
// writeup).
const (
	OutcomeReturningUser        CallbackOutcome = "returning_user"
	OutcomeAutoLinked           CallbackOutcome = "auto_linked"
	OutcomeFirstTimeAllowed     CallbackOutcome = "first_time_allowed"
	OutcomeFirstTimeDenied      CallbackOutcome = "first_time_denied"
	OutcomeAmbiguousMatch       CallbackOutcome = "ambiguous_match"
	OutcomeSameProviderConflict CallbackOutcome = "same_provider_conflict"
	OutcomeNoVerifiedEmail      CallbackOutcome = "no_verified_email"
	OutcomeStateMismatch        CallbackOutcome = "state_mismatch"
	OutcomeExchangeFailed       CallbackOutcome = "exchange_failed"
)

// auditActionGitHubAmbiguousMatch is the audit_log action recorded when a
// first-time GitHub identity's verified email matches more than one
// existing user -- the GitHub-side counterpart of oidccallback.go's own
// auditActionOIDCAmbiguousMatch, kept as a DISTINCT string (never the
// same constant) so an operator reading audit_log can tell which
// provider's sign-in attempt hit the "never guess" refusal without
// having to cross-reference detail_json's own "provider" key.
const auditActionGitHubAmbiguousMatch = "identity.github_ambiguous_match"

// auditActionGitHubSameProviderConflict is the audit_log action recorded
// when a first-time GitHub sign-in's verified email matches exactly one
// existing user who ALREADY has a (different) github identity (review
// round 2, findings P2/P3) -- refused rather than merged, see
// resolveFirstTimeIdentity's own identityConflictsWithExistingProvider
// doc comment.
const auditActionGitHubSameProviderConflict = "identity.github_already_linked"

// NewCallbackHandler backs GET /auth/github/callback (§13.1/§13.2/§13.4).
// See doc.go for the complete outcome table this flow implements.
//
// apiBaseURL is an overridable parameter -- NEVER a hardcoded
// "https://api.github.com" literal in this function body -- specifically
// so this Step's own tests can point it at a local httptest.Server
// standing in for GitHub's REST API (§13's own design decision 12).
// Production wiring (cmd/control-plane/main.go) passes the real
// "https://api.github.com".
//
// pool and initialAdminEmails are threaded through in addition to the
// stores/allowlist/etc. named directly by this Step's own design decisions:
// pool is what lets the first-time-sign-in branch run UserStore.Create and
// IdentityStore.Create inside ONE real Postgres transaction (§13.1: "in ONE
// transaction"); initialAdminEmails is what lets that same branch decide
// admin-vs-member at creation time (§13.4: "initial admins set by config").
//
// auditLog (audit-fix batch, observability M18) is what lets that SAME
// first-time-sign-in branch write a "user.created" audit_log row inside
// that identical transaction -- see createUserAndIdentity's own doc
// comment for why a first-time sign-in (bootstrap admin included) was
// previously the one identity/role mutation in this codebase with no
// audit trail at all.
//
// linkPrompts (review round 1, findings O1/O2/O9) is the SAME
// identity_link_prompts store NewOIDCCallbackHandler already takes --
// threaded through here too because this handler's own first-time branch
// now shares resolveFirstTimeIdentity (firsttimeidentity.go) with the
// OIDC callback: identitylink.AutoLink (called from that shared branch's
// own graph-merge match) unconditionally deletes any still-pending link
// prompt for the identity it just linked, exactly like it does for
// Slack/Linear/OIDC.
func NewCallbackHandler(
	pool *pgxpool.Pool,
	oauthConfig *oauth2.Config,
	users *postgres.UserStore,
	identities *postgres.IdentityStore,
	auditLog *postgres.AuditLogStore,
	userSessions *postgres.UserSessionStore,
	linkPrompts *postgres.IdentityLinkPromptStore,
	allowlist AllowlistConfig,
	initialAdminEmails []string,
	tokenEncryptionKey []byte,
	timeouts platform.Timeouts,
	secureCookies bool,
	apiBaseURL string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		// a. State check: exact-match against the short-lived cookie set by
		// NewLoginHandler. Missing or mismatched -> 400, the token exchange
		// is NEVER attempted -- this is exactly the CSRF protection state
		// exists for.
		stateCookie, cookieErr := r.Cookie(oauthStateCookieName)
		queryState := r.URL.Query().Get("state")
		if cookieErr != nil || stateCookie.Value == "" || stateCookie.Value != queryState {
			logger.Warn("auth: oauth callback rejected", "outcome", OutcomeStateMismatch)
			http.Error(w, "invalid or missing oauth state", http.StatusBadRequest)
			return
		}
		// Consumed: clear the state cookie regardless of what happens next,
		// so it can never be replayed.
		http.SetCookie(w, expiredCookie(oauthStateCookieName, secureCookies))

		// §13.2 ("identities + full RBAC", §13.2) update: read and clear
		// the optional next-redirect cookie (login.go's own doc comment)
		// the SAME way, right alongside the state cookie -- read ONCE,
		// here, before any of this handler's own early-rejection paths
		// below; redirectTarget defaults to "/" (this handler's own
		// PRE-EXISTING behavior, completely unchanged when no next cookie
		// was ever set) and is only overridden on a SUCCESSFUL sign-in
		// further down. isSafeRedirectNext is checked again here (not just
		// trusted from login.go's own already-applied check) -- defense in
		// depth against a tampered/forged cookie value.
		redirectTarget := "/"
		if nextCookie, nextErr := r.Cookie(oauthNextCookieName); nextErr == nil && isSafeRedirectNext(nextCookie.Value) {
			redirectTarget = nextCookie.Value
		}
		http.SetCookie(w, expiredCookie(oauthNextCookieName, secureCookies))

		// b. Exchange the code for a token.
		code := r.URL.Query().Get("code")
		token, err := oauthConfig.Exchange(ctx, code)
		if err != nil {
			// A failed exchange is either a bad/reused code (client-caused)
			// or a genuine backend/network problem (server-caused) --
			// x/oauth2 wraps both similarly (a *oauth2.RetrieveError from
			// GitHub's own token endpoint, or a plain transport error), and
			// reliably telling them apart would mean heuristically parsing
			// GitHub's own error body. Simplification, documented honestly
			// rather than silently picked: this always responds 401,
			// treating every exchange failure as "the presented code was
			// not valid" from the caller's point of view.
			logger.Warn("auth: oauth callback rejected", "outcome", OutcomeExchangeFailed, "error", err)
			http.Error(w, "oauth exchange failed", http.StatusUnauthorized)
			return
		}

		httpClient := oauthConfig.Client(ctx, token)
		// Never follow redirects automatically: checkOrgMembership's own
		// fail-closed logic (see that function's doc comment) depends on
		// observing GitHub's literal 302 status code, not whatever page it
		// would otherwise redirect to.
		httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}

		ghUser, err := fetchGitHubUser(ctx, httpClient, apiBaseURL)
		if err != nil {
			logger.Error("auth: fetch github user failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		verifiedEmail, hasVerifiedEmail, err := fetchVerifiedPrimaryEmail(ctx, httpClient, apiBaseURL)
		if err != nil {
			logger.Error("auth: fetch github emails failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !hasVerifiedEmail {
			logger.Warn("auth: oauth callback rejected", "outcome", OutcomeNoVerifiedEmail, "github_login", ghUser.Login)
			http.Error(w, "no verified primary email", http.StatusForbidden)
			return
		}

		// Encrypt now (never log the plaintext OR the resulting ciphertext
		// -- see this package's own security notes in doc.go) so both the
		// returning-user and first-time branches below share one encrypted
		// value.
		encryptedToken, err := platform.EncryptToken(tokenEncryptionKey, []byte(token.AccessToken))
		if err != nil {
			logger.Error("auth: encrypt provider token failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		externalID := strconv.FormatInt(ghUser.ID, 10)

		var userID pgtype.UUID
		existing, err := identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, externalID)
		switch {
		case err == nil:
			// d (returning user): skip the allowlist entirely (§13.1's own
			// "evaluated at first sign-in" -- re-checking every login would
			// let a later allowlist tightening silently lock out an
			// already-provisioned user mid-session, which is not what
			// "at first sign-in" means).
			if _, updateErr := identities.UpdateAccessToken(ctx, sqlcgen.UpdateIdentityAccessTokenParams{
				ID:                   existing.ID,
				AccessTokenEncrypted: encryptedToken,
			}); updateErr != nil {
				logger.Error("auth: update access token failed", "error", updateErr)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			userID = existing.UserID
			logger.Info("auth: oauth callback", "outcome", OutcomeReturningUser)

		case errors.Is(err, pgx.ErrNoRows):
			// d (first-time sign-in): §13.2 step 3's own email-based graph
			// merge (review round 1, findings O1/O2/O9) -- shared,
			// verbatim, with oidccallback.go's own identical first-time
			// branch (resolveFirstTimeIdentity, firsttimeidentity.go), so
			// an OIDC-only user's verified email is found here exactly the
			// same way an OIDC sign-in finds a GitHub-only user's. Only
			// once that merge finds ZERO matching users is the allowlist
			// (including the org-membership check, a real GitHub API call)
			// evaluated at all -- checkAllowed below is called lazily, so
			// a merge match never pays for it.
			checkAllowed := func() bool {
				allowed := allowlist.EmailAllowed(verifiedEmail)
				if !allowed && len(allowlist.GitHubOrgs) > 0 {
					allowed = checkAnyOrgMembership(ctx, httpClient, apiBaseURL, ghUser.Login, allowlist.GitHubOrgs)
				}
				return allowed
			}
			createFirstTime := func(ctx context.Context) (pgtype.UUID, error) {
				return createUserAndIdentity(ctx, pool, users, identities, auditLog, createUserAndIdentityParams{
					verifiedEmail:      verifiedEmail,
					githubLogin:        ghUser.Login,
					githubName:         ghUser.Name,
					externalID:         externalID,
					encryptedToken:     encryptedToken,
					initialAdminEmails: initialAdminEmails,
				})
			}

			firstTimeDeps := firstTimeIdentityDeps{
				pool:        pool,
				users:       users,
				identities:  identities,
				auditLog:    auditLog,
				linkPrompts: linkPrompts,
			}
			resolvedUserID, outcome, resolveErr := resolveFirstTimeIdentity(ctx, firstTimeDeps, sqlcgen.IdentityProviderGithub, externalID, verifiedEmail, encryptedToken, auditActionGitHubAmbiguousMatch, auditActionGitHubSameProviderConflict, checkAllowed, createFirstTime)
			if resolveErr != nil {
				logger.Error("auth: create user+identity failed", "error", resolveErr)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			switch outcome {
			case firstTimeAutoLinked:
				userID = resolvedUserID
				logger.Info("auth: oauth callback", "outcome", OutcomeAutoLinked)
			case firstTimeCreated:
				userID = resolvedUserID
				logger.Info("auth: oauth callback", "outcome", OutcomeFirstTimeAllowed)
			case firstTimeDenied:
				// Deliberately generic: does not say WHICH of the 3
				// mechanisms almost matched -- that's enumeration
				// information an attacker could use to probe the
				// allowlist's own configuration.
				logger.Warn("auth: oauth callback rejected", "outcome", OutcomeFirstTimeDenied)
				http.Error(w, "not authorized to sign up", http.StatusForbidden)
				return
			case firstTimeSameProviderConflict:
				// Review round 2, findings P2/P3: already audited (its own
				// action string) inside resolveFirstTimeIdentity above --
				// same generic public body as every other refusal in this
				// class, never distinguishing WHY.
				logger.Warn("auth: oauth callback rejected", "outcome", OutcomeSameProviderConflict)
				http.Error(w, "not authorized to sign up", http.StatusForbidden)
				return
			default: // firstTimeAmbiguous
				logger.Warn("auth: oauth callback rejected", "outcome", OutcomeAmbiguousMatch)
				http.Error(w, "not authorized to sign up", http.StatusForbidden)
				return
			}

		default:
			logger.Error("auth: lookup identity failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// e. Mint a fresh user-session either way.
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
		// redirectTarget is "/" (there is no SPA to land on yet -- Phase 6
		// is what makes "/" a meaningful landing page; an intentional,
		// forward-compatible interim behavior, not a bug) UNLESS a caller
		// arrived via a real ?next= redirect (§13.2's own addition,
		// this func's own top -- e.g. internal/adapters/inbound/
		// identitylink's magic-link consume handler sending a signed-out
		// visitor through this same flow).
		http.Redirect(w, r, redirectTarget, http.StatusFound)
	}
}

// createUserAndIdentityParams bundles createUserAndIdentity's inputs,
// avoiding an unwieldy positional-argument list.
type createUserAndIdentityParams struct {
	verifiedEmail      string
	githubLogin        string
	githubName         string
	externalID         string
	encryptedToken     []byte
	initialAdminEmails []string
}

// resolveInitialRole implements §13.4's own "initial admins set by
// config" rule: a verified email matching any entry in initialAdminEmails
// (case-insensitive) becomes admin, every other first-time sign-in
// defaults to member. Extracted out of createUserAndIdentity so
// oidccallback.go's own generic-provider first-time-sign-in path
// (createOIDCUserAndIdentity) applies the EXACT SAME role-assignment rule
// GitHub sign-in always has, rather than a second, drifting copy of this
// loop -- the plan's own §41.3 requirement ("the same default-role
// assignment... as GitHub sign-in").
func resolveInitialRole(verifiedEmail string, initialAdminEmails []string) sqlcgen.UserRole {
	for _, adminEmail := range initialAdminEmails {
		if strings.EqualFold(adminEmail, verifiedEmail) {
			return sqlcgen.UserRoleAdmin
		}
	}
	return sqlcgen.UserRoleMember
}

// createUserAndIdentity runs the first-time-sign-in write path: a users row
// then an identities row, then a "user.created" audit_log row, in ONE
// Postgres transaction (§13.1's own explicit requirement) so a failure
// partway through never leaves an orphaned user with no identity (or no
// audit trail) or vice versa.
//
// Audit-fix batch (observability, M18): this was previously the one
// identity/role mutation in this codebase's own audit-fix series with NO
// auditlog.Record call at all -- including the bootstrap-admin case, where
// a verified email matching initialAdminEmails silently grants the admin
// role with no audit trail (§13.3/auditlog.Record's own doc comment: "the
// audit row is not best-effort, it is transactionally bound to the change
// it describes"). actorUserID is the newly-created user's OWN id, not a
// NULL/system actor: unlike a genuinely system-triggered transition (e.g.
// sessionactor's own "plan.superseded" row, attributed to no one because
// nothing a human did caused it), THIS event has a real, meaningful human
// actor -- the person who just authenticated is exactly who this action
// should be attributed to; there is simply no OTHER, distinct user to
// attribute it to instead (a self-registration/self-authentication event).
func createUserAndIdentity(ctx context.Context, pool *pgxpool.Pool, users *postgres.UserStore, identities *postgres.IdentityStore, auditLog *postgres.AuditLogStore, p createUserAndIdentityParams) (pgtype.UUID, error) {
	role := resolveInitialRole(p.verifiedEmail, p.initialAdminEmails)

	displayName := p.githubLogin
	if p.githubName != "" {
		displayName = p.githubName
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: begin user-creation tx: %w", err)
	}
	// Rollback is a safety net for every return path other than a
	// successful Commit below; pgx reports (and this discards) an
	// already-closed-transaction error on the no-op case where Commit
	// already ran -- same pattern as app/sessionactor's own transact.
	defer func() { _ = tx.Rollback(ctx) }()

	createdUser, err := users.WithTx(tx).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: p.verifiedEmail,
		DisplayName:  displayName,
		Role:         role,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: create user: %w", err)
	}

	// linked_via="admin" is a slight overload here (nothing about this
	// sign-in is admin-initiated) but is the least-wrong of the 3 existing
	// identity_linked_via enum values for "no real linking algorithm ran,
	// this identity was simply created": auto_email specifically means
	// "matched an ALREADY-existing user's OTHER identity by email" (§13.2's
	// own auto-linking algorithm, not built yet); prompt
	// means a human was asked to confirm a link (the same job). This is a
	// deliberate, documented hand-off note, not a silent choice.
	if _, err := identities.WithTx(tx).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               createdUser.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           p.externalID,
		Email:                &p.verifiedEmail,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: p.encryptedToken,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: create identity: %w", err)
	}

	if err := auditlog.Record(ctx, auditLog.WithTx(tx), createdUser.ID, "user.created", "user", createdUser.ID.String(), map[string]any{
		"role":         string(role),
		"github_login": p.githubLogin,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: record user-creation audit log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: commit user-creation tx: %w", err)
	}

	return createdUser.ID, nil
}

// fetchGitHubUser calls GET {apiBaseURL}/user with client (already carrying
// the signing-in user's bearer token via oauthConfig.Client).
func fetchGitHubUser(ctx context.Context, client *http.Client, apiBaseURL string) (githubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/user", nil)
	if err != nil {
		return githubUser{}, fmt.Errorf("build /user request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return githubUser{}, fmt.Errorf("GET /user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return githubUser{}, fmt.Errorf("GET /user: unexpected status %d", resp.StatusCode)
	}

	var u githubUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return githubUser{}, fmt.Errorf("decode /user response: %w", err)
	}
	return u, nil
}

// fetchVerifiedPrimaryEmail calls GET {apiBaseURL}/user/emails and returns
// the entry with Primary && Verified, if any. A false second return means
// no such entry exists -- callers must treat this as an allowlist/signup
// FAILURE, never fall back to githubUser's own unverified top-level email
// field (see githubUser's own doc comment).
func fetchVerifiedPrimaryEmail(ctx context.Context, client *http.Client, apiBaseURL string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/user/emails", nil)
	if err != nil {
		return "", false, fmt.Errorf("build /user/emails request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("GET /user/emails: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("GET /user/emails: unexpected status %d", resp.StatusCode)
	}

	var emails []githubEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", false, fmt.Errorf("decode /user/emails response: %w", err)
	}

	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, true, nil
		}
	}
	return "", false, nil
}

// checkAnyOrgMembership reports whether the signing-in user (identified by
// username, using their OWN token via client) is a member of ANY org in
// orgs -- a single 204 from any configured org is enough to pass (§13's
// own design decision 8).
func checkAnyOrgMembership(ctx context.Context, client *http.Client, apiBaseURL, username string, orgs []string) bool {
	for _, org := range orgs {
		if checkOrgMembership(ctx, client, apiBaseURL, org, username) {
			return true
		}
	}
	return false
}

// checkOrgMembership calls GET {apiBaseURL}/orgs/{org}/members/{username}
// using the SIGNING-IN user's own token (checking their OWN membership,
// never anyone else's).
//
// GitHub's own documented status-code semantics here (verified live
// against docs.github.com during this Step's design phase) are a real
// quirk worth getting right: 204 = the requester is an org member AND the
// checked user is a member; 404 = the requester is an org member but the
// checked user is NOT a member; 302 = the REQUESTER is not themselves a
// member of that org AT ALL (meaning if the signing-in user isn't
// privileged in that org, GitHub returns a redirect instead of a clean
// 404).
//
// Since this function only ever checks "is the signing-in user themselves
// a member of one of the configured allowed orgs" (using that SAME user's
// own token, requester == checked user), it treats literally ANY response
// other than exactly 204 (302, 404, or any transport/decode error) as "not
// a member of this org" -- fail-closed, and deliberately does NOT
// special-case 302 differently from 404: either way, the answer to "is
// this user a member" is no from this endpoint's own point of view for the
// one case this function is ever used for.
//
// org and username are both escaped via url.PathEscape before being
// concatenated into the request path: org comes from server-side
// configured allowlist orgs (trusted), but username comes from GitHub's
// own OAuth /user response (githubUser.Login) -- attested by GitHub but
// never independently validated by this codebase's own character-set
// rules, so relying on an external provider's own username policy as an
// implicit security boundary against path injection would be fragile
// defense-in-depth to skip.
func checkOrgMembership(ctx context.Context, client *http.Client, apiBaseURL, org, username string) bool {
	reqURL := apiBaseURL + "/orgs/" + url.PathEscape(org) + "/members/" + url.PathEscape(username)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusNoContent
}
