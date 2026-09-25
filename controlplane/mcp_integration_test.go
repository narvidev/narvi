//go:build integration

// This file proves the /mcp route group's own wiring in serve.go (§43.2/
// §43.6/§43.11) end to end against the REAL composition root -- not
// merely against internal/adapters/inbound/mcp's own package-level rigs
// (newTestHandler, newMCPTestRig), each of which builds its OWN copy of
// the gate chain (RequireTrustedOrigin, RequireEnabled, auth.Middleware)
// and its OWN Twins, never the wiring serve.go actually registers. A
// regression in serve.go itself -- auth.Middleware dropped, the gates
// reordered, NARVI_MCP_ENABLED hardwired to true, or the wrong twin
// handler wired to the wrong tool -- would pass every test in that
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

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestBuild_MCPSurface_RealRouter drives four cases against the REAL
// /mcp wiring, each a distinct gate that a regression in serve.go's own
// route group could silently drop:
//  1. flag off -> 503 (RequireEnabled, mounted first among the
//     flag/auth/version gates -- before the session store is ever
//     touched).
//  2. flag on, no cookie -> 401 (auth.Middleware, the SAME gate every
//     /api/** route group already uses).
//  3. flag on, invalid Origin -> 403, REGARDLESS of the missing cookie
//     above (RequireTrustedOrigin, mounted first of all -- technical
//     plan §43.2/§43.6).
//  4. flag on, valid cookie, trusted Origin -> 200, a real tools/list
//     response naming this build's own three tools.
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

	t.Run("enabled, no cookie: POST /mcp -> 401", func(t *testing.T) {
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
	})

	t.Run("enabled, invalid Origin: POST /mcp -> 403, regardless of the missing cookie", func(t *testing.T) {
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
			t.Fatalf("status = %d, body = %s, want 403 (the Origin gate must run before auth.Middleware, so a missing cookie must never be observed first)", rec.Code, rec.Body.String())
		}
	})

	t.Run("enabled, valid cookie: POST /mcp tools/list -> 200", func(t *testing.T) {
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
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/list")
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
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

		// tools/list alone is not enough to prove auth.Middleware ran:
		// buildServer's own "no authenticated user in context" defect
		// fallback (handler.go's defectServer) advertises the SAME three
		// tools but answers -32603 to every actual CALL -- so a mutant
		// that dropped auth.Middleware from this route group entirely
		// would still pass the tools/list check above. A real tool CALL
		// succeeding is what actually depends on auth.Middleware having
		// populated platform.UserFromContext.
		callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		callReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(callBody))
		callReq.Header.Set("Content-Type", "application/json")
		callReq.Header.Set("Accept", "application/json, text/event-stream")
		callReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
		callReq.Header.Set("Mcp-Method", "tools/call")
		callReq.Header.Set("Mcp-Name", "narvi_list_models")
		callReq.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
		callRec := httptest.NewRecorder()
		app.Router.ServeHTTP(callRec, callReq)

		if callRec.Code != http.StatusOK {
			t.Fatalf("tools/call narvi_list_models: status = %d, body = %s, want 200", callRec.Code, callRec.Body.String())
		}
		if strings.Contains(callRec.Body.String(), `"error"`) || strings.Contains(callRec.Body.String(), `"isError":true`) {
			t.Errorf("tools/call narvi_list_models: body = %s, want a successful result (proves auth.Middleware populated the authenticated user, not the defectServer fallback)", callRec.Body.String())
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
// This test instead calls the REST route directly AND the matching
// tools/call, for the SAME user/cookie, for all three 180 tools, and
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
			callReq.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
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
