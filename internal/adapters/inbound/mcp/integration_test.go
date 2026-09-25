//go:build integration

// This file is the Postgres-backed parity suite technical plan §43.9
// item 3 requires: over every principal (viewer through admin, plus no/
// expired/disabled-user auth), the SAME assertion runs twice -- once
// through the REST route directly, once through the MCP tool that
// bridges to it -- and both must agree on status-shape and body. It
// mirrors internal/adapters/inbound/httpapi's own *_integration_test.go
// rig conventions (newTestRig, createUserWithRole, createSessionForUser,
// doJSON) exactly, rebuilt here (rather than imported) because those
// helpers are unexported in a different package.
package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	mcpadapter "github.com/narvidev/narvi/internal/adapters/inbound/mcp"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// mcpTestRig is this package's own minimal rig: just enough real Postgres
// stores to construct BOTH the REST routes (/api/models, /api/sessions,
// /api/sessions/{sessionID}) and the MCP route (/mcp) behind the
// IDENTICAL gates controlplane/serve.go wires in production for /mcp --
// mcpadapter.RequireTrustedOrigin first, then RequireEnabled, then auth.
// Middleware (§43.2/§43.6's own gate order) -- so a parity test exercises
// the real thing, never a stand-in. (A prior revision of this rig omitted
// RequireTrustedOrigin entirely, despite this same comment already
// claiming "the IDENTICAL gates" -- round 2 review of PR #324, finding
// N19's own "why nothing catches it" note. No test in this file sends an
// Origin header, so adding it changes no existing test's outcome.)
type mcpTestRig struct {
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	userSessions *narvipg.UserSessionStore
	sessions     *narvipg.SessionStore
	server       *httptest.Server
}

func newMCPTestRig(t *testing.T) mcpTestRig {
	t.Helper()
	pool := IntegrationTestPool(t)

	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	sessions := narvipg.NewSessionStore(pool)

	mcpHandler, err := mcpadapter.NewHandler(mcpadapter.Config{PublicBaseURL: "http://example.test"}, mcpadapter.Twins{
		ListModels:   httpapi.GetModelCatalog(),
		ListSessions: httpapi.ListSessions(sessions),
		GetSession:   httpapi.GetSession(sessions),
	})
	if err != nil {
		t.Fatalf("mcpadapter.NewHandler: %v", err)
	}
	mcpOriginGate, err := mcpadapter.RequireTrustedOrigin(mcpadapter.Config{PublicBaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("mcpadapter.RequireTrustedOrigin: %v", err)
	}

	router := chi.NewRouter()
	router.Route("/api/models", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Get("/", httpapi.GetModelCatalog())
	})
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Get("/", httpapi.ListSessions(sessions))
		r.Get("/{sessionID}", httpapi.GetSession(sessions))
	})
	router.Route("/mcp", func(r chi.Router) {
		r.Use(mcpOriginGate)
		r.Use(mcpadapter.RequireEnabled(true))
		r.Use(auth.Middleware(userSessions, users))
		r.Post("/", mcpHandler.ServeHTTP)
	})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return mcpTestRig{
		users:        users,
		identities:   identities,
		userSessions: userSessions,
		sessions:     sessions,
		server:       server,
	}
}

// doJSON mirrors httpapi_test's own identical helper -- a plain REST call
// with an optional session cookie, decoding the JSON body into v (if
// non-nil) and returning the status code.
func (r mcpTestRig) doJSON(t *testing.T, method, path string, body []byte, v any, token string) int {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, r.server.URL+path, reqBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
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

// callTool POSTs a modern (2026-07-28) tools/call over /mcp, with token
// carried as a real cookie (via a cookie jar, exactly like a browser-based
// MCP client would -- §43.2: this surface is cookie-authenticated,
// nothing else). Returns the raw HTTP status and the decoded JSON-RPC
// envelope.
func (r mcpTestRig) callTool(t *testing.T, toolName, argumentsJSON, token string) (int, jsonrpcCallToolEnvelope) {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, toolName, argumentsJSON)
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/mcp", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", toolName)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var env jsonrpcCallToolEnvelope
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode tools/call response: %v", err)
		}
	}
	return resp.StatusCode, env
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
func createUserWithRole(ctx context.Context, t *testing.T, r mcpTestRig, role sqlcgen.UserRole) (sqlcgen.User, string) {
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
func createExpiredUserSession(ctx context.Context, t *testing.T, r mcpTestRig, userID pgtype.UUID) string {
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
func createSessionForUser(ctx context.Context, t *testing.T, r mcpTestRig, ownerID pgtype.UUID) sqlcgen.Session {
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

// --- Parity: no cookie / expired / disabled user ---

func TestParity_NoCookie(t *testing.T) {
	rig := newMCPTestRig(t)

	restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, nil, "")
	if restStatus != http.StatusUnauthorized {
		t.Fatalf("REST GET /api/models with no cookie: status = %d, want 401", restStatus)
	}

	mcpStatus, _ := rig.callTool(t, "narvi_list_models", "{}", "")
	if mcpStatus != http.StatusUnauthorized {
		t.Fatalf("MCP narvi_list_models with no cookie: status = %d, want 401", mcpStatus)
	}
}

func TestParity_ExpiredSession(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	expiredToken := createExpiredUserSession(ctx, t, rig, user.ID)

	restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, nil, expiredToken)
	if restStatus != http.StatusUnauthorized {
		t.Fatalf("REST with expired session: status = %d, want 401", restStatus)
	}
	mcpStatus, _ := rig.callTool(t, "narvi_list_models", "{}", expiredToken)
	if mcpStatus != http.StatusUnauthorized {
		t.Fatalf("MCP with expired session: status = %d, want 401", mcpStatus)
	}
}

// --- Parity: every role can list models (§13.3 row 1: everyone,
// including viewer) ---

func TestParity_ListModels_EveryRole(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			_, token := createUserWithRole(ctx, t, rig, role)

			var restBody json.RawMessage
			restStatus := rig.doJSON(t, http.MethodGet, "/api/models", nil, &restBody, token)
			if restStatus != http.StatusOK {
				t.Fatalf("REST GET /api/models as %s: status = %d, want 200", role, restStatus)
			}

			mcpStatus, env := rig.callTool(t, "narvi_list_models", "{}", token)
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
	sessionA := createSessionForUser(ctx, t, rig, userA.ID)
	_ = userB

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

	mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{"filter":"all"}`, tokenB)
	if mcpStatus != http.StatusOK {
		t.Fatalf("MCP narvi_list_sessions filter=all: status = %d, want 200", mcpStatus)
	}
	assertCanonicalJSONEqual(t, "narvi_list_sessions(all)", restRaw, env.Result.StructuredContent)
}

func TestParity_ListSessions_FilterMine_Default(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	userA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	_, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
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

	mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{}`, tokenB)
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
	_, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sessionA := createSessionForUser(ctx, t, rig, userA.ID)
	sessionAID := sessionA.ID.String()

	var restBody json.RawMessage
	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions/"+sessionAID, nil, &restBody, tokenB)
	if restStatus != http.StatusOK {
		t.Fatalf("REST GET another user's session: status = %d, want 200", restStatus)
	}

	mcpStatus, env := rig.callTool(t, "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, sessionAID), tokenB)
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
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	unknown := "00000000-0000-0000-0000-000000000000"

	var restBody struct {
		Error string `json:"error"`
	}
	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions/"+unknown, nil, &restBody, token)
	if restStatus != http.StatusNotFound {
		t.Fatalf("REST GET unknown session: status = %d, want 404", restStatus)
	}

	mcpStatus, env := rig.callTool(t, "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, unknown), token)
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
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions/not-a-uuid", nil, nil, token)
	if restStatus != http.StatusBadRequest {
		t.Fatalf("REST GET malformed session id: status = %d, want 400", restStatus)
	}

	mcpStatus, env := rig.callTool(t, "narvi_get_session", `{"sessionId":"not-a-uuid"}`, token)
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
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	t.Run("filter=x", func(t *testing.T) {
		restStatus := rig.doJSON(t, http.MethodGet, "/api/sessions?filter=x", nil, nil, token)
		if restStatus != http.StatusBadRequest {
			t.Fatalf("REST filter=x: status = %d, want 400", restStatus)
		}
		mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{"filter":"x"}`, token)
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
		mcpStatus, env := rig.callTool(t, "narvi_list_sessions", `{"limit":0}`, token)
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
	// for parity is that every principal sees the identical, deterministic
	// order, which this test asserts by comparing against the same fixed
	// slice for all four roles below.
	want := []string{"narvi_get_session", "narvi_list_models", "narvi_list_sessions"}
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			_, token := createUserWithRole(ctx, t, rig, role)

			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New: %v", err)
			}
			client := &http.Client{Jar: jar}
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
			req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/mcp", bytes.NewReader([]byte(body)))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", "tools/list")
			req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("tools/list as %s: status = %d, want 200", role, resp.StatusCode)
			}
			var env struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode tools/list response: %v", err)
			}
			if len(env.Result.Tools) != len(want) {
				t.Fatalf("tools/list as %s: got %d tools, want %d", role, len(env.Result.Tools), len(want))
			}
			for i, tool := range env.Result.Tools {
				if tool.Name != want[i] {
					t.Errorf("tools/list as %s: tools[%d] = %q, want %q", role, i, tool.Name, want[i])
				}
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
	userA, tokenA := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	_, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
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
