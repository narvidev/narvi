//go:build integration

// This file is row 181's exit proof, driven by the official Go SDK's own
// MCP client and OAuth handler against the real rig (newMCPTestRig: the
// same gates, the same authorization server, the same stores serve.go
// wires): a third-party client discovers the resource and the
// authorization server from a live 401, a user approves it on the consent
// page, the client exchanges the code, sees the tools its grant allows,
// and calls one; a revoked authorization stops working on the very next
// call; and a scope-less client's tool list omits what it cannot call
// (technical plan §43.13-§43.19).
package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// settingsRoutes mounts the Settings routes a person uses around the flow
// -- registering the client, listing and revoking authorizations --
// exactly as serve.go does (cookie-authenticated /api groups).
func settingsRoutes(router chi.Router, rig *mcpTestRig) {
	audit := narvipg.NewAuditLogStore(rig.pool)
	router.Route("/api/me/mcp-authorizations", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListMyMCPAuthorizations(rig.grants))
		r.Delete("/{authorizationID}", httpapi.RevokeMyMCPAuthorization(rig.pool, rig.grants, rig.clients, audit))
	})
	router.Route("/api/mcp-clients", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListMCPClients(rig.clients))
		r.Post("/", httpapi.CreateMCPClient(rig.pool, rig.clients, audit))
		r.Delete("/{clientID}", httpapi.DeleteMCPClient(rig.pool, rig.clients, rig.grants, audit))
	})
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
func connectSDKClient(ctx context.Context, t *testing.T, rig *mcpTestRig, configure func(*consentDriver)) *sdkFlow {
	t.Helper()
	_, adminCookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	member, memberCookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	var client restdtos.MCPClient
	body := []byte(`{"clientName":"Editor Plugin","redirectUris":["http://127.0.0.1/callback"]}`)
	if status := rig.doJSON(t, http.MethodPost, "/api/mcp-clients", body, &client, adminCookie); status != http.StatusCreated {
		t.Fatalf("register client: status %d", status)
	}

	driver := &consentDriver{baseURL: rig.server.URL, cookie: memberCookie}
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
		Endpoint:             rig.server.URL + "/mcp",
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

func toolNames(t *testing.T, ctx context.Context, s *sdkmcp.ClientSession) []string {
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

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestOAuth_EndToEnd_SDKClient is technical plan §43.19's end-to-end proof:
// the official SDK client discovers everything from one live 401, the
// member approves on the consent page, and the resulting token lists
// exactly the three tools and calls one with the same bytes the REST twin
// gives the member's own cookie. The SDK's own RFC 9207 issuer check
// (reject a missing or mismatched iss when support is advertised) passes
// along the way.
func TestOAuth_EndToEnd_SDKClient(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t, settingsRoutes)
	flow := connectSDKClient(ctx, t, rig, nil)

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
	if got := toolNames(t, ctx, flow.session); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ListTools = %v, want %v", got, want)
	}

	res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool narvi_list_models: res %+v err %v", res, err)
	}
	var restBody json.RawMessage
	if status := rig.doJSON(t, http.MethodGet, "/api/models", nil, &restBody, flow.cookie); status != http.StatusOK {
		t.Fatalf("REST /api/models: status %d", status)
	}
	mcpBody, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalJSONEqual(t, "narvi_list_models over OAuth", restBody, mcpBody)

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
}

// revokedCallFails asserts the token flow last used now answers 401 with
// the exact generic body and error="invalid_token", on a raw call.
func revokedCallFails(ctx context.Context, t *testing.T, rig *mcpTestRig, token string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, header, raw := rig.postMCP(t, "tools/call", "narvi_list_models", body, mcpCredential{bearer: token})
	if status != http.StatusUnauthorized || string(raw) != `{"error":"unauthorized"}` {
		t.Fatalf("call with the revoked token: status %d body %s, want 401 {\"error\":\"unauthorized\"}", status, raw)
	}
	challenges, err := oauthex.ParseWWWAuthenticate(header.Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 || challenges[0].Params["error"] != "invalid_token" {
		t.Fatalf("challenge = %q (err %v), want error=\"invalid_token\"", header.Values("WWW-Authenticate"), err)
	}
}

// TestOAuth_RevokedAuthorizationStopsOnNextCall_User is the revocation
// exit criterion through the user's own Settings route: after DELETE
// /api/me/mcp-authorizations/{id}, the very next call through the SAME
// SDK session is refused -- the server answers 401 invalid_token, the
// SDK's one Authorize retry sends the user back through consent (the
// fetcher's second invocation, which this user declines), and the call
// fails. No access token survives for the grant.
func TestOAuth_RevokedAuthorizationStopsOnNextCall_User(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t, settingsRoutes)
	flow := connectSDKClient(ctx, t, rig, func(d *consentDriver) {
		d.deny = func(n int32) bool { return n > 1 }
	})
	if res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}}); err != nil || res.IsError {
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
	_, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}})
	if err == nil {
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
	revokedCallFails(ctx, t, rig, token)

	var tokens int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_access_tokens WHERE grant_id = $1`, grantID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("access tokens left for the revoked grant = %d (err %v), want 0", tokens, err)
	}
}

// TestOAuth_RevokedAuthorizationStopsOnNextCall_ClientDeleted: an admin
// deleting the client revokes every authorization issued to it; the
// member's token is refused on its very next call.
func TestOAuth_RevokedAuthorizationStopsOnNextCall_ClientDeleted(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t, settingsRoutes)
	flow := connectSDKClient(ctx, t, rig, nil)
	if res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("call before deletion: res %+v err %v", res, err)
	}
	token := flow.recorder.lastBearer()
	if status := rig.doJSON(t, http.MethodDelete, "/api/mcp-clients/"+flow.client.Id, nil, nil, flow.adminCookie); status != http.StatusNoContent {
		t.Fatalf("delete client: status %d, want 204", status)
	}
	revokedCallFails(ctx, t, rig, token)
}

// TestOAuth_RevokedAuthorizationStopsOnNextCall_DisabledUser: disabling
// the user refuses their token on its very next call -- the bearer check
// re-reads the users row on every call.
func TestOAuth_RevokedAuthorizationStopsOnNextCall_DisabledUser(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t, settingsRoutes)
	flow := connectSDKClient(ctx, t, rig, nil)
	if res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("call before disabling: res %+v err %v", res, err)
	}
	token := flow.recorder.lastBearer()
	if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, flow.member.ID); err != nil {
		t.Fatal(err)
	}
	revokedCallFails(ctx, t, rig, token)
}

// TestBearer_DisabledClientIs401NextCall: an operator disabling a client
// (without deleting it) refuses every token issued to it on the next call.
func TestBearer_DisabledClientIs401NextCall(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t, settingsRoutes)
	flow := connectSDKClient(ctx, t, rig, nil)
	if res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("call before disabling: res %+v err %v", res, err)
	}
	token := flow.recorder.lastBearer()
	if _, err := rig.pool.Exec(ctx, `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE client_id = $1`, flow.client.ClientId); err != nil {
		t.Fatal(err)
	}
	revokedCallFails(ctx, t, rig, token)
}

// TestBearer_GrantResourceMismatchIs401: a grant stored for another
// resource (another deployment's /mcp) never authenticates here, however
// valid its token otherwise is.
func TestBearer_GrantResourceMismatchIs401(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t)
	user, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	token := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})
	if status, _ := rig.callTool(t, "narvi_list_models", "{}", token); status != http.StatusOK {
		t.Fatalf("before: status %d, want 200", status)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE mcp_oauth_grants SET resource = 'https://other.example/mcp' WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	revokedCallFails(ctx, t, rig, token)
}

// TestOAuth_ScopelessGrant_ToolsListEmpty is the discovery exit criterion
// end to end: the member clears every checkbox, the SDK client still
// connects (server/discover succeeds), its tool list is empty, the
// instructions name no tool, and calling a tool it cannot see answers
// exactly what calling a tool that does not exist answers.
func TestOAuth_ScopelessGrant_ToolsListEmpty(t *testing.T) {
	ctx := testCtx(t)
	rig := newMCPTestRig(t, settingsRoutes)
	flow := connectSDKClient(ctx, t, rig, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return nil }
	})

	var scopes []string
	if err := rig.pool.QueryRow(ctx, `SELECT scopes FROM mcp_oauth_grants WHERE user_id = $1`, flow.member.ID).Scan(&scopes); err != nil || len(scopes) != 0 {
		t.Fatalf("grant scopes = %v (err %v), want none", scopes, err)
	}
	if got := toolNames(t, ctx, flow.session); len(got) != 0 {
		t.Fatalf("ListTools under a scope-less grant = %v, want none", got)
	}
	if init := flow.session.InitializeResult(); init == nil || strings.Contains(init.Instructions, "narvi_") {
		t.Fatalf("instructions under a scope-less grant name a tool: %+v", init)
	}
	if _, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_list_models", Arguments: map[string]any{}}); err == nil {
		t.Fatalf("calling a hidden tool succeeded")
	}

	// Byte for byte: the hidden tool's JSON-RPC error, raw, against an
	// unknown tool's under a full-scope grant (the SDK echoes the name the
	// caller sent, so that one substitution is the only difference).
	scopeless := flow.recorder.lastBearer()
	other, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	full := mintMCPToken(ctx, t, rig, other.ID, []string{"mcp:read"})
	call := func(token, tool string) (int, string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tool)
		status, _, raw := rig.postMCP(t, "tools/call", tool, body, mcpCredential{bearer: token})
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
}
