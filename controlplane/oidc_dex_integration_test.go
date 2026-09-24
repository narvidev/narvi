//go:build integration

// TestOIDCSignIn_RealIdP_Dex is §41.3's own exit criterion #1: a user with
// no GitHub account signs in through a REAL IdP -- not a fake httptest
// stand-in (internal/adapters/inbound/auth's own oidc_integration_test.go
// already covers the fake-provider case exhaustively) -- lands
// authenticated, has the role the allowlist assigned, appears once in
// Settings -> Members, and a second sign-in through the same IdP creates
// no second user.
//
// Dex (ghcr.io/dexidp/dex, Apache-2.0) is a real, open-source, spec-
// compliant OIDC provider -- pinned by tag AND digest (verified via a real
// `docker pull` during this Step's own test-writing, not assumed), run
// via testcontainers-go with a static-password user and a static OAuth2
// client. This test drives the REAL browser-less flow against the REAL
// control-plane router (Build, the SAME composition root cmd/control-plane
// itself uses) end to end: it follows every redirect programmatically and
// POSTs Dex's own real login form (an html/template-free plain form:
// `login`/`password` fields, no CSRF token -- confirmed live against a
// throwaway Dex container during this Step's own design phase) rather than
// calling any handler function directly.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/platform"
)

// dexImage is pinned by BOTH tag and digest -- verified live via `docker
// pull ghcr.io/dexidp/dex:v2.41.1` during this Step's own test-writing
// (the digest below is that pull's own, real Docker Content Digest, not a
// value invented for this file). Apache-2.0 licensed
// (github.com/dexidp/dex/blob/master/LICENSE).
const dexImage = "ghcr.io/dexidp/dex:v2.41.1@sha256:bc7cfce7c17f52864e2bb2a4dc1d2f86a41e3019f6d42e81d92a301fad0c8a1d"

// dexStaticPasswordHash is the bcrypt hash of the plain password
// "password" -- Dex's own published example static-password hash
// (dexidp/dex's own example config in their public repository), reused
// here rather than computed fresh: this is a throwaway, single-use test
// credential against a throwaway container that only ever exists for the
// duration of this one test, so there is no secret to protect by hashing
// a freshly generated password instead.
const dexStaticPasswordHash = "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W"
const dexStaticPasswordPlain = "password"
const dexStaticUserEmail = "testuser@example.com"

// findFreeTCPPort reserves a free, ephemeral TCP port on 127.0.0.1 and
// releases it immediately -- needed because Dex's own config.yaml must
// declare its `issuer` URL (which bakes the exact host:port into every
// signed token and the discovery document) BEFORE the container starts,
// but testcontainers only assigns a real host port once the container
// actually starts. The small release-then-rebind race window this
// implies is the standard, widely-used pattern for exactly this
// "config must know the port in advance" class of problem; a Docker host
// under normal test-suite load reliably does not race this one port
// back into use inside the few milliseconds before the container claims
// it.
func findFreeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return port
}

// dexConfigYAML renders a minimal, real Dex config: one static OAuth2
// client (confidential -- Dex treats a staticClients entry as confidential
// unless marked public), one static-password user, PKCE and the
// authorization-code flow both natively supported by Dex with no
// additional config (confirmed live: Dex's own discovery document lists
// `code_challenge_methods_supported: ["S256","plain"]` unconditionally).
// skipApprovalScreen avoids a SECOND HTML form this test would otherwise
// have to drive.
func dexConfigYAML(issuer, clientID, clientSecret, redirectURI string) string {
	return fmt.Sprintf(`issuer: %s
storage:
  type: sqlite3
  config:
    file: /tmp/dex.db
web:
  http: 0.0.0.0:5556
oauth2:
  skipApprovalScreen: true
staticClients:
- id: %s
  secret: %s
  name: 'Narvi integration test'
  redirectURIs:
  - '%s'
enablePasswordDB: true
staticPasswords:
- email: %q
  hash: %q
  username: "testuser"
  userID: "08a8684b-db88-4b73-90a9-3cd1661f5466"
`, issuer, clientID, clientSecret, redirectURI, dexStaticUserEmail, dexStaticPasswordHash)
}

// startDex starts a real Dex container bound to the given fixed host
// port (findFreeTCPPort's own reservation), configured with configYAML.
func startDex(t *testing.T, hostPort int, configYAML string) {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        dexImage,
		ExposedPorts: []string{"5556/tcp"},
		Cmd:          []string{"dex", "serve", "/etc/dex/config.yaml"},
		Files: []testcontainers.ContainerFile{
			{
				Reader:            strings.NewReader(configYAML),
				ContainerFilePath: "/etc/dex/config.yaml",
				FileMode:          0o644,
			},
		},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("5556/tcp"): []network.PortBinding{
					{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: strconv.Itoa(hostPort)},
				},
			}
		},
		WaitingFor: wait.ForHTTP("/dex/.well-known/openid-configuration").WithPort("5556/tcp"),
	}
	dexContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start dex container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(dexContainer); err != nil {
			t.Errorf("terminate dex container: %v", err)
		}
	})
}

// dexTestClient returns an http.Client with its own cookie jar (for
// narvi_oidc_state/nonce/verifier round-tripping into the control
// plane's own callback, exactly like auth_integration_test.go's own
// newClient) and CheckRedirect disabled so this test inspects and drives
// every hop itself.
func dexTestClient(t *testing.T) *http.Client {
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

// driveDexSignIn drives one full sign-in attempt through the real Dex
// container, starting from cpBaseURL+"/auth/oidc/login" and ending at
// whatever the control plane's own /auth/oidc/callback responds with --
// every redirect followed programmatically (resp.Location() resolves a
// relative Location header against the response's own request URL
// automatically, so no manual URL-joining is needed for Dex's own
// internal, relative hops), and Dex's real login form POSTed with the
// static-password credentials. Returns the FINAL response (the control
// plane's own callback response, whatever it is -- the caller asserts on
// it).
func driveDexSignIn(t *testing.T, client *http.Client, cpBaseURL string) *http.Response {
	t.Helper()

	resp1, err := client.Get(cpBaseURL + "/auth/oidc/login")
	if err != nil {
		t.Fatalf("GET /auth/oidc/login: %v", err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusFound {
		t.Fatalf("GET /auth/oidc/login status = %d, want %d", resp1.StatusCode, http.StatusFound)
	}
	loc1, err := resp1.Location()
	if err != nil {
		t.Fatalf("resp1.Location(): %v", err)
	}

	resp2, err := client.Get(loc1.String())
	if err != nil {
		t.Fatalf("GET dex authorize (%s): %v", loc1, err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("dex authorize status = %d, want %d", resp2.StatusCode, http.StatusFound)
	}
	loc2, err := resp2.Location()
	if err != nil {
		t.Fatalf("resp2.Location(): %v", err)
	}

	resp3, err := client.Get(loc2.String())
	if err != nil {
		t.Fatalf("GET dex auth/local (%s): %v", loc2, err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusFound {
		t.Fatalf("dex auth/local status = %d, want %d", resp3.StatusCode, http.StatusFound)
	}
	loc3, err := resp3.Location()
	if err != nil {
		t.Fatalf("resp3.Location(): %v", err)
	}

	resp4, err := client.Get(loc3.String())
	if err != nil {
		t.Fatalf("GET dex login form (%s): %v", loc3, err)
	}
	defer func() { _ = resp4.Body.Close() }()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("dex login form status = %d, want %d", resp4.StatusCode, http.StatusOK)
	}
	// Dex's own real login form's action attribute is, byte for byte, the
	// URL that rendered it (verified live against a real Dex container
	// during this Step's own design phase) -- resp4.Request.URL is
	// therefore the correct POST target with no HTML parsing required.
	loginFormURL := resp4.Request.URL.String()

	resp5, err := client.PostForm(loginFormURL, url.Values{
		"login":    {dexStaticUserEmail},
		"password": {dexStaticPasswordPlain},
	})
	if err != nil {
		t.Fatalf("POST dex login form (%s): %v", loginFormURL, err)
	}
	defer func() { _ = resp5.Body.Close() }()
	if resp5.StatusCode != http.StatusFound && resp5.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST dex login form status = %d, want %d or %d", resp5.StatusCode, http.StatusFound, http.StatusSeeOther)
	}
	loc5, err := resp5.Location()
	if err != nil {
		t.Fatalf("resp5.Location(): %v", err)
	}
	if !strings.HasPrefix(loc5.String(), cpBaseURL+"/auth/oidc/callback") {
		t.Fatalf("post-login redirect = %s, want the control plane's own /auth/oidc/callback", loc5)
	}

	resp6, err := client.Get(loc5.String())
	if err != nil {
		t.Fatalf("GET /auth/oidc/callback (%s): %v", loc5, err)
	}
	return resp6
}

// authCookieFromResponse extracts platform.AuthSessionCookieName's value
// from resp, or "" if absent.
func authCookieFromResponse(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == platform.AuthSessionCookieName {
			return c.Value
		}
	}
	return ""
}

// getJSON issues a GET against url via client (whose cookie jar already
// carries a real narvi_auth_session cookie from a prior driveDexSignIn
// call) and decodes the JSON response body into T -- a small, generic
// helper so this file's own two authenticated read-back assertions
// (GET /api/me, GET /api/members) share one implementation.
func getJSON[T any](client *http.Client, url string) (T, error) {
	var zero T
	resp, err := client.Get(url)
	if err != nil {
		return zero, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("GET %s status = %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, fmt.Errorf("decode GET %s response: %w", url, err)
	}
	return out, nil
}

func TestOIDCSignIn_RealIdP_Dex(t *testing.T) {
	setRequiredEnv(t)

	dexPort := findFreeTCPPort(t)
	issuer := fmt.Sprintf("http://127.0.0.1:%d/dex", dexPort)

	// cpListener is bound NOW, before Build/httptest.Server exist, so its
	// exact address can be baked into Dex's own static client
	// redirectURIs below -- the same "reserve the address before the
	// thing that needs to know it in advance exists" trick as dexPort,
	// but with no race window: this listener is never released, just
	// handed straight to httptest.Server (below).
	cpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve control-plane listener: %v", err)
	}
	cpBaseURL := "http://" + cpListener.Addr().String()

	const oidcClientID = "narvi-test-client"
	const oidcClientSecret = "narvi-test-secret"
	startDex(t, dexPort, dexConfigYAML(issuer, oidcClientID, oidcClientSecret, cpBaseURL+"/auth/oidc/callback"))

	t.Setenv("NARVI_PUBLIC_BASE_URL", cpBaseURL)
	t.Setenv("NARVI_OIDC_ISSUER", issuer)
	t.Setenv("NARVI_OIDC_CLIENT_ID", oidcClientID)
	t.Setenv("NARVI_OIDC_CLIENT_SECRET", oidcClientSecret)
	t.Setenv("NARVI_ALLOWED_EMAIL_DOMAINS", "example.com")
	t.Setenv("NARVI_ALLOWED_GITHUB_ORGS", "")
	t.Setenv("NARVI_ALLOWED_EMAILS", "")
	// The allowlist assigns this user admin -- both a real exercise of
	// "has the role the allowlist assigned" AND what lets this same test
	// call the admin-only GET /api/members below to prove "appears once
	// in Settings -> Members" without a second, separately-provisioned
	// admin account.
	t.Setenv("NARVI_INITIAL_ADMIN_EMAILS", dexStaticUserEmail)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ts := &httptest.Server{Listener: cpListener, Config: &http.Server{Handler: app.Router}}
	ts.Start()
	t.Cleanup(ts.Close)

	// --- first sign-in ---

	client1 := dexTestClient(t)
	resp1 := driveDexSignIn(t, client1, cpBaseURL)
	defer func() { _ = resp1.Body.Close() }()

	if resp1.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", resp1.StatusCode, http.StatusFound)
	}
	if loc, _ := resp1.Location(); loc == nil || loc.Path != "/" {
		t.Errorf("callback redirected to %v, want \"/\" (the decision inbox -- the SPA's own home route)", loc)
	}
	sessionCookie1 := authCookieFromResponse(resp1)
	if sessionCookie1 == "" {
		t.Fatal("no narvi_auth_session cookie on the callback response")
	}

	me, err := getJSON[restdtos.Member](client1, cpBaseURL+"/api/me")
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	if me.Email != dexStaticUserEmail {
		t.Errorf("GET /api/me email = %q, want %q", me.Email, dexStaticUserEmail)
	}
	if me.Role != "admin" {
		t.Errorf("GET /api/me role = %q, want %q (NARVI_INITIAL_ADMIN_EMAILS match)", me.Role, "admin")
	}

	members, err := getJSON[restdtos.ListMembersResponse](client1, cpBaseURL+"/api/members")
	if err != nil {
		t.Fatalf("GET /api/members: %v", err)
	}
	matchCount := 0
	for _, m := range members.Members {
		if m.Email == dexStaticUserEmail {
			matchCount++
		}
	}
	if matchCount != 1 {
		t.Errorf("GET /api/members has %d row(s) for %s, want exactly 1", matchCount, dexStaticUserEmail)
	}

	// --- second sign-in through the SAME IdP: no second user ---

	client2 := dexTestClient(t)
	resp2 := driveDexSignIn(t, client2, cpBaseURL)
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("second callback status = %d, want %d", resp2.StatusCode, http.StatusFound)
	}
	if authCookieFromResponse(resp2) == "" {
		t.Fatal("no narvi_auth_session cookie on the second callback response")
	}

	membersAfter, err := getJSON[restdtos.ListMembersResponse](client1, cpBaseURL+"/api/members")
	if err != nil {
		t.Fatalf("GET /api/members (after second sign-in): %v", err)
	}
	matchCountAfter := 0
	for _, m := range membersAfter.Members {
		if m.Email == dexStaticUserEmail {
			matchCountAfter++
		}
	}
	if matchCountAfter != 1 {
		t.Errorf("GET /api/members has %d row(s) for %s after a SECOND sign-in, want still exactly 1 (no second user)", matchCountAfter, dexStaticUserEmail)
	}
}
