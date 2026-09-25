package auth

import (
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/narvidev/narvi/internal/platform"
)

// oidcStateCookieName, oidcNonceCookieName, and oidcVerifierCookieName are
// the SAME short-lived, pre-auth cookie mechanism the GitHub flow's own
// oauthStateCookieName uses (login.go) -- HttpOnly, host-scoped, cleared
// the moment they are read -- widened to the three values PKCE+nonce+state
// together need, where GitHub's plain state-only flow needed just one.
// Kept as three separate, narrowly-named cookies rather than one packed
// value: mirrors oauthStateCookieName/oauthNextCookieName's own existing
// "one concern, one cookie" precedent in this same package, and keeps
// each value's own lifecycle (minted here, read+cleared exactly once by
// NewOIDCCallbackHandler, never replayed) independently legible.
const (
	oidcStateCookieName    = "narvi_oidc_state"
	oidcNonceCookieName    = "narvi_oidc_nonce"
	oidcVerifierCookieName = "narvi_oidc_verifier"
	// oidcNextCookieName carries an optional post-sign-in redirect target
	// through the OIDC round trip -- the SAME role oauthNextCookieName
	// plays for the GitHub flow (login.go), and validated by the SAME
	// isSafeRedirectNext on the way in and again on the way out. Its
	// first caller is the MCP consent page (technical plan §43.14): a
	// signed-out browser arriving at /oauth/authorize is sent through
	// sign-in with next=/oauth/consent?request=<id>, and an OIDC-only
	// deployment needs this cookie to come back there.
	oidcNextCookieName = "narvi_oidc_next"
)

// setOIDCPreAuthCookie is the one place this file builds a pre-auth OIDC
// cookie -- same shape as login.go's own inline narvi_oauth_state
// Set-Cookie (HttpOnly, Path=/, SameSite=Lax, Secure-per-stage,
// Expires = now + timeouts.OAuthStateTTL), parameterized over name/value
// so state/nonce/verifier share exactly one construction site rather than
// three copies that could drift.
//
// Reuses timeouts.OAuthStateTTL rather than minting a second, OIDC-only
// timeout constant: both cookies exist for the identical reason (bound an
// abandoned or in-flight browser round trip to the IdP and back), and
// §11's "every timeout lives in platform/timeouts.go" is already
// satisfied by OAuthStateTTL -- a second field with the same value would
// be a distinction with no behavioral difference.
func setOIDCPreAuthCookie(w http.ResponseWriter, name, value string, timeouts platform.Timeouts, secureCookies bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  time.Now().Add(timeouts.OAuthStateTTL),
		HttpOnly: true,
		Secure:   secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// NewOIDCLoginHandler backs GET /auth/oidc/login (§41.3):
// authorization-code flow with PKCE (S256), state, and nonce, mirroring
// NewLoginHandler's own shape (login.go) one level wider. Mounted
// unconditionally (controlplane/serve.go); an unconfigured deployment
// answers every request here with 503 (the guard at the top of the
// handler below) rather than having no route at all. An optional ?next=
// is honored exactly like NewLoginHandler's own (oidcNextCookieName).
//
// Unlike GitHub's plain state-parameter CSRF protection (NewLoginHandler's
// own doc comment: "this is a confidential, server-side client... plain
// state-parameter protection is the correct, standard choice"), a GENERIC
// OIDC provider is not assumed to be GitHub -- PKCE is added regardless of
// whether this particular IdP would strictly need it for a confidential
// client, both because §41.3 requires it explicitly and because PKCE
// costs nothing extra for a confidential client and closes the
// authorization-code-interception class of attack unconditionally.
//
// cache.get is called here too (not only at callback time) so a
// discovery failure surfaces immediately, on the FIRST request of a login
// attempt, rather than after the visitor has already round-tripped to an
// IdP that (it turns out) this control plane can't actually complete a
// token exchange with.
func NewOIDCLoginHandler(cache *OIDCProviderCache, timeouts platform.Timeouts, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		// This route is mounted UNCONDITIONALLY (controlplane/serve.go's
		// own doc comment on why) -- an unconfigured deployment refuses
		// every request here with 503, mirroring the cloud-identity
		// discovery routes' own identical "fail closed (503) when ...
		// is unset" precedent, rather than not existing as a route at
		// all.
		if !cache.cfg.Configured() {
			http.Error(w, "oidc sign-in is not configured for this deployment", http.StatusServiceUnavailable)
			return
		}

		rt, err := cache.get(ctx)
		if err != nil {
			logger.Error("auth: oidc login discovery failed", "error", err)
			http.Error(w, "oidc provider unavailable", http.StatusServiceUnavailable)
			return
		}

		state, err := platform.GenerateToken()
		if err != nil {
			logger.Error("auth: generate oidc state failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		nonce, err := platform.GenerateToken()
		if err != nil {
			logger.Error("auth: generate oidc nonce failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		verifier := oauth2.GenerateVerifier()

		setOIDCPreAuthCookie(w, oidcStateCookieName, state, timeouts, secureCookies)
		setOIDCPreAuthCookie(w, oidcNonceCookieName, nonce, timeouts, secureCookies)
		setOIDCPreAuthCookie(w, oidcVerifierCookieName, verifier, timeouts, secureCookies)
		// An optional ?next= (a same-origin absolute path only --
		// isSafeRedirectNext) is carried to the callback exactly like the
		// GitHub flow's own; absent or unsafe next sets no cookie at all.
		if next := r.URL.Query().Get("next"); isSafeRedirectNext(next) {
			setOIDCPreAuthCookie(w, oidcNextCookieName, next, timeouts, secureCookies)
		}

		authCodeURL := rt.oauth2Config.AuthCodeURL(state,
			oauth2.S256ChallengeOption(verifier),
			// nonce is a plain, provider-agnostic OIDC authorization
			// request parameter (not an x/oauth2 built-in like PKCE) --
			// passed as a raw query param, read back off the verified ID
			// token's own Nonce field at callback time
			// (NewOIDCCallbackHandler, oidccallback.go).
			oauth2.SetAuthURLParam("nonce", nonce),
		)
		http.Redirect(w, r, authCodeURL, http.StatusFound)
	}
}
