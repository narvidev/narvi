//go:build integration

// Integration tests for internal/adapters/inbound/auth's OAuth login ->
// callback -> allowlist -> user/identity creation -> session-cookie flow,
// against a real Postgres instance -- gated behind the "integration" build
// tag, matching internal/adapters/inbound/httpapi's own testcontainers-
// Postgres-plus-embedded-migrations convention exactly (each DB-touching
// package builds its own copy of this small helper rather than sharing one
// across package boundaries, per that package's own precedent). Run via
// `make test-integration`.
//
// GitHub itself (both the OAuth token endpoint and the REST API) is stood
// in for by two local httptest.Server fakes (fakeTokenServer, fakeGitHubAPI
// below) -- design decision 12's own point: NewCallbackHandler's apiBaseURL
// parameter and a second oauth2.Config pointed at a fake token endpoint are
// exactly what make this flow testable with zero real network calls and
// zero real GitHub credentials.
package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go), not freshly per test/container as this function
// used to do itself. Kept as a thin wrapper under its own original
// name/signature so every existing call site in this file keeps
// compiling unchanged. See sharedpool_integration_test.go's own top doc
// comment for the full container-reuse story.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// fakeGitHubAPI stands in for api.github.com's own REST endpoints this
// package calls: GET /user, GET /user/emails, and
// GET /orgs/{org}/members/{username}. Every field is mutable per-test
// (each test builds its own rig, so no cross-test sharing), guarded by a
// mutex since the httptest.Server's handler runs on its own goroutine.
type fakeGitHubAPI struct {
	mu        sync.Mutex
	userID    int64
	login     string
	name      string
	emails    []map[string]any
	orgStatus map[string]int // org -> exact status code to respond with; missing entry -> 404

	server *httptest.Server
}

func newFakeGitHubAPI(t *testing.T) *fakeGitHubAPI {
	t.Helper()

	f := &fakeGitHubAPI{
		userID: 555000111,
		login:  "octocat",
		name:   "The Octocat",
		emails: []map[string]any{
			{"email": "octocat@example.com", "primary": true, "verified": true},
		},
		orgStatus: map[string]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    f.userID,
			"login": f.login,
			"name":  f.name,
		})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.emails)
	})
	mux.HandleFunc("/orgs/", func(w http.ResponseWriter, r *http.Request) {
		// Path shape: /orgs/{org}/members/{username}.
		rest := r.URL.Path[len("/orgs/"):]
		org, _, ok := cutOnMembers(rest)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		status, configured := f.orgStatus[org]
		f.mu.Unlock()
		if !configured {
			status = http.StatusNotFound
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "/orgs/"+org)
		}
		w.WriteHeader(status)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// cutOnMembers splits "{org}/members/{username}" into org and username.
func cutOnMembers(path string) (org, username string, ok bool) {
	const sep = "/members/"
	for i := 0; i+len(sep) <= len(path); i++ {
		if path[i:i+len(sep)] == sep {
			return path[:i], path[i+len(sep):], true
		}
	}
	return "", "", false
}

func (f *fakeGitHubAPI) setEmails(emails []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emails = emails
}

func (f *fakeGitHubAPI) setOrgStatus(org string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgStatus[org] = status
}

// fakeTokenServer stands in for github.com's own OAuth token endpoint
// (POST /login/oauth/access_token). Always succeeds UNLESS the request's
// own "code" form value is exactly failCode, in which case it responds
// like GitHub's own documented error shape for a bad/reused code -- keyed
// off the code value (rather than shared mutable state) so tests can
// trigger success/failure deterministically without any ordering
// dependency between them.
type fakeTokenServer struct {
	mu       sync.Mutex
	calls    int
	server   *httptest.Server
	failCode string
}

const exchangeFailureCode = "trigger-exchange-failure"

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	t.Helper()

	f := &fakeTokenServer{failCode: exchangeFailureCode}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()

		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("code") == f.failCode {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "bad_verification_code",
				"error_description": "The code passed is incorrect or expired.",
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "gho_fake_access_token_" + r.FormValue("code"),
			"token_type":   "bearer",
			"scope":        "user:email,read:org,repo",
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTokenServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testRig bundles a fresh pool + the 3 auth stores + an httptest.Server
// mounting all 3 auth routes exactly as cmd/control-plane/main.go does,
// plus the 2 GitHub fakes it's wired against.
type testRig struct {
	pool         *pgxpool.Pool
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	auditLog     *narvipg.AuditLogStore
	userSessions *narvipg.UserSessionStore
	linkPrompts  *narvipg.IdentityLinkPromptStore
	github       *fakeGitHubAPI
	token        *fakeTokenServer
	server       *httptest.Server
}

// riggedOptions lets individual tests override the allowlist/initial-admin
// config the rig is wired with -- most tests just want a default
// permissive allowlist (a broad email domain covering the fake GitHub
// user's own email) since allowlist behavior itself is exercised by
// dedicated tests.
type riggedOptions struct {
	allowlist          auth.AllowlistConfig
	initialAdminEmails []string
}

func defaultRiggedOptions() riggedOptions {
	return riggedOptions{
		allowlist: auth.AllowlistConfig{EmailDomains: []string{"example.com"}},
	}
}

func newTestRig(t *testing.T, opts riggedOptions) testRig {
	t.Helper()
	pool := newTestPool(t)
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

	tokenKey := make([]byte, 32)
	for i := range tokenKey {
		tokenKey[i] = byte(i)
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
		tokenKey,
		timeouts,
		false,
		githubAPI.server.URL,
	))
	router.Post("/auth/logout", auth.NewLogoutHandler(rig.userSessions, false))

	rig.server = httptest.NewServer(router)
	t.Cleanup(rig.server.Close)

	return rig
}

// newClient returns an http.Client with its own cookie jar (so the
// short-lived oauth-state cookie set by /auth/github/login round-trips
// automatically into the following /auth/github/callback request, exactly
// like a real browser) and CheckRedirect disabled (http.ErrUseLastResponse)
// so the test can inspect each 302's own Location/Set-Cookie headers
// directly instead of following them.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// doLogin issues GET /auth/github/login and returns the `state` query
// param GitHub's own authorize URL (in the Location header) was minted
// with. The oauth-state cookie itself is captured into client's own jar as
// a side effect of the response's Set-Cookie header.
func doLogin(t *testing.T, client *http.Client, serverURL string) string {
	t.Helper()

	resp, err := client.Get(serverURL + "/auth/github/login")
	if err != nil {
		t.Fatalf("GET /auth/github/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /auth/github/login status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("resp.Location(): %v", err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect Location has no state query param")
	}
	return state
}

// doLoginWithNext mirrors doLogin but also passes ?next=next -- §13.2's
// ("identities + full RBAC", §13.2) own addition, letting a test drive the
// post-login-redirect-target flow the SAME way internal/adapters/inbound/
// identitylink's magic-link consume handler will (see login.go's own doc
// comment on NewLoginHandler).
func doLoginWithNext(t *testing.T, client *http.Client, serverURL, next string) string {
	t.Helper()

	resp, err := client.Get(serverURL + "/auth/github/login?next=" + url.QueryEscape(next))
	if err != nil {
		t.Fatalf("GET /auth/github/login?next=...: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /auth/github/login?next=... status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("resp.Location(): %v", err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect Location has no state query param")
	}
	return state
}

// doCallback issues GET /auth/github/callback?state=...&code=... via
// client (whose jar carries whatever oauth-state cookie a prior doLogin
// call captured, if any).
func doCallback(t *testing.T, client *http.Client, serverURL, state, code string) *http.Response {
	t.Helper()

	u := serverURL + "/auth/github/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code)
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET /auth/github/callback: %v", err)
	}
	return resp
}

// pgtypeTimestamptz wraps t as a valid pgtype.Timestamptz -- a tiny local
// helper so every direct test-fixture session-row insert in this file
// shares one conversion, matching sqlcgen's own field type exactly.
func pgtypeTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// authCookieFrom extracts the narvi_auth_session cookie's value from resp,
// failing the test if it isn't present.
func authCookieFrom(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == platform.AuthSessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("response has no %s cookie; got: %v", platform.AuthSessionCookieName, resp.Cookies())
	return ""
}

// --- (a) full first-time-sign-in flow end to end ---

func TestCallback_FirstTimeSignIn_HappyPath(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()
	client := newClient(t)

	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "a-fresh-code")
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

	// The token exchange must actually have happened exactly once.
	if got := rig.token.callCount(); got != 1 {
		t.Errorf("token endpoint call count = %d, want 1", got)
	}

	// A user + identity row must now exist.
	identity, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.Email == nil || *identity.Email != "octocat@example.com" {
		t.Errorf("identity.Email = %v, want octocat@example.com", identity.Email)
	}
	if !identity.EmailVerified {
		t.Error("identity.EmailVerified = false, want true")
	}
	if identity.LinkedVia != sqlcgen.IdentityLinkedViaAdmin {
		t.Errorf("identity.LinkedVia = %q, want %q", identity.LinkedVia, sqlcgen.IdentityLinkedViaAdmin)
	}
	if len(identity.AccessTokenEncrypted) == 0 {
		t.Error("identity.AccessTokenEncrypted is empty, want a non-empty ciphertext")
	}

	user, err := rig.users.GetByID(ctx, identity.UserID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if user.PrimaryEmail != "octocat@example.com" {
		t.Errorf("user.PrimaryEmail = %q, want %q", user.PrimaryEmail, "octocat@example.com")
	}
	if user.Role != sqlcgen.UserRoleMember {
		t.Errorf("user.Role = %q, want %q (not in InitialAdminEmails)", user.Role, sqlcgen.UserRoleMember)
	}
	if user.Disabled {
		t.Error("user.Disabled = true, want false for a freshly created user")
	}

	// The session cookie's hash must match a real, correctly-scoped
	// user_sessions row.
	sessionRow, err := rig.userSessions.GetByHash(ctx, platform.HashToken(sessionToken))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if sessionRow.UserID != user.ID {
		t.Errorf("sessionRow.UserID = %v, want %v", sessionRow.UserID, user.ID)
	}
	wantExpiry := time.Now().Add(platform.DefaultTimeouts().UserSessionTTL)
	if sessionRow.ExpiresAt.Time.Sub(wantExpiry).Abs() > time.Minute {
		t.Errorf("sessionRow.ExpiresAt = %v, want close to %v", sessionRow.ExpiresAt.Time, wantExpiry)
	}
}

// TestCallback_HonorsSafeNextRedirect proves §13.2's ("identities + full
// RBAC", §13.2) own ?next= addition: a login started with a safe,
// same-origin absolute-path next redirects there on success, instead of
// this flow's own fixed "/" default.
func TestCallback_HonorsSafeNextRedirect(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	client := newClient(t)

	state := doLoginWithNext(t, client, rig.server.URL, "/auth/identity-link/some-nonce")
	resp := doCallback(t, client, rig.server.URL, state, "a-fresh-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if loc, _ := resp.Location(); loc == nil || loc.Path != "/auth/identity-link/some-nonce" {
		t.Errorf("callback redirected to %v, want \"/auth/identity-link/some-nonce\"", loc)
	}
}

// TestCallback_IgnoresUnsafeNextRedirect proves an unsafe next value (here:
// a scheme-relative "//evil.example.com" URL, the classic open-redirect
// vector isSafeRedirectNext exists to reject) is silently ignored --
// falling back to this flow's own fixed "/" default, never redirecting
// off-site.
func TestCallback_IgnoresUnsafeNextRedirect(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	client := newClient(t)

	state := doLoginWithNext(t, client, rig.server.URL, "//evil.example.com")
	resp := doCallback(t, client, rig.server.URL, state, "a-fresh-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	// resp.Location() always resolves against the request's own URL (so
	// loc.Host is the test server's OWN host even for a plain "/" target,
	// never empty) -- what matters here is that loc.Path is the fixed "/"
	// default and loc.Host is the SAME server, never an attacker-chosen
	// one.
	serverURL, err := url.Parse(rig.server.URL)
	if err != nil {
		t.Fatalf("url.Parse(rig.server.URL): %v", err)
	}
	if loc, _ := resp.Location(); loc == nil || loc.Path != "/" || loc.Host != serverURL.Host {
		t.Errorf("callback redirected to %v, want a same-origin \"/\" (unsafe next must be ignored)", loc)
	}
}

// TestCallback_FirstTimeSignIn_InitialAdminGetsAdminRole proves a verified
// email present in InitialAdminEmails is created with role "admin" instead
// of the enum's own "member" default.
func TestCallback_FirstTimeSignIn_InitialAdminGetsAdminRole(t *testing.T) {
	opts := defaultRiggedOptions()
	opts.initialAdminEmails = []string{"OCTOCAT@EXAMPLE.COM"} // case-insensitive match
	rig := newTestRig(t, opts)
	ctx := context.Background()
	client := newClient(t)

	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "admin-signup-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	identity, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	user, err := rig.users.GetByID(ctx, identity.UserID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if user.Role != sqlcgen.UserRoleAdmin {
		t.Errorf("user.Role = %q, want %q (verified email is in InitialAdminEmails)", user.Role, sqlcgen.UserRoleAdmin)
	}
}

// getAuditLogRowsForResource fetches every audit_log row matching
// (resourceType, resourceID) -- mirrors internal/app/sessionactor's own
// identically-named test helper (planrecord_integration_test.go), rebuilt
// locally since this package's own integration tests live in a separate
// (auth_test) package and so cannot reach that one's unexported helper.
func getAuditLogRowsForResource(ctx context.Context, t *testing.T, pool *pgxpool.Pool, resourceType, resourceID string) []sqlcgen.AuditLog {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT id, actor_user_id, action, resource_type, resource_id, detail_json, correlation_id, created_at FROM audit_log WHERE resource_type = $1 AND resource_id = $2`,
		resourceType, resourceID)
	if err != nil {
		t.Fatalf("query audit_log rows: %v", err)
	}
	defer rows.Close()

	var out []sqlcgen.AuditLog
	for rows.Next() {
		var a sqlcgen.AuditLog
		if err := rows.Scan(&a.ID, &a.ActorUserID, &a.Action, &a.ResourceType, &a.ResourceID, &a.DetailJson, &a.CorrelationID, &a.CreatedAt); err != nil {
			t.Fatalf("scan audit_log row: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit_log rows: %v", err)
	}
	return out
}

// TestCallback_FirstTimeSignIn_RecordsUserCreatedAuditLog proves the
// audit-fix batch's own M18 fix: an ordinary (non-admin-email) first-time
// sign-in writes exactly one "user.created" audit_log row -- previously
// this codebase's one identity/role mutation with no audit trail at all --
// attributed to the newly-created user's OWN id (a self-registration
// event: there is no OTHER, distinct acting user to attribute it to).
func TestCallback_FirstTimeSignIn_RecordsUserCreatedAuditLog(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()
	client := newClient(t)

	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "audit-happy-path-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	identity, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	user, err := rig.users.GetByID(ctx, identity.UserID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if user.Role != sqlcgen.UserRoleMember {
		t.Fatalf("user.Role = %q, want %q (sanity check on the fixture itself)", user.Role, sqlcgen.UserRoleMember)
	}

	auditRows := getAuditLogRowsForResource(ctx, t, rig.pool, "user", user.ID.String())
	if len(auditRows) != 1 {
		t.Fatalf("audit_log rows for created user = %d, want exactly 1", len(auditRows))
	}
	auditRow := auditRows[0]
	if auditRow.Action != "user.created" {
		t.Errorf("Action = %q, want %q", auditRow.Action, "user.created")
	}
	if auditRow.ResourceType != "user" {
		t.Errorf("ResourceType = %q, want %q", auditRow.ResourceType, "user")
	}
	if auditRow.ResourceID != user.ID.String() {
		t.Errorf("ResourceID = %q, want %q", auditRow.ResourceID, user.ID.String())
	}
	if !auditRow.ActorUserID.Valid || auditRow.ActorUserID != user.ID {
		t.Errorf("ActorUserID = %v, want %v (the newly-created user's own id -- a self-registration event has no OTHER human actor to attribute it to)", auditRow.ActorUserID, user.ID)
	}

	var detail map[string]any
	if err := json.Unmarshal(auditRow.DetailJson, &detail); err != nil {
		t.Fatalf("unmarshal detail_json: %v", err)
	}
	if detail["role"] != "member" {
		t.Errorf("detail_json[role] = %v, want %q", detail["role"], "member")
	}
	if detail["github_login"] != "octocat" {
		t.Errorf("detail_json[github_login] = %v, want %q", detail["github_login"], "octocat")
	}
}

// TestCallback_FirstTimeSignIn_BootstrapAdmin_RecordsUserCreatedAuditLog is
// TestCallback_FirstTimeSignIn_RecordsUserCreatedAuditLog's sibling for the
// bootstrap-admin case (verified email matches InitialAdminEmails): same
// assertions, except detail_json["role"] must be "admin" -- this is
// exactly the case the finding calls out as silently granting a role with
// no audit trail.
func TestCallback_FirstTimeSignIn_BootstrapAdmin_RecordsUserCreatedAuditLog(t *testing.T) {
	opts := defaultRiggedOptions()
	opts.initialAdminEmails = []string{"octocat@example.com"}
	rig := newTestRig(t, opts)
	ctx := context.Background()
	client := newClient(t)

	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "audit-bootstrap-admin-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	identity, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	user, err := rig.users.GetByID(ctx, identity.UserID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if user.Role != sqlcgen.UserRoleAdmin {
		t.Fatalf("user.Role = %q, want %q (sanity check on the fixture itself)", user.Role, sqlcgen.UserRoleAdmin)
	}

	auditRows := getAuditLogRowsForResource(ctx, t, rig.pool, "user", user.ID.String())
	if len(auditRows) != 1 {
		t.Fatalf("audit_log rows for created user = %d, want exactly 1", len(auditRows))
	}
	auditRow := auditRows[0]
	if auditRow.Action != "user.created" {
		t.Errorf("Action = %q, want %q", auditRow.Action, "user.created")
	}
	if !auditRow.ActorUserID.Valid || auditRow.ActorUserID != user.ID {
		t.Errorf("ActorUserID = %v, want %v (the newly-created user's own id)", auditRow.ActorUserID, user.ID)
	}

	var detail map[string]any
	if err := json.Unmarshal(auditRow.DetailJson, &detail); err != nil {
		t.Fatalf("unmarshal detail_json: %v", err)
	}
	if detail["role"] != "admin" {
		t.Errorf("detail_json[role] = %v, want %q (bootstrap-admin case)", detail["role"], "admin")
	}
	if detail["github_login"] != "octocat" {
		t.Errorf("detail_json[github_login] = %v, want %q", detail["github_login"], "octocat")
	}
}

// --- (b) returning user skips the allowlist entirely ---

func TestCallback_ReturningUser_SkipsAllowlistAndRefreshesToken(t *testing.T) {
	// An allowlist that would deny EVERY sign-in -- proves the returning-
	// user branch truly never re-evaluates it.
	denyAllOpts := riggedOptions{allowlist: auth.AllowlistConfig{Emails: []string{"nobody@nowhere.invalid"}}}
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()

	// First sign-in: creates the user+identity (uses the rig's own
	// permissive default allowlist).
	client1 := newClient(t)
	state1 := doLogin(t, client1, rig.server.URL)
	resp1 := doCallback(t, client1, rig.server.URL, state1, "first-signin-code")
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusFound {
		t.Fatalf("first callback status = %d, want %d", resp1.StatusCode, http.StatusFound)
	}
	identityBefore, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (before): %v", err)
	}

	// Re-wire a SECOND rig sharing the SAME pool but a deny-all allowlist,
	// simulating "the allowlist was tightened after this user already
	// signed up" -- the returning-user branch must still succeed.
	rig2 := newRigSharingPool(t, rig, denyAllOpts)

	client2 := newClient(t)
	state2 := doLogin(t, client2, rig2.server.URL)
	resp2 := doCallback(t, client2, rig2.server.URL, state2, "second-signin-code")
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("returning-user callback status = %d, want %d (allowlist must be skipped)", resp2.StatusCode, http.StatusFound)
	}

	identityAfter, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (after): %v", err)
	}
	if identityAfter.ID != identityBefore.ID {
		t.Errorf("a NEW identity row was created (id %v != %v) -- want the SAME row reused", identityAfter.ID, identityBefore.ID)
	}
	if identityAfter.UserID != identityBefore.UserID {
		t.Errorf("identity now points at a different user (%v != %v)", identityAfter.UserID, identityBefore.UserID)
	}

	// A second, distinct session must have been minted.
	sessionToken2 := authCookieFrom(t, resp2)
	sessionRow, err := rig.userSessions.GetByHash(ctx, platform.HashToken(sessionToken2))
	if err != nil {
		t.Fatalf("GetByHash for returning-user session: %v", err)
	}
	if sessionRow.UserID != identityBefore.UserID {
		t.Errorf("returning-user session UserID = %v, want %v", sessionRow.UserID, identityBefore.UserID)
	}
}

// newRigSharingPool builds a second testRig sharing base.pool (and hence
// its already-created users/identities/user_sessions rows) but wired with
// its own fresh GitHub fakes and allowlist -- used to simulate "the same
// backend, a later sign-in, under different allowlist config" without
// spinning up a second Postgres container.
func newRigSharingPool(t *testing.T, base testRig, opts riggedOptions) testRig {
	t.Helper()

	rig := testRig{
		pool:         base.pool,
		users:        base.users,
		identities:   base.identities,
		auditLog:     base.auditLog,
		userSessions: base.userSessions,
		linkPrompts:  base.linkPrompts,
		github:       newFakeGitHubAPI(t),
		token:        newFakeTokenServer(t),
	}
	// Reuse the SAME github user identity as the base rig's own default
	// fixture (id 555000111, octocat@example.com) so the identity lookup
	// in the callback hits the SAME existing row.

	oauthConfig := &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  rig.token.server.URL + "/login/oauth/authorize",
			TokenURL: rig.token.server.URL + "/login/oauth/access_token",
		},
		RedirectURL: "http://narvi.test/auth/github/callback",
		Scopes:      []string{"user:email", "read:org", "repo"},
	}
	tokenKey := make([]byte, 32)
	for i := range tokenKey {
		tokenKey[i] = byte(i)
	}
	timeouts := platform.DefaultTimeouts()

	router := chi.NewRouter()
	router.Get("/auth/github/login", auth.NewLoginHandler(oauthConfig, timeouts, false))
	router.Get("/auth/github/callback", auth.NewCallbackHandler(
		rig.pool,
		oauthConfig,
		rig.users,
		rig.identities,
		rig.auditLog,
		rig.userSessions,
		rig.linkPrompts,
		opts.allowlist,
		opts.initialAdminEmails,
		tokenKey,
		timeouts,
		false,
		rig.github.server.URL,
	))
	router.Post("/auth/logout", auth.NewLogoutHandler(rig.userSessions, false))

	rig.server = httptest.NewServer(router)
	t.Cleanup(rig.server.Close)

	return rig
}

// --- (c) missing/mismatched state -> 400, exchange never attempted ---

func TestCallback_StateMismatch(t *testing.T) {
	tests := []struct {
		name          string
		doLoginFirst  bool
		callbackState string
	}{
		{name: "no oauth-state cookie at all", doLoginFirst: false, callbackState: "some-state-value"},
		{name: "cookie present but state param differs", doLoginFirst: true, callbackState: "a-completely-different-value"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t, defaultRiggedOptions())
			client := newClient(t)

			if tc.doLoginFirst {
				doLogin(t, client, rig.server.URL)
			}

			resp := doCallback(t, client, rig.server.URL, tc.callbackState, "irrelevant-code")
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			if got := rig.token.callCount(); got != 0 {
				t.Errorf("token endpoint call count = %d, want 0 (exchange must never be attempted)", got)
			}
		})
	}
}

// --- exchange failure -> 401 ---

func TestCallback_ExchangeFailure(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	client := newClient(t)

	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, exchangeFailureCode)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	// golang.org/x/oauth2's own Exchange retries once, using a different
	// client-authentication style (credentials in the body vs. an
	// Authorization header), when the token endpoint's first response is a
	// non-2xx status -- verified directly against this exact pinned
	// version (v0.36.0) during this Step's own test-writing, not assumed.
	// Either way, the exchange WAS genuinely attempted (never zero calls).
	if got := rig.token.callCount(); got == 0 {
		t.Error("token endpoint call count = 0, want at least 1 (exchange WAS attempted, just failed)")
	}
}

// --- (f) no verified primary email -> 403, nothing created ---

func TestCallback_NoVerifiedPrimaryEmail(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()
	rig.github.setEmails([]map[string]any{
		{"email": "unverified@example.com", "primary": true, "verified": false},
	})

	client := newClient(t)
	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "no-verified-email-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByProviderAndExternalID error = %v, want pgx.ErrNoRows (no identity should have been created)", err)
	}
}

// TestCallback_AmbiguousMatch_Refused proves the GitHub-side half of
// review round 2's own finding P6: cae3c81 gave the GitHub callback its
// own §13.2 "never guess" ambiguous-match branch (shared with the OIDC
// callback via resolveFirstTimeIdentity), but no test drove it -- only
// TestOIDCCallback_AmbiguousMatch_Refused (oidc_integration_test.go)
// covered the OIDC variant. Mirrors that test exactly, one provider over:
// two existing users each already have their OWN verified identity
// sharing the SAME email (via Slack, so this test proves the merge logic
// itself, not merely a users.primary_email collision) -- a first-time
// GitHub sign-in reporting that same verified email must be refused,
// never guessed, and audited under auditActionGitHubAmbiguousMatch.
func TestCallback_AmbiguousMatch_Refused(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()

	userA, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "gh-ambiguous-person-a@example.com",
		DisplayName:  "GH Ambiguous Person A",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create userA: %v", err)
	}
	userB, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "gh-ambiguous-person-b@example.com",
		DisplayName:  "GH Ambiguous Person B",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create userB: %v", err)
	}
	ambiguousEmail := "gh-ambiguous@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        userA.ID,
		Provider:      sqlcgen.IdentityProviderSlack,
		ExternalID:    "gh-ambiguous-slack-a",
		Email:         &ambiguousEmail,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("create identity for userA: %v", err)
	}
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        userB.ID,
		Provider:      sqlcgen.IdentityProviderSlack,
		ExternalID:    "gh-ambiguous-slack-b",
		Email:         &ambiguousEmail,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("create identity for userB: %v", err)
	}

	rig.github.setEmails([]map[string]any{
		{"email": ambiguousEmail, "primary": true, "verified": true},
	})

	client := newClient(t)
	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "gh-ambiguous-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d (never guess between two matching users)", resp.StatusCode, http.StatusForbidden)
	}

	// The fake GitHub API's own default userID (555000111, set by
	// newFakeGitHubAPI) is this sign-in's own external_id.
	if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByProviderAndExternalID error = %v, want pgx.ErrNoRows (no identity should have been created for the ambiguous sign-in)", err)
	}

	rows := getAuditLogRowsForResource(context.Background(), t, rig.pool, "identity", "555000111")
	if len(rows) != 1 {
		t.Fatalf("audit_log rows for the ambiguous refusal = %d, want 1", len(rows))
	}
	if rows[0].Action != "identity.github_ambiguous_match" {
		t.Errorf("audit_log action = %q, want %q", rows[0].Action, "identity.github_ambiguous_match")
	}
}

// --- (d) allowlist rejection/acceptance via each of the 3 mechanisms ---

func TestCallback_Allowlist_EmailExactMatch(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		opts := riggedOptions{allowlist: auth.AllowlistConfig{Emails: []string{"octocat@example.com"}}}
		rig := newTestRig(t, opts)
		client := newClient(t)
		state := doLogin(t, client, rig.server.URL)
		resp := doCallback(t, client, rig.server.URL, state, "email-allowed-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
	})

	t.Run("denied", func(t *testing.T) {
		opts := riggedOptions{allowlist: auth.AllowlistConfig{Emails: []string{"someone-else@example.com"}}}
		rig := newTestRig(t, opts)
		ctx := context.Background()
		client := newClient(t)
		state := doLogin(t, client, rig.server.URL)
		resp := doCallback(t, client, rig.server.URL, state, "email-denied-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
		}
		if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111"); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetByProviderAndExternalID error = %v, want pgx.ErrNoRows", err)
		}
	})
}

func TestCallback_Allowlist_EmailDomain(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		opts := riggedOptions{allowlist: auth.AllowlistConfig{EmailDomains: []string{"example.com"}}}
		rig := newTestRig(t, opts)
		client := newClient(t)
		state := doLogin(t, client, rig.server.URL)
		resp := doCallback(t, client, rig.server.URL, state, "domain-allowed-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
	})

	t.Run("denied", func(t *testing.T) {
		opts := riggedOptions{allowlist: auth.AllowlistConfig{EmailDomains: []string{"other-company.com"}}}
		rig := newTestRig(t, opts)
		client := newClient(t)
		state := doLogin(t, client, rig.server.URL)
		resp := doCallback(t, client, rig.server.URL, state, "domain-denied-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
		}
	})
}

func TestCallback_Allowlist_GitHubOrgMembership(t *testing.T) {
	t.Run("allowed (204)", func(t *testing.T) {
		opts := riggedOptions{allowlist: auth.AllowlistConfig{GitHubOrgs: []string{"my-org"}}}
		rig := newTestRig(t, opts)
		rig.github.setOrgStatus("my-org", http.StatusNoContent)
		client := newClient(t)
		state := doLogin(t, client, rig.server.URL)
		resp := doCallback(t, client, rig.server.URL, state, "org-allowed-code")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
	})

	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "denied (404, requester is an org member but checked user is not)", status: http.StatusNotFound},
		{name: "denied (302, requester is not an org member at all)", status: http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := riggedOptions{allowlist: auth.AllowlistConfig{GitHubOrgs: []string{"my-org"}}}
			rig := newTestRig(t, opts)
			rig.github.setOrgStatus("my-org", tc.status)
			ctx := context.Background()
			client := newClient(t)
			state := doLogin(t, client, rig.server.URL)
			resp := doCallback(t, client, rig.server.URL, state, "org-denied-code")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
			}
			if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, "555000111"); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("GetByProviderAndExternalID error = %v, want pgx.ErrNoRows", err)
			}
		})
	}
}

func TestCallback_Allowlist_AllThreeMechanismsFail(t *testing.T) {
	opts := riggedOptions{
		allowlist: auth.AllowlistConfig{
			Emails:       []string{"nobody@nowhere.invalid"},
			EmailDomains: []string{"other-company.com"},
			GitHubOrgs:   []string{"my-org"},
		},
	}
	rig := newTestRig(t, opts)
	rig.github.setOrgStatus("my-org", http.StatusNotFound)

	client := newClient(t)
	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "all-denied-code")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// --- (g) Middleware ---

func TestMiddleware(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()

	// A real, valid user + session for the "genuinely valid" case.
	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "valid@example.com",
		DisplayName:  "Valid User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	validToken, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := rig.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(validToken),
		ExpiresAt: pgtypeTimestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("create test session: %v", err)
	}

	// An expired session.
	expiredToken, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := rig.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(expiredToken),
		ExpiresAt: pgtypeTimestamptz(time.Now().Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("create expired test session: %v", err)
	}

	// A disabled user with an otherwise-valid session.
	disabledUser, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "disabled@example.com",
		DisplayName:  "Disabled User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create disabled test user: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, disabledUser.ID); err != nil {
		t.Fatalf("disable test user: %v", err)
	}
	disabledUserToken, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := rig.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    disabledUser.ID,
		TokenHash: platform.HashToken(disabledUserToken),
		ExpiresAt: pgtypeTimestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("create disabled-user test session: %v", err)
	}

	router := chi.NewRouter()
	router.Route("/protected", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			authUser, ok := platform.UserFromContext(r.Context())
			if !ok {
				t.Error("platform.UserFromContext: ok = false inside a middleware-gated handler")
			}
			if authUser.ID != user.ID.String() {
				t.Errorf("AuthenticatedUser.ID = %q, want %q", authUser.ID, user.ID.String())
			}
			if authUser.Role != string(sqlcgen.UserRoleMember) {
				t.Errorf("AuthenticatedUser.Role = %q, want %q", authUser.Role, sqlcgen.UserRoleMember)
			}
			if authUser.Email != "valid@example.com" {
				t.Errorf("AuthenticatedUser.Email = %q, want %q", authUser.Email, "valid@example.com")
			}
			w.WriteHeader(http.StatusOK)
		})
	})
	server := httptest.NewServer(router)
	defer server.Close()

	tests := []struct {
		name       string
		cookie     *http.Cookie
		wantStatus int
	}{
		{name: "no cookie", cookie: nil, wantStatus: http.StatusUnauthorized},
		{name: "hash not found", cookie: &http.Cookie{Name: platform.AuthSessionCookieName, Value: "not-a-real-token"}, wantStatus: http.StatusUnauthorized},
		{name: "expired session", cookie: &http.Cookie{Name: platform.AuthSessionCookieName, Value: expiredToken}, wantStatus: http.StatusUnauthorized},
		{name: "disabled user", cookie: &http.Cookie{Name: platform.AuthSessionCookieName, Value: disabledUserToken}, wantStatus: http.StatusUnauthorized},
		{name: "valid session", cookie: &http.Cookie{Name: platform.AuthSessionCookieName, Value: validToken}, wantStatus: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, server.URL+"/protected/", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// --- (h) logout ---

func TestLogout(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()

	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "logout@example.com",
		DisplayName:  "Logout Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := rig.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtypeTimestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("create test session: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})

	resp1, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("first logout request: %v", err)
	}
	defer func() { _ = resp1.Body.Close() }()

	foundCleared := false
	for _, c := range resp1.Cookies() {
		if c.Name == platform.AuthSessionCookieName {
			foundCleared = true
			if c.MaxAge >= 0 {
				t.Errorf("cleared cookie MaxAge = %d, want < 0", c.MaxAge)
			}
		}
	}
	if !foundCleared {
		t.Error("logout response did not clear the auth session cookie")
	}

	// The row must actually be gone -- a real DB delete, not just a
	// response that looks right.
	if _, err := rig.userSessions.GetByHash(ctx, platform.HashToken(token)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByHash after logout: error = %v, want pgx.ErrNoRows (row must be deleted)", err)
	}

	// Calling logout again (same, now-stale cookie) must be idempotent --
	// no error, still clears the cookie.
	req2, err := http.NewRequest(http.MethodPost, rig.server.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatalf("build second request: %v", err)
	}
	req2.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second logout request: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNoContent {
		t.Errorf("second logout status = %d, want %d", resp2.StatusCode, http.StatusNoContent)
	}
}
