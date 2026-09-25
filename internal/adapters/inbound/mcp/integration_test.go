//go:build integration

// This file is the Postgres-backed parity suite technical plan §43.12
// requires: over every principal (viewer through admin, plus no/expired/
// disabled-user auth), the SAME assertion runs twice -- once through the
// REST route directly (with the user's session cookie), once through the
// MCP tool that bridges to it (with a bearer token minted for the SAME
// user, §43.2) -- and both must agree on status-shape and body. It mirrors
// internal/adapters/inbound/httpapi's own *_integration_test.go rig
// conventions (createUserWithRole, createSessionForUser, doJSON), rebuilt
// here (rather than imported) because those helpers are unexported in a
// different package.
package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	mcpadapter "github.com/narvidev/narvi/internal/adapters/inbound/mcp"
	"github.com/narvidev/narvi/internal/adapters/inbound/mcpauth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// mcpTestRig is this package's own rig: just enough real Postgres stores
// to construct BOTH the REST routes (/api/models, /api/sessions,
// /api/sessions/{sessionID}, cookie-authenticated) and the MCP route
// (/mcp) behind the IDENTICAL gates controlplane/serve.go wires in
// production -- mcpadapter.RequireTrustedOrigin first, then
// RequireEnabled, then auth.RequireMCPBearer (§43.2/§43.6's own gate
// order) -- plus the MCP authorization server's own routes (mcpauth,
// §43.14), so a parity test and the SDK-driven end-to-end tests
// (oauth_integration_test.go) exercise the real thing, never a stand-in.
//
// The server is built UNSTARTED so its real listener address is known
// before the router is: PublicBaseURL -- and so the canonical /mcp
// resource every token is bound to, and the protected-resource
// metadata's "resource" -- must equal the URL a client actually dials,
// which the official SDK client checks byte for byte (§43.13).
type mcpTestRig struct {
	pool         *pgxpool.Pool
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	userSessions *narvipg.UserSessionStore
	sessions     *narvipg.SessionStore
	clients      *narvipg.MCPOAuthClientStore
	grants       *narvipg.MCPOAuthGrantStore
	ids          mcpauth.Identifiers
	server       *httptest.Server
	// tokenClient is the pre-registered client mintMCPToken issues
	// tokens under, created on first use.
	tokenClient *sqlcgen.McpOauthClient
}

// rigRoutes lets a test file mount extra routes on the rig's router
// before the server starts (oauth_integration_test.go's Settings routes).
type rigRoutes func(router chi.Router, rig *mcpTestRig)

func newMCPTestRig(t *testing.T, extra ...rigRoutes) *mcpTestRig {
	t.Helper()
	pool := IntegrationTestPool(t)

	rig := &mcpTestRig{
		pool:         pool,
		users:        narvipg.NewUserStore(pool),
		identities:   narvipg.NewIdentityStore(pool),
		userSessions: narvipg.NewUserSessionStore(pool),
		sessions:     narvipg.NewSessionStore(pool),
		clients:      narvipg.NewMCPOAuthClientStore(pool),
		grants:       narvipg.NewMCPOAuthGrantStore(pool),
	}

	router := chi.NewRouter()
	server := httptest.NewUnstartedServer(router)
	baseURL := "http://" + server.Listener.Addr().String()
	rig.server = server

	mcpHandler, err := mcpadapter.NewHandler(mcpadapter.Config{PublicBaseURL: baseURL}, mcpadapter.Twins{
		ListModels:   httpapi.GetModelCatalog(),
		ListSessions: httpapi.ListSessions(rig.sessions),
		GetSession:   httpapi.GetSession(rig.sessions),
	})
	if err != nil {
		t.Fatalf("mcpadapter.NewHandler: %v", err)
	}
	mcpOriginGate, err := mcpadapter.RequireTrustedOrigin(mcpadapter.Config{PublicBaseURL: baseURL})
	if err != nil {
		t.Fatalf("mcpadapter.RequireTrustedOrigin: %v", err)
	}
	advertised := mcpadapter.AdvertisedScopes()
	authServer, err := mcpauth.New(mcpauth.Config{
		PublicBaseURL: baseURL,
		Enabled:       true,
		Scopes:        advertised,
		Timeouts:      platform.DefaultTimeouts(),
	}, mcpauth.Deps{
		Pool:         pool,
		Clients:      rig.clients,
		Grants:       rig.grants,
		UserSessions: rig.userSessions,
		Users:        rig.users,
		AuditLog:     narvipg.NewAuditLogStore(pool),
	})
	if err != nil {
		t.Fatalf("mcpauth.New: %v", err)
	}
	rig.ids = authServer.Identifiers()

	router.Route("/api/models", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetModelCatalog())
	})
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListSessions(rig.sessions))
		r.Get("/{sessionID}", httpapi.GetSession(rig.sessions))
	})
	router.Route("/.well-known/oauth-protected-resource", func(r chi.Router) {
		r.Use(mcpadapter.RequireEnabled(true))
		r.Get("/mcp", authServer.ProtectedResourceMetadata)
	})
	router.Route("/.well-known/oauth-authorization-server", func(r chi.Router) {
		r.Use(mcpadapter.RequireEnabled(true))
		r.Get("/oauth", authServer.AuthorizationServerMetadata)
	})
	router.Route("/oauth", func(r chi.Router) {
		r.Use(mcpadapter.RequireEnabled(true))
		r.Get("/authorize", authServer.Authorize)
		r.Get("/consent", authServer.ConsentPage)
		r.Post("/consent", authServer.ConsentDecision)
		r.Post("/token", authServer.Token)
	})
	router.Route("/mcp", func(r chi.Router) {
		r.Use(mcpOriginGate)
		r.Use(mcpadapter.RequireEnabled(true))
		r.Use(auth.RequireMCPBearer(rig.grants, auth.MCPBearerConfig{
			Resource:              rig.ids.Resource,
			ResourceMetadataURL:   rig.ids.ProtectedResourceMetadataURL,
			Scopes:                mcpscope.Strings(advertised),
			LastUsedWriteInterval: platform.DefaultTimeouts().MCPGrantLastUsedWriteInterval,
		}))
		r.Post("/", mcpHandler.ServeHTTP)
	})
	for _, mount := range extra {
		mount(router, rig)
	}

	server.Start()
	t.Cleanup(server.Close)
	return rig
}

// mintMCPToken issues an access token for userID straight through the
// stores -- a grant with exactly scopes under the rig's own pre-registered
// client, and one token under it -- for the parity tests, whose subject is
// the bridge, not the authorization flow (oauth_integration_test.go drives
// that flow end to end through the official SDK client instead). Returns
// the plaintext token.
func mintMCPToken(ctx context.Context, t *testing.T, r *mcpTestRig, userID pgtype.UUID, scopes []string) string {
	t.Helper()
	if r.tokenClient == nil {
		c, err := r.clients.Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
			ClientID:     "narvi_mcp_c_parity",
			Kind:         sqlcgen.McpOauthClientKindPreregistered,
			ClientName:   "Parity Test Client",
			RedirectUris: []string{"http://127.0.0.1/callback"},
		})
		if err != nil {
			t.Fatalf("create parity client: %v", err)
		}
		r.tokenClient = &c
	}
	grant, err := r.grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID:    userID,
		ClientID:  r.tokenClient.ID,
		Scopes:    scopes,
		Resource:  r.ids.Resource,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	return mintTokenForGrant(ctx, t, r, grant.ID, time.Now().Add(time.Hour))
}

// mintTokenForGrant issues one more access token under grantID.
func mintTokenForGrant(ctx context.Context, t *testing.T, r *mcpTestRig, grantID pgtype.UUID, expires time.Time) string {
	t.Helper()
	raw, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	token := "narvi_mcp_at_" + raw
	if _, err := r.grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID:   grantID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		t.Fatalf("mint access token: %v", err)
	}
	return token
}

// doJSON mirrors httpapi_test's own identical helper -- a plain REST call
// with an optional session cookie, decoding the JSON body into v (if
// non-nil) and returning the status code.
func (r *mcpTestRig) doJSON(t *testing.T, method, path string, body []byte, v any, cookie string) int {
	t.Helper()
	req, err := http.NewRequest(method, r.server.URL+path, bytes.NewReader(body))
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

// mcpCredential is how a raw /mcp call authenticates: a bearer token (the
// only credential /mcp accepts), or -- to prove it is refused -- a cookie.
type mcpCredential struct {
	bearer string
	cookie string
}

// postMCP POSTs one modern (2026-07-28) JSON-RPC request to /mcp.
func (r *mcpTestRig) postMCP(t *testing.T, method, toolName, body string, cred mcpCredential) (int, http.Header, []byte) {
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
	if cred.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+cred.bearer)
	}
	if cred.cookie != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cred.cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, respBody
}

// callTool POSTs a modern (2026-07-28) tools/call over /mcp with the given
// bearer token (§43.2: bearer only). Returns the raw HTTP status and the
// decoded JSON-RPC envelope.
func (r *mcpTestRig) callTool(t *testing.T, toolName, argumentsJSON, bearer string) (int, jsonrpcCallToolEnvelope) {
	t.Helper()
	return r.callToolWith(t, toolName, argumentsJSON, mcpCredential{bearer: bearer})
}

func (r *mcpTestRig) callToolWith(t *testing.T, toolName, argumentsJSON string, cred mcpCredential) (int, jsonrpcCallToolEnvelope) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, toolName, argumentsJSON)
	status, _, raw := r.postMCP(t, "tools/call", toolName, body, cred)
	var env jsonrpcCallToolEnvelope
	if status == http.StatusOK || status == http.StatusBadRequest {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode tools/call response: %v (%s)", err, raw)
		}
	}
	return status, env
}

// listTools POSTs tools/list with bearer and returns the tool names.
func (r *mcpTestRig) listTools(t *testing.T, bearer string) (int, []string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, _, raw := r.postMCP(t, "tools/list", "", body, mcpCredential{bearer: bearer})
	if status != http.StatusOK {
		return status, nil
	}
	var env struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, raw)
	}
	names := make([]string, 0, len(env.Result.Tools))
	for _, tool := range env.Result.Tools {
		names = append(names, tool.Name)
	}
	return status, names
}

type jsonrpcCallToolEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	Result  *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// createUserWithRole mirrors httpapi_test's own identical helper exactly.
func createUserWithRole(ctx context.Context, t *testing.T, r *mcpTestRig, role sqlcgen.UserRole) (sqlcgen.User, string) {
	t.Helper()

	externalID := fmt.Sprintf("test-github-id-%s-%d", role, time.Now().UnixNano())
	email := externalID + "@example.com"

	user, err := r.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: email,
		DisplayName:  "Test User",
		Role:         role,
	})
	if err != nil {
		t.Fatalf("create test user (role=%s): %v", role, err)
	}
	if _, err := r.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        user.ID,
		Provider:      sqlcgen.IdentityProviderGithub,
		ExternalID:    externalID,
		Email:         &email,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := r.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create test user session: %v", err)
	}
	return user, token
}

// createExpiredUserSession mints a session cookie whose row is already
// expired -- for TestParity_ExpiredSession below.
func createExpiredUserSession(ctx context.Context, t *testing.T, r *mcpTestRig, userID pgtype.UUID) string {
	t.Helper()
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := r.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    userID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create expired test user session: %v", err)
	}
	return token
}

// createSessionForUser mirrors httpapi_test's own identical helper.
func createSessionForUser(ctx context.Context, t *testing.T, r *mcpTestRig, ownerID pgtype.UUID) sqlcgen.Session {
	t.Helper()
	row, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   ownerID,
	})
	if err != nil {
		t.Fatalf("create test session for user: %v", err)
	}
	return row
}

// --- Parity: no credential / expired / disabled user ---

// TestParity_NoCredential: no credential is 401 on both sides -- and on
// /mcp a perfectly valid session COOKIE is not a credential at all (§43.2:
// bearer only), so it is 401 too, with the bearer challenge.
func TestParity_NoCredential(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)

	restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, nil, "")
	if restStatus != http.StatusUnauthorized {
		t.Fatalf("REST GET /api/models with no cookie: status = %d, want 401", restStatus)
	}
	mcpStatus, _ := rig.callTool(t, "narvi_list_models", "{}", "")
	if mcpStatus != http.StatusUnauthorized {
		t.Fatalf("MCP narvi_list_models with no credential: status = %d, want 401", mcpStatus)
	}

	_, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	if restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, nil, cookie); restStatus != http.StatusOK {
		t.Fatalf("REST with a valid cookie: status = %d, want 200", restStatus)
	}
	status, header, body := rig.postMCP(t, "tools/call", "narvi_list_models",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		mcpCredential{cookie: cookie})
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` || !strings.HasPrefix(header.Get("WWW-Authenticate"), "Bearer ") {
		t.Fatalf("MCP with only a valid session cookie: status %d body %s challenge %q, want the bearer 401", status, body, header.Get("WWW-Authenticate"))
	}
}

// TestParity_ExpiredCredential: an expired session cookie (REST) and an
// expired access token (MCP) are both 401.
func TestParity_ExpiredCredential(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	expiredCookie := createExpiredUserSession(ctx, t, rig, user.ID)

	restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, nil, expiredCookie)
	if restStatus != http.StatusUnauthorized {
		t.Fatalf("REST with expired session: status = %d, want 401", restStatus)
	}
	live := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})
	lookup, err := rig.grants.LookupAccessToken(ctx, platform.HashToken(live))
	if err != nil {
		t.Fatalf("lookup minted token: %v", err)
	}
	expired := mintTokenForGrant(ctx, t, rig, lookup.GrantID, time.Now().Add(-time.Second))
	mcpStatus, _ := rig.callTool(t, "narvi_list_models", "{}", expired)
	if mcpStatus != http.StatusUnauthorized {
		t.Fatalf("MCP with expired token: status = %d, want 401", mcpStatus)
	}
	if mcpStatus, _ := rig.callTool(t, "narvi_list_models", "{}", live); mcpStatus != http.StatusOK {
		t.Fatalf("MCP with the live token under the same grant: status = %d, want 200", mcpStatus)
	}
}

// --- Parity: every role can list models (§13.3 row 1: everyone,
// including viewer) ---

func TestParity_ListModels_EveryRole(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			user, token := createUserWithRole(ctx, t, rig, role)
			bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})

			var restBody json.RawMessage
			restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, &restBody, token)
			if restStatus != http.StatusOK {
				t.Fatalf("REST GET /api/models as %s: status = %d, want 200", role, restStatus)
			}

			mcpStatus, env := rig.callTool(t, "narvi_list_models", "{}", bearer)
			if mcpStatus != http.StatusOK {
				t.Fatalf("MCP narvi_list_models as %s: status = %d, want 200", role, mcpStatus)
			}
			if env.Result == nil || env.Result.IsError {
				t.Fatalf("MCP narvi_list_models as %s: result = %+v, want a successful result", role, env.Result)
			}
			assertCanonicalJSONEqual(t, "narvi_list_models", restBody, env.Result.StructuredContent)
		})
	}
}

// --- Parity: list sessions, filter:"all" crosses users, filter:"mine"
// does not (finding 6: no per-session visibility in this codebase) ---

func TestParity_ListSessions_FilterAll_CrossesUsers(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	userA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	userB, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearerB := mintMCPToken(ctx, t, rig, userB.ID, []string{"mcp:read"})
	sessionA := createSessionForUser(ctx, t, rig, userA.ID)

	var restRaw json.RawMessage
	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions?filter=all", nil, &restRaw, tokenB)
	if restStatus != http.StatusOK {
		t.Fatalf("REST GET /api/sessions?filter=all: status = %d, want 200", restStatus)
	}
	var restBody struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(restRaw, &restBody); err != nil {
		t.Fatalf("unmarshal REST body: %v", err)
	}
	if !containsSessionID(restBody.Sessions, sessionA.ID) {
		t.Fatalf("REST filter=all as member B does not contain member A's session %s: %+v", sessionA.ID, restBody.Sessions)
	}

	mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{"filter":"all"}`, bearerB)
	if mcpStatus != http.StatusOK {
		t.Fatalf("MCP narvi_list_sessions filter=all: status = %d, want 200", mcpStatus)
	}
	assertCanonicalJSONEqual(t, "narvi_list_sessions(all)", restRaw, env.Result.StructuredContent)
}

func TestParity_ListSessions_FilterMine_Default(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	userA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	userB, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearerB := mintMCPToken(ctx, t, rig, userB.ID, []string{"mcp:read"})
	sessionA := createSessionForUser(ctx, t, rig, userA.ID)

	var restRaw json.RawMessage
	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions", nil, &restRaw, tokenB)
	if restStatus != http.StatusOK {
		t.Fatalf("REST GET /api/sessions (default filter): status = %d, want 200", restStatus)
	}
	var restBody struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(restRaw, &restBody); err != nil {
		t.Fatalf("unmarshal REST body: %v", err)
	}
	if containsSessionID(restBody.Sessions, sessionA.ID) {
		t.Fatalf("REST default filter (mine) as member B unexpectedly contains member A's session")
	}

	mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{}`, bearerB)
	if mcpStatus != http.StatusOK {
		t.Fatalf("MCP narvi_list_sessions (no filter): status = %d, want 200", mcpStatus)
	}
	assertCanonicalJSONEqual(t, "narvi_list_sessions(mine)", restRaw, env.Result.StructuredContent)
}

// --- Parity: get another user's session succeeds (finding 6, pinned
// deliberately so a future tightening is a deliberate test edit) ---

func TestParity_GetSession_AnotherUsersSessionSucceeds(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	userA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	userB, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearerB := mintMCPToken(ctx, t, rig, userB.ID, []string{"mcp:read"})
	sessionA := createSessionForUser(ctx, t, rig, userA.ID)
	sessionAID := sessionA.ID.String()

	var restBody json.RawMessage
	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions/"+sessionAID, nil, &restBody, tokenB)
	if restStatus != http.StatusOK {
		t.Fatalf("REST GET another user's session: status = %d, want 200", restStatus)
	}

	mcpStatus, env := rig.callTool(t, "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, sessionAID), bearerB)
	if mcpStatus != http.StatusOK {
		t.Fatalf("MCP narvi_get_session on another user's session: status = %d, want 200", mcpStatus)
	}
	if env.Result == nil || env.Result.IsError {
		t.Fatalf("MCP narvi_get_session result = %+v, want success", env.Result)
	}
	assertCanonicalJSONEqual(t, "narvi_get_session", restBody, env.Result.StructuredContent)
}

// --- Parity: unknown uuid -> 404 both, isError:true on the MCP side ---

func TestParity_GetSession_UnknownUUID(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})
	unknown := "00000000-0000-0000-0000-000000000000"

	var restBody struct {
		Error string `json:"error"`
	}
	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions/"+unknown, nil, &restBody, token)
	if restStatus != http.StatusNotFound {
		t.Fatalf("REST GET unknown session: status = %d, want 404", restStatus)
	}

	mcpStatus, env := rig.callTool(t, "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, unknown), bearer)
	if mcpStatus != http.StatusOK {
		t.Fatalf("MCP narvi_get_session unknown uuid: status = %d, want 200 (isError:true is a successful response)", mcpStatus)
	}
	if env.Result == nil || !env.Result.IsError {
		t.Fatalf("MCP narvi_get_session unknown uuid: result = %+v, want IsError:true", env.Result)
	}
	if len(env.Result.Content) != 1 || env.Result.Content[0].Text != restBody.Error {
		t.Errorf("MCP narvi_get_session content text = %+v, want REST's own error text %q", env.Result.Content, restBody.Error)
	}
}

// --- Parity: malformed sessionId -> both reject (technical plan §43.8):
// REST 400, MCP isError:true. The MCP side now rejects "not-a-uuid"
// BEFORE the twin is ever invoked -- this package's own bridge validates
// arguments against the SAME contracts $def its InputSchema advertises
// (format:"uuid" included, schemas.go's validateArguments), so the
// REST-side 400 the twin would otherwise answer is unreachable for this
// exact value. Parity means "both reject", never byte-identical text:
// the MCP side's own message comes from the schema validator, not the
// REST route. ---

func TestParity_GetSession_MalformedUUID(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})

	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions/not-a-uuid", nil, nil, token)
	if restStatus != http.StatusBadRequest {
		t.Fatalf("REST GET malformed session id: status = %d, want 400", restStatus)
	}

	mcpStatus, env := rig.callTool(t, "narvi_get_session", `{"sessionId":"not-a-uuid"}`, bearer)
	if mcpStatus != http.StatusOK {
		t.Fatalf("MCP narvi_get_session malformed uuid: status = %d, want 200 (isError:true is a SUCCESSFUL JSON-RPC response, per the MCP tools spec's own input-validation-failure classification)", mcpStatus)
	}
	if env.Error != nil {
		t.Fatalf("MCP narvi_get_session malformed uuid: error = %+v, want no top-level JSON-RPC error", env.Error)
	}
	if env.Result == nil || !env.Result.IsError {
		t.Fatalf("MCP narvi_get_session malformed uuid: result = %+v, want IsError:true", env.Result)
	}
}

// --- Parity: bad filter/limit -> both reject (REST 400, MCP
// isError:true), same reasoning as TestParity_GetSession_MalformedUUID
// above -- filter/limit now carry real enum/minimum/maximum constraints
// (contracts/rest/v1/dtos.schema.json), so this package's own bridge
// rejects both values before the twin is ever invoked. ---

func TestParity_ListSessions_BadFilterAndLimit(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})

	t.Run("filter=x", func(t *testing.T) {
		restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions?filter=x", nil, nil, token)
		if restStatus != http.StatusBadRequest {
			t.Fatalf("REST filter=x: status = %d, want 400", restStatus)
		}
		mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{"filter":"x"}`, bearer)
		if mcpStatus != http.StatusOK {
			t.Fatalf("MCP filter=x: status = %d, want 200 (isError:true is a SUCCESSFUL JSON-RPC response)", mcpStatus)
		}
		if env.Error != nil {
			t.Fatalf("MCP filter=x: error = %+v, want no top-level JSON-RPC error", env.Error)
		}
		if env.Result == nil || !env.Result.IsError {
			t.Fatalf("MCP filter=x: result = %+v, want IsError:true", env.Result)
		}
	})

	t.Run("limit=0", func(t *testing.T) {
		restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions?limit=0", nil, nil, token)
		if restStatus != http.StatusBadRequest {
			t.Fatalf("REST limit=0: status = %d, want 400", restStatus)
		}
		mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{"limit":0}`, bearer)
		if mcpStatus != http.StatusOK {
			t.Fatalf("MCP limit=0: status = %d, want 200 (isError:true is a SUCCESSFUL JSON-RPC response)", mcpStatus)
		}
		if env.Error != nil {
			t.Fatalf("MCP limit=0: error = %+v, want no top-level JSON-RPC error", env.Error)
		}
		if env.Result == nil || !env.Result.IsError {
			t.Fatalf("MCP limit=0: result = %+v, want IsError:true", env.Result)
		}
	})
}

// --- Parity: tools/list is role-independent (§43.9 item 3's own
// parity row: "no discovery gating in 180") ---

func TestParity_ToolsListIsRoleIndependent(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)

	// The pinned SDK's own tools/list response sorts by tool NAME
	// (verified against the real wire response), not by this package's
	// own table-declaration order (tools.go's toolSpecs) -- what matters
	// for parity is that every principal holding the same scopes sees the
	// identical, deterministic list, which this test asserts by comparing
	// against the same fixed slice for all four roles below. Role does not
	// gate discovery today (technical plan §43.17): every read tool is
	// open to every role.
	want := []string{"narvi_get_session", "narvi_list_models", "narvi_list_sessions"}
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			user, _ := createUserWithRole(ctx, t, rig, role)
			status, got := rig.listTools(t, mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"}))
			if status != http.StatusOK {
				t.Fatalf("tools/list as %s: status = %d, want 200", role, status)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("tools/list as %s = %v, want %v", role, got, want)
			}
		})
	}
}

// --- Parity: user A's request then user B's on the same handler never
// leaks A's own "mine" scoping into B's response (per-request server
// construction, §43.7's own principal-isolation row) ---

func TestParity_PrincipalDoesNotLeakAcrossRequests(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	userA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	userB, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	tokenA := mintMCPToken(ctx, t, rig, userA.ID, []string{"mcp:read"})
	tokenB := mintMCPToken(ctx, t, rig, userB.ID, []string{"mcp:read"})
	sessionA := createSessionForUser(ctx, t, rig, userA.ID)

	// A's own "mine" list contains A's session.
	statusA, envA := rig.callTool(t, "narvi_list_sessions", `{"filter":"mine"}`, tokenA)
	if statusA != http.StatusOK || envA.Result == nil {
		t.Fatalf("A's own request: status = %d, result = %+v", statusA, envA.Result)
	}
	if !bytes.Contains(envA.Result.StructuredContent, []byte(sessionA.ID.String())) {
		t.Fatalf("A's own filter=mine response does not contain A's own session %s", sessionA.ID.String())
	}

	// B's own "mine" list, immediately after, on the SAME handler, must
	// NOT contain A's session.
	statusB, envB := rig.callTool(t, "narvi_list_sessions", `{"filter":"mine"}`, tokenB)
	if statusB != http.StatusOK || envB.Result == nil {
		t.Fatalf("B's own request: status = %d, result = %+v", statusB, envB.Result)
	}
	if bytes.Contains(envB.Result.StructuredContent, []byte(sessionA.ID.String())) {
		t.Fatalf("B's own filter=mine response unexpectedly contains A's own session %s -- principal leaked across requests", sessionA.ID.String())
	}
}

// --- helpers ---

func containsSessionID(sessions []struct {
	ID string `json:"id"`
}, id pgtype.UUID) bool {
	target := id.String()
	for _, s := range sessions {
		if s.ID == target {
			return true
		}
	}
	return false
}

// assertCanonicalJSONEqual compares a and b as CANONICAL JSON (decoded
// then re-marshaled, so key order/whitespace differences never fail the
// comparison) -- technical plan §43.9 item 3's own "canonical JSON
// compare" parity requirement.
func assertCanonicalJSONEqual(t *testing.T, label string, a, b []byte) {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("%s: unmarshal REST body: %v (%s)", label, err, a)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("%s: unmarshal MCP structuredContent: %v (%s)", label, err, b)
	}
	aCanon, _ := json.Marshal(av)
	bCanon, _ := json.Marshal(bv)
	if string(aCanon) != string(bCanon) {
		t.Errorf("%s: REST and MCP bodies differ.\nREST: %s\nMCP:  %s", label, aCanon, bCanon)
	}
}

// TestParity_BearerEqualsCookieForEveryRole is the "a token never does
// more than its user" threat row (technical plan §43.16): for every role,
// every tool call made with a bearer token minted for a user answers
// exactly what the REST twin answers that same user's cookie -- same
// success/refusal shape, same body -- including the reads of ANOTHER
// user's session and of a session that does not exist.
func TestParity_BearerEqualsCookieForEveryRole(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	owner, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	othersSession := createSessionForUser(ctx, t, rig, owner.ID)

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			user, cookie := createUserWithRole(ctx, t, rig, role)
			own := createSessionForUser(ctx, t, rig, user.ID)
			bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})

			cases := []struct {
				name, restPath, tool, args string
			}{
				{"list models", "/api/models", "narvi_list_models", `{}`},
				{"list my sessions", "/api/sessions", "narvi_list_sessions", `{}`},
				{"list all sessions", "/api/sessions?filter=all", "narvi_list_sessions", `{"filter":"all"}`},
				{"get my session", "/api/sessions/" + own.ID.String(), "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, own.ID.String())},
				{"get another user's session", "/api/sessions/" + othersSession.ID.String(), "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, othersSession.ID.String())},
				{"get an unknown session", "/api/sessions/00000000-0000-0000-0000-000000000000", "narvi_get_session", `{"sessionId":"00000000-0000-0000-0000-000000000000"}`},
			}
			for _, tc := range cases {
				var restBody json.RawMessage
				restStatus := rig.doJSON(t, http.MethodGet, tc.restPath, nil, &restBody, cookie)
				mcpStatus, env := rig.callTool(t, tc.tool, tc.args, bearer)
				if mcpStatus != http.StatusOK || env.Result == nil {
					t.Fatalf("%s: MCP status %d result %+v, want a 200 tool result", tc.name, mcpStatus, env.Result)
				}
				switch restStatus {
				case http.StatusOK:
					if env.Result.IsError {
						t.Fatalf("%s: REST 200 but MCP isError: %+v", tc.name, env.Result.Content)
					}
					assertCanonicalJSONEqual(t, tc.name, restBody, env.Result.StructuredContent)
				default:
					var restErr struct {
						Error string `json:"error"`
					}
					_ = json.Unmarshal(restBody, &restErr)
					if !env.Result.IsError || len(env.Result.Content) != 1 || env.Result.Content[0].Text != restErr.Error {
						t.Fatalf("%s: REST %d %q, MCP result %+v -- want the same refusal as isError", tc.name, restStatus, restErr.Error, env.Result)
					}
				}
			}
		})
	}
}

// TestParity_DisabledUser: disabling a user refuses their cookie AND their
// bearer token on the very next request -- the bearer check re-reads the
// users row on every call, exactly as the cookie check does.
func TestParity_DisabledUser(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})
	if status, _ := rig.callTool(t, "narvi_list_models", "{}", bearer); status != http.StatusOK {
		t.Fatalf("before disable: MCP status %d, want 200", status)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if status := rig.doJSON(t, http.MethodGet, "/api/models", nil, nil, cookie); status != http.StatusUnauthorized {
		t.Fatalf("REST after disable: status %d, want 401", status)
	}
	if status, _ := rig.callTool(t, "narvi_list_models", "{}", bearer); status != http.StatusUnauthorized {
		t.Fatalf("MCP after disable: status %d, want 401", status)
	}
}
