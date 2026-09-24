package auth

import (
	"context"
	"fmt"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oidcCallbackPath mirrors oauthCallbackPath's own precedent (oauth.go) --
// a named constant so the RedirectURL this package builds and
// cmd/control-plane/main.go's own route registration (GET
// /auth/oidc/callback) can never drift apart.
const oidcCallbackPath = "/auth/oidc/callback"

// OIDCConfig bundles the three NARVI_OIDC_* values platform.Config
// validated together at boot (all-or-none, internal/platform/config.go's
// own oidcIssuerEnvVarName doc comment) plus the externally-reachable
// base URL needed to build the OAuth RedirectURL -- mirrors
// NewGitHubOAuthConfig's own cfg-shaped input one level up (that
// constructor takes the whole platform.Config; this one takes only the
// 4 fields it needs, since platform.Config is not otherwise a dependency
// of this file).
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	PublicBaseURL string
}

// Configured reports whether cfg names a real OIDC provider -- platform.
// Load's own all-or-none validation means checking any ONE of the three
// credential fields is equivalent to checking all three (§41.3).
func (cfg OIDCConfig) Configured() bool {
	return cfg.Issuer != ""
}

// oidcRuntime is everything a login/callback handler needs once discovery
// has succeeded once: the oauth2.Config wired to the discovered authorize/
// token endpoints, and an IDTokenVerifier sharing the SAME underlying
// *oidc.Provider (and therefore the SAME cached, lazily-refreshed JWKS key
// set -- oidc.Provider.remoteKeySet's own doc comment: "shared between all
// code paths", built once, reused by every IDTokenVerifier the SAME
// *oidc.Provider ever mints).
type oidcRuntime struct {
	oauth2Config *oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

// OIDCProviderCache lazily discovers cfg.Issuer's own
// /.well-known/openid-configuration document (go-oidc's oidc.NewProvider)
// at most once, caching the result for the process lifetime of this
// control plane -- discovery is a real network call to a third-party IdP,
// and this control plane's own boot must never depend on that IdP being
// reachable (§37's boot-time-validation principle stops at THIS
// process's OWN config; a remote IdP's uptime is that IdP's problem, not
// a reason to refuse to serve every other route). A discovery FAILURE is
// not cached -- the next login/callback request simply tries again, so a
// transiently-unreachable IdP recovers on its own without a control-plane
// restart.
//
// Once discovery succeeds, the SAME *oidc.Provider (and therefore the
// SAME cached JWKS key set, refreshed on a signature-verification miss --
// oidc.RemoteKeySet's own documented "check for new keys from the
// remote... recommended by the spec" behavior, github.com/coreos/
// go-oidc/v3's own verify() implementation) backs every subsequent
// request for the rest of this process's life; nothing here re-fetches
// discovery or re-constructs the key set on every request.
type OIDCProviderCache struct {
	cfg OIDCConfig

	mu     sync.Mutex
	cached *oidcRuntime
}

// NewOIDCProviderCache builds a cache for cfg. Safe to construct even when
// !cfg.Configured() -- get simply never succeeds in that case (every
// caller checks Configured() first; see oidclogin.go/oidccallback.go).
func NewOIDCProviderCache(cfg OIDCConfig) *OIDCProviderCache {
	return &OIDCProviderCache{cfg: cfg}
}

// get returns the cached runtime, discovering it first if this is the
// first call (or every prior call failed). Mutex-guarded so concurrent
// requests arriving before the first successful discovery never race
// oidc.NewProvider's own network call -- one discovery attempt in flight
// at a time; a concurrent request simply waits for it rather than firing
// a redundant one.
func (c *OIDCProviderCache) get(ctx context.Context) (*oidcRuntime, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil {
		return c.cached, nil
	}

	provider, err := oidc.NewProvider(ctx, c.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery against %q failed: %w", c.cfg.Issuer, err)
	}

	oauth2Config := &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		RedirectURL:  c.cfg.PublicBaseURL + oidcCallbackPath,
		Endpoint:     provider.Endpoint(),
		// openid is mandatory (oidc.ScopeOpenID's own doc comment: "the
		// mandatory scope for all OpenID Connect OAuth2 requests");
		// "email" requests the email/email_verified claims §41.3's own
		// email-verification gate reads -- neither claim is guaranteed
		// present without it, per the OIDC standard claims spec.
		Scopes: []string{oidc.ScopeOpenID, "email"},
	}

	// SkipClientIDCheck: true -- the audience check is performed
	// EXPLICITLY, by this package's own verifyOIDCIDToken (oidccallback.go),
	// rather than delegated to go-oidc's own internal one: §41.3's exit
	// criteria require a discrete, mutation-testable "wrong audience ->
	// refused, with this exact text" guard, which a check buried inside a
	// dependency cannot offer. go-oidc still verifies issuer, expiry, and
	// signature -- exactly the three checks THIS package has no reason to
	// reimplement (hand-rolling JWT verification is exactly what §41.3
	// says to avoid).
	verifier := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})

	rt := &oidcRuntime{oauth2Config: oauth2Config, verifier: verifier}
	c.cached = rt
	return rt, nil
}
