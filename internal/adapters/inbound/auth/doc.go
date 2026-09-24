// Package auth implements §13.1 ("auth v1", §13.1/§13.4): GitHub OAuth
// login, backend-issued host-scoped session cookies, the first-sign-in
// allowlist gate, and the "must be logged in" route middleware. Every
// piece of state/API-client logic this package depends on (the 3 new
// internal/adapters/outbound/postgres stores, the GitHub REST API,
// cookies) lives HERE -- internal/platform stays pure/dependency-free
// (hmacauth.go, tokenhash.go, correlation.go, and this Step's own
// tokenencrypt.go/authcontext.go/authcookie.go), consumed by this package
// but never importing anything back from it.
//
// # Routes (mounted by cmd/control-plane/main.go, ALL outside the auth
// gate -- these are how a session is obtained/discarded in the first
// place, so they cannot themselves require one)
//
//   - GET  /auth/github/login    -- login.go's NewLoginHandler: mints a
//     CSRF state value, stores it in a short-lived narvi_oauth_state
//     cookie, redirects (302) to GitHub's own authorize URL.
//   - GET  /auth/github/callback -- callback.go's NewCallbackHandler: the
//     full flow below.
//   - POST /auth/logout          -- logout.go's NewLogoutHandler: revokes
//     the real DB row (if any) and always clears the cookie.
//
// # The OAuth-callback outcome table
//
//   - Missing/mismatched `state` query param vs. the narvi_oauth_state
//     cookie -> 400 (OutcomeStateMismatch). The token exchange is NEVER
//     attempted.
//   - oauthConfig.Exchange failure (bad/reused code, or a genuine
//     backend/network problem -- these two causes are NOT distinguished,
//     see callback.go's own comment on this simplification) ->
//     401 (OutcomeExchangeFailed).
//   - No entry with Primary && Verified in GET /user/emails -> 403
//     (OutcomeNoVerifiedEmail); no user/identity/session row is ever
//     created.
//   - Identity already linked (GetByProviderAndExternalID hits) ->
//     "returning user" (OutcomeReturningUser): the allowlist is SKIPPED
//     entirely (§13.1: "evaluated at first sign-in"), the stored
//     access_token_encrypted is refreshed, a fresh session is minted --
//     302 to "/".
//   - Identity not linked, allowlist check fails on ALL 3 mechanisms
//     (exact email, email domain, GitHub org membership) -> 403
//     (OutcomeFirstTimeDenied); no user/identity/session row is ever
//     created; the response body does NOT say which mechanism almost
//     matched (enumeration hardening).
//   - Identity not linked, allowlist passes -> "first-time-allowed"
//     (OutcomeFirstTimeAllowed): a users row + an identities row are
//     created in ONE Postgres transaction (createUserAndIdentity in
//     callback.go) -- role = admin iff the verified email is in
//     InitialAdminEmails, else member; linked_via = "admin", a deliberate,
//     documented overload (see that function's own comment, a §13.2
//     hand-off note) -- then a session is minted -- 302 to "/".
//
// Either of the last two branches then mints a fresh user_sessions row
// (platform.GenerateToken + HashToken, expires_at = now +
// timeouts.UserSessionTTL), sets the narvi_auth_session cookie
// (platform.WithAuthSessionCookie) to the PLAINTEXT token, and redirects
// (302) to "/" -- there is no SPA to land on yet (Phase 6 is what makes
// that landing meaningful); this is an intentional, forward-compatible
// interim behavior, not a bug.
//
// # Org-membership check (a real GitHub API quirk, verified live against
// docs.github.com during this Step's design phase)
//
// GET /orgs/{org}/members/{username}, called using the SIGNING-IN user's
// OWN token (so requester == checked user, always): 204 = member, 404 =
// the requester is an org member but the checked user is not, 302 = the
// requester isn't themselves a member of that org AT ALL. Since this
// package only ever checks "is the signing-in user a member of one of the
// configured orgs, using their own token", checkOrgMembership (callback.go)
// treats literally ANY response other than exactly 204 as "not a member"
// -- fail-closed, and 302 is deliberately NOT special-cased differently
// from 404 (see that function's own comment for the complete reasoning).
//
// # Security notes
//
//   - GitHubClientSecret and TokenEncryptionKey (platform.Config) are
//     never logged anywhere in this package, under any circumstance.
//   - The user-session bearer token is logged only as "present"/"absent"
//     (never the plaintext), matching internal/adapters/inbound/wshub's
//     own ws-token precedent exactly -- see middleware.go/logout.go, which
//     log outcome labels, never cookie values.
//   - The encrypted-at-rest GitHub access token's plaintext is never
//     logged; error paths touching it (platform.EncryptToken's own error
//     wrapping) are deliberately narrow so a stack trace can never
//     accidentally include it.
//   - The oauth-state CSRF cookie is exact-string-compared and consumed
//     (cleared) the moment it is read, so it cannot be replayed across
//     requests.
//   - Cookie attributes (HttpOnly/SameSite=Lax/Path=/, no Domain,
//     Secure-per-stage) are defined in exactly one place
//     (internal/platform/authcookie.go) and never weakened here for
//     convenience -- every test in this package exercises the REAL
//     cookie-issuing code path.
//
// # §13.2 ("identities + full RBAC", §13.2) second-half additions
//
//   - Authenticate (middleware.go): Middleware's own 4-step check
//     (cookie present -> hash found -> not expired -> user not disabled),
//     extracted so internal/adapters/inbound/identitylink's magic-link
//     consume handler can reuse the IDENTICAL authentication check
//     without going through chi middleware at all -- that handler needs
//     to REDIRECT an unauthenticated visitor into login, never respond
//     with Middleware's own bare 401 JSON body.
//   - NewLoginHandler's own optional ?next= query parameter
//     (login.go): a same-origin-only redirect target (isSafeRedirectNext),
//     stored in a second short-lived cookie and honored by
//     NewCallbackHandler as the final post-login redirect (instead of
//     this flow's own fixed "/" default) -- lets the magic-link consume
//     handler send a signed-out visitor through this SAME GitHub OAuth
//     flow and land back on the magic-link URL afterward.
//
// The actual identity auto-linking ALGORITHM (matching a fetched provider
// profile email against users.primary_email/verified identities.email,
// auto-linking or creating a magic-link prompt) and the members API both
// still live OUTSIDE this package -- internal/app/identitylink and
// internal/adapters/inbound/{identitylink,httpapi} respectively; this
// package still only ever links a user to a provider identity at
// first-sign-in time (createUserAndIdentity, callback.go) for its own
// GitHub OAuth login flow.
//
// # Explicitly out of scope (see cmd/control-plane/main.go and the plan's
// own §13.4 phasing for the owning Step)
//
//   - The full four-role permission matrix, a real domain/authz package,
//     the viewer guard, and audit-log WRITES ARE now real -- §13.2's own
//     RBAC half (§13.3) landed as internal/domain/authz (a table-driven
//     Authorize(actor, action, resource) error), wired into every
//     state-changing REST handler in internal/adapters/inbound/httpapi
//     (CreateSession/CreateTurn/ApprovePlan/RejectPlan), a defense-in-depth
//     viewer guard in internal/app/sessionactor's own PR-creation path, and
//     real audit_log writes (postgres.AuditLogStore) inside the SAME
//     transaction as each change -- this package itself (auth) still
//     issues no role-gated route of its own; Middleware here remains the
//     identical "must be logged in" gate it always was.
//   - Actually USING the stored, encrypted GitHub access token for
//     anything (creating a PR, pushing a branch, minting a git
//     credential) -- §9.3's own SourceControl adapter job ("createPR,
//     credential minting", §8.11). This package only obtains and stores
//     it.
//   - Wiring authenticated-user identity into internal/app/sessionactor or
//     internal/app/ports -- nothing downstream needs "which user issued
//     this command" yet.
//   - Wiring the participants table (multiplayer/presence, §8.11) -- a
//     distinct, not-yet-scoped concern.
//   - Any role-based (admin-only) ROUTE gating -- Middleware here is a
//     "must be logged in" gate only; "role skeleton" this Step means the
//     DATA MODEL (real role assignment at creation time) is correct, not
//     that any route enforces a role check yet.
//
// # §41.3 ("OIDC as a second sign-in provider") additions
//
// An earlier version of this doc comment (and of §13.1 itself) called
// generic OIDC/enterprise SSO "configuration, not code" and out of this
// package's scope. It is code: GET /auth/oidc/login (oidclogin.go) and
// GET /auth/oidc/callback (oidccallback.go), beside the GitHub pair --
// discovery + JWKS via github.com/coreos/go-oidc/v3 (oidcconfig.go's own
// OIDCProviderCache, lazily discovered and cached for this process's
// life), authorization-code + PKCE (S256) + nonce + state bound to the
// SAME short-lived pre-auth cookie mechanism the GitHub flow uses (three
// cookies instead of one -- oidclogin.go's own doc comment). Both routes
// are mounted UNCONDITIONALLY (controlplane/serve.go) and refuse with 503
// when NARVI_OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET are unset (all-or-none,
// internal/platform/config.go) -- never absent as routes, so the static
// route scanner and the real router never diverge.
//
// The verified `email` claim (`email_verified` required literally `true`
// -- absent, `false`, or any non-boolean value is refused, audited) enters
// the SAME allowlist gate, default-role assignment, and users row model as
// GitHub sign-in. identities gains provider `oidc`, external_id
// `{issuer}|{sub}` (migrations/000140); a second sign-in through the same
// issuer+sub resolves to the SAME user, and an unrecognized OIDC identity
// whose verified email matches exactly one existing user auto-links onto
// it (identitylink.MatchUserIDs/AutoLink, exported and reused from the
// Slack/Linear algorithm, §13.2 step 3) rather than creating a duplicate
// account -- oidccallback.go's own NewOIDCCallbackHandler doc comment has
// the complete outcome table.
package auth
