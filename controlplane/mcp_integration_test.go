//go:build integration

// This file proves the /mcp route group's own wiring in serve.go (§43.2/
// §43.6/§43.11) end to end against the REAL composition root -- not
// merely against internal/adapters/inbound/mcp's own package-level rigs
// (newTestHandler, newMCPTestRig), each of which builds its OWN copy of
// the gate chain (RequireTrustedOrigin, RequireEnabled,
// auth.RequireMCPBearer) and its OWN Twins, never the wiring serve.go
// actually registers. A regression in serve.go itself -- the bearer gate
// dropped or swapped back for the cookie gate, the gates reordered,
// NARVI_MCP_ENABLED hardwired to true, or the wrong twin handler wired to
// the wrong tool -- would pass every test in that
// package and in this package's own TestBuild_RouteTableMatchesGolden
// (which only checks that "POST /mcp" exists as a route STRING, never
// what sits in front of it) undetected.
//
// This test builds the REAL composition root (Build, the SAME function
// cmd/control-plane itself calls) and drives real requests at
// app.Router.ServeHTTP directly -- mirroring this package's own
// TestBuild_IngressDisabled_RoutesUnmounted precedent (live requests
// against the real router, not a stand-in).
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/mcpauth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// mintBuildBearer issues an mcp:read access token for userID straight
// through the stores, bound to the resource Build derives from
// cfg.PublicBaseURL -- the token the authorization server would have
// issued after consent (whose own flow TestOAuth_ProductionRouter drives
// end to end through the official SDK client).
func mintBuildBearer(ctx context.Context, t *testing.T, pool *pgxpool.Pool, cfg *platform.Config, userID pgtype.UUID) string {
	t.Helper()
	ids, err := mcpauth.DeriveIdentifiers(cfg.PublicBaseURL)
	if err != nil {
		t.Fatalf("DeriveIdentifiers: %v", err)
	}
	client, err := narvipg.NewMCPOAuthClientStore(pool).Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     fmt.Sprintf("narvi_mcp_c_build_%d", time.Now().UnixNano()),
		Kind:         sqlcgen.McpOauthClientKindPreregistered,
		ClientName:   "Build Test Client",
		RedirectUris: []string{"http://127.0.0.1/callback"},
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	grants := narvipg.NewMCPOAuthGrantStore(pool)
	grant, err := grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID: userID, ClientID: client.ID, Scopes: []string{"mcp:read"}, Resource: ids.Resource,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	raw, err := platform.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	token := "narvi_mcp_at_" + raw
	if _, err := grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID: grant.ID, TokenHash: platform.HashToken(token), Scopes: []string{"mcp:read"},
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token
}

// TestBuild_MCPSurface_RealRouter drives the cases below against the REAL
// /mcp wiring, each a distinct gate that a regression in serve.go's own
// route group could silently drop:
//  1. flag off -> 503 (RequireEnabled, mounted first among the
//     flag/auth/version gates -- before any credential store is ever
//     touched), for /mcp AND every route of the authorization server.
//  2. flag on, no credential -> 401 with the bearer challenge
//     (auth.RequireMCPBearer).
//  3. flag on, invalid Origin -> 403, REGARDLESS of the missing credential
//     above (RequireTrustedOrigin, mounted first of all -- technical
//     plan §43.2/§43.6).
//  4. flag on, a valid session COOKIE and nothing else -> 401: the cookie
//     is not a credential on /mcp (§43.2).
//  5. flag on, a valid bearer token, trusted Origin -> 200, a real
//     tools/list naming this build's own three tools and a real call.
func TestBuild_MCPSurface_RealRouter(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	t.Run("disabled: POST /mcp -> 503, before auth", func(t *testing.T) {
		cfg, err := platform.Load()
		if err != nil {
			t.Fatalf("platform.Load: %v", err)
		}
		if cfg.MCPEnabled {
			t.Fatal("cfg.MCPEnabled = true, want false by default (NARVI_MCP_ENABLED unset)")
		}
		app, err := Build(context.Background(), cfg, pool)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		app.Router.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, body = %s, want 503", rec.Code, rec.Body.String())
		}

		// Every route of the authorization server answers the same 503
		// while the surface is off (§43.11/§43.14): observable as off, never
		// a route that is missing.
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/.well-known/oauth-protected-resource/mcp"},
			{http.MethodGet, "/.well-known/oauth-authorization-server/oauth"},
			{http.MethodGet, "/oauth/authorize"},
			{http.MethodGet, "/oauth/consent"},
			{http.MethodPost, "/oauth/consent"},
			{http.MethodPost, "/oauth/token"},
			{http.MethodPost, "/oauth/revoke"},
		} {
			rec := httptest.NewRecorder()
			app.Router.ServeHTTP(rec, httptest.NewRequest(route.method, route.path, nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s while disabled: status = %d, want 503", route.method, route.path, rec.Code)
			}
		}
	})

	// Round 2 review of PR #324, finding N15: the subtest above proves
	// RequireEnabled answers 503 on an ORDINARY request while the flag is
	// off. It says nothing about GATE ORDER -- whether mcpOriginGate or
	// RequireEnabled runs first in serve.go's own r.Use(...) sequence --
	// because it never sends an invalid Origin. The "enabled, invalid
	// Origin" subtest below proves Origin-before-auth, but ALSO with the
	// flag on, so together the two subtests still cannot tell apart the
	// real order (mcpOriginGate, RequireEnabled, auth.Middleware) from a
	// mutant that swaps the first two: both orders answer 503 for a
	// well-formed disabled request and 403 for an enabled request with a
	// bad Origin. This subtest is the one case that DOES tell them apart:
	// flag OFF (the default, never touched by NARVI_MCP_ENABLED) plus an
	// invalid Origin must still answer 403, never 503 -- proving
	// mcpOriginGate truly runs before RequireEnabled, not merely before
	// auth.Middleware.
	t.Run("disabled + invalid Origin: POST /mcp -> still 403, not 503", func(t *testing.T) {
		cfg, err := platform.Load()
		if err != nil {
			t.Fatalf("platform.Load: %v", err)
		}
		if cfg.MCPEnabled {
			t.Fatal("cfg.MCPEnabled = true, want false by default (NARVI_MCP_ENABLED unset)")
		}
		app, err := Build(context.Background(), cfg, pool)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		app.Router.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the Origin gate must run before RequireEnabled, so a disabled surface must never be observed first)", rec.Code, rec.Body.String())
		}
	})

	t.Run("enabled, no credential: POST /mcp -> 401 with the bearer challenge", func(t *testing.T) {
		t.Setenv("NARVI_MCP_ENABLED", "true")
		cfg, err := platform.Load()
		if err != nil {
			t.Fatalf("platform.Load: %v", err)
		}
		app, err := Build(context.Background(), cfg, pool)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		app.Router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s, want 401", rec.Code, rec.Body.String())
		}
		want := `Bearer resource_metadata="http://localhost:8080/.well-known/oauth-protected-resource/mcp", scope="mcp:read"`
		if got := rec.Header().Get("WWW-Authenticate"); got != want {
			t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
		}

		// The discovery documents are served, and name this deployment.
		prm := httptest.NewRecorder()
		app.Router.ServeHTTP(prm, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
		if prm.Code != http.StatusOK || !strings.Contains(prm.Body.String(), `"resource":"http://localhost:8080/mcp"`) {
			t.Fatalf("protected resource metadata: status %d body %s", prm.Code, prm.Body.String())
		}
	})

	t.Run("enabled, invalid Origin: POST /mcp -> 403, regardless of the missing credential", func(t *testing.T) {
		t.Setenv("NARVI_MCP_ENABLED", "true")
		cfg, err := platform.Load()
		if err != nil {
			t.Fatalf("platform.Load: %v", err)
		}
		app, err := Build(context.Background(), cfg, pool)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		app.Router.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the Origin gate must run before the bearer gate, so a missing credential must never be observed first)", rec.Code, rec.Body.String())
		}
	})

	t.Run("enabled: a cookie alone is refused, a bearer token is served", func(t *testing.T) {
		t.Setenv("NARVI_MCP_ENABLED", "true")
		cfg, err := platform.Load()
		if err != nil {
			t.Fatalf("platform.Load: %v", err)
		}
		app, err := Build(context.Background(), cfg, pool)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		ctx := context.Background()
		users := narvipg.NewUserStore(pool)
		identities := narvipg.NewIdentityStore(pool)
		userSessions := narvipg.NewUserSessionStore(pool)

		user, err := users.Create(ctx, sqlcgen.CreateUserParams{
			PrimaryEmail: "mcp-real-router-test@example.com",
			DisplayName:  "MCP Real-Router Test User",
			Role:         sqlcgen.UserRoleMember,
		})
		if err != nil {
			t.Fatalf("create test user: %v", err)
		}
		email := user.PrimaryEmail
		if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
			UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub,
			ExternalID: "mcp-real-router-test-id", Email: &email, EmailVerified: true,
			LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
		}); err != nil {
			t.Fatalf("create test identity: %v", err)
		}
		token, err := platform.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
			UserID:    user.ID,
			TokenHash: platform.HashToken(token),
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
		}); err != nil {
			t.Fatalf("create test user session: %v", err)
		}

		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		mcpRequest := func(body, method, name string) *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", method)
			if name != "" {
				req.Header.Set("Mcp-Name", name)
			}
			return req
		}

		// A valid session cookie is NOT a credential on /mcp (§43.2).
		cookieReq := mcpRequest(body, "tools/list", "")
		cookieReq.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
		cookieRec := httptest.NewRecorder()
		app.Router.ServeHTTP(cookieRec, cookieReq)
		if cookieRec.Code != http.StatusUnauthorized {
			t.Fatalf("cookie only: status = %d, body = %s, want 401", cookieRec.Code, cookieRec.Body.String())
		}

		bearer := mintBuildBearer(ctx, t, pool, cfg, user.ID)
		req := mcpRequest(body, "tools/list", "")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		app.Router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
		}
		for _, want := range []string{"narvi_list_models", "narvi_list_sessions", "narvi_get_session"} {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("tools/list body does not mention %q: %s", want, rec.Body.String())
			}
		}

		// A real tool CALL succeeding is what proves the bearer gate
		// populated both the principal and the grant: without either,
		// buildServer's own defect fallback (handler.go's defectServer)
		// registers no tool at all.
		callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		callReq := mcpRequest(callBody, "tools/call", "narvi_list_models")
		callReq.Header.Set("Authorization", "Bearer "+bearer)
		callRec := httptest.NewRecorder()
		app.Router.ServeHTTP(callRec, callReq)

		if callRec.Code != http.StatusOK {
			t.Fatalf("tools/call narvi_list_models: status = %d, body = %s, want 200", callRec.Code, callRec.Body.String())
		}
		if strings.Contains(callRec.Body.String(), `"error"`) || strings.Contains(callRec.Body.String(), `"isError":true`) {
			t.Errorf("tools/call narvi_list_models: body = %s, want a successful result (proves the bearer gate populated the principal and grant, not the defectServer fallback)", callRec.Body.String())
		}
	})
}

// TestBuild_MCPSurface_TwinParity proves serve.go wires each 180 tool's
// twin to the SAME httpapi handler the equivalent REST route already
// uses, for the SAME authenticated user (round 2 review of PR #324,
// finding N5): TestBuild_MCPSurface_RealRouter's own tools/call check,
// above, only ever confirms narvi_list_models returns SOME 200 body with
// no "error"/"isError":true substring. A twin swapped for a DIFFERENT one
// in serve.go's own Twins{} literal -- e.g. narvi_list_models wired to
// httpapi.ListSessions(sessionStore) instead of httpapi.GetModelCatalog()
// -- still answers 200 with neither substring (a session list contains
// no "error" key), so it passes that check undetected; neither this
// package's own route-table golden nor the mcp package's own unit/
// integration rigs (which each build their OWN Twins, never serve.go's)
// can catch it either.
//
// This test instead calls the REST route directly (with the user's cookie)
// AND the matching tools/call (with a bearer token for the SAME user), for
// all three tools, and
// requires the two bodies to be canonically equal JSON -- a swapped twin
// returns the wrong SHAPE, which fails this comparison immediately,
// regardless of whether the wrong shape happens to contain "error".
func TestBuild_MCPSurface_TwinParity(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)
	t.Setenv("NARVI_MCP_ENABLED", "true")
	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	sessions := narvipg.NewSessionStore(pool)

	user, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "mcp-twin-parity-test@example.com",
		DisplayName:  "MCP Twin Parity Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	email := user.PrimaryEmail
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub,
		ExternalID: "mcp-twin-parity-test-id", Email: &email, EmailVerified: true,
		LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create test user session: %v", err)
	}
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	sessionID := session.ID.String()
	bearer := mintBuildBearer(ctx, t, pool, cfg, user.ID)

	tests := []struct {
		name        string
		restPath    string
		toolName    string
		argumentsJS string
	}{
		{"narvi_list_models", "/api/models", "narvi_list_models", "{}"},
		{"narvi_list_sessions", "/api/sessions?filter=all", "narvi_list_sessions", `{"filter":"all"}`},
		{"narvi_get_session", "/api/sessions/" + sessionID, "narvi_get_session", fmt.Sprintf(`{"sessionId":%q}`, sessionID)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restReq := httptest.NewRequest(http.MethodGet, tt.restPath, nil)
			restReq.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
			restRec := httptest.NewRecorder()
			app.Router.ServeHTTP(restRec, restReq)
			if restRec.Code != http.StatusOK {
				t.Fatalf("REST %s: status = %d, body = %s, want 200", tt.restPath, restRec.Code, restRec.Body.String())
			}

			callBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tt.toolName, tt.argumentsJS)
			callReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(callBody))
			callReq.Header.Set("Content-Type", "application/json")
			callReq.Header.Set("Accept", "application/json, text/event-stream")
			callReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
			callReq.Header.Set("Mcp-Method", "tools/call")
			callReq.Header.Set("Mcp-Name", tt.toolName)
			callReq.Header.Set("Authorization", "Bearer "+bearer)
			callRec := httptest.NewRecorder()
			app.Router.ServeHTTP(callRec, callReq)
			if callRec.Code != http.StatusOK {
				t.Fatalf("tools/call %s: status = %d, body = %s, want 200", tt.toolName, callRec.Code, callRec.Body.String())
			}

			var env struct {
				Result *struct {
					StructuredContent json.RawMessage `json:"structuredContent"`
					IsError           bool            `json:"isError"`
				} `json:"result"`
			}
			if err := json.Unmarshal(callRec.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal tools/call response: %v (body: %s)", err, callRec.Body.String())
			}
			if env.Result == nil || env.Result.IsError {
				t.Fatalf("tools/call %s: result = %+v, want a successful result", tt.toolName, env.Result)
			}

			var restBody, mcpBody any
			if err := json.Unmarshal(restRec.Body.Bytes(), &restBody); err != nil {
				t.Fatalf("unmarshal REST body: %v", err)
			}
			if err := json.Unmarshal(env.Result.StructuredContent, &mcpBody); err != nil {
				t.Fatalf("unmarshal MCP structuredContent: %v", err)
			}
			restCanon, _ := json.Marshal(restBody)
			mcpCanon, _ := json.Marshal(mcpBody)
			if string(restCanon) != string(mcpCanon) {
				t.Errorf("%s: REST and MCP bodies differ -- the wrong twin may be wired in serve.go's own Twins{} literal.\nREST: %s\nMCP:  %s", tt.name, restCanon, mcpCanon)
			}
		})
	}
}
