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
// token allows and calls one; a revoked authorization, a deleted or
// disabled client and a disabled user each stop working on the very next
// call; and a scope-less approval's tool list is empty.
//
// With the surface OFF, the discovery documents and every /oauth route
// answer the documented disabled response (§43.11/§43.14), while the
// Settings routes keep serving, so an authorization can always be listed
// and revoked (§43.18).
//
// One Postgres container backs every subtest; each subtest creates its
// own users and clients.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
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
// and serves its router on that listener.
func newOAuthRouterRig(t *testing.T, pool *pgxpool.Pool) *oauthRouterRig {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	t.Setenv("NARVI_PUBLIC_BASE_URL", "http://"+server.Listener.Addr().String())
	t.Setenv("NARVI_MCP_ENABLED", "true")
	cfg, err := platform.Load()
	if err != nil {
		server.Close()
		t.Fatalf("platform.Load: %v", err)
	}
	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		server.Close()
		t.Fatalf("Build: %v", err)
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

// sdkFlow is one connected official-SDK client and what it went through.
type sdkFlow struct {
	session     *sdkmcp.ClientSession
	driver      *consentDriver
	recorder    *recordingTransport
	member      sqlcgen.User
	cookie      string
	adminCookie string
	client      restdtos.MCPClient
}

// connectSDKClient registers a client as an admin (through the REST
// route, as a person would), then connects the official SDK client as a
// signed-in member, whose consent the driver gives.
func (r *oauthRouterRig) connectSDKClient(ctx context.Context, t *testing.T, configure func(*consentDriver)) *sdkFlow {
	t.Helper()
	_, adminCookie := createRouterUser(ctx, t, r.pool, sqlcgen.UserRoleAdmin)
	member, memberCookie := createRouterUser(ctx, t, r.pool, sqlcgen.UserRoleMember)

	var client restdtos.MCPClient
	body := []byte(`{"clientName":"Editor Plugin","redirectUris":["http://127.0.0.1/callback"]}`)
	if status := r.doJSON(t, http.MethodPost, "/api/mcp-clients", body, &client, adminCookie); status != http.StatusCreated {
		t.Fatalf("register client: status %d", status)
	}

	driver := &consentDriver{baseURL: r.server.URL, cookie: memberCookie}
	if configure != nil {
		configure(driver)
	}
	recorder := &recordingTransport{}
	httpClient := &http.Client{Transport: recorder}
	handler, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      &oauthex.ClientCredentials{ClientID: client.ClientId},
		RedirectURL:              "http://127.0.0.1:1/callback",
		AuthorizationCodeFetcher: driver.fetch,
		Client:                   httpClient,
	})
	if err != nil {
		t.Fatalf("NewAuthorizationCodeHandler: %v", err)
	}
	sdkClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "sdk-e2e", Version: "1"}, nil)
	session, err := sdkClient.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint:             r.server.URL + "/mcp",
		HTTPClient:           httpClient,
		OAuthHandler:         handler,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("SDK Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return &sdkFlow{session: session, driver: driver, recorder: recorder, member: member, cookie: memberCookie, adminCookie: adminCookie, client: client}
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
}
