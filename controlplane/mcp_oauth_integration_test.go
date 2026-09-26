//go:build integration

// This file proves the MCP authorization server as serve.go actually
// wires it (technical plan §43.13-§43.19): every request below goes
// through the router Build returns -- the SAME function cmd/control-plane
// calls -- never a hand-built copy of the routes, so a regression in
// serve.go's own /oauth group, discovery routes, mcpauth.New config or
// Settings groups fails here.
//
// With the surface ON, Build's router is served on a real listener whose
// address IS PublicBaseURL (the resource every token is bound to must be
// the URL a client dials, §43.13), and the official Go SDK's own MCP
// client and OAuth handler drive the whole flow: they discover the
// resource and the authorization server from a live 401, a user approves
// on the consent page, the client exchanges the code, sees the tools its
// token allows and calls one; once its access token expires the client
// refreshes it and keeps working with no second consent; a revoked
// authorization -- from Settings or through the client's own RFC 7009
// revocation -- a deleted or disabled client and a disabled user each stop
// working on the very next call; and a scope-less approval's tool list is
// empty.
//
// Clients without a prior relationship (§43.15), on the same production
// routers: with the shipped defaults, dynamic registration answers the
// disabled response and is not advertised, while metadata documents are;
// the SDK client identifying itself by a metadata document on an in-test
// HTTPS server is refused by the production fetch guard before a single
// connection reaches that server -- counted, and the refusal's logged cause
// is the guard's own -- and completes the whole flow on a router whose
// guard was built with the one test seam allowing that server's exact
// address -- the consent page naming the document's host first; with
// dynamic registration on, the SDK registers itself and completes the flow
// too; registration is braked per client network; and with either
// mechanism switched off, on a router serving the very resource both
// clients' live tokens were issued for, that mechanism's token stops on
// its next call while the other's still works (and with metadata
// documents off, an https client_id is unknown).
//
// The admin view and the brakes (§43.14/§43.18): an administrator lists a
// member's authorizations and revokes one on the member's behalf -- after
// every other role is refused and the grant under any other user's path
// is a 404 -- and the member's SDK session stops on its very next call.
// On a router built with the shipped brakes (the shared rig lifts the two
// new ones: newOAuthRouterRig's own doc comment says why), each of the
// authorization and token endpoints refuses a network past its burst --
// a page, never a redirect; 429 temporarily_unavailable -- spending and
// storing nothing and logging no credential; one network's flood never
// spends another's bucket, the SDK client refreshing through it; the SDK
// client whose own refresh is braked keeps its refresh token and is never
// sent back to consent; and a concurrent flood of authorizations of one
// client stores exactly the pending-request cap.
//
// With the surface OFF, the discovery documents and every /oauth route
// answer the documented disabled response (§43.11/§43.14), while the
// Settings routes -- the admin view of a member's authorizations included
// -- keep serving, so an authorization can always be listed and revoked
// (§43.18).
//
// One Postgres container backs every subtest; each subtest creates its
// own users and clients.
package controlplane

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// mcpDisabledBody is the documented response of every route of the MCP
// surface while it is off (§43.11): 503 with the standard
// disabled-capability body.
const mcpDisabledBody = `{"error":"this capability is not enabled on this deployment"}`

// createRouterUser creates a user of role, with a linked sign-in identity and a
// live session cookie.
func createRouterUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, role sqlcgen.UserRole) (sqlcgen.User, string) {
	t.Helper()
	externalID := fmt.Sprintf("oauth-router-%s-%d", role, time.Now().UnixNano())
	email := externalID + "@example.com"
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: "OAuth Router Test", Role: role})
	if err != nil {
		t.Fatalf("create user (role=%s): %v", role, err)
	}
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := narvipg.NewUserSessionStore(pool).Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return user, token
}

// serveRouter serves one request through h in process and returns the
// recorded response.
func serveRouter(h http.Handler, method, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// oauthRouterRig is Build's own router, with the MCP surface ON, served on
// a real listener.
type oauthRouterRig struct {
	pool   *pgxpool.Pool
	cfg    *platform.Config
	server *httptest.Server
}

// newOAuthRouterRig sets NARVI_MCP_ENABLED and a PublicBaseURL equal to a
// fresh listener's own address for the rest of t, builds the composition
// root through platform.Load and Build exactly as production boot does,
// and serves its router on that listener -- with the shipped timeouts,
// except the token and authorization endpoints' brakes, lifted
// (liftEndpointBrakes): every official-SDK client in this file dials from
// the same loopback address, so on the shipped values this rig's many
// flows would meet the brake whenever they ran faster than it refills --
// an outcome decided by timing, never by the code under test. The brakes
// themselves are proven on routers built with the shipped values (the
// "brakes" section of TestOAuth_ProductionRouter).
func newOAuthRouterRig(t *testing.T, pool *pgxpool.Pool) *oauthRouterRig {
	t.Helper()
	return newOAuthRouterRigWith(t, pool, nil, cimdfetch.GuardConfig{}, liftEndpointBrakes)
}

// liftEndpointBrakes raises the token and authorization endpoints' bursts
// far past anything one test makes (newOAuthRouterRig's own doc comment
// says why), leaving every other timeout as shipped.
func liftEndpointBrakes(to *platform.Timeouts) {
	to.MCPTokenEndpointRateBurst = 1_000_000
	to.MCPAuthorizeRateBurst = 1_000_000
}

// newOAuthRouterRigWith is newOAuthRouterRig with more environment set for
// the Build, the client ID metadata document fetcher's SSRF guard built
// with guard -- the one test seam Build has (cimdFetchGuard) -- and the
// timeouts adjusted by adjust (nil: exactly as shipped). The seam is set
// for the Build alone and restored before this returns: every other
// router in the test, and every production boot, gets the full guard.
func newOAuthRouterRigWith(t *testing.T, pool *pgxpool.Pool, env map[string]string, guard cimdfetch.GuardConfig, adjust func(*platform.Timeouts)) *oauthRouterRig {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	t.Setenv("NARVI_PUBLIC_BASE_URL", "http://"+server.Listener.Addr().String())
	t.Setenv("NARVI_MCP_ENABLED", "true")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := platform.Load()
	if err != nil {
		server.Close()
		t.Fatalf("platform.Load: %v", err)
	}
	if adjust != nil {
		adjust(&cfg.Timeouts)
		if err := cfg.Timeouts.Validate(); err != nil {
			server.Close()
			t.Fatalf("adjusted timeouts: %v", err)
		}
	}
	cimdFetchGuard = guard
	app, err := Build(context.Background(), cfg, pool)
	cimdFetchGuard = cimdfetch.GuardConfig{}
	if err != nil {
		server.Close()
		t.Fatalf("Build: %v", err)
	}
	for k := range env {
		// Only this Build reads them: later rigs start from the defaults.
		t.Setenv(k, "")
	}
	server.Config.Handler = app.Router
	server.Start()
	t.Cleanup(server.Close)
	return &oauthRouterRig{pool: pool, cfg: cfg, server: server}
}

// doJSON makes one REST call with an optional session cookie, decoding
// the JSON body into v when non-nil, and returns the status.
func (r *oauthRouterRig) doJSON(t *testing.T, method, path string, body []byte, v any, cookie string) int {
	t.Helper()
	req, err := http.NewRequest(method, r.server.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode response body: %v", err)
		}
	}
	return resp.StatusCode
}

// postMCP POSTs one modern (2026-07-28) JSON-RPC request to /mcp with a
// bearer token.
func (r *oauthRouterRig) postMCP(t *testing.T, method, toolName, body, bearer string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if toolName != "" {
		req.Header.Set("Mcp-Name", toolName)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, raw
}

// revokedCallFails asserts token now answers 401 with the exact generic
// body and error="invalid_token", on a raw call.
func (r *oauthRouterRig) revokedCallFails(t *testing.T, token string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, header, raw := r.postMCP(t, "tools/call", "narvi_list_models", body, token)
	if status != http.StatusUnauthorized || string(raw) != `{"error":"unauthorized"}` {
		t.Fatalf("call with the revoked token: status %d body %s, want 401 {\"error\":\"unauthorized\"}", status, raw)
	}
	challenges, err := oauthex.ParseWWWAuthenticate(header.Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 || challenges[0].Params["error"] != "invalid_token" {
		t.Fatalf("challenge = %q (err %v), want error=\"invalid_token\"", header.Values("WWW-Authenticate"), err)
	}
}

// observed is one /mcp exchange the recording transport saw.
type observed struct {
	status        int
	authorization string
	challenge     []string
}

// recordingTransport records every /mcp exchange the SDK client makes,
// so a test can count the 401s the flow went through, read the challenge
// the server sent, and learn the bearer token the client ended up with.
type recordingTransport struct {
	mu   sync.Mutex
	seen []observed
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || req.URL.Path != "/mcp" {
		return resp, err
	}
	rt.mu.Lock()
	rt.seen = append(rt.seen, observed{
		status:        resp.StatusCode,
		authorization: req.Header.Get("Authorization"),
		challenge:     resp.Header.Values("WWW-Authenticate"),
	})
	rt.mu.Unlock()
	return resp, nil
}

func (rt *recordingTransport) snapshot() []observed {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]observed(nil), rt.seen...)
}

// lastBearer returns the bearer token of the latest successful /mcp call.
func (rt *recordingTransport) lastBearer() string {
	seen := rt.snapshot()
	for i := len(seen) - 1; i >= 0; i-- {
		if seen[i].status == http.StatusOK && strings.HasPrefix(seen[i].authorization, "Bearer ") {
			return strings.TrimPrefix(seen[i].authorization, "Bearer ")
		}
	}
	return ""
}

var (
	consentNoncePattern  = regexp.MustCompile(`name="nonce" value="([^"]+)"`)
	consentScopePattern  = regexp.MustCompile(`name="scope" value="([^"]+)"`)
	consentLocationShape = regexp.MustCompile(`^/oauth/consent\?request=([0-9a-f-]{36})$`)
)

// consentDriver is the SDK's AuthorizationCodeFetcher: it plays the
// user's browser -- signed in with a session cookie -- through the real
// authorization endpoint and consent page, and returns what the redirect
// back to the client carried. It never follows that final redirect (the
// client's loopback listener is the SDK caller's job). Errors are
// returned, never t.Fatal'd: the SDK calls this from its own call stack.
type consentDriver struct {
	baseURL string
	cookie  string
	// deny, when true for invocation n (1-based), denies instead of
	// approving -- a user declining to re-authorize.
	deny func(n int32) bool
	// keepScopes, when non-nil, decides which offered scope checkboxes
	// stay checked; nil keeps every one.
	keepScopes func(offered []string) []string
	calls      atomic.Int32

	mu sync.Mutex
	// page is the last consent page rendered.
	page []byte
}

// lastPage is the last consent page the driver saw.
func (d *consentDriver) lastPage() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return string(d.page)
}

func (d *consentDriver) fetch(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	n := d.calls.Add(1)
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	withCookie := func(req *http.Request) *http.Request {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: d.cookie})
		return req
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := browser.Do(withCookie(req))
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	m := consentLocationShape.FindStringSubmatch(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || m == nil {
		return nil, fmt.Errorf("authorize: status %d Location %q, want 302 to the consent page", resp.StatusCode, resp.Header.Get("Location"))
	}
	requestID := m[1]

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+m[0], nil)
	if err != nil {
		return nil, err
	}
	resp, err = browser.Do(withCookie(req))
	if err != nil {
		return nil, err
	}
	page, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.page = page
	d.mu.Unlock()
	nonce := consentNoncePattern.FindSubmatch(page)
	if resp.StatusCode != http.StatusOK || nonce == nil {
		return nil, fmt.Errorf("consent page: status %d, no nonce in %s", resp.StatusCode, page)
	}
	var offered []string
	for _, sm := range consentScopePattern.FindAllSubmatch(page, -1) {
		offered = append(offered, string(sm[1]))
	}
	keep := offered
	if d.keepScopes != nil {
		keep = d.keepScopes(offered)
	}

	form := url.Values{"request": {requestID}, "nonce": {string(nonce[1])}, "decision": {"approve"}}
	if d.deny != nil && d.deny(n) {
		form.Set("decision", "deny")
	}
	for _, s := range keep {
		form.Add("scope", s)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/oauth/consent", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// What a browser sends on the consent page's own form POST.
	req.Header.Set("Origin", d.baseURL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = browser.Do(withCookie(req))
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil {
		return nil, fmt.Errorf("consent decision: status %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	q := back.Query()
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("authorization refused: %s", e)
	}
	return &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}

// clientClock is the SDK OAuth handler's token source (its NewTokenSource
// hook) with the one thing a test cannot wait for put under its control:
// the client's own reading of when its access token expires. Through
// golang.org/x/oauth2, the SDK refreshes only once its token source reads
// the token as expired -- a time it computed from expires_in -- and until
// then keeps sending it even if the server has let it lapse, answering the
// resulting 401 by running the whole authorization flow again. In
// production the two lapse together; expire makes the client's lapse now,
// so a test can expire both. Otherwise it is exactly the SDK's default,
// cfg.TokenSource.
type clientClock struct {
	mu    sync.Mutex
	ctx   context.Context
	cfg   *oauth2.Config
	inner oauth2.TokenSource
	last  *oauth2.Token
}

func (c *clientClock) newTokenSource(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ctx, c.cfg, c.inner, c.last = ctx, cfg, cfg.TokenSource(ctx, tok), tok
	return c, nil
}

// Token is oauth2.TokenSource: the SDK's own token source's answer,
// remembered.
func (c *clientClock) Token() (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, err := c.inner.Token()
	if err == nil {
		c.last = tok
	}
	return tok, err
}

// current returns the tokens the client holds now.
func (c *clientClock) current() oauth2.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.last
}

// expire makes the client read its current access token as expired --
// what happens by itself once expires_in elapses -- so its next request
// refreshes first, with the refresh token it holds.
func (c *clientClock) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	lapsed := *c.last
	lapsed.Expiry = time.Now().Add(-time.Second)
	c.inner = c.cfg.TokenSource(c.ctx, &lapsed)
}

// sdkFlow is one connected official-SDK client and what it went through.
type sdkFlow struct {
	session     *sdkmcp.ClientSession
	driver      *consentDriver
	clock       *clientClock
	recorder    *recordingTransport
	member      sqlcgen.User
	cookie      string
	admin       sqlcgen.User
	adminCookie string
	client      restdtos.MCPClient
}

// connectSDKClient registers a client as an admin (through the REST
// route, as a person would), then connects the official SDK client as a
// signed-in member, whose consent the driver gives.
func (r *oauthRouterRig) connectSDKClient(ctx context.Context, t *testing.T, configure func(*consentDriver)) *sdkFlow {
	t.Helper()
	admin, adminCookie := createRouterUser(ctx, t, r.pool, sqlcgen.UserRoleAdmin)
	member, memberCookie := createRouterUser(ctx, t, r.pool, sqlcgen.UserRoleMember)

	var client restdtos.MCPClient
	body := []byte(`{"clientName":"Editor Plugin","redirectUris":["http://127.0.0.1/callback"]}`)
	if status := r.doJSON(t, http.MethodPost, "/api/mcp-clients", body, &client, adminCookie); status != http.StatusCreated {
		t.Fatalf("register client: status %d", status)
	}

	flow, err := r.dialSDKClient(ctx, t, member, memberCookie, configure, func(c *sdkauth.AuthorizationCodeHandlerConfig) {
		c.PreregisteredClient = &oauthex.ClientCredentials{ClientID: client.ClientId}
		c.RedirectURL = "http://127.0.0.1:1/callback"
	})
	if err != nil {
		t.Fatalf("SDK Connect: %v", err)
	}
	flow.admin, flow.adminCookie, flow.client = admin, adminCookie, client
	return flow
}

// dialSDKClient connects the official SDK client as member, whose consent
// the driver gives, registering however register configures the SDK's
// OAuth handler: a pre-registered client, a client ID metadata document,
// or dynamic client registration. Connect's own error is returned, so a
// refused registration can be asserted.
func (r *oauthRouterRig) dialSDKClient(ctx context.Context, t *testing.T, member sqlcgen.User, memberCookie string, configure func(*consentDriver), register func(*sdkauth.AuthorizationCodeHandlerConfig)) (*sdkFlow, error) {
	t.Helper()
	driver := &consentDriver{baseURL: r.server.URL, cookie: memberCookie}
	if configure != nil {
		configure(driver)
	}
	recorder := &recordingTransport{}
	httpClient := &http.Client{Transport: recorder}
	clock := &clientClock{}
	cfg := &sdkauth.AuthorizationCodeHandlerConfig{
		AuthorizationCodeFetcher: driver.fetch,
		Client:                   httpClient,
		NewTokenSource:           clock.newTokenSource,
	}
	register(cfg)
	handler, err := sdkauth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		t.Fatalf("NewAuthorizationCodeHandler: %v", err)
	}
	sdkClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "sdk-e2e", Version: "1"}, nil)
	flow := &sdkFlow{driver: driver, clock: clock, recorder: recorder, member: member, cookie: memberCookie}
	session, err := sdkClient.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint:             r.server.URL + "/mcp",
		HTTPClient:           httpClient,
		OAuthHandler:         handler,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return flow, err
	}
	t.Cleanup(func() { _ = session.Close() })
	flow.session = session
	return flow, nil
}

func toolNames(ctx context.Context, t *testing.T, s *sdkmcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func oauthTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func callListModels(ctx context.Context, s *sdkmcp.ClientSession) (*sdkmcp.CallToolResult, error) {
	return s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}})
}

// TestOAuth_ProductionRouter -- this file's own top doc comment.
func TestOAuth_ProductionRouter(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	offCfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	if offCfg.MCPEnabled {
		t.Fatal("cfg.MCPEnabled = true, want false by default (NARVI_MCP_ENABLED unset)")
	}
	off, err := Build(context.Background(), offCfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Surface OFF: discovery and every /oauth route answer the documented
	// disabled response -- observable as off, never a missing route.
	t.Run("FlagOff_AuthorizationServerAnswersDisabled", func(t *testing.T) {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/.well-known/oauth-protected-resource/mcp"},
			{http.MethodGet, "/.well-known/oauth-authorization-server/oauth"},
			{http.MethodGet, "/oauth/authorize"},
			{http.MethodGet, "/oauth/consent"},
			{http.MethodPost, "/oauth/consent"},
			{http.MethodPost, "/oauth/token"},
			{http.MethodPost, "/oauth/revoke"},
			{http.MethodPost, "/oauth/register"},
		} {
			rec := serveRouter(off.Router, route.method, route.path, "")
			if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != mcpDisabledBody || rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("%s %s while off: status %d Content-Type %q body %s, want 503 application/json %s", route.method, route.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), mcpDisabledBody)
			}
		}
	})

	// Surface OFF: the Settings routes are deliberately not behind the
	// flag -- a user can still list and revoke an authorization issued
	// while it was on, and an admin can still see and delete the client.
	t.Run("FlagOff_SettingsRoutesStillServe", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		member, memberCookie := createRouterUser(ctx, t, pool, sqlcgen.UserRoleMember)
		_, adminCookie := createRouterUser(ctx, t, pool, sqlcgen.UserRoleAdmin)
		mintBuildBearer(ctx, t, pool, offCfg, member.ID)

		rec := serveRouter(off.Router, http.MethodGet, "/api/me/mcp-authorizations", memberCookie)
		var mine restdtos.ListMCPAuthorizationsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil || len(mine.Authorizations) != 1 {
			t.Fatalf("GET /api/me/mcp-authorizations while off: status %d body %s, want 200 listing the one authorization", rec.Code, rec.Body.String())
		}
		rec = serveRouter(off.Router, http.MethodGet, "/api/mcp-clients", adminCookie)
		var clients restdtos.ListMCPClientsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &clients) != nil {
			t.Fatalf("GET /api/mcp-clients while off: status %d body %s, want 200", rec.Code, rec.Body.String())
		}
		clientID := ""
		for _, c := range clients.Clients {
			if c.ClientId == mine.Authorizations[0].ClientId {
				clientID = c.Id
			}
		}
		if clientID == "" {
			t.Fatalf("GET /api/mcp-clients while off = %+v, want the authorization's client listed", clients)
		}
		// The admin view of a member's authorizations too, and revocation
		// on a member's behalf.
		rec = serveRouter(off.Router, http.MethodGet, "/api/members/"+member.ID.String()+"/mcp-authorizations", adminCookie)
		var theirs restdtos.ListMCPAuthorizationsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &theirs) != nil || len(theirs.Authorizations) != 1 || theirs.Authorizations[0].Id != mine.Authorizations[0].Id {
			t.Fatalf("GET /api/members/{userID}/mcp-authorizations while off: status %d body %s, want 200 listing the member's one authorization", rec.Code, rec.Body.String())
		}
		second, _ := createRouterUser(ctx, t, pool, sqlcgen.UserRoleMember)
		mintBuildBearer(ctx, t, pool, offCfg, second.ID)
		rec = serveRouter(off.Router, http.MethodGet, "/api/members/"+second.ID.String()+"/mcp-authorizations", adminCookie)
		var secondList restdtos.ListMCPAuthorizationsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &secondList) != nil || len(secondList.Authorizations) != 1 {
			t.Fatalf("GET /api/members/{userID}/mcp-authorizations while off: status %d body %s", rec.Code, rec.Body.String())
		}
		if rec := serveRouter(off.Router, http.MethodDelete, "/api/members/"+second.ID.String()+"/mcp-authorizations/"+secondList.Authorizations[0].Id, adminCookie); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE /api/members/{userID}/mcp-authorizations/{id} while off: status %d body %s, want 204", rec.Code, rec.Body.String())
		}
		if rec := serveRouter(off.Router, http.MethodDelete, "/api/me/mcp-authorizations/"+mine.Authorizations[0].Id, memberCookie); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE /api/me/mcp-authorizations/{id} while off: status %d body %s, want 204", rec.Code, rec.Body.String())
		}
		if rec := serveRouter(off.Router, http.MethodDelete, "/api/mcp-clients/"+clientID, adminCookie); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE /api/mcp-clients/{id} while off: status %d body %s, want 204", rec.Code, rec.Body.String())
		}
	})

	rig := newOAuthRouterRig(t, pool)

	// The exit criterion end to end (§43.19): the official SDK client
	// discovers everything from one live 401, the member approves on the
	// consent page, and the resulting token lists exactly the three tools
	// and calls one with the same bytes the REST twin gives the member's
	// own cookie. The SDK's own RFC 9207 issuer check passes along the way.
	t.Run("EndToEnd_SDKClient", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, nil)

		seen := flow.recorder.snapshot()
		unauthorized := 0
		for _, o := range seen {
			if o.status == http.StatusUnauthorized {
				unauthorized++
			}
		}
		if unauthorized != 1 || seen[0].status != http.StatusUnauthorized {
			t.Fatalf("/mcp exchanges = %+v, want the connect to succeed after exactly one 401", seen)
		}
		challenges, err := oauthex.ParseWWWAuthenticate(seen[0].challenge)
		if err != nil || len(challenges) != 1 {
			t.Fatalf("WWW-Authenticate %q: %v", seen[0].challenge, err)
		}
		if got := challenges[0].Params["resource_metadata"]; got != rig.server.URL+"/.well-known/oauth-protected-resource/mcp" {
			t.Errorf("resource_metadata = %q", got)
		}
		if got := challenges[0].Params["scope"]; got != "mcp:read" {
			t.Errorf("challenge scope = %q, want mcp:read", got)
		}
		if _, ok := challenges[0].Params["error"]; ok {
			t.Errorf("first challenge carries an error param, but no credential had been sent: %v", challenges[0].Params)
		}
		if n := flow.driver.calls.Load(); n != 1 {
			t.Fatalf("consent flow ran %d times, want 1", n)
		}

		want := []string{"narvi_get_session", "narvi_list_models", "narvi_list_sessions"}
		if got := toolNames(ctx, t, flow.session); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("ListTools = %v, want %v", got, want)
		}

		res, err := callListModels(ctx, flow.session)
		if err != nil || res.IsError {
			t.Fatalf("CallTool narvi_list_models: res %+v err %v", res, err)
		}
		var restBody any
		if status := rig.doJSON(t, http.MethodGet, "/api/models", nil, &restBody, flow.cookie); status != http.StatusOK {
			t.Fatalf("REST /api/models: status %d", status)
		}
		mcpRaw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var mcpBody any
		if err := json.Unmarshal(mcpRaw, &mcpBody); err != nil {
			t.Fatal(err)
		}
		restCanon, _ := json.Marshal(restBody)
		mcpCanon, _ := json.Marshal(mcpBody)
		if string(restCanon) != string(mcpCanon) {
			t.Errorf("narvi_list_models over OAuth differs from REST.\nREST: %s\nMCP:  %s", restCanon, mcpCanon)
		}

		var scopes []string
		var resource string
		var grants int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) OVER (), scopes, resource FROM mcp_oauth_grants WHERE user_id = $1`, flow.member.ID).Scan(&grants, &scopes, &resource); err != nil {
			t.Fatalf("read grant: %v", err)
		}
		if grants != 1 || strings.Join(scopes, ",") != "mcp:read" || resource != rig.server.URL+"/mcp" {
			t.Fatalf("grants = %d scopes %v resource %q, want one {mcp:read} grant for %s/mcp", grants, scopes, resource, rig.server.URL)
		}
		var granted int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'mcp_authorization.granted' AND actor_user_id = $1`, flow.member.ID).Scan(&granted); err != nil || granted != 1 {
			t.Fatalf("mcp_authorization.granted audit rows = %d (err %v), want 1", granted, err)
		}
	})

	// The revocation exit criterion through the user's own Settings
	// route: after DELETE /api/me/mcp-authorizations/{id}, the very next
	// call through the SAME SDK session is refused -- the server answers
	// 401 invalid_token, the SDK's one Authorize retry sends the user back
	// through consent (the fetcher's second invocation, which this user
	// declines), and the call fails. No access token survives.
	t.Run("RevokedAuthorizationStopsOnNextCall_User", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
			d.deny = func(n int32) bool { return n > 1 }
		})
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before revocation: res %+v err %v", res, err)
		}
		token := flow.recorder.lastBearer()

		var list restdtos.ListMCPAuthorizationsResponse
		if status := rig.doJSON(t, http.MethodGet, "/api/me/mcp-authorizations", nil, &list, flow.cookie); status != http.StatusOK || len(list.Authorizations) != 1 {
			t.Fatalf("list authorizations: status %d body %+v", status, list)
		}
		grantID := list.Authorizations[0].Id
		if status := rig.doJSON(t, http.MethodDelete, "/api/me/mcp-authorizations/"+grantID, nil, nil, flow.cookie); status != http.StatusNoContent {
			t.Fatalf("revoke: status %d, want 204", status)
		}

		before := len(flow.recorder.snapshot())
		if _, err := callListModels(ctx, flow.session); err == nil {
			t.Fatalf("the very next CallTool after revocation succeeded")
		}
		after := flow.recorder.snapshot()[before:]
		if len(after) == 0 || after[0].status != http.StatusUnauthorized || after[0].authorization != "Bearer "+token {
			t.Fatalf("exchanges after revocation = %+v, want the old token refused 401 first", after)
		}
		challenges, perr := oauthex.ParseWWWAuthenticate(after[0].challenge)
		if perr != nil || len(challenges) != 1 || challenges[0].Params["error"] != "invalid_token" {
			t.Fatalf("challenge after revocation = %q, want error=\"invalid_token\"", after[0].challenge)
		}
		if n := flow.driver.calls.Load(); n != 2 {
			t.Fatalf("consent flow ran %d times, want 2 (the SDK's single Authorize retry)", n)
		}
		rig.revokedCallFails(t, token)

		var tokens int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_access_tokens WHERE grant_id = $1`, grantID).Scan(&tokens); err != nil || tokens != 0 {
			t.Fatalf("access tokens left for the revoked grant = %d (err %v), want 0", tokens, err)
		}
	})

	// The revocation exit criterion through an administrator (§43.18): the
	// admin lists the member's authorizations and revokes one on the
	// member's behalf, and the very next call through the member's SDK
	// session is refused -- 401 invalid_token, the SDK's one Authorize retry
	// declined -- and the refresh token refreshes nothing. Before that: no
	// other role may list or revoke through the admin routes (403, the
	// member included), and the grant under any other user's path is a 404
	// that leaves the member's token working.
	t.Run("RevokedAuthorizationStopsOnNextCall_Admin", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
			d.deny = func(n int32) bool { return n > 1 }
		})
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before revocation: res %+v err %v", res, err)
		}
		token := flow.recorder.lastBearer()
		refresh := flow.clock.current().RefreshToken
		memberPath := "/api/members/" + flow.member.ID.String() + "/mcp-authorizations"

		var list restdtos.ListMCPAuthorizationsResponse
		if status := rig.doJSON(t, http.MethodGet, memberPath, nil, &list, flow.adminCookie); status != http.StatusOK || len(list.Authorizations) != 1 || list.Authorizations[0].ClientId != flow.client.ClientId {
			t.Fatalf("admin list: status %d body %+v, want the member's one authorization", status, list)
		}
		grantID := list.Authorizations[0].Id

		_, maintainerCookie := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMaintainer)
		for who, cookie := range map[string]string{"the member": flow.cookie, "a maintainer": maintainerCookie} {
			if status := rig.doJSON(t, http.MethodGet, memberPath, nil, nil, cookie); status != http.StatusForbidden {
				t.Fatalf("%s listing through the admin route: status %d, want 403", who, status)
			}
			if status := rig.doJSON(t, http.MethodDelete, memberPath+"/"+grantID, nil, nil, cookie); status != http.StatusForbidden {
				t.Fatalf("%s revoking through the admin route: status %d, want 403", who, status)
			}
		}
		other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
		for _, owner := range []string{other.ID.String(), flow.admin.ID.String()} {
			if status := rig.doJSON(t, http.MethodDelete, "/api/members/"+owner+"/mcp-authorizations/"+grantID, nil, nil, flow.adminCookie); status != http.StatusNotFound {
				t.Fatalf("admin revoking the member's grant under user %s: status %d, want 404", owner, status)
			}
		}
		rig.acceptedCall(t, token)

		if status := rig.doJSON(t, http.MethodDelete, memberPath+"/"+grantID, nil, nil, flow.adminCookie); status != http.StatusNoContent {
			t.Fatalf("admin revoke: status %d, want 204", status)
		}
		before := len(flow.recorder.snapshot())
		if _, err := callListModels(ctx, flow.session); err == nil {
			t.Fatalf("the very next CallTool after the admin's revocation succeeded")
		}
		after := flow.recorder.snapshot()[before:]
		if len(after) == 0 || after[0].status != http.StatusUnauthorized || after[0].authorization != "Bearer "+token {
			t.Fatalf("exchanges after revocation = %+v, want the old token refused 401 first", after)
		}
		if n := flow.driver.calls.Load(); n != 2 {
			t.Fatalf("consent flow ran %d times, want 2 (the SDK's single Authorize retry)", n)
		}
		rig.revokedCallFails(t, token)

		resp, err := http.PostForm(rig.server.URL+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {flow.client.ClientId}})
		if err != nil {
			t.Fatalf("refresh after revocation: %v", err)
		}
		var refused struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&refused)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || refused.Error != "invalid_grant" {
			t.Fatalf("refresh after revocation: status %d error %q, want 400 invalid_grant", resp.StatusCode, refused.Error)
		}
		var grants, tokens, audited int
		if err := rig.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM mcp_oauth_grants WHERE user_id = $1),
			       (SELECT count(*) FROM mcp_oauth_access_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1),
			       (SELECT count(*) FROM audit_log WHERE action = 'mcp_authorization.revoked' AND resource_id = $2
			          AND actor_user_id = $3 AND detail_json->>'reason' = 'admin' AND detail_json->>'target_user_id' = $4)`,
			flow.member.ID, grantID, flow.admin.ID, flow.member.ID.String()).Scan(&grants, &tokens, &audited); err != nil {
			t.Fatal(err)
		}
		if grants != 0 || tokens != 0 || audited != 1 {
			t.Fatalf("after the admin's revocation: grants %d, access tokens %d, admin revocation audit rows %d; want 0, 0, 1", grants, tokens, audited)
		}
	})

	// Refresh end to end (§43.16): once the access token lapses -- on the
	// server, as MCPAccessTokenTTL elapsing would, and in the client's own
	// clock -- the SDK client refreshes with the refresh token it was
	// issued and keeps working, with no second consent and no 401 on the
	// way. Twice, so the second refresh presents the token the first one
	// rotated in.
	t.Run("RefreshAfterAccessTokenExpires_NoSecondConsent", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, nil)
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before expiry: res %+v err %v", res, err)
		}

		presented := map[string]bool{}
		for cycle := 1; cycle <= 2; cycle++ {
			lapsed := flow.recorder.lastBearer()
			refresh := flow.clock.current().RefreshToken
			if !strings.HasPrefix(refresh, "narvi_mcp_rt_") || presented[refresh] {
				t.Fatalf("cycle %d: the client holds refresh token %q, want a new narvi_mcp_rt_ token", cycle, refresh)
			}
			presented[refresh] = true
			tag, err := rig.pool.Exec(ctx, `UPDATE mcp_oauth_access_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, platform.HashToken(lapsed))
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("cycle %d: expire the access token on the server: rows %d err %v", cycle, tag.RowsAffected(), err)
			}
			rig.revokedCallFails(t, lapsed)
			flow.clock.expire()

			before := len(flow.recorder.snapshot())
			if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
				t.Fatalf("cycle %d: call after expiry: res %+v err %v", cycle, res, err)
			}
			for _, o := range flow.recorder.snapshot()[before:] {
				if o.status != http.StatusOK || o.authorization == "Bearer "+lapsed {
					t.Fatalf("cycle %d: /mcp exchanges after expiry = %+v, want only 200s with a refreshed token", cycle, flow.recorder.snapshot()[before:])
				}
			}
			if n := flow.driver.calls.Load(); n != 1 {
				t.Fatalf("cycle %d: consent flow ran %d times, want 1 (a refresh needs no consent)", cycle, n)
			}
		}

		var grants, live, rotated, revoked int
		if err := rig.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM mcp_oauth_grants WHERE user_id = $1),
			       (SELECT count(*) FROM mcp_oauth_refresh_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1 AND t.rotated_at IS NULL),
			       (SELECT count(*) FROM mcp_oauth_refresh_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1 AND t.rotated_at IS NOT NULL),
			       (SELECT count(*) FROM audit_log WHERE action = 'mcp_authorization.revoked' AND actor_user_id = $1)`,
			flow.member.ID).Scan(&grants, &live, &rotated, &revoked); err != nil {
			t.Fatal(err)
		}
		if grants != 1 || live != 1 || rotated != 2 || revoked != 0 {
			t.Fatalf("after two refreshes: grants %d, live refresh tokens %d, rotated %d, revocations %d; want 1, 1, 2, 0", grants, live, rotated, revoked)
		}
	})

	// RFC 7009 end to end (§43.16): the client gives its refresh token back
	// at the revocation endpoint the authorization-server metadata
	// advertises; the whole authorization is gone, so the very next call
	// with its access token is refused -- the SDK's one Authorize retry
	// sends the user back through consent, declined -- and the refresh
	// token refreshes nothing.
	t.Run("RevokedAuthorizationStopsOnNextCall_RFC7009", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
			d.deny = func(n int32) bool { return n > 1 }
		})
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before revocation: res %+v err %v", res, err)
		}
		token := flow.recorder.lastBearer()
		refresh := flow.clock.current().RefreshToken

		var asm struct {
			RevocationEndpoint          string   `json:"revocation_endpoint"`
			RevocationEndpointAuthMeths []string `json:"revocation_endpoint_auth_methods_supported"`
			GrantTypesSupported         []string `json:"grant_types_supported"`
		}
		if status := rig.doJSON(t, http.MethodGet, "/.well-known/oauth-authorization-server/oauth", nil, &asm, ""); status != http.StatusOK {
			t.Fatalf("authorization-server metadata: status %d", status)
		}
		if asm.RevocationEndpoint != rig.server.URL+"/oauth/revoke" || strings.Join(asm.RevocationEndpointAuthMeths, ",") != "none" ||
			strings.Join(asm.GrantTypesSupported, ",") != "authorization_code,refresh_token" {
			t.Fatalf("authorization-server metadata = %+v, want the revocation endpoint and the refresh_token grant advertised", asm)
		}

		form := url.Values{"token": {refresh}, "token_type_hint": {"refresh_token"}, "client_id": {flow.client.ClientId}}
		resp, err := http.PostForm(asm.RevocationEndpoint, form)
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(body) != 0 {
			t.Fatalf("revoke: status %d body %q, want 200 and an empty body", resp.StatusCode, body)
		}

		before := len(flow.recorder.snapshot())
		if _, err := callListModels(ctx, flow.session); err == nil {
			t.Fatalf("the very next CallTool after the revocation succeeded")
		}
		after := flow.recorder.snapshot()[before:]
		if len(after) == 0 || after[0].status != http.StatusUnauthorized || after[0].authorization != "Bearer "+token {
			t.Fatalf("exchanges after revocation = %+v, want the old token refused 401 first", after)
		}
		if n := flow.driver.calls.Load(); n != 2 {
			t.Fatalf("consent flow ran %d times, want 2 (the SDK's single Authorize retry)", n)
		}
		rig.revokedCallFails(t, token)

		resp, err = http.PostForm(rig.server.URL+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {flow.client.ClientId}})
		if err != nil {
			t.Fatalf("refresh after revocation: %v", err)
		}
		var refused struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&refused)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || refused.Error != "invalid_grant" {
			t.Fatalf("refresh after revocation: status %d error %q, want 400 invalid_grant", resp.StatusCode, refused.Error)
		}
		var grants, audited int
		if err := rig.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM mcp_oauth_grants WHERE user_id = $1),
			       (SELECT count(*) FROM audit_log WHERE action = 'mcp_authorization.revoked' AND actor_user_id = $1 AND detail_json->>'reason' = 'client')`,
			flow.member.ID).Scan(&grants, &audited); err != nil || grants != 0 || audited != 1 {
			t.Fatalf("after the revocation: grants %d, client revocation audit rows %d (err %v); want 0, 1", grants, audited, err)
		}
	})

	// An admin deleting the client revokes every authorization issued to
	// it; the member's token is refused on its very next call.
	t.Run("RevokedAuthorizationStopsOnNextCall_ClientDeleted", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, nil)
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before deletion: res %+v err %v", res, err)
		}
		token := flow.recorder.lastBearer()
		if status := rig.doJSON(t, http.MethodDelete, "/api/mcp-clients/"+flow.client.Id, nil, nil, flow.adminCookie); status != http.StatusNoContent {
			t.Fatalf("delete client: status %d, want 204", status)
		}
		rig.revokedCallFails(t, token)
	})

	// Disabling the user refuses their token on its very next call -- the
	// bearer check re-reads the users row on every call.
	t.Run("RevokedAuthorizationStopsOnNextCall_DisabledUser", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, nil)
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before disabling: res %+v err %v", res, err)
		}
		token := flow.recorder.lastBearer()
		if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, flow.member.ID); err != nil {
			t.Fatal(err)
		}
		rig.revokedCallFails(t, token)
	})

	// An operator disabling a client (without deleting it) refuses every
	// token issued to it on the next call.
	t.Run("DisabledClientIs401NextCall", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, nil)
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before disabling: res %+v err %v", res, err)
		}
		token := flow.recorder.lastBearer()
		if _, err := rig.pool.Exec(ctx, `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE client_id = $1`, flow.client.ClientId); err != nil {
			t.Fatal(err)
		}
		rig.revokedCallFails(t, token)
	})

	// The discovery exit criterion end to end: the member clears every
	// checkbox, the SDK client still connects (server/discover succeeds),
	// its tool list is empty, the instructions name no tool, and calling a
	// tool it cannot see answers exactly what calling a tool that does not
	// exist answers.
	t.Run("ScopelessGrant_ToolsListEmpty", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
			d.keepScopes = func([]string) []string { return nil }
		})

		var tokenScopes []string
		if err := rig.pool.QueryRow(ctx, `SELECT t.scopes FROM mcp_oauth_access_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1`, flow.member.ID).Scan(&tokenScopes); err != nil || len(tokenScopes) != 0 {
			t.Fatalf("access token scopes = %v (err %v), want none", tokenScopes, err)
		}
		if got := toolNames(ctx, t, flow.session); len(got) != 0 {
			t.Fatalf("ListTools under a scope-less token = %v, want none", got)
		}
		if init := flow.session.InitializeResult(); init == nil || strings.Contains(init.Instructions, "narvi_") {
			t.Fatalf("instructions under a scope-less token name a tool: %+v", init)
		}
		if _, err := callListModels(ctx, flow.session); err == nil {
			t.Fatalf("calling a hidden tool succeeded")
		}

		// Byte for byte: the hidden tool's JSON-RPC error, raw, against an
		// unknown tool's under a full-scope token (the SDK echoes the name
		// the caller sent, so that one substitution is the only difference).
		scopeless := flow.recorder.lastBearer()
		other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
		full := mintBuildBearer(ctx, t, rig.pool, rig.cfg, other.ID)
		call := func(token, tool string) (int, string) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tool)
			status, _, raw := rig.postMCP(t, "tools/call", tool, body, token)
			return status, string(raw)
		}
		hiddenStatus, hidden := call(scopeless, "narvi_list_models")
		unknownStatus, unknown := call(full, "narvi_does_not_exist")
		if hiddenStatus != unknownStatus || hidden != strings.ReplaceAll(unknown, "narvi_does_not_exist", "narvi_list_models") {
			t.Fatalf("hidden tool response differs from an unknown tool's:\n hidden:  %d %s\n unknown: %d %s", hiddenStatus, hidden, unknownStatus, unknown)
		}
		var env struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(hidden), &env); err != nil || env.Error == nil || env.Error.Code != -32602 {
			t.Fatalf("hidden tool response %s, want a -32602 JSON-RPC error", hidden)
		}
	})

	// --- Clients without a prior relationship (§43.15) ---
	//
	// doc is an in-test HTTPS server on loopback serving one client ID
	// metadata document. Only a router built with the fetcher's test seam
	// (cimdFetchGuard, allowing this server's exact address and trusting
	// its certificate) can fetch it; every other router here -- rig
	// included -- runs the production guard, which refuses it.
	doc := newMetadataDocumentServer(t)

	// The shipped defaults: metadata documents ON, dynamic registration
	// OFF. The metadata says so, and POST /oauth/register answers the
	// surface's own disabled response while writing nothing.
	t.Run("Register_DisabledByDefault", func(t *testing.T) {
		if rig.cfg.MCPDCREnabled || !rig.cfg.MCPCIMDEnabled {
			t.Fatalf("defaults: MCPCIMDEnabled %v MCPDCREnabled %v, want true, false", rig.cfg.MCPCIMDEnabled, rig.cfg.MCPDCREnabled)
		}
		asm := rig.authorizationServerMetadata(t)
		if _, present := asm["registration_endpoint"]; present {
			t.Errorf("registration_endpoint advertised while dynamic registration is off: %v", asm["registration_endpoint"])
		}
		if asm["client_id_metadata_document_supported"] != true {
			t.Errorf("client_id_metadata_document_supported = %v, want true by default", asm["client_id_metadata_document_supported"])
		}
		rec := rig.registerFrom(t, "198.51.100.40:5000", "", "Disabled Probe")
		if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != mcpDisabledBody || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("POST /oauth/register with dynamic registration off: status %d body %s, want 503 %s", rec.Code, rec.Body.String(), mcpDisabledBody)
		}
		if n := rig.countClientsNamed(t, "Disabled Probe"); n != 0 {
			t.Fatalf("%d clients registered while dynamic registration is off", n)
		}
	})

	// The production guard: the official SDK client identifying itself by
	// a metadata document on loopback is refused before a single packet
	// reaches the document's server -- a page, never a redirect -- and
	// nothing is stored. Proven by its CAUSE, not only its outcome: this
	// rig's fetcher trusts only the system roots, so a guard that let the
	// dial through would fail the same way one step later, at the TLS
	// handshake. No connection is ever accepted, and every refusal logged
	// names the guard's own error.
	t.Run("MetadataDocument_ProductionGuardRefusesLoopback", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		member, cookie := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
		logs := captureWarnings(t)
		hitsBefore, connsBefore := doc.hits.Load(), doc.conns.Load()
		flow, err := rig.dialSDKClient(ctx, t, member, cookie, nil, doc.register)
		if err == nil {
			t.Fatal("the SDK client connected through a metadata document on loopback")
		}
		// The SDK may try more than once; every attempt is refused.
		if n := flow.driver.calls.Load(); n < 1 || !strings.Contains(err.Error(), "status 400") {
			t.Fatalf("authorization attempts %d, err %v; want each refused with a 400 page", n, err)
		}
		if n := doc.conns.Load() - connsBefore; n != 0 {
			t.Fatalf("the production fetcher opened %d connections to the loopback document server, want none", n)
		}
		if doc.hits.Load() != hitsBefore {
			t.Fatal("the production fetcher reached the loopback document server")
		}
		refusals := logs.matching("mcpauth: authorize refused", "outcome", "metadata_document_unusable")
		if len(refusals) == 0 {
			t.Fatal("no refused metadata document was logged")
		}
		for _, e := range refusals {
			if cause, _ := e["error"].(string); !strings.Contains(cause, cimdfetch.ErrForbiddenAddress.Error()) {
				t.Errorf("a refusal was caused by %q, want the guard's own %q", cause, cimdfetch.ErrForbiddenAddress)
			}
		}
		if n := rig.countClients(t, doc.clientID); n != 0 {
			t.Fatalf("%d clients stored for a document the guard refused", n)
		}
	})

	cimdRig := newOAuthRouterRigWith(t, pool, map[string]string{"NARVI_MCP_DCR_ENABLED": "true"}, doc.seam(), nil)
	var cimdToken, dcrToken string

	// A client identified by a metadata document, end to end through the
	// official SDK (which prefers it once the metadata advertises it): the
	// document is fetched through the guard's test seam, the consent page
	// names the document's HOST first and the name it chose second, and the
	// token lists and calls the tools.
	t.Run("EndToEnd_SDKClient_MetadataDocument", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		member, cookie := createRouterUser(ctx, t, cimdRig.pool, sqlcgen.UserRoleMember)
		flow, err := cimdRig.dialSDKClient(ctx, t, member, cookie, nil, doc.register)
		if err != nil {
			t.Fatalf("SDK Connect with a metadata document: %v", err)
		}
		want := []string{"narvi_get_session", "narvi_list_models", "narvi_list_sessions"}
		if got := toolNames(ctx, t, flow.session); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("ListTools = %v, want %v", got, want)
		}
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("CallTool narvi_list_models: res %+v err %v", res, err)
		}
		cimdToken = flow.recorder.lastBearer()
		page := flow.driver.lastPage()
		host := doc.addr.String()
		for _, want := range []string{
			`<h1>Allow the app at <strong class="host">` + host + `</strong> to use Narvi as you?</h1>`,
			`It calls itself <strong>Editor Plugin (metadata document)</strong>.`,
			"That is this computer.",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("consent page lacks %q", want)
			}
		}
		if doc.hits.Load() == 0 || doc.conns.Load() == 0 {
			t.Fatalf("the document server was reached by %d connections and %d requests, want both: the counters must see what the production guard is proven to prevent", doc.conns.Load(), doc.hits.Load())
		}
		var kind string
		var grants int
		if err := cimdRig.pool.QueryRow(ctx, `
			SELECT c.kind::text, count(g.id)
			FROM mcp_oauth_clients c LEFT JOIN mcp_oauth_grants g ON g.client_id = c.id AND g.user_id = $2
			WHERE c.client_id = $1 GROUP BY c.kind`, doc.clientID, member.ID).Scan(&kind, &grants); err != nil || kind != "metadata_document" || grants != 1 {
			t.Fatalf("client kind %q with %d grants for the member (err %v), want one metadata_document client with one grant", kind, grants, err)
		}
	})

	// Dynamic registration end to end through the official SDK, the flag
	// on: the metadata advertises the registration endpoint, the SDK
	// registers, and the flow completes with the client registered as a
	// public client.
	t.Run("EndToEnd_SDKClient_DynamicRegistration", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		if got := cimdRig.authorizationServerMetadata(t)["registration_endpoint"]; got != cimdRig.server.URL+"/oauth/register" {
			t.Fatalf("registration_endpoint = %v, want %s/oauth/register", got, cimdRig.server.URL)
		}
		member, cookie := createRouterUser(ctx, t, cimdRig.pool, sqlcgen.UserRoleMember)
		flow, err := cimdRig.dialSDKClient(ctx, t, member, cookie, nil, func(c *sdkauth.AuthorizationCodeHandlerConfig) {
			c.DynamicClientRegistrationConfig = &sdkauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs: []string{"http://127.0.0.1:1/callback"},
				ClientName:   "Desktop Assistant (dynamic)",
				GrantTypes:   []string{"authorization_code", "refresh_token"},
			}}
		})
		if err != nil {
			t.Fatalf("SDK Connect with dynamic registration: %v", err)
		}
		if got := toolNames(ctx, t, flow.session); len(got) != 3 {
			t.Fatalf("ListTools = %v, want the three tools", got)
		}
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("CallTool narvi_list_models: res %+v err %v", res, err)
		}
		dcrToken = flow.recorder.lastBearer()
		if page := flow.driver.lastPage(); !strings.Contains(page, "This app registered itself with this deployment, so nothing vouches for its name.") {
			t.Error("the consent page does not say the app registered itself")
		}
		var clientID, method string
		if err := cimdRig.pool.QueryRow(ctx, `
			SELECT c.client_id, c.kind::text FROM mcp_oauth_clients c JOIN mcp_oauth_grants g ON g.client_id = c.id
			WHERE g.user_id = $1`, member.ID).Scan(&clientID, &method); err != nil || method != "dynamic" || !strings.HasPrefix(clientID, "narvi_mcp_d_") {
			t.Fatalf("the member's grant is for client %q of kind %q (err %v), want one dynamic narvi_mcp_d_ client", clientID, method, err)
		}
	})

	// The brake on an unauthenticated table write: per client address --
	// RemoteAddr, never a forwarded header -- MCPRegisterRateBurst
	// registrations, then 429 with Retry-After and nothing written.
	t.Run("Register_RateLimited", func(t *testing.T) {
		burst := cimdRig.cfg.Timeouts.MCPRegisterRateBurst
		for i := range burst {
			if rec := cimdRig.registerFrom(t, "198.51.100.23:40000", "", "Rate Limit Probe"); rec.Code != http.StatusCreated {
				t.Fatalf("registration %d of the burst: status %d body %s, want 201", i+1, rec.Code, rec.Body.String())
			}
		}
		for _, tc := range []struct{ name, remote, forwarded string }{
			{"past the burst, another source port", "198.51.100.23:40001", ""},
			{"a forwarded header naming another address changes nothing", "198.51.100.23:40002", "198.51.100.99"},
		} {
			rec := cimdRig.registerFrom(t, tc.remote, tc.forwarded, "Rate Limit Probe")
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"error":"temporarily_unavailable"`) {
				t.Fatalf("%s: status %d Retry-After %q body %s, want 429 with Retry-After", tc.name, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
			}
		}
		if rec := cimdRig.registerFrom(t, "198.51.100.24:40000", "", "Rate Limit Probe"); rec.Code != http.StatusCreated {
			t.Fatalf("another address: status %d, want 201", rec.Code)
		}
		if n := cimdRig.countClientsNamed(t, "Rate Limit Probe"); n != burst+1 {
			t.Fatalf("clients registered = %d, want %d: a refused registration wrote a row", n, burst+1)
		}
	})

	// Each mechanism switched off, on a router serving cimdRig's own
	// public base URL -- so its resource is the one both live tokens were
	// issued for, and the mechanism switch is the only thing that can
	// refuse either: each router accepts the token of the mechanism it
	// kept and refuses the other's on its very next call.
	sameResource := map[string]string{"NARVI_PUBLIC_BASE_URL": cimdRig.cfg.PublicBaseURL}
	offCIMD := newOAuthRouterRigWith(t, pool, withEnv(sameResource, "NARVI_MCP_CIMD_ENABLED", "false", "NARVI_MCP_DCR_ENABLED", "true"), doc.seam(), nil)
	offDCR := newOAuthRouterRigWith(t, pool, withEnv(sameResource, "NARVI_MCP_CIMD_ENABLED", "true", "NARVI_MCP_DCR_ENABLED", "false"), doc.seam(), nil)
	for _, r := range []*oauthRouterRig{offCIMD, offDCR} {
		if r.cfg.PublicBaseURL != cimdRig.cfg.PublicBaseURL {
			t.Fatalf("an off-flag router serves %s, want cimdRig's own %s", r.cfg.PublicBaseURL, cimdRig.cfg.PublicBaseURL)
		}
	}

	// Metadata documents switched off: the metadata stops advertising them,
	// an https client_id is an unknown client (no fetch at all), and a
	// metadata-document client already stored -- with a live token -- is
	// refused on its very next call.
	t.Run("MetadataDocuments_Disabled", func(t *testing.T) {
		if _, present := offCIMD.authorizationServerMetadata(t)["client_id_metadata_document_supported"]; present {
			t.Error("client_id_metadata_document_supported advertised while metadata documents are off")
		}
		const fresh = "https://fresh.example/client.json"
		before := doc.hits.Load()
		req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+url.Values{
			"client_id": {fresh}, "redirect_uri": {"http://127.0.0.1:1/callback"}, "response_type": {"code"},
		}.Encode(), nil)
		rec := httptest.NewRecorder()
		offCIMD.server.Config.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" || doc.hits.Load() != before {
			t.Fatalf("authorize with an https client_id while off: status %d Location %q, fetched %v; want a 400 page, no fetch", rec.Code, rec.Header().Get("Location"), doc.hits.Load() != before)
		}
		if cimdToken == "" || dcrToken == "" {
			t.Fatal("no metadata-document or dynamic-client token from the end-to-end subtests")
		}
		offCIMD.acceptedCall(t, dcrToken)
		offCIMD.revokedCallFails(t, cimdToken)
	})

	// Dynamic registration switched off: a dynamically registered client
	// already stored -- with a live token -- is refused on its very next
	// call, while a metadata-document client's token still works.
	t.Run("DynamicRegistration_Disabled", func(t *testing.T) {
		if _, present := offDCR.authorizationServerMetadata(t)["registration_endpoint"]; present {
			t.Error("registration_endpoint advertised while dynamic registration is off")
		}
		offDCR.acceptedCall(t, cimdToken)
		offDCR.revokedCallFails(t, dcrToken)
	})

	// --- The brakes and the pending-request cap (§43.14) ---
	//
	// On a router built with the shipped values, never the shared rig's
	// lifted ones. Requests are served in process where the test must choose
	// the client network the brake keys on (RemoteAddr); the official SDK
	// client dials from loopback, a network of its own.
	braked := newOAuthRouterRigWith(t, pool, nil, cimdfetch.GuardConfig{}, nil)
	shipped := platform.DefaultTimeouts()
	if got := braked.cfg.Timeouts; got.MCPTokenEndpointRateInterval != shipped.MCPTokenEndpointRateInterval || got.MCPTokenEndpointRateBurst != shipped.MCPTokenEndpointRateBurst ||
		got.MCPAuthorizeRateInterval != shipped.MCPAuthorizeRateInterval || got.MCPAuthorizeRateBurst != shipped.MCPAuthorizeRateBurst ||
		got.MCPMaxPendingAuthorizationRequestsPerClient != shipped.MCPMaxPendingAuthorizationRequestsPerClient {
		t.Fatalf("the braked router's brakes are not the shipped ones: %+v", got)
	}

	// The token endpoint's brake: a network's burst reaches the handler;
	// past it, 429 with Retry-After and temporarily_unavailable, whatever
	// the source port or a forwarded header says. A real refresh token
	// presented from that network is refused before it is read -- not
	// rotated, still good from any other network -- and each refusal is
	// logged at WARN with the path and the network, never the token.
	t.Run("RateLimit_TokenEndpoint429", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := braked.connectSDKClient(ctx, t, nil)
		refresh := flow.clock.current().RefreshToken
		logs := captureWarnings(t)
		const flooder = "198.51.100.61"
		junk := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"narvi_mcp_rt_junk"}, "client_id": {"narvi_mcp_c_unknown"}}
		for i := range braked.cfg.Timeouts.MCPTokenEndpointRateBurst {
			if rec := braked.tokenFrom(t, flooder+":4000", "", junk); rec.Code != http.StatusBadRequest {
				t.Fatalf("token request %d of the burst: status %d body %s, want the handler's own 400", i+1, rec.Code, rec.Body.String())
			}
		}
		asFlow := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {flow.client.ClientId}}
		for _, tc := range []struct {
			name, remote, forwarded string
			form                    url.Values
		}{
			{"past the burst, another source port", flooder + ":4001", "", junk},
			{"a forwarded header naming another address changes nothing", flooder + ":4002", "203.0.113.9", junk},
			{"a real refresh token from the braked network", flooder + ":4003", "", asFlow},
		} {
			rec := braked.tokenFrom(t, tc.remote, tc.forwarded, tc.form)
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" ||
				rec.Body.String() != `{"error":"temporarily_unavailable","error_description":"too many token requests from this network; retry later"}`+"\n" {
				t.Fatalf("%s: status %d headers %v body %s, want 429 temporarily_unavailable with Retry-After", tc.name, rec.Code, rec.Header(), rec.Body.String())
			}
		}
		var rotated bool
		if err := braked.pool.QueryRow(ctx, `SELECT rotated_at IS NOT NULL FROM mcp_oauth_refresh_tokens WHERE token_hash = $1`, platform.HashToken(refresh)).Scan(&rotated); err != nil || rotated {
			t.Fatalf("the refresh token the brake refused: rotated %v (err %v), want untouched", rotated, err)
		}
		rec := braked.tokenFrom(t, "198.51.100.62:4000", "", asFlow)
		var refreshed struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &refreshed) != nil || refreshed.AccessToken == "" || refreshed.RefreshToken == refresh {
			t.Fatalf("the same refresh token from another network: status %d body %s, want 200 and new tokens -- the refusal spent nothing", rec.Code, rec.Body.String())
		}
		refusals := logs.matching("mcpauth: rate limited", "path", "/oauth/token")
		if len(refusals) != 3 {
			t.Fatalf("rate-limit refusals logged = %d, want 3", len(refusals))
		}
		for _, e := range refusals {
			line, _ := json.Marshal(e)
			if e["level"] != "WARN" || e["client_address"] != flooder || strings.Contains(string(line), refresh) || strings.Contains(string(line), "narvi_mcp_rt_") {
				t.Fatalf("refusal log %s: want a WARN naming the network and no token", line)
			}
		}
	})

	// The authorization endpoint's brake: a network's burst is served;
	// past it, the error page with 429 and Retry-After -- never a redirect,
	// even for a request whose client and redirect_uri are registered --
	// logged without the request's query. Another network is served.
	t.Run("RateLimit_AuthorizeEndpoint", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		_, adminCookie := createRouterUser(ctx, t, braked.pool, sqlcgen.UserRoleAdmin)
		client := braked.registerClientAsAdmin(t, adminCookie, "Braked Plugin")
		logs := captureWarnings(t)
		const flooder, state, challenge = "198.51.100.71", "state-not-for-logs", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
		valid := url.Values{
			"client_id": {client.ClientId}, "redirect_uri": {"http://127.0.0.1:1/callback"}, "response_type": {"code"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {braked.server.URL + "/mcp"}, "state": {state},
		}
		for i := range braked.cfg.Timeouts.MCPAuthorizeRateBurst {
			if rec := braked.authorizeFrom(t, flooder+":5000", valid); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/sign-in?next=") {
				t.Fatalf("authorization %d of the burst: status %d Location %q, want a 302 on to sign-in", i+1, rec.Code, rec.Header().Get("Location"))
			}
		}
		unregistered := url.Values{}
		for k, v := range valid {
			unregistered[k] = v
		}
		unregistered.Set("redirect_uri", "https://elsewhere.example/cb")
		for name, q := range map[string]url.Values{"a valid request": valid, "an unregistered redirect_uri": unregistered} {
			rec := braked.authorizeFrom(t, flooder+":5001", q)
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Location") != "" || rec.Header().Get("Retry-After") == "" ||
				rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(rec.Body.String(), "Too many requests from your network") {
				t.Fatalf("%s past the burst: status %d Location %q Retry-After %q, want the 429 page and no redirect", name, rec.Code, rec.Header().Get("Location"), rec.Header().Get("Retry-After"))
			}
		}
		if rec := braked.authorizeFrom(t, "198.51.100.72:5000", valid); rec.Code != http.StatusFound {
			t.Fatalf("another network: status %d, want 302", rec.Code)
		}
		var stored int
		if err := braked.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_authorization_requests r JOIN mcp_oauth_clients c ON c.id = r.client_id WHERE c.client_id = $1`, client.ClientId).Scan(&stored); err != nil || stored != braked.cfg.Timeouts.MCPAuthorizeRateBurst+1 {
			t.Fatalf("requests stored = %d (err %v), want the burst and the other network's one: a braked request stored nothing", stored, err)
		}
		refusals := logs.matching("mcpauth: rate limited", "path", "/oauth/authorize")
		if len(refusals) != 2 {
			t.Fatalf("rate-limit refusals logged = %d, want 2", len(refusals))
		}
		for _, e := range refusals {
			line, _ := json.Marshal(e)
			if e["client_address"] != flooder || strings.Contains(string(line), state) || strings.Contains(string(line), challenge) {
				t.Fatalf("refusal log %s: want the network and nothing from the query", line)
			}
		}
	})

	// A flood of token requests from one network -- an IPv4 address, or
	// one IPv6 /48 spraying a different /64 each time -- spends that
	// network's bucket alone: another /48 still reaches the handler, and
	// the official SDK client, on its own network, refreshes and carries
	// on with no 429 and no second consent.
	t.Run("RateLimit_OneNetworkCannotLockOutAnotherRefresh", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		flow := braked.connectSDKClient(ctx, t, nil)
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before the flood: res %+v err %v", res, err)
		}
		junk := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"narvi_mcp_rt_junk"}, "client_id": {"narvi_mcp_c_unknown"}}
		burst := braked.cfg.Timeouts.MCPTokenEndpointRateBurst
		for _, network := range []func(i int) string{
			func(int) string { return "198.51.100.81:4000" },
			func(i int) string { return fmt.Sprintf("[2001:db8:81:%x::1]:4000", i) },
		} {
			for i := range burst {
				if rec := braked.tokenFrom(t, network(i), "", junk); rec.Code != http.StatusBadRequest {
					t.Fatalf("flood request %d from %s: status %d, want the handler's own 400", i+1, network(i), rec.Code)
				}
			}
			if rec := braked.tokenFrom(t, network(burst), "", junk); rec.Code != http.StatusTooManyRequests {
				t.Fatalf("the flood from %s past its burst: status %d, want 429", network(burst), rec.Code)
			}
		}
		if rec := braked.tokenFrom(t, "[2001:db8:82::1]:4000", "", junk); rec.Code != http.StatusBadRequest {
			t.Fatalf("another /48 after the flood: status %d, want the handler's own 400", rec.Code)
		}

		lapsed := flow.recorder.lastBearer()
		held := flow.clock.current().RefreshToken
		tag, err := braked.pool.Exec(ctx, `UPDATE mcp_oauth_access_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, platform.HashToken(lapsed))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("expire the access token on the server: rows %d err %v", tag.RowsAffected(), err)
		}
		flow.clock.expire()
		before := len(flow.recorder.snapshot())
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("the SDK client's call after the flood: res %+v err %v, want its refresh to go through", res, err)
		}
		for _, o := range flow.recorder.snapshot()[before:] {
			if o.status != http.StatusOK || o.authorization == "Bearer "+lapsed {
				t.Fatalf("/mcp exchanges after the flood = %+v, want only 200s with a refreshed token", flow.recorder.snapshot()[before:])
			}
		}
		if flow.clock.current().RefreshToken == held || flow.driver.calls.Load() != 1 {
			t.Fatalf("the SDK client did not refresh (refresh token unchanged %v) or consented again (%d consents)", flow.clock.current().RefreshToken == held, flow.driver.calls.Load())
		}
	})

	// The pending-request cap on the production router: a concurrent flood
	// of authorizations of one client, each from a network of its own so no
	// brake stands in the way, stores exactly the shipped cap; every other
	// request gets the 503 page and never a redirect.
	t.Run("Authorize_PendingCapRefused", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		_, adminCookie := createRouterUser(ctx, t, braked.pool, sqlcgen.UserRoleAdmin)
		client := braked.registerClientAsAdmin(t, adminCookie, "Popular Plugin")
		maxPending := braked.cfg.Timeouts.MCPMaxPendingAuthorizationRequestsPerClient
		flood := maxPending + 10
		results := make([]*httptest.ResponseRecorder, flood)
		var eg errgroup.Group
		for i := range flood {
			eg.Go(func() error {
				results[i] = braked.authorizeFrom(t, fmt.Sprintf("203.0.113.%d:6000", i+1), url.Values{
					"client_id": {client.ClientId}, "redirect_uri": {"http://127.0.0.1:1/callback"}, "response_type": {"code"},
					"code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"}, "resource": {braked.server.URL + "/mcp"},
				})
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		stored, capped := 0, 0
		for _, rec := range results {
			switch {
			case rec.Code == http.StatusFound:
				stored++
			case rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Location") == "" && strings.Contains(rec.Body.String(), "This app has too many sign-ins waiting"):
				capped++
			default:
				t.Errorf("an authorization of the flood answered %d Location %q", rec.Code, rec.Header().Get("Location"))
			}
		}
		if stored != maxPending || capped != flood-maxPending {
			t.Fatalf("flood of %d: %d stored, %d refused at the cap; want %d and %d", flood, stored, capped, maxPending, flood-maxPending)
		}
		var pending int
		if err := braked.pool.QueryRow(ctx, `
			SELECT count(*) FROM mcp_oauth_authorization_requests r JOIN mcp_oauth_clients c ON c.id = r.client_id
			WHERE c.client_id = $1 AND r.consumed_at IS NULL AND r.expires_at > now()`, client.ClientId).Scan(&pending); err != nil || pending != maxPending {
			t.Fatalf("pending requests after the flood = %d (err %v), want exactly %d", pending, err, maxPending)
		}
	})

	// What the official SDK client does with the token endpoint's 429 --
	// on a router of its own, since it spends the loopback network's whole
	// token budget: a refresh refused by the brake fails that one call and
	// nothing else. The client keeps its refresh token (temporarily_unavailable
	// is not invalid_grant, the one error it drops its tokens for), is never
	// sent back to consent, and the token it kept is unspent: it still
	// refreshes.
	t.Run("RateLimit_RefusedRefreshSpendsNothing_SDK", func(t *testing.T) {
		ctx := oauthTestCtx(t)
		own := newOAuthRouterRigWith(t, pool, nil, cimdfetch.GuardConfig{}, nil)
		flow := own.connectSDKClient(ctx, t, nil)
		if res, err := callListModels(ctx, flow.session); err != nil || res.IsError {
			t.Fatalf("call before the brake: res %+v err %v", res, err)
		}
		held := flow.clock.current().RefreshToken
		lapsed := flow.recorder.lastBearer()
		// The code exchange spent one of loopback's token requests; spend
		// the rest.
		junk := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"narvi_mcp_rt_junk"}, "client_id": {"narvi_mcp_c_unknown"}}
		for i := range own.cfg.Timeouts.MCPTokenEndpointRateBurst - 1 {
			if rec := own.tokenFrom(t, "127.0.0.1:4000", "", junk); rec.Code != http.StatusBadRequest {
				t.Fatalf("loopback token request %d: status %d, want the handler's own 400", i+2, rec.Code)
			}
		}
		tag, err := own.pool.Exec(ctx, `UPDATE mcp_oauth_access_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, platform.HashToken(lapsed))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("expire the access token on the server: rows %d err %v", tag.RowsAffected(), err)
		}
		flow.clock.expire()
		before := len(flow.recorder.snapshot())
		_, err = callListModels(ctx, flow.session)
		if err == nil || !strings.Contains(err.Error(), "temporarily_unavailable") {
			t.Fatalf("the SDK client's call with its refresh braked: err %v, want the token endpoint's temporarily_unavailable", err)
		}
		if seen := flow.recorder.snapshot()[before:]; len(seen) != 0 {
			t.Fatalf("/mcp exchanges after the braked refresh = %+v, want none: the SDK sends no request it holds no valid token for", seen)
		}
		if n := flow.driver.calls.Load(); n != 1 {
			t.Fatalf("consent flow ran %d times, want 1: a braked refresh never sends the user back to consent", n)
		}
		if flow.clock.current().RefreshToken != held {
			t.Fatal("the SDK client lost the refresh token it held")
		}
		rec := own.tokenFrom(t, "198.51.100.91:4000", "", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {held}, "client_id": {flow.client.ClientId}})
		if rec.Code != http.StatusOK {
			t.Fatalf("the refresh token the SDK kept, from another network: status %d body %s, want 200 -- the braked refresh spent nothing", rec.Code, rec.Body.String())
		}
	})
}

// tokenFrom posts form to /oauth/token as the peer remote -- in process,
// so the address the brake keys on is chosen -- with an X-Forwarded-For of
// forwarded when non-empty.
func (r *oauthRouterRig) tokenFrom(t *testing.T, remote, forwarded string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	rec := httptest.NewRecorder()
	r.server.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// authorizeFrom GETs /oauth/authorize with query q as the peer remote, in
// process and signed out.
func (r *oauthRouterRig) authorizeFrom(t *testing.T, remote string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	r.server.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// registerClientAsAdmin pre-registers a client named name, redirecting to
// loopback, through the admin route.
func (r *oauthRouterRig) registerClientAsAdmin(t *testing.T, adminCookie, name string) restdtos.MCPClient {
	t.Helper()
	var client restdtos.MCPClient
	body := []byte(`{"clientName":"` + name + `","redirectUris":["http://127.0.0.1/callback"]}`)
	if status := r.doJSON(t, http.MethodPost, "/api/mcp-clients", body, &client, adminCookie); status != http.StatusCreated {
		t.Fatalf("register client: status %d", status)
	}
	return client
}

// withEnv is base plus the name/value pairs kv, in a new map.
func withEnv(base map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// acceptedCall asserts token is accepted at /mcp: a raw tools/call answers
// 200.
func (r *oauthRouterRig) acceptedCall(t *testing.T, token string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	if status, _, raw := r.postMCP(t, "tools/call", "narvi_list_models", body, token); status != http.StatusOK {
		t.Fatalf("call with a live token of a mechanism still on: status %d body %s, want 200", status, raw)
	}
}

// warningLog records every Warn-or-above log line written while it is
// installed as the default logger, decoded.
type warningLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *warningLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// captureWarnings installs a warningLog as the default logger -- the one
// platform.Logger hands every handler -- until the calling test ends.
// TestOAuth_ProductionRouter's subtests run one at a time, so nothing else
// logs through it meanwhile. The cleanup puts back the standard log
// package's output and flags as well as slog's default: slog.SetDefault
// points the log package at the handler it installs, and setting the
// original default back does not undo that -- every later line of the
// test binary, slog's included, would go on into the dead capture
// handler, which drops it.
func captureWarnings(t *testing.T) *warningLog {
	t.Helper()
	l := &warningLog{}
	prev, prevOutput, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})
	return l
}

// TestCaptureWarnings_RestoresTheStandardLogger: once the test that
// captured warnings ends, logging is as it was -- slog's default, the
// standard log package's output and flags -- and a line logged then
// reaches that output.
func TestCaptureWarnings_RestoresTheStandardLogger(t *testing.T) {
	prev, prevOutput, prevFlags := slog.Default(), log.Writer(), log.Flags()
	t.Run("capturing", func(t *testing.T) { captureWarnings(t) })
	if slog.Default() != prev || log.Writer() != prevOutput || log.Flags() != prevFlags {
		t.Fatalf("after the capture: slog default restored %v, log output %T (was %T), flags %d (were %d)",
			slog.Default() == prev, log.Writer(), prevOutput, log.Flags(), prevFlags)
	}
	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(prevOutput)
	slog.Warn("logged after the capture")
	if !strings.Contains(out.String(), "logged after the capture") {
		t.Fatalf("a warning logged after the capture never reached the log output: %q", out.String())
	}
}

// matching returns the recorded entries with message msg whose field
// key is value.
func (l *warningLog) matching(msg, key, value string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e["msg"] == msg && e[key] == value {
			out = append(out, e)
		}
	}
	return out
}

// metadataDocumentServer is an in-test HTTPS server on loopback serving
// one client ID metadata document at /mcp/client.json, counting requests.
type metadataDocumentServer struct {
	*httptest.Server
	clientID string
	addr     netip.AddrPort
	roots    *x509.CertPool
	// hits counts requests served; conns counts TCP connections accepted,
	// handshake or not -- what "no packet reached the server" is measured
	// by.
	hits  atomic.Int32
	conns atomic.Int32
}

func newMetadataDocumentServer(t *testing.T) *metadataDocumentServer {
	t.Helper()
	d := &metadataDocumentServer{}
	d.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.hits.Add(1)
		if r.URL.Path != "/mcp/client.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  d.clientID,
			"client_name":                "Editor Plugin (metadata document)",
			"redirect_uris":              []string{"http://127.0.0.1/callback"},
			"grant_types":                []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_method": "none",
		})
	}))
	d.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			d.conns.Add(1)
		}
	}
	d.StartTLS()
	t.Cleanup(d.Close)
	d.clientID = d.URL + "/mcp/client.json"
	d.addr = netip.MustParseAddrPort(d.Listener.Addr().String())
	d.roots = x509.NewCertPool()
	d.roots.AddCert(d.Certificate())
	return d
}

// seam is the fetcher guard that can reach this server, and nothing else
// the production guard refuses.
func (d *metadataDocumentServer) seam() cimdfetch.GuardConfig {
	return cimdfetch.GuardConfig{AllowAddrPorts: []netip.AddrPort{d.addr}, RootCAs: d.roots}
}

// register configures the SDK's OAuth handler to identify the client by
// this server's metadata document.
func (d *metadataDocumentServer) register(c *sdkauth.AuthorizationCodeHandlerConfig) {
	c.ClientIDMetadataDocumentConfig = &sdkauth.ClientIDMetadataDocumentConfig{URL: d.clientID}
	c.RedirectURL = "http://127.0.0.1:1/callback"
}

// authorizationServerMetadata GETs the RFC 8414 document.
func (r *oauthRouterRig) authorizationServerMetadata(t *testing.T) map[string]any {
	t.Helper()
	var asm map[string]any
	if status := r.doJSON(t, http.MethodGet, "/.well-known/oauth-authorization-server/oauth", nil, &asm, ""); status != http.StatusOK {
		t.Fatalf("authorization-server metadata: status %d", status)
	}
	return asm
}

// registerFrom posts one valid dynamic registration naming name, as the
// peer remote -- in process, so the address the rate limit keys on is
// chosen -- with an X-Forwarded-For of forwarded when non-empty.
func (r *oauthRouterRig) registerFrom(t *testing.T, remote, forwarded, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"client_name":"`+name+`","redirect_uris":["http://127.0.0.1/callback"]}`))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	rec := httptest.NewRecorder()
	r.server.Config.Handler.ServeHTTP(rec, req)
	return rec
}

func (r *oauthRouterRig) countClientsNamed(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM mcp_oauth_clients WHERE client_name = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *oauthRouterRig) countClients(t *testing.T, clientID string) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM mcp_oauth_clients WHERE client_id = $1`, clientID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
