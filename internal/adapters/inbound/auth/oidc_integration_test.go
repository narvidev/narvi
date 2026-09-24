//go:build integration

// Integration tests for internal/adapters/inbound/auth's generic OIDC
// sign-in flow (§41.3) -- login -> callback -> ID-token
// verification -> allowlist/graph-merge -> user/identity/session-cookie,
// against a real Postgres instance, mirroring auth_integration_test.go's
// own GitHub-flow precedent exactly (same shared pool via newTestPool,
// same cookiejar-based httptest.Server rig, same doLogin/doCallback
// shape one level wider for the extra nonce/PKCE-verifier cookie this
// flow adds).
//
// A generic OIDC IdP is stood in for by fakeOIDCProvider (below): a real
// httptest.Server serving a real discovery document, a real JWKS
// endpoint, and a token endpoint returning a REAL RS256-signed ID token
// -- signed with internal/adapters/outbound/oidcsigning.Sign, the exact
// same signer this codebase's own §27.3 cloud-identity token minting
// already uses and already tests, not a hand-rolled JWT encoder written
// just for this file. This is what makes every negative test below
// (wrong nonce, wrong audience, expired token, wrong signing key) a REAL
// exercise of go-oidc's own verifier and this package's own explicit
// nonce/audience/email_verified checks, never a mocked-out verifier.
package auth_test

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/outbound/oidcsigning"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeOIDCProvider stands in for a real generic OIDC IdP: discovery,
// JWKS, and token-exchange endpoints, all real HTTP over a real
// httptest.Server -- only the /authorize leg is never actually hit (this
// package's own doLogin/doOIDCLogin precedent: a test extracts state/
// nonce straight from the login redirect's own Location query params,
// exactly like doLogin already does for GitHub's state, and never
// follows the redirect for real).
type fakeOIDCProvider struct {
	mu sync.Mutex

	priv     *rsa.PrivateKey
	kid      string
	clientID string

	tokenCalls int

	// nextIDToken is returned verbatim as the token response's own
	// id_token field on the NEXT /token call -- set by each test via
	// signIDToken/signIDTokenWithKey before driving the flow, giving full
	// per-test control over exactly what the "IdP" asserts.
	nextIDToken string

	// requireCodeVerifier, when non-empty, makes /token respond 400
	// unless the incoming code_verifier form value equals this exactly --
	// used by the PKCE-mismatch test. Left empty (the default) for every
	// other test: this fake's token endpoint otherwise never validates
	// PKCE at all, since a real verifier is minted randomly inside
	// NewOIDCLoginHandler and no test needs to predict it to prove
	// anything else.
	requireCodeVerifier string

	// requireCodeChallenge, when non-empty, makes /token respond 400
	// unless S256(code_verifier) (base64url, no padding -- RFC 7636)
	// equals this exactly -- the real PKCE binding, used by
	// TestOIDCCallback_PKCE_S256BindsVerifierToChallenge to prove the
	// verifier this flow presents at /token is cryptographically tied to
	// the code_challenge minted at /authorize, not merely "some string
	// the login handler remembered" (see that test's own doc comment for
	// why requireCodeVerifier above cannot prove this). code_challenge
	// itself is never hit at a real /authorize endpoint here (this
	// file's own top doc comment: /authorize is never actually called) --
	// a test reads it straight off the login redirect's own query string
	// (oauth2.S256ChallengeOption appends it there) and sets it here
	// before driving the callback.
	requireCodeChallenge string

	// issuerOverride, when non-empty, replaces this fake's own
	// server.URL as the discovery document's `issuer` field (and
	// therefore the value go-oidc.NewProvider requires the CONFIGURED
	// issuer to equal exactly) -- lets a test simulate an IdP whose real
	// issuer differs from this httptest.Server's own URL, e.g. one
	// carrying a trailing slash (TestOIDCIssuer_TrailingSlash_
	// DiscoverySucceeds) or one that plain differs from what was
	// configured (TestOIDCIssuer_Mismatch_DiscoveryRefused).
	issuerOverride string

	server *httptest.Server
}

func newFakeOIDCProvider(t *testing.T, clientID string) *fakeOIDCProvider {
	t.Helper()

	priv, err := oidcsigning.GenerateKeyPair()
	if err != nil {
		t.Fatalf("oidcsigning.GenerateKeyPair: %v", err)
	}
	kid, err := oidcsigning.GenerateKid()
	if err != nil {
		t.Fatalf("oidcsigning.GenerateKid: %v", err)
	}

	f := &fakeOIDCProvider{priv: priv, kid: kid, clientID: clientID}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		issuer := f.server.URL
		if f.issuerOverride != "" {
			issuer = f.issuerOverride
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint":         f.server.URL + "/token",
			"jwks_uri":               f.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		jwks, err := oidcsigning.MarshalJWKS([]oidcsigning.JWK{oidcsigning.PublicJWK(&f.priv.PublicKey, f.kid)})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenCalls++
		requireVerifier := f.requireCodeVerifier
		requireChallenge := f.requireCodeChallenge
		idToken := f.nextIDToken
		f.mu.Unlock()

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if requireVerifier != "" && r.FormValue("code_verifier") != requireVerifier {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "code_verifier mismatch"})
			return
		}
		if requireChallenge != "" {
			// The real RFC 7636 S256 binding: BASE64URL(SHA256(code_verifier))
			// must equal the code_challenge minted at login time -- proves
			// the verifier THIS request presents is cryptographically tied
			// to that specific challenge, not merely "some non-empty
			// string" (requireVerifier's own exact-string-match above
			// cannot distinguish a real S256 relationship from a
			// coincidentally-matching fixed value).
			verifier := r.FormValue("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			computedChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
			if verifier == "" || computedChallenge != requireChallenge {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "code_verifier does not match code_challenge (S256)"})
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOIDCProvider) issuer() string {
	return f.server.URL
}

func (f *fakeOIDCProvider) tokenCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func (f *fakeOIDCProvider) setNextIDToken(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextIDToken = tok
}

func (f *fakeOIDCProvider) setRequireCodeVerifier(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requireCodeVerifier = v
}

func (f *fakeOIDCProvider) setRequireCodeChallenge(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requireCodeChallenge = v
}

func (f *fakeOIDCProvider) setIssuerOverride(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuerOverride = v
}

// defaultClaims builds a baseline, otherwise-valid ID token claims map
// (iss/aud/sub/iat/exp) -- callers add/override email/email_verified/
// nonce/aud themselves per scenario.
func (f *fakeOIDCProvider) defaultClaims(sub string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": f.issuer(),
		"aud": []string{f.clientID},
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
}

// signIDToken signs claims with this fake IdP's own published key (the
// happy-path/ordinary case: the token verifies against the JWKS this
// same server publishes).
func (f *fakeOIDCProvider) signIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	tok, err := oidcsigning.Sign(f.priv, f.kid, claims)
	if err != nil {
		t.Fatalf("oidcsigning.Sign: %v", err)
	}
	return tok
}

// signIDTokenWithKey signs claims with a DIFFERENT private key than the
// one this fake IdP's own /jwks publishes -- the "token signed by
// another key" negative test: verification must fail even though every
// OTHER claim is otherwise perfectly valid.
func (f *fakeOIDCProvider) signIDTokenWithKey(t *testing.T, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	tok, err := oidcsigning.Sign(priv, f.kid, claims)
	if err != nil {
		t.Fatalf("oidcsigning.Sign (wrong key): %v", err)
	}
	return tok
}

// oidcTestRig bundles a fresh pool + the 4 auth stores + an httptest.
// Server mounting the 2 OIDC routes exactly as controlplane/serve.go
// does, plus the fake IdP it's wired against -- mirrors testRig
// (auth_integration_test.go) one level wider.
type oidcTestRig struct {
	pool         *pgxpool.Pool
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	auditLog     *narvipg.AuditLogStore
	userSessions *narvipg.UserSessionStore
	linkPrompts  *narvipg.IdentityLinkPromptStore
	provider     *fakeOIDCProvider
	server       *httptest.Server
}

type oidcRiggedOptions struct {
	allowlist          auth.AllowlistConfig
	initialAdminEmails []string
}

func defaultOIDCRiggedOptions() oidcRiggedOptions {
	return oidcRiggedOptions{
		allowlist: auth.AllowlistConfig{EmailDomains: []string{"example.com"}},
	}
}

const oidcTestClientID = "test-oidc-client-id"

func newOIDCTestRig(t *testing.T, opts oidcRiggedOptions) oidcTestRig {
	t.Helper()
	pool := newTestPool(t)
	provider := newFakeOIDCProvider(t, oidcTestClientID)

	rig := oidcTestRig{
		users:        narvipg.NewUserStore(pool),
		identities:   narvipg.NewIdentityStore(pool),
		auditLog:     narvipg.NewAuditLogStore(pool),
		userSessions: narvipg.NewUserSessionStore(pool),
		linkPrompts:  narvipg.NewIdentityLinkPromptStore(pool),
		provider:     provider,
	}

	cfg := auth.OIDCConfig{
		Issuer:        provider.issuer(),
		ClientID:      oidcTestClientID,
		ClientSecret:  "test-oidc-client-secret",
		PublicBaseURL: "http://narvi.test",
	}
	cache := auth.NewOIDCProviderCache(cfg)

	timeouts := platform.DefaultTimeouts()

	router := chi.NewRouter()
	router.Get("/auth/oidc/login", auth.NewOIDCLoginHandler(cache, timeouts, false))
	router.Get("/auth/oidc/callback", auth.NewOIDCCallbackHandler(
		pool,
		cache,
		rig.users,
		rig.identities,
		rig.auditLog,
		rig.userSessions,
		rig.linkPrompts,
		opts.allowlist,
		opts.initialAdminEmails,
		timeouts,
		false,
	))

	rig.server = httptest.NewServer(router)
	t.Cleanup(rig.server.Close)
	rig.pool = pool

	return rig
}

// doOIDCLogin issues GET /auth/oidc/login and returns the `state` and
// `nonce` query params the redirect's own Location was minted with --
// mirrors doLogin (auth_integration_test.go) one field wider: nonce
// travels in cleartext in the (never-followed) authorize URL exactly
// like state does, so a test that needs to sign a MATCHING ID token can
// read the real, randomly-minted value back out here rather than
// guessing it. The client's own cookie jar captures the state/nonce/
// verifier cookies as a side effect of the response's Set-Cookie header,
// exactly like the GitHub flow's single state cookie.
func doOIDCLogin(t *testing.T, client *http.Client, serverURL string) (state, nonce string) {
	t.Helper()

	resp, err := client.Get(serverURL + "/auth/oidc/login")
	if err != nil {
		t.Fatalf("GET /auth/oidc/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /auth/oidc/login status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("resp.Location(): %v", err)
	}
	state = loc.Query().Get("state")
	nonce = loc.Query().Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("login redirect Location missing state/nonce: %v", loc)
	}
	return state, nonce
}

// doOIDCCallback issues GET /auth/oidc/callback?state=...&code=... via
// client (whose jar carries whatever state/nonce/verifier cookies a
// prior doOIDCLogin call captured, if any).
func doOIDCCallback(t *testing.T, client *http.Client, serverURL, state, code string) *http.Response {
	t.Helper()

	u := serverURL + "/auth/oidc/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code)
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET /auth/oidc/callback: %v", err)
	}
	return resp
}

// --- (a) happy path: brand-new user, first-time sign-in ---

func TestOIDCCallback_FirstTimeSignIn_HappyPath(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()
	client := newClient(t)

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-1")
	claims["email"] = "newperson@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	claims["name"] = "New Person"
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "a-fresh-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if loc, _ := resp.Location(); loc == nil || loc.Path != "/" {
		t.Errorf("callback redirected to %v, want \"/\"", loc)
	}

	sessionToken := authCookieFrom(t, resp)
	if sessionToken == "" {
		t.Fatal("auth session cookie value is empty")
	}
	if got := rig.provider.tokenCallCount(); got != 1 {
		t.Errorf("token endpoint call count = %d, want 1", got)
	}

	wantExternalID := rig.provider.issuer() + "|oidc-subject-1"
	identity, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.Email == nil || *identity.Email != "newperson@example.com" {
		t.Errorf("identity.Email = %v, want newperson@example.com", identity.Email)
	}
	if !identity.EmailVerified {
		t.Error("identity.EmailVerified = false, want true")
	}

	user, err := rig.users.GetByID(ctx, identity.UserID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if user.PrimaryEmail != "newperson@example.com" {
		t.Errorf("user.PrimaryEmail = %q, want %q", user.PrimaryEmail, "newperson@example.com")
	}
	if user.Role != sqlcgen.UserRoleMember {
		t.Errorf("user.Role = %q, want %q", user.Role, sqlcgen.UserRoleMember)
	}

	sessionRow, err := rig.userSessions.GetByHash(ctx, platform.HashToken(sessionToken))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if sessionRow.UserID != user.ID {
		t.Errorf("sessionRow.UserID = %v, want %v", sessionRow.UserID, user.ID)
	}
}

// --- (b) a second sign-in through the same issuer+sub resolves to the
// SAME user, never a second one ---

func TestOIDCCallback_ReturningUser_NoSecondUserCreated(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()

	client1 := newClient(t)
	state1, nonce1 := doOIDCLogin(t, client1, rig.server.URL)
	claims1 := rig.provider.defaultClaims("oidc-subject-returning")
	claims1["email"] = "returning@example.com"
	claims1["email_verified"] = true
	claims1["nonce"] = nonce1
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims1))
	resp1 := doOIDCCallback(t, client1, rig.server.URL, state1, "first-signin-code")
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusFound {
		t.Fatalf("first callback status = %d, want %d", resp1.StatusCode, http.StatusFound)
	}

	wantExternalID := rig.provider.issuer() + "|oidc-subject-returning"
	identityBefore, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (before): %v", err)
	}

	// Second sign-in, same issuer+sub, fresh browser/client/cookie jar.
	client2 := newClient(t)
	state2, nonce2 := doOIDCLogin(t, client2, rig.server.URL)
	claims2 := rig.provider.defaultClaims("oidc-subject-returning")
	claims2["email"] = "returning@example.com"
	claims2["email_verified"] = true
	claims2["nonce"] = nonce2
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims2))
	resp2 := doOIDCCallback(t, client2, rig.server.URL, state2, "second-signin-code")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("second callback status = %d, want %d", resp2.StatusCode, http.StatusFound)
	}

	identityAfter, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (after): %v", err)
	}
	if identityAfter.ID != identityBefore.ID || identityAfter.UserID != identityBefore.UserID {
		t.Errorf("second sign-in resolved to a DIFFERENT identity/user (%v/%v != %v/%v) -- want the SAME row reused",
			identityAfter.ID, identityAfter.UserID, identityBefore.ID, identityBefore.UserID)
	}

	rows, err := rig.users.GetByID(ctx, identityBefore.UserID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	_ = rows // exists, no error -- exactly one user row for this identity throughout
}

// --- (c) the graph merges on verified email, exactly like Slack/Linear
// (§13.2 step 3): an OIDC identity with no prior link, whose verified
// email matches an EXISTING GitHub-linked user, auto-links onto that
// SAME user rather than creating a second account ---

func TestOIDCCallback_GraphMergesOnVerifiedEmail_AutoLinksExistingUser(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()

	existingUser, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "shared@example.com",
		DisplayName:  "Shared Person",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create existing user: %v", err)
	}

	client := newClient(t)
	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-merge")
	claims["email"] = "shared@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))
	resp := doOIDCCallback(t, client, rig.server.URL, state, "merge-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	wantExternalID := rig.provider.issuer() + "|oidc-subject-merge"
	identity, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != existingUser.ID {
		t.Errorf("identity.UserID = %v, want %v (auto-linked onto the EXISTING user, never a new one)", identity.UserID, existingUser.ID)
	}
	if identity.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail {
		t.Errorf("identity.LinkedVia = %q, want %q", identity.LinkedVia, sqlcgen.IdentityLinkedViaAutoEmail)
	}

	// Exactly one user row for this email -- never a second one.
	byEmail, err := rig.users.GetByPrimaryEmail(ctx, "shared@example.com")
	if err != nil {
		t.Fatalf("GetByPrimaryEmail: %v", err)
	}
	if byEmail.ID != existingUser.ID {
		t.Errorf("GetByPrimaryEmail resolved to %v, want the original %v", byEmail.ID, existingUser.ID)
	}
}

// --- (d) ambiguous match (more than one existing user shares the
// verified email) -> refused, audited, never a guess ---

func TestOIDCCallback_AmbiguousMatch_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()

	userA, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "person-a@example.com",
		DisplayName:  "Person A",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create userA: %v", err)
	}
	userB, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "person-b@example.com",
		DisplayName:  "Person B",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create userB: %v", err)
	}
	ambiguousEmail := "ambiguous@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        userA.ID,
		Provider:      sqlcgen.IdentityProviderSlack,
		ExternalID:    "slack-a",
		Email:         &ambiguousEmail,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("create identity for userA: %v", err)
	}
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        userB.ID,
		Provider:      sqlcgen.IdentityProviderSlack,
		ExternalID:    "slack-b",
		Email:         &ambiguousEmail,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("create identity for userB: %v", err)
	}

	client := newClient(t)
	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-ambiguous")
	claims["email"] = ambiguousEmail
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))
	resp := doOIDCCallback(t, client, rig.server.URL, state, "ambiguous-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d (never guess between two matching users)", resp.StatusCode, http.StatusForbidden)
	}

	wantExternalID := rig.provider.issuer() + "|oidc-subject-ambiguous"
	if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID); !errorsIsNoRows(err) {
		t.Errorf("an identities row was created for the ambiguous sign-in (err=%v) -- want none", err)
	}

	rows := getAuditLogRowsForResource(context.Background(), t, rig.pool, "identity", wantExternalID)
	if len(rows) != 1 {
		t.Fatalf("audit_log rows for the ambiguous refusal = %d, want 1", len(rows))
	}
	if rows[0].Action != "identity.oidc_ambiguous_match" {
		t.Errorf("audit_log action = %q, want %q", rows[0].Action, "identity.oidc_ambiguous_match")
	}
}

func errorsIsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// --- (e) email_verified: absent, false, and a non-boolean "true" string
// are all refused, never trusted ---

func TestOIDCCallback_EmailVerified_RefusalShapes(t *testing.T) {
	tests := []struct {
		name          string
		setClaims     func(claims map[string]any)
		wantAuditable bool
	}{
		{
			name: "email_verified absent entirely",
			setClaims: func(claims map[string]any) {
				claims["email"] = "absent-verified@example.com"
				// email_verified deliberately never set.
			},
		},
		{
			name: "email_verified is the boolean false",
			setClaims: func(claims map[string]any) {
				claims["email"] = "false-verified@example.com"
				claims["email_verified"] = false
			},
		},
		{
			name: "email_verified is the STRING \"true\", not the JSON boolean",
			setClaims: func(claims map[string]any) {
				claims["email"] = "string-true-verified@example.com"
				claims["email_verified"] = "true"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
			client := newClient(t)

			state, nonce := doOIDCLogin(t, client, rig.server.URL)
			claims := rig.provider.defaultClaims("oidc-subject-" + tc.name)
			claims["nonce"] = nonce
			tc.setClaims(claims)
			rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

			resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
			}
		})
	}
}

// --- (f) negative security tests: wrong nonce, wrong state,
// missing/incorrect PKCE verifier, wrong audience, expired token, wrong
// signing key -- all refused ---

func TestOIDCCallback_WrongState_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	client := newClient(t)

	doOIDCLogin(t, client, rig.server.URL) // mints real cookies, state discarded
	resp := doOIDCCallback(t, client, rig.server.URL, "a-completely-different-state-value", "irrelevant-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if got := rig.provider.tokenCallCount(); got != 0 {
		t.Errorf("token endpoint call count = %d, want 0 (exchange must never be attempted)", got)
	}
}

func TestOIDCCallback_WrongNonce_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	client := newClient(t)

	state, _ := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-wrong-nonce")
	claims["email"] = "wrongnonce@example.com"
	claims["email_verified"] = true
	claims["nonce"] = "not-the-real-nonce-value"
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestOIDCCallback_MissingOrWrongPKCEVerifier_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	// No real verifier can ever equal this fixed string (the real one is
	// 32 random bytes minted fresh by NewOIDCLoginHandler) -- guarantees
	// the token endpoint's own PKCE check fails for a wrong/absent
	// verifier without needing to intercept the real value.
	rig.provider.setRequireCodeVerifier("this-will-never-match-the-real-verifier")
	client := newClient(t)

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-wrong-pkce")
	claims["email"] = "wrongpkce@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (exchange fails: the token endpoint rejects the wrong code_verifier)", resp.StatusCode, http.StatusUnauthorized)
	}
	if got := rig.provider.tokenCallCount(); got == 0 {
		t.Error("token endpoint call count = 0, want at least 1 (the exchange WAS attempted, just refused)")
	}
}

func TestOIDCCallback_WrongAudience_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	client := newClient(t)

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-wrong-aud")
	claims["email"] = "wrongaud@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	claims["aud"] = []string{"someone-elses-client-id"}
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestOIDCCallback_ExpiredToken_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	client := newClient(t)

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-expired")
	claims["email"] = "expired@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	past := time.Now().Add(-10 * time.Minute)
	claims["iat"] = past.Add(-time.Minute).Unix()
	claims["exp"] = past.Unix()
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestOIDCCallback_WrongSigningKey_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	client := newClient(t)

	otherKey, err := oidcsigning.GenerateKeyPair()
	if err != nil {
		t.Fatalf("oidcsigning.GenerateKeyPair (other key): %v", err)
	}

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-wrong-key")
	claims["email"] = "wrongkey@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDTokenWithKey(t, otherKey, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestOIDCCallback_WrongIssuer_Refused proves the ID token's own `iss`
// claim is actually checked against the configured/discovered issuer --
// review round 1, finding O7: every OTHER negative test in this file
// (state, nonce, PKCE, audience, expiry, signing key, email_verified)
// leaves `iss` at its default (the fake IdP's own real issuer), so none
// of them can catch the issuer check itself being disabled -- e.g.
// SkipIssuerCheck: true added to the oidc.Config in oidcconfig.go's own
// provider.Verifier(...) call. A token signed by the IdP's own real key,
// with a real nonce and a real email_verified claim, but a DIFFERENT
// `iss`, must still be refused: oidcExternalID(idToken.Issuer, ...)
// (oidccallback.go) makes the issuer value load-bearing for identity
// keying, not just a formality -- accepting a wrong issuer here would let
// a token meant for a different (but key-sharing, e.g. multi-tenant)
// issuer be replayed against this one.
func TestOIDCCallback_WrongIssuer_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()
	client := newClient(t)

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-wrong-iss")
	claims["iss"] = "https://attacker-tenant.example.test"
	claims["email"] = "wrongiss@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (an id_token whose iss differs from the configured/discovered issuer must be refused)", resp.StatusCode, http.StatusUnauthorized)
	}

	wantExternalID := "https://attacker-tenant.example.test|oidc-subject-wrong-iss"
	if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID); !errorsIsNoRows(err) {
		t.Errorf("an identities row was created keyed to the attacker-chosen issuer (err=%v) -- want none", err)
	}
}

// --- (g) allowlist still gates a brand-new OIDC identity exactly like
// GitHub's own first-time sign-in ---

func TestOIDCCallback_FirstTimeSignIn_AllowlistDenied(t *testing.T) {
	rig := newOIDCTestRig(t, oidcRiggedOptions{allowlist: auth.AllowlistConfig{Emails: []string{"nobody@nowhere.invalid"}}})
	ctx := context.Background()
	client := newClient(t)

	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-denied")
	claims["email"] = "denied@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, rig.server.URL, state, "denied-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	wantExternalID := rig.provider.issuer() + "|oidc-subject-denied"
	if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalID); !errorsIsNoRows(err) {
		t.Errorf("an identities row was created for the denied sign-in (err=%v) -- want none", err)
	}
}

// --- (h) PKCE: S256 code_challenge/code_verifier are genuinely bound to
// each other, not merely present -- review round 1, finding O6. ---
//
// TestOIDCCallback_MissingOrWrongPKCEVerifier_Refused (above) cannot
// catch PKCE being removed entirely: its fake /token rejects every
// code_verifier that is not one fixed, never-matching string, so it
// returns 400 whether the real flow sends the correct verifier, a wrong
// one, or none at all -- it only proves "a failed Exchange returns 401".
// The two tests below exercise the REAL RFC 7636 S256 relationship: the
// fake IdP's /token now recomputes BASE64URL(SHA256(code_verifier)) and
// compares it against the code_challenge captured off the login
// redirect's own query string (oauth2.S256ChallengeOption's doc
// comment -- exactly what a real IdP does), so only the genuine verifier
// this flow's own narvi_oidc_verifier cookie carries can ever satisfy it.

// oidcVerifierCookieNameForTest mirrors oidclogin.go's own unexported
// oidcVerifierCookieName constant ("narvi_oidc_verifier") -- this file is
// package auth_test (external, black-box), so it cannot reference that
// unexported identifier directly; duplicated here, by value, rather than
// exporting it just for this one test.
const oidcVerifierCookieNameForTest = "narvi_oidc_verifier"

func TestOIDCLogin_PKCE_ChallengeMethodS256Present(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	client := newClient(t)

	resp, err := client.Get(rig.server.URL + "/auth/oidc/login")
	if err != nil {
		t.Fatalf("GET /auth/oidc/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("resp.Location(): %v", err)
	}
	if loc.Query().Get("code_challenge") == "" {
		t.Error("login redirect Location has no code_challenge query param")
	}
	if got := loc.Query().Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want %q", got, "S256")
	}
}

func TestOIDCCallback_PKCE_S256BindsVerifierToChallenge(t *testing.T) {
	t.Run("real verifier satisfies the challenge recorded at login", func(t *testing.T) {
		rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
		client := newClient(t)

		loginResp, err := client.Get(rig.server.URL + "/auth/oidc/login")
		if err != nil {
			t.Fatalf("GET /auth/oidc/login: %v", err)
		}
		defer func() { _ = loginResp.Body.Close() }()
		loc, err := loginResp.Location()
		if err != nil {
			t.Fatalf("resp.Location(): %v", err)
		}
		state, nonce, challenge := loc.Query().Get("state"), loc.Query().Get("nonce"), loc.Query().Get("code_challenge")
		if state == "" || nonce == "" || challenge == "" {
			t.Fatalf("login redirect missing state/nonce/code_challenge: %v", loc)
		}
		rig.provider.setRequireCodeChallenge(challenge)

		claims := rig.provider.defaultClaims("oidc-subject-pkce-real")
		claims["email"] = "pkce-real@example.com"
		claims["email_verified"] = true
		claims["nonce"] = nonce
		rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

		resp := doOIDCCallback(t, client, rig.server.URL, state, "pkce-real-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d (the REAL verifier this flow's own cookie carries must satisfy the S256 challenge it minted)", resp.StatusCode, http.StatusFound)
		}
	})

	t.Run("a tampered verifier cookie does not satisfy the challenge, refused", func(t *testing.T) {
		rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
		client := newClient(t)

		loginResp, err := client.Get(rig.server.URL + "/auth/oidc/login")
		if err != nil {
			t.Fatalf("GET /auth/oidc/login: %v", err)
		}
		defer func() { _ = loginResp.Body.Close() }()
		loc, err := loginResp.Location()
		if err != nil {
			t.Fatalf("resp.Location(): %v", err)
		}
		state, nonce, challenge := loc.Query().Get("state"), loc.Query().Get("nonce"), loc.Query().Get("code_challenge")
		if state == "" || nonce == "" || challenge == "" {
			t.Fatalf("login redirect missing state/nonce/code_challenge: %v", loc)
		}
		rig.provider.setRequireCodeChallenge(challenge)

		// Tamper: overwrite the httpOnly narvi_oidc_verifier cookie the
		// login handler just minted with a DIFFERENT, syntactically-valid
		// verifier -- code_verifier reaching /token no longer matches
		// what code_challenge was actually derived from.
		serverURL, err := url.Parse(rig.server.URL)
		if err != nil {
			t.Fatalf("url.Parse: %v", err)
		}
		client.Jar.SetCookies(serverURL, []*http.Cookie{{
			Name:  oidcVerifierCookieNameForTest,
			Value: "an-attacker-controlled-verifier-that-does-not-match-the-real-one",
		}})

		claims := rig.provider.defaultClaims("oidc-subject-pkce-tampered")
		claims["email"] = "pkce-tampered@example.com"
		claims["email_verified"] = true
		claims["nonce"] = nonce
		rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))

		resp := doOIDCCallback(t, client, rig.server.URL, state, "pkce-tampered-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d (a verifier that does not satisfy the recorded S256 challenge must be refused)", resp.StatusCode, http.StatusUnauthorized)
		}
	})
}

// --- (i) issuer exactness: the configured issuer must equal the
// discovery document's own `issuer` field EXACTLY, trailing slash and
// all -- review round 1, findings O3/O4/O10. See
// internal/platform/config_test.go's own TestLoadOIDCIssuerURL for the
// config-layer half of this fix (canonicalOIDCIssuerURL no longer strips
// a trailing slash); these two tests prove the SAME fix end-to-end
// through real discovery and a real sign-in.

func TestOIDCSignIn_TrailingSlashIssuer_DiscoverySucceeds(t *testing.T) {
	pool := newTestPool(t)
	provider := newFakeOIDCProvider(t, oidcTestClientID)
	// An Auth0-shaped issuer: the discovery document's own `issuer` field
	// carries a trailing slash, and the CONFIGURED issuer below matches it
	// exactly, trailing slash and all.
	issuer := provider.issuer() + "/"
	provider.setIssuerOverride(issuer)

	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)

	cfg := auth.OIDCConfig{
		Issuer:        issuer,
		ClientID:      oidcTestClientID,
		ClientSecret:  "test-oidc-client-secret",
		PublicBaseURL: "http://narvi.test",
	}
	cache := auth.NewOIDCProviderCache(cfg)
	timeouts := platform.DefaultTimeouts()

	router := chi.NewRouter()
	router.Get("/auth/oidc/login", auth.NewOIDCLoginHandler(cache, timeouts, false))
	router.Get("/auth/oidc/callback", auth.NewOIDCCallbackHandler(
		pool, cache, users, identities, auditLog, userSessions, linkPrompts,
		defaultOIDCRiggedOptions().allowlist, nil, timeouts, false,
	))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	client := newClient(t)
	state, nonce := doOIDCLogin(t, client, server.URL)
	claims := provider.defaultClaims("oidc-subject-trailing-slash")
	claims["iss"] = issuer // must equal the discovered/configured issuer exactly
	claims["email"] = "trailingslash@example.com"
	claims["email_verified"] = true
	claims["nonce"] = nonce
	provider.setNextIDToken(provider.signIDToken(t, claims))

	resp := doOIDCCallback(t, client, server.URL, state, "trailing-slash-code")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d (discovery must succeed when the configured issuer matches the discovery document's own trailing-slash issuer exactly)", resp.StatusCode, http.StatusFound)
	}
}

func TestOIDCSignIn_MismatchedDiscoveryIssuer_Refused(t *testing.T) {
	provider := newFakeOIDCProvider(t, oidcTestClientID)
	// Discovery reports a DIFFERENT issuer than what is configured below
	// -- go-oidc.NewProvider must refuse this (IssuerMismatchError),
	// exactly the failure mode a misconfigured/typo'd NARVI_OIDC_ISSUER
	// would hit against a real IdP.
	provider.setIssuerOverride(provider.issuer() + "/unexpected-path")

	cfg := auth.OIDCConfig{
		Issuer:        provider.issuer(), // does NOT match what discovery reports
		ClientID:      oidcTestClientID,
		ClientSecret:  "test-oidc-client-secret",
		PublicBaseURL: "http://narvi.test",
	}
	cache := auth.NewOIDCProviderCache(cfg)
	timeouts := platform.DefaultTimeouts()

	router := chi.NewRouter()
	router.Get("/auth/oidc/login", auth.NewOIDCLoginHandler(cache, timeouts, false))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	client := newClient(t)
	resp, err := client.Get(server.URL + "/auth/oidc/login")
	if err != nil {
		t.Fatalf("GET /auth/oidc/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (discovery must fail closed when the configured issuer does not match the discovery document's own issuer)", resp.StatusCode, http.StatusServiceUnavailable)
	}
}
