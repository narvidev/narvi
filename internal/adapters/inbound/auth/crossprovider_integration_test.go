//go:build integration

// Cross-provider integration tests for review round 1, findings O1/O2/O9:
// an OIDC-only user must be able to link a GitHub identity later "through
// the ordinary GitHub flow" (§41.3), and the reverse (a GitHub-only user
// signing in later through OIDC) must keep working exactly as it already
// did -- both directions merge on verified email (§13.2 step 3), sharing
// resolveFirstTimeIdentity (firsttimeidentity.go) with Slack/Linear's own
// identitylink.MatchUserIDs/AutoLink algorithm.
//
// Every test here drives BOTH real HTTP flows (the GitHub OAuth pair and
// the OIDC pair) against the SAME shared Postgres pool, via a fresh
// second httptest.Server for whichever flow runs second -- proving the
// merge through the actual callback handlers, never by hand-inserting
// rows that skip the code path under test.
package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// githubTestTokenKey mirrors newTestRig's own fixed, deterministic
// 32-byte token-encryption key -- redeclared here (rather than threading
// it out of newTestRig) so githubRigOnPool below can decrypt
// AccessTokenEncrypted straight back to plaintext and assert on the
// exact value the fake GitHub token endpoint issued.
var githubTestTokenKey = func() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}()

// githubRigOnPool builds a GitHub-flow testRig sharing pool (rather than
// spinning up a fresh one via newTestPool) -- mirrors newTestRig's own
// construction (auth_integration_test.go) one level wider, and
// newRigSharingPool's own identical "share the pool, fresh fakes/
// allowlist" precedent, except the base rig there is always ALSO a
// GitHub testRig -- here the base may be an oidcTestRig instead, which is
// exactly what these cross-provider tests need: one real database, both
// providers' own real HTTP flows in turn.
func githubRigOnPool(t *testing.T, pool *pgxpool.Pool, opts riggedOptions) testRig {
	t.Helper()

	githubAPI := newFakeGitHubAPI(t)
	tokenServer := newFakeTokenServer(t)

	rig := testRig{
		pool:         pool,
		users:        narvipg.NewUserStore(pool),
		identities:   narvipg.NewIdentityStore(pool),
		auditLog:     narvipg.NewAuditLogStore(pool),
		userSessions: narvipg.NewUserSessionStore(pool),
		linkPrompts:  narvipg.NewIdentityLinkPromptStore(pool),
		github:       githubAPI,
		token:        tokenServer,
	}

	oauthConfig := &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  tokenServer.server.URL + "/login/oauth/authorize",
			TokenURL: tokenServer.server.URL + "/login/oauth/access_token",
		},
		RedirectURL: "http://narvi.test/auth/github/callback",
		Scopes:      []string{"user:email", "read:org", "repo"},
	}
	timeouts := platform.DefaultTimeouts()

	router := chi.NewRouter()
	router.Get("/auth/github/login", auth.NewLoginHandler(oauthConfig, timeouts, false))
	router.Get("/auth/github/callback", auth.NewCallbackHandler(
		pool,
		oauthConfig,
		rig.users,
		rig.identities,
		rig.auditLog,
		rig.userSessions,
		rig.linkPrompts,
		opts.allowlist,
		opts.initialAdminEmails,
		githubTestTokenKey,
		timeouts,
		false,
		githubAPI.server.URL,
	))

	rig.server = httptest.NewServer(router)
	t.Cleanup(rig.server.Close)
	return rig
}

// oidcRigOnPool is githubRigOnPool's own OIDC-side counterpart -- mirrors
// newOIDCTestRig (oidc_integration_test.go) one level wider, sharing an
// existing pool rather than starting a fresh one.
func oidcRigOnPool(t *testing.T, pool *pgxpool.Pool, opts oidcRiggedOptions) oidcTestRig {
	t.Helper()

	provider := newFakeOIDCProvider(t, oidcTestClientID)
	rig := oidcTestRig{
		pool:         pool,
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
	return rig
}

// --- OIDC-first, then GitHub with the SAME verified email: one user,
// both identities, the GitHub token stored (O1/O2/O9's own headline
// case: before this fix, this sequence gave a 500 on
// users_primary_email_key). ---

func TestCrossProvider_OIDCFirst_ThenGitHub_SameEmail_MergesOntoOneUser(t *testing.T) {
	ctx := context.Background()
	const email = "octocat@example.com" // matches fakeGitHubAPI's own default fixture exactly

	oidcRig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	oidcClient := newClient(t)
	state, nonce := doOIDCLogin(t, oidcClient, oidcRig.server.URL)
	claims := oidcRig.provider.defaultClaims("oidc-subject-merge-github")
	claims["email"] = email
	claims["email_verified"] = true
	claims["nonce"] = nonce
	oidcRig.provider.setNextIDToken(oidcRig.provider.signIDToken(t, claims))
	oidcResp := doOIDCCallback(t, oidcClient, oidcRig.server.URL, state, "oidc-first-code")
	defer func() { _ = oidcResp.Body.Close() }()
	if oidcResp.StatusCode != http.StatusFound {
		t.Fatalf("oidc callback status = %d, want %d", oidcResp.StatusCode, http.StatusFound)
	}

	oidcExternalID := oidcRig.provider.issuer() + "|oidc-subject-merge-github"
	oidcIdentity, err := oidcRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, oidcExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (oidc): %v", err)
	}

	githubRig := githubRigOnPool(t, oidcRig.pool, defaultRiggedOptions())
	githubRig.github.emails = []map[string]any{{"email": email, "primary": true, "verified": true}}
	githubClient := newClient(t)
	ghState := doLogin(t, githubClient, githubRig.server.URL)
	const githubCode = "github-second-code"
	ghResp := doCallback(t, githubClient, githubRig.server.URL, ghState, githubCode)
	defer func() { _ = ghResp.Body.Close() }()
	if ghResp.StatusCode != http.StatusFound {
		t.Fatalf("github callback status = %d, want %d (must merge onto the existing OIDC user, not 500 on a duplicate primary_email)", ghResp.StatusCode, http.StatusFound)
	}

	githubIdentity, err := githubRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (github): %v", err)
	}
	if githubIdentity.UserID != oidcIdentity.UserID {
		t.Errorf("github identity.UserID = %v, want %v (the SAME user the OIDC sign-in created)", githubIdentity.UserID, oidcIdentity.UserID)
	}
	if githubIdentity.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail {
		t.Errorf("github identity.LinkedVia = %q, want %q", githubIdentity.LinkedVia, sqlcgen.IdentityLinkedViaAutoEmail)
	}

	// Exactly one user row for this email.
	byEmail, err := githubRig.users.GetByPrimaryEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetByPrimaryEmail: %v", err)
	}
	if byEmail.ID != oidcIdentity.UserID {
		t.Errorf("GetByPrimaryEmail resolved to %v, want the original OIDC user %v", byEmail.ID, oidcIdentity.UserID)
	}
	var userCount int
	if err := githubRig.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(primary_email) = lower($1)`, email).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 1 {
		t.Errorf("users with this email = %d, want 1", userCount)
	}

	// The GitHub token really is stored on the newly-created identity row
	// -- the merged user now has git-push credentials, not just a second
	// dead-end identity.
	if len(githubIdentity.AccessTokenEncrypted) == 0 {
		t.Fatal("github identity.AccessTokenEncrypted is empty -- want the fake token endpoint's own issued token stored")
	}
	plaintext, err := platform.DecryptToken(githubTestTokenKey, githubIdentity.AccessTokenEncrypted)
	if err != nil {
		t.Fatalf("DecryptToken: %v", err)
	}
	wantToken := "gho_fake_access_token_" + githubCode
	if string(plaintext) != wantToken {
		t.Errorf("decrypted github token = %q, want %q", string(plaintext), wantToken)
	}

	// Both identities point at the same user, and that user has exactly 2
	// identity rows: the original oidc one and the newly auto-linked
	// github one.
	rows, err := githubRig.pool.Query(ctx, `SELECT provider FROM identities WHERE user_id = $1 ORDER BY provider`, oidcIdentity.UserID)
	if err != nil {
		t.Fatalf("query identities for user: %v", err)
	}
	defer rows.Close()
	var providers []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan provider: %v", err)
		}
		providers = append(providers, p)
	}
	if len(providers) != 2 {
		t.Errorf("identities for the merged user = %v, want exactly 2 (oidc + github)", providers)
	}
}

// --- OIDC-first, then GitHub with a CASE-DIFFERENT verified email:
// still exactly one user (the UNIQUE constraint on users.primary_email
// is case-sensitive, but the graph merge itself compares lower(email) on
// both sides, so this must not silently create a second, unlinked
// user). ---

func TestCrossProvider_OIDCFirst_ThenGitHub_CaseDifferentEmail_MergesOntoOneUser(t *testing.T) {
	ctx := context.Background()
	const oidcEmail = "Jane.Doe@Example.com"
	const githubEmail = "jane.doe@example.com" // same address, different case

	oidcRig := newOIDCTestRig(t, oidcRiggedOptions{allowlist: auth.AllowlistConfig{EmailDomains: []string{"example.com"}}})
	oidcClient := newClient(t)
	state, nonce := doOIDCLogin(t, oidcClient, oidcRig.server.URL)
	claims := oidcRig.provider.defaultClaims("oidc-subject-case-diff")
	claims["email"] = oidcEmail
	claims["email_verified"] = true
	claims["nonce"] = nonce
	oidcRig.provider.setNextIDToken(oidcRig.provider.signIDToken(t, claims))
	oidcResp := doOIDCCallback(t, oidcClient, oidcRig.server.URL, state, "oidc-case-diff-code")
	defer func() { _ = oidcResp.Body.Close() }()
	if oidcResp.StatusCode != http.StatusFound {
		t.Fatalf("oidc callback status = %d, want %d", oidcResp.StatusCode, http.StatusFound)
	}

	oidcExternalID := oidcRig.provider.issuer() + "|oidc-subject-case-diff"
	oidcIdentity, err := oidcRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, oidcExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (oidc): %v", err)
	}

	githubRig := githubRigOnPool(t, oidcRig.pool, riggedOptions{allowlist: auth.AllowlistConfig{EmailDomains: []string{"example.com"}}})
	githubRig.github.emails = []map[string]any{{"email": githubEmail, "primary": true, "verified": true}}
	githubClient := newClient(t)
	ghState := doLogin(t, githubClient, githubRig.server.URL)
	ghResp := doCallback(t, githubClient, githubRig.server.URL, ghState, "github-case-diff-code")
	defer func() { _ = ghResp.Body.Close() }()
	if ghResp.StatusCode != http.StatusFound {
		t.Fatalf("github callback status = %d, want %d (a case-different email must still merge)", ghResp.StatusCode, http.StatusFound)
	}

	githubIdentity, err := githubRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (github): %v", err)
	}
	if githubIdentity.UserID != oidcIdentity.UserID {
		t.Errorf("github identity.UserID = %v, want %v (the SAME user, despite the case-different email)", githubIdentity.UserID, oidcIdentity.UserID)
	}

	var userCount int
	if err := githubRig.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(primary_email) = lower($1)`, oidcEmail).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 1 {
		t.Errorf("users with this email (case-insensitive) = %d, want 1 -- a case-different email must never create a second, unlinked user", userCount)
	}
}

// --- GitHub-first, then OIDC with the SAME verified email: one user --
// the pre-existing direction (this Step's own commit ef51a91 already
// covered it via TestOIDCCallback_GraphMergesOnVerifiedEmail_
// AutoLinksExistingUser using a hand-created user row); this test drives
// it through the REAL GitHub callback instead, proving the shared
// resolveFirstTimeIdentity path didn't change GitHub's own existing
// first-time behavior. ---

func TestCrossProvider_GitHubFirst_ThenOIDC_SameEmail_MergesOntoOneUser(t *testing.T) {
	ctx := context.Background()
	const email = "octocat@example.com"

	githubRig := newTestRig(t, defaultRiggedOptions())
	githubClient := newClient(t)
	ghState := doLogin(t, githubClient, githubRig.server.URL)
	ghResp := doCallback(t, githubClient, githubRig.server.URL, ghState, "github-first-code")
	defer func() { _ = ghResp.Body.Close() }()
	if ghResp.StatusCode != http.StatusFound {
		t.Fatalf("github callback status = %d, want %d", ghResp.StatusCode, http.StatusFound)
	}

	githubIdentity, err := githubRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (github): %v", err)
	}

	oidcRig := oidcRigOnPool(t, githubRig.pool, defaultOIDCRiggedOptions())
	oidcClient := newClient(t)
	state, nonce := doOIDCLogin(t, oidcClient, oidcRig.server.URL)
	claims := oidcRig.provider.defaultClaims("oidc-subject-after-github")
	claims["email"] = email
	claims["email_verified"] = true
	claims["nonce"] = nonce
	oidcRig.provider.setNextIDToken(oidcRig.provider.signIDToken(t, claims))
	oidcResp := doOIDCCallback(t, oidcClient, oidcRig.server.URL, state, "oidc-second-code")
	defer func() { _ = oidcResp.Body.Close() }()
	if oidcResp.StatusCode != http.StatusFound {
		t.Fatalf("oidc callback status = %d, want %d", oidcResp.StatusCode, http.StatusFound)
	}

	oidcExternalID := oidcRig.provider.issuer() + "|oidc-subject-after-github"
	oidcIdentity, err := oidcRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, oidcExternalID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (oidc): %v", err)
	}
	if oidcIdentity.UserID != githubIdentity.UserID {
		t.Errorf("oidc identity.UserID = %v, want %v (the SAME user the GitHub sign-in created)", oidcIdentity.UserID, githubIdentity.UserID)
	}

	var userCount int
	if err := githubRig.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(primary_email) = lower($1)`, email).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 1 {
		t.Errorf("users with this email = %d, want 1", userCount)
	}
}
