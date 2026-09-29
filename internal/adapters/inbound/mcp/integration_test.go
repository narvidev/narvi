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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	mcpadapter "github.com/narvidev/narvi/internal/adapters/inbound/mcp"
	"github.com/narvidev/narvi/internal/adapters/inbound/mcpauth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// mcpTestRig is this package's own rig: just enough real Postgres stores
// to construct BOTH the REST routes (/api/models, /api/sessions,
// /api/sessions/{sessionID}[/status|/events], cookie-authenticated) and
// the MCP route
// (/mcp) behind the same gates, in the same order, controlplane/serve.go
// wires -- mcpadapter.RequireTrustedOrigin first, then RequireEnabled,
// then auth.RequireMCPBearer (§43.2/§43.6) -- so a parity test compares
// the bridge against its REST twins over real stores. It is a copy, not
// the production router: the authorization flow end to end, and the
// production wiring itself, are proven against controlplane.Build's own
// router (controlplane's TestOAuth_ProductionRouter). Tokens here are
// minted straight through the stores (mintMCPToken).
//
// The server is built UNSTARTED so its real listener address is known
// before the router is: PublicBaseURL -- and so the canonical /mcp
// resource every token is bound to -- must equal the URL a client
// actually dials (§43.13).
type mcpTestRig struct {
	pool         *pgxpool.Pool
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	userSessions *narvipg.UserSessionStore
	sessions     *narvipg.SessionStore
	events       *narvipg.EventStore
	turns        *narvipg.TurnStore
	clients      *narvipg.MCPOAuthClientStore
	grants       *narvipg.MCPOAuthGrantStore
	ids          mcpauth.Identifiers
	server       *httptest.Server
	waiter       *sessionactivity.Waiter
	// artifacts/prSessions/reviewVerdicts back row 182's result (technical
	// plan §43.20), whose verdicts' freshness is read from codeHost.
	artifacts      *narvipg.ArtifactStore
	prSessions     *narvipg.GitHubPRSessionStore
	reviewVerdicts *narvipg.ReviewVerdictStore
	// tokenClient is the pre-registered client mintMCPToken issues
	// tokens under, created on first use.
	tokenClient *sqlcgen.McpOauthClient
}

// unlimitedBrake is the create brake this rig's handler is built with: its
// subject is the bridge, not the brake (the brake has its own tests, and
// controlplane's production router ships the real one).
type unlimitedBrake struct{}

func (unlimitedBrake) Allow(string) (bool, time.Duration) { return true, 0 }

// parityRepo is the repository every create in this file names; the rig
// makes it known to the deployment, as the entitlement gate requires.
const parityRepo = "acme/widgets"

func newMCPTestRig(t *testing.T) *mcpTestRig {
	t.Helper()
	ctx := context.Background()
	pool, connStr := IntegrationTestPoolAndConnStr(t)

	rig := &mcpTestRig{
		pool:         pool,
		users:        narvipg.NewUserStore(pool),
		identities:   narvipg.NewIdentityStore(pool),
		userSessions: narvipg.NewUserSessionStore(pool),
		sessions:     narvipg.NewSessionStore(pool),
		events:       narvipg.NewEventStore(pool),
		turns:        narvipg.NewTurnStore(pool),
		clients:      narvipg.NewMCPOAuthClientStore(pool),
		grants:       narvipg.NewMCPOAuthGrantStore(pool),

		artifacts:      narvipg.NewArtifactStore(pool),
		prSessions:     narvipg.NewGitHubPRSessionStore(pool),
		reviewVerdicts: narvipg.NewReviewVerdictStore(pool),
	}

	router := chi.NewRouter()
	server := httptest.NewUnstartedServer(router)
	baseURL := "http://" + server.Listener.Addr().String()
	rig.server = server
	// One waiter for the REST route and the MCP twin alike, as
	// controlplane wires it: one replica's caps.
	rig.waiter = sessionactivity.NewWaiter(sessionactivity.ConfigFrom(platform.DefaultTimeouts()))
	// One result handler for the REST route and the MCP twin alike, as
	// controlplane wires it, reading freshness from a fixed fake code host.
	getSessionResult := httpapi.GetSessionResult(httpapi.SessionResultDeps{
		Pool:           pool,
		Sessions:       rig.sessions,
		Turns:          rig.turns,
		Events:         rig.events,
		Artifacts:      rig.artifacts,
		PRSessions:     rig.prSessions,
		ReviewVerdicts: rig.reviewVerdicts,
		SourceControl:  parityCodeHost{},
		BotToken:       "parity-bot-token",
		Timeouts:       platform.DefaultTimeouts(),
	})

	// One create handler for the REST route and the MCP twin alike, as
	// controlplane wires it (technical plan §43.8). Every live session actor
	// holds a pool connection until its registry shuts down, and each
	// create with a prompt spawns one, so the registry has a pool of its own
	// on the same database; closed after the registry shuts down, and both
	// before the shared database is reset.
	actorCfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse actor pool config: %v", err)
	}
	actorCfg.MaxConns = 32
	actorPool, err := pgxpool.NewWithConfig(ctx, actorCfg)
	if err != nil {
		t.Fatalf("actor pool: %v", err)
	}
	t.Cleanup(actorPool.Close)
	registry, err := sessionactor.NewRegistry(ctx, actorPool, platform.DefaultTimeouts(), nil, nil, nil, baseURL, nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	if err := rig.prSessions.EnsureRow(ctx, parityRepo, 1); err != nil {
		t.Fatalf("make %s known: %v", parityRepo, err)
	}
	createSession := httpapi.CreateSession(pool, rig.sessions, rig.turns, narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), registry, nil, false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(pool), rig.prSessions)
	// One handler per plan and turn route for the REST route and the MCP
	// twin alike, as controlplane wires them (technical plan §43.21).
	plans, planDocuments := narvipg.NewPlanStore(pool), narvipg.NewPlanDocumentStore(pool)
	participants, auditLog := narvipg.NewParticipantStore(pool), narvipg.NewAuditLogStore(pool)
	outbox, linearAgentSessions := narvipg.NewOutboxStore(pool, false), narvipg.NewLinearAgentSessionStore(pool)
	listPlans := httpapi.ListPlans(rig.sessions, plans, rig.turns, rig.events, planDocuments)
	approvePlan := httpapi.ApprovePlan(pool, rig.sessions, rig.turns, plans, rig.events, planDocuments, participants, outbox, linearAgentSessions, auditLog, registry, false)
	rejectPlan := httpapi.RejectPlan(pool, rig.sessions, rig.turns, plans, rig.events, planDocuments, participants, outbox, linearAgentSessions, auditLog, false)
	createTurn := httpapi.CreateTurn(pool, rig.sessions, rig.turns, plans, participants, auditLog, registry, nil, nil, false)
	// One stop handler for the REST route and the MCP twin alike, as
	// controlplane wires it (technical plan §43.22).
	stopSession := httpapi.StopSession(httpapi.StopSessionDeps{
		Pool:             pool,
		Sessions:         rig.sessions,
		Turns:            rig.turns,
		Timers:           narvipg.NewTimerStore(pool),
		Participants:     participants,
		AuditLog:         auditLog,
		GitHubPRSessions: rig.prSessions,
		Registry:         registry,
	})

	mcpHandler, err := mcpadapter.NewHandler(mcpadapter.Config{PublicBaseURL: baseURL, CreateBrake: unlimitedBrake{}}, mcpadapter.Twins{
		ListModels:       httpapi.GetModelCatalog(),
		ListSessions:     httpapi.ListSessions(rig.sessions),
		GetSession:       httpapi.GetSession(rig.sessions),
		GetSessionStatus: httpapi.GetSessionStatus(rig.sessions, rig.waiter, platform.DefaultTimeouts()),
		ListEvents:       httpapi.ListEvents(rig.sessions, rig.events),
		GetSessionResult: getSessionResult,
		CreateSession:    createSession,
		ListPlans:        listPlans,
		ApprovePlan:      approvePlan,
		RejectPlan:       rejectPlan,
		CreateTurn:       createTurn,
		StopSession:      stopSession,
	})
	if err != nil {
		t.Fatalf("mcpadapter.NewHandler: %v", err)
	}
	mcpOriginGate, err := mcpadapter.RequireTrustedOrigin(mcpadapter.Config{PublicBaseURL: baseURL})
	if err != nil {
		t.Fatalf("mcpadapter.RequireTrustedOrigin: %v", err)
	}
	advertised := mcpadapter.AdvertisedScopes()
	rig.ids, err = mcpauth.DeriveIdentifiers(baseURL)
	if err != nil {
		t.Fatalf("mcpauth.DeriveIdentifiers: %v", err)
	}

	router.Route("/api/models", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetModelCatalog())
	})
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", createSession)
		r.Get("/", httpapi.ListSessions(rig.sessions))
		r.Get("/{sessionID}", httpapi.GetSession(rig.sessions))
		r.Get("/{sessionID}/status", httpapi.GetSessionStatus(rig.sessions, rig.waiter, platform.DefaultTimeouts()))
		r.Get("/{sessionID}/events", httpapi.ListEvents(rig.sessions, rig.events))
		r.Get("/{sessionID}/result", getSessionResult)
		r.Get("/{sessionID}/plans", listPlans)
		r.Post("/{sessionID}/plans/{planId}/approve", approvePlan)
		r.Post("/{sessionID}/plans/{planId}/reject", rejectPlan)
		r.Post("/{sessionID}/turns", createTurn)
		r.Post("/{sessionID}/stop", stopSession)
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

	server.Start()
	t.Cleanup(server.Close)
	return rig
}

// mintMCPToken issues an access token for userID straight through the
// stores -- a grant under the rig's own pre-registered client, and one
// token under it holding exactly scopes -- for the parity tests, whose
// subject is the bridge, not the authorization flow (controlplane's
// TestOAuth_ProductionRouter drives that flow end to end through the
// official SDK client instead). Returns the plaintext token.
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
	return mintTokenForGrant(ctx, t, r, grant.ID, scopes, time.Now().Add(time.Hour))
}

// mintTokenForGrant issues one more access token under grantID, holding
// exactly scopes (a token's scopes are its own, fixed at issuance).
func mintTokenForGrant(ctx context.Context, t *testing.T, r *mcpTestRig, grantID pgtype.UUID, scopes []string, expires time.Time) string {
	t.Helper()
	raw, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	token := "narvi_mcp_at_" + raw
	if _, err := r.grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID:   grantID,
		TokenHash: platform.HashToken(token),
		Scopes:    scopes,
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
	expired := mintTokenForGrant(ctx, t, rig, lookup.GrantID, []string{"mcp:read"}, time.Now().Add(-time.Second))
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
	want := []string{"narvi_get_session", "narvi_get_session_result", "narvi_get_session_status", "narvi_get_session_transcript", "narvi_list_models", "narvi_list_plans", "narvi_list_sessions", "narvi_wait_for_session"}
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
// user's session and of a session that does not exist. narvi_create_session
// is in the table too (§43.8), under a read+write token: a viewer is
// refused exactly as REST refuses it, writing nothing, and every other
// role starts a session whose body equals REST's for the same request but
// for what differs by construction -- its id and times, and its source,
// mcp over MCP and web by cookie.
func TestParity_BearerEqualsCookieForEveryRole(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	owner, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	othersSession := createSessionForUser(ctx, t, rig, owner.ID)
	// Another user's session with live work and a history: a completed
	// turn, a queued follow-up, and a few events -- so the status and
	// transcript rows compare real content, not empty shapes.
	seedBusySession(ctx, t, rig, othersSession.ID, 3)
	// Another user's review session with an assessed verdict, whose
	// freshness the fake code host confirms live -- so the result rows
	// compare a real review, its live freshness included.
	othersReview := seedReviewedSession(ctx, t, rig, owner.ID)
	// Another user's session with two plan versions, the first superseded
	// by the second, awaiting approval -- so the plan rows compare real
	// versions, not an empty list.
	othersPlanned := createSessionForUser(ctx, t, rig, owner.ID)
	seedPlanVersions(ctx, t, rig, othersPlanned.ID)
	const unknown = "00000000-0000-0000-0000-000000000000"

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
				// Row 182 (technical plan §43.20): the status and the
				// transcript, of the caller's own session, of another
				// user's, and of one that does not exist.
				{"status of my session", "/api/sessions/" + own.ID.String() + "/status", "narvi_get_session_status", fmt.Sprintf(`{"sessionId":%q}`, own.ID.String())},
				{"status of another user's session", "/api/sessions/" + othersSession.ID.String() + "/status", "narvi_get_session_status", fmt.Sprintf(`{"sessionId":%q}`, othersSession.ID.String())},
				{"status of an unknown session", "/api/sessions/" + unknown + "/status", "narvi_get_session_status", fmt.Sprintf(`{"sessionId":%q}`, unknown)},
				// Row 182's wait (piece (b)): the same twin with
				// ?waitSeconds=. The caller's own session has no turn
				// (idle, settled: answered at once); another user's is
				// queued, so both sides wait out the second and time out.
				{"wait on my session", "/api/sessions/" + own.ID.String() + "/status?waitSeconds=1", "narvi_wait_for_session", fmt.Sprintf(`{"sessionId":%q,"waitSeconds":1}`, own.ID.String())},
				{"wait on another user's session", "/api/sessions/" + othersSession.ID.String() + "/status?waitSeconds=1", "narvi_wait_for_session", fmt.Sprintf(`{"sessionId":%q,"waitSeconds":1}`, othersSession.ID.String())},
				{"wait on an unknown session", "/api/sessions/" + unknown + "/status?waitSeconds=1", "narvi_wait_for_session", fmt.Sprintf(`{"sessionId":%q,"waitSeconds":1}`, unknown)},
				// Row 182's result (piece (c)): the caller's own session (no
				// run, no pull request: reviewScope none), another user's
				// review session with a live-confirmed verdict, and one that
				// does not exist.
				{"result of my session", "/api/sessions/" + own.ID.String() + "/result", "narvi_get_session_result", fmt.Sprintf(`{"sessionId":%q}`, own.ID.String())},
				{"result of another user's session", "/api/sessions/" + othersReview.ID.String() + "/result", "narvi_get_session_result", fmt.Sprintf(`{"sessionId":%q}`, othersReview.ID.String())},
				{"result of an unknown session", "/api/sessions/" + unknown + "/result", "narvi_get_session_result", fmt.Sprintf(`{"sessionId":%q}`, unknown)},
				{"transcript of my session", "/api/sessions/" + own.ID.String() + "/events", "narvi_get_session_transcript", fmt.Sprintf(`{"sessionId":%q}`, own.ID.String())},
				{"transcript of another user's session", "/api/sessions/" + othersSession.ID.String() + "/events?limit=2", "narvi_get_session_transcript", fmt.Sprintf(`{"sessionId":%q,"limit":2}`, othersSession.ID.String())},
				{"transcript of an unknown session", "/api/sessions/" + unknown + "/events", "narvi_get_session_transcript", fmt.Sprintf(`{"sessionId":%q}`, unknown)},
				{"transcript with a cursor naming no event id", "/api/sessions/" + othersSession.ID.String() + "/events?cursor=9999999999999999999", "narvi_get_session_transcript", fmt.Sprintf(`{"sessionId":%q,"cursor":"9999999999999999999"}`, othersSession.ID.String())},
				// Row 183 (technical plan §43.21): a session's plans -- the
				// caller's own (none yet), another user's two versions, and
				// a session that does not exist.
				{"plans of my session", "/api/sessions/" + own.ID.String() + "/plans", "narvi_list_plans", fmt.Sprintf(`{"sessionId":%q}`, own.ID.String())},
				{"plans of another user's session", "/api/sessions/" + othersPlanned.ID.String() + "/plans", "narvi_list_plans", fmt.Sprintf(`{"sessionId":%q}`, othersPlanned.ID.String())},
				{"plans of an unknown session", "/api/sessions/" + unknown + "/plans", "narvi_list_plans", fmt.Sprintf(`{"sessionId":%q}`, unknown)},
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
					if tc.tool == "narvi_get_session_status" || tc.tool == "narvi_wait_for_session" {
						assertStatusParity(t, tc.name, restBody, env.Result.StructuredContent, tc.tool == "narvi_wait_for_session")
						continue
					}
					assertCanonicalJSONEqual(t, tc.name, restBody, env.Result.StructuredContent)
					if tc.name == "result of another user's session" && !strings.Contains(string(restBody), `"freshness":{"reason":null,"state":"current"}`) {
						t.Fatalf("%s: body %s, want the reviewed verdict current -- read live through the same twin on both sides", tc.name, restBody)
					}
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

			assertCreateParity(ctx, t, rig, role, user.ID, cookie, mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read", "mcp:write"}))
		})
	}
}

// assertCreateParity is TestParity_BearerEqualsCookieForEveryRole's create
// row: the same request, once by cookie through POST /api/sessions and once
// through narvi_create_session with bearer (each under its own
// idempotency key, so neither is a replay of the other).
func assertCreateParity(ctx context.Context, t *testing.T, rig *mcpTestRig, role sqlcgen.UserRole, userID pgtype.UUID, cookie, bearer string) {
	t.Helper()
	const prompt = "fix the flaky test"
	repos := `[{"name":"widgets","url":"https://github.com/` + parityRepo + `"}]`
	restKey, mcpKey := "2b3c4d5e-6f70-4a81-9b2c-3d4e5f607182", "8a9b0c1d-2e3f-4a5b-8c6d-7e8f9a0b1c2d"
	restRequest := fmt.Sprintf(`{"spawnSource":"web","title":"Parity","prompt":%q,"repos":[{"name":"widgets","url":"https://github.com/%s","branch":null}],"modelId":null,"effort":null,"planMode":false,"idempotencyKey":%q}`, prompt, parityRepo, restKey)
	arguments := fmt.Sprintf(`{"title":"Parity","prompt":%q,"repos":%s,"idempotencyKey":%q}`, prompt, repos, mcpKey)

	var restBody json.RawMessage
	restStatus := rig.doJSON(t, http.MethodPost, "/api/sessions", []byte(restRequest), &restBody, cookie)
	mcpStatus, env := rig.callTool(t, "narvi_create_session", arguments, bearer)
	if mcpStatus != http.StatusOK || env.Result == nil {
		t.Fatalf("create: MCP status %d result %+v, want a 200 tool result", mcpStatus, env.Result)
	}
	sessions := func() int {
		var n int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE created_by = $1 AND create_idempotency_key IS NOT NULL`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if role == sqlcgen.UserRoleViewer {
		var restErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(restBody, &restErr)
		if restStatus != http.StatusForbidden || !env.Result.IsError || len(env.Result.Content) != 1 || env.Result.Content[0].Text != restErr.Error {
			t.Fatalf("create as a viewer: REST %d %q, MCP %+v -- want REST's 403 as isError, same text", restStatus, restErr.Error, env.Result)
		}
		if n := sessions(); n != 0 {
			t.Fatalf("create as a viewer wrote %d session(s)", n)
		}
		return
	}
	if restStatus != http.StatusCreated || env.Result.IsError {
		t.Fatalf("create as %s: REST %d %s, MCP %+v -- want both to start a session", role, restStatus, restBody, env.Result)
	}
	normalize := func(raw []byte, wantSource string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if m["spawnSource"] != wantSource {
			t.Fatalf("spawnSource = %v, want %s: %s", m["spawnSource"], wantSource, raw)
		}
		for _, differs := range []string{"id", "createdAt", "updatedAt", "spawnSource"} {
			delete(m, differs)
		}
		return m
	}
	mcpRaw, err := json.Marshal(env.Result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	restCanon, _ := json.Marshal(normalize(restBody, "web"))
	mcpCanon, _ := json.Marshal(normalize(mcpRaw, "mcp"))
	if string(restCanon) != string(mcpCanon) {
		t.Fatalf("create as %s: REST and MCP sessions differ beyond id, times and source.\nREST: %s\nMCP:  %s", role, restCanon, mcpCanon)
	}
	if n := sessions(); n != 2 {
		t.Fatalf("create as %s: %d session(s) under a key, want REST's and MCP's", role, n)
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

// seedBusySession gives sessionID a completed turn, a queued follow-up,
// and n events -- a session whose row still says "created" while a turn
// waits, and a transcript longer than one small page.
func seedBusySession(ctx context.Context, t *testing.T, r *mcpTestRig, sessionID pgtype.UUID, n int) {
	t.Helper()
	done, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: done.ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	if _, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending}); err != nil {
		t.Fatalf("create follow-up: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionID,
			Type:      "token",
			MessageID: fmt.Sprintf("parity-%s-%d", sessionID.String(), i),
			Payload:   []byte(fmt.Sprintf(`{"n":%d}`, i)),
		}); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
	}
}

// seedPlanVersions gives sessionID two plan versions, each produced by a
// completed plan-mode turn: v1 superseded, v2 awaiting approval -- what a
// request for changes leaves behind.
func seedPlanVersions(ctx context.Context, t *testing.T, r *mcpTestRig, sessionID pgtype.UUID) {
	t.Helper()
	plans := narvipg.NewPlanStore(r.pool)
	for _, v := range []struct {
		version int32
		status  sqlcgen.PlanStatus
	}{{1, sqlcgen.PlanStatusSuperseded}, {2, sqlcgen.PlanStatusAwaitingApproval}} {
		producing, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
		if err != nil {
			t.Fatalf("create plan-mode turn: %v", err)
		}
		if _, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sessionID, TurnID: producing.ID, Version: v.version, Status: v.status}); err != nil {
			t.Fatalf("create plan v%d: %v", v.version, err)
		}
	}
}

// sessionActivityProperties is SessionActivity's own property set, read
// from the embedded contract: the status body carries exactly these keys,
// so no transcript (or anything else) rides along with it -- the wait
// object only when waited (a read that waited, §43.20 piece (b)).
func sessionActivityProperties(t *testing.T, waited bool) []string {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(doc.Defs["SessionActivity"].Properties))
	for name := range doc.Defs["SessionActivity"].Properties {
		if name == "wait" && !waited {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// assertStatusParity is assertCanonicalJSONEqual for a status body:
// observedAt is each snapshot's own database clock, so two reads never
// share it -- it is compared for order (the REST read came first), and
// every other byte must be equal. Both bodies carry exactly
// SessionActivity's keys -- wait exactly when waited -- so no events, no
// transcript. Two waits that timed out never blocked for exactly the same
// time, so their waitedMs is compared for reason only; every other wait
// (settled or over capacity: 0 ms) compares whole.
func assertStatusParity(t *testing.T, label string, restBody, mcpBody []byte, waited bool) {
	t.Helper()
	var rest, viaMCP map[string]any
	if err := json.Unmarshal(restBody, &rest); err != nil {
		t.Fatalf("%s: unmarshal REST body: %v (%s)", label, err, restBody)
	}
	if err := json.Unmarshal(mcpBody, &viaMCP); err != nil {
		t.Fatalf("%s: unmarshal MCP structuredContent: %v (%s)", label, err, mcpBody)
	}
	want := sessionActivityProperties(t, waited)
	for side, body := range map[string]map[string]any{"REST": rest, "MCP": viaMCP} {
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: %s status keys = %v, want exactly SessionActivity's %v", label, side, keys, want)
		}
	}
	restAt, err1 := time.Parse(time.RFC3339Nano, fmt.Sprint(rest["observedAt"]))
	mcpAt, err2 := time.Parse(time.RFC3339Nano, fmt.Sprint(viaMCP["observedAt"]))
	if err1 != nil || err2 != nil || mcpAt.Before(restAt) {
		t.Fatalf("%s: observedAt REST %v MCP %v (errs %v, %v), want two timestamps, the MCP read's not before the REST read's", label, rest["observedAt"], viaMCP["observedAt"], err1, err2)
	}
	delete(rest, "observedAt")
	delete(viaMCP, "observedAt")
	if waited {
		restWait, _ := rest["wait"].(map[string]any)
		mcpWait, _ := viaMCP["wait"].(map[string]any)
		if restWait == nil || mcpWait == nil || restWait["reason"] != mcpWait["reason"] {
			t.Fatalf("%s: wait REST %v MCP %v, want the same reason on both", label, rest["wait"], viaMCP["wait"])
		}
		if restWait["reason"] == "timeout" {
			delete(restWait, "waitedMs")
			delete(mcpWait, "waitedMs")
		}
	}
	restCanon, _ := json.Marshal(rest)
	mcpCanon, _ := json.Marshal(viaMCP)
	if string(restCanon) != string(mcpCanon) {
		t.Errorf("%s: REST and MCP status bodies differ beyond observedAt.\nREST: %s\nMCP:  %s", label, restCanon, mcpCanon)
	}
}

// TestParity_GetSessionTranscript_CursorWalkEqualsREST walks a session's
// whole transcript a page at a time twice -- through the REST route with a
// cookie, and through narvi_get_session_transcript with a bearer token for
// the same user, each following its own nextCursor -- for every role: the
// pages, and the cursors between them, are identical, and both walks end
// on the same last page with nextCursor null.
func TestParity_GetSessionTranscript_CursorWalkEqualsREST(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	owner, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, owner.ID)
	seedBusySession(ctx, t, rig, sess.ID, 7)

	type page struct {
		Events     []json.RawMessage `json:"events"`
		NextCursor *string           `json:"nextCursor"`
	}
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			user, cookie := createUserWithRole(ctx, t, rig, role)
			bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})

			var restCursor, mcpCursor *string
			pages, events := 0, 0
			for {
				path := "/api/sessions/" + sess.ID.String() + "/events?limit=3"
				args := fmt.Sprintf(`{"sessionId":%q,"limit":3}`, sess.ID.String())
				if restCursor != nil {
					path += "&cursor=" + *restCursor
					args = fmt.Sprintf(`{"sessionId":%q,"limit":3,"cursor":%q}`, sess.ID.String(), *mcpCursor)
				}
				var restRaw json.RawMessage
				if status := rig.doJSON(t, http.MethodGet, path, nil, &restRaw, cookie); status != http.StatusOK {
					t.Fatalf("REST page %d: status %d", pages, status)
				}
				mcpStatus, env := rig.callTool(t, "narvi_get_session_transcript", args, bearer)
				if mcpStatus != http.StatusOK || env.Result == nil || env.Result.IsError {
					t.Fatalf("MCP page %d: status %d result %+v", pages, mcpStatus, env.Result)
				}
				assertCanonicalJSONEqual(t, fmt.Sprintf("page %d", pages), restRaw, env.Result.StructuredContent)

				var restPage, mcpPage page
				if err := json.Unmarshal(restRaw, &restPage); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(env.Result.StructuredContent, &mcpPage); err != nil {
					t.Fatal(err)
				}
				pages++
				events += len(restPage.Events)
				restCursor, mcpCursor = restPage.NextCursor, mcpPage.NextCursor
				if (restCursor == nil) != (mcpCursor == nil) || (restCursor != nil && *restCursor != *mcpCursor) {
					t.Fatalf("page %d: nextCursor REST %v MCP %v, want equal", pages, restCursor, mcpCursor)
				}
				if restCursor == nil {
					break
				}
				if pages > 10 {
					t.Fatal("the walk did not end")
				}
			}
			if pages != 3 || events != 7 {
				t.Fatalf("walked %d pages and %d events, want 3 pages (3+3+1) and 7 events", pages, events)
			}
		})
	}
}

// parityCodeHost is the result's code host in this rig: every pull request
// open at head h1 on main, main's tip b1 -- so a verdict recorded at h1 on
// main at b1 reads current, the same on the REST route and through the
// tool. Any other port method panics: the result never calls one.
type parityCodeHost struct{ ports.SourceControl }

func (parityCodeHost) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{Owner: owner, Repo: repo, Number: number, HeadSHA: "h1", BaseRef: "main"}, true, nil
}

func (parityCodeHost) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return "b1", spec.Branch, nil
}

// seedReviewedSession creates ownerID's review session for acme/widgets#7
// with an ended run that streamed text, a pull request it opened, and an
// assessed verdict recorded at h1 on main at b1 under the current policy.
func seedReviewedSession(ctx context.Context, t *testing.T, r *mcpTestRig, ownerID pgtype.UUID) sqlcgen.Session {
	t.Helper()
	sess := createSessionForUser(ctx, t, r, ownerID)
	if err := r.prSessions.EnsureRow(ctx, "acme/widgets", 7); err != nil {
		t.Fatalf("ensure claim: %v", err)
	}
	if err := r.prSessions.SetSessionID(ctx, "acme/widgets", 7, sess.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	attempt, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	watermark, err := r.events.MaxEventIDForSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: attempt.ID, Status: sqlcgen.TurnStatusDispatched, DispatchedAt: now, DispatchedEventID: &watermark}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sess.ID, Type: "token", MessageID: "prt_final", Payload: []byte(`{"type":"token","messageId":"prt_final","text":"Reviewed: low risk."}`)}); err != nil {
		t.Fatalf("token: %v", err)
	}
	for _, status := range []sqlcgen.TurnStatus{sqlcgen.TurnStatusProcessing, sqlcgen.TurnStatusCompleted} {
		arg := sqlcgen.UpdateTurnStatusParams{ID: attempt.ID, Status: status}
		if status == sqlcgen.TurnStatusCompleted {
			arg.CompletedAt = now
		}
		if _, err := r.turns.UpdateStatus(ctx, arg); err != nil {
			t.Fatalf("end attempt: %v", err)
		}
	}
	if _, err := r.artifacts.Create(ctx, sqlcgen.CreateArtifactParams{SessionID: sess.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://github.com/acme/widgets/pull/8", Metadata: []byte(`{"repo":"widgets","number":8}`)}); err != nil {
		t.Fatalf("pull request artifact: %v", err)
	}
	baseRef, baseSHA := "main", "b1"
	if _, err := r.reviewVerdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName: "acme/widgets", PrNumber: 7, HeadSha: "h1",
		RiskLevel: "low", Premise: "ok", BlastRadius: []byte(`[]`), FilesChanged: 1,
		TestsCoverage: "adequate", DocsDrift: "none", ProposedShippable: "auto", Shippable: "auto",
		SessionID: sess.ID, ArchDecisionTags: []byte(`[]`), ArchDecisionRoots: []byte(`[]`), AncestorChain: []byte(`[]`),
		BaseRef: &baseRef, BaseSha: &baseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion, AttemptID: attempt.ID,
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	return sess
}
