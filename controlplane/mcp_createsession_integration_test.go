//go:build integration

// This file is row 183's create over MCP (technical plan §43.8) on the
// router Build returns, driven by the official SDK client through the real
// consent flow, as TestOAuth_ProductionRouter's subtests: a member holding
// a read+write grant starts a session whose answer equals the REST twin's
// for the same request by the member's cookie (id, times and source aside),
// whose row records mcp and the member as creator, whose audit row carries
// the grant, and whose intent decision names the mcp surface; a retry with
// the same key yields that one session, while a key the member used by
// cookie is refused to the app and the other way round; a read-only grant,
// and a scope-less one, neither see the tool nor can call it -- a call
// answers exactly what an unknown tool answers, and writes nothing; a
// viewer is refused exactly as REST refuses the viewer's cookie. Then the
// brakes, on routers built with the shipped create brake: /mcp refuses a
// grant past its burst with 429 before any tool runs, while another grant
// of the same user, and another user's grant of the same client, carry
// on; narvi_create_session starts no session past its own burst, a retry
// included; and, on a router with every shipped value, /mcp refills one
// call a second.
//
// Every session a create starts spawns a session actor, and every live
// actor holds a pool connection until its registry shuts down. So the
// routers here are built on a pool of their own (createRouterRig), and
// their registries are shut down before it closes: the shared routers'
// pool is pgx's default size, four connections on CI.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/mcpauth"
	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// createRepo is the repository every create here names; createRouterRig
// makes it known to the deployment, as the entitlement gate requires.
const createRepo = "acme/widgets"

// createRouterRig is newOAuthRouterRigWith on a pool of its own (this
// file's top doc comment says why), with createRepo known and the router's
// session registry shut down when the test ends -- before the pool closes,
// since pgxpool's Close waits for every connection an actor still holds.
func createRouterRig(t *testing.T, connStr string, adjust func(*platform.Timeouts)) *oauthRouterRig {
	t.Helper()
	return createRouterRigWith(t, connStr, nil, adjust)
}

// createRouterRigWith is createRouterRig with env set for its Build alone
// (newOAuthRouterRigWith's own env).
func createRouterRigWith(t *testing.T, connStr string, env map[string]string, adjust func(*platform.Timeouts)) *oauthRouterRig {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create router pool: %v", err)
	}
	t.Cleanup(pool.Close)
	rig := newOAuthRouterRigWith(t, pool, env, cimdfetch.GuardConfig{}, adjust)
	t.Cleanup(func() { _ = rig.app.registry.Shutdown() })
	if err := narvipg.NewGitHubPRSessionStore(pool).EnsureRow(ctx, createRepo, 1); err != nil {
		t.Fatalf("make %s known: %v", createRepo, err)
	}
	return rig
}

// connectSDKClientAs is connectSDKClient for a signed-in user of role.
func (r *oauthRouterRig) connectSDKClientAs(ctx context.Context, t *testing.T, role sqlcgen.UserRole, configure func(*consentDriver)) *sdkFlow {
	t.Helper()
	admin, adminCookie := createRouterUser(ctx, t, r.pool, sqlcgen.UserRoleAdmin)
	user, userCookie := createRouterUser(ctx, t, r.pool, role)
	var client restdtos.MCPClient
	body := []byte(`{"clientName":"Editor Plugin","redirectUris":["http://127.0.0.1/callback"]}`)
	if status := r.doJSON(t, http.MethodPost, "/api/mcp-clients", body, &client, adminCookie); status != http.StatusCreated {
		t.Fatalf("register client: status %d", status)
	}
	flow, err := r.dialSDKClient(ctx, t, user, userCookie, configure, func(c *sdkauth.AuthorizationCodeHandlerConfig) {
		c.PreregisteredClient = &oauthex.ClientCredentials{ClientID: client.ClientId}
		c.RedirectURL = "http://127.0.0.1:1/callback"
	})
	if err != nil {
		t.Fatalf("SDK Connect as %s: %v", role, err)
	}
	flow.admin, flow.adminCookie, flow.client = admin, adminCookie, client
	return flow
}

// createArguments is narvi_create_session's arguments for prompt under key.
func createArguments(prompt, key string) map[string]any {
	return map[string]any{
		"title":          "Delegated",
		"prompt":         prompt,
		"repos":          []any{map[string]any{"name": "widgets", "url": "https://github.com/" + createRepo}},
		"idempotencyKey": key,
	}
}

// restCreateBody is the request the bridge sends for createArguments --
// the body a browser would post -- under key.
func restCreateBody(prompt, key string) []byte {
	return []byte(fmt.Sprintf(`{"spawnSource":"web","title":"Delegated","prompt":%q,"repos":[{"name":"widgets","url":"https://github.com/%s","branch":null}],"modelId":null,"effort":null,"planMode":false,"idempotencyKey":%q}`, prompt, createRepo, key))
}

// callCreate calls narvi_create_session through the SDK session.
func callCreate(ctx context.Context, s *sdkmcp.ClientSession, prompt, key string) (*sdkmcp.CallToolResult, error) {
	return s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_create_session", Arguments: createArguments(prompt, key)})
}

// structured decodes a successful tool result's structured content.
func structured(t *testing.T, res *sdkmcp.CallToolResult) (map[string]any, []byte) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	return m, raw
}

// countOf runs one count query on rig's database.
func (r *oauthRouterRig) countOf(ctx context.Context, t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// grantOf returns the id of the one grant userID holds.
func (r *oauthRouterRig) grantOf(ctx context.Context, t *testing.T, userID pgtype.UUID) string {
	t.Helper()
	var id pgtype.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM mcp_oauth_grants WHERE user_id = $1`, userID).Scan(&id); err != nil {
		t.Fatalf("read the grant: %v", err)
	}
	return id.String()
}

// sdkCreateSession is TestOAuth_ProductionRouter's CreateSession_SDKClient.
func sdkCreateSession(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)

	res, err := callCreate(ctx, flow.session, "fix the flaky test", "5d6e7f80-9a1b-4c2d-8e3f-4a5b6c7d8e9f")
	if err != nil || res.IsError {
		t.Fatalf("CallTool narvi_create_session: res %+v err %v", res, err)
	}
	got, _ := structured(t, res)
	id, _ := got["id"].(string)
	if got["spawnSource"] != "mcp" || got["createdBy"] != flow.member.ID.String() {
		t.Fatalf("the created session = %v, want spawnSource mcp and the member as creator", got)
	}

	// The REST twin's answer to the same request by the member's cookie,
	// under a key of its own: the same bytes but for id, times and source.
	var rest map[string]any
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", restCreateBody("fix the flaky test", "0e1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b"), &rest, flow.cookie); status != http.StatusCreated {
		t.Fatalf("REST create by cookie: status %d body %v", status, rest)
	}
	if rest["spawnSource"] != "web" {
		t.Fatalf("REST create by cookie recorded %v, want web", rest["spawnSource"])
	}
	for _, differs := range []string{"id", "createdAt", "updatedAt", "spawnSource"} {
		delete(got, differs)
		delete(rest, differs)
	}
	gotCanon, _ := json.Marshal(got)
	restCanon, _ := json.Marshal(rest)
	if string(gotCanon) != string(restCanon) {
		t.Fatalf("MCP and REST sessions differ beyond id, times and source:\nMCP:  %s\nREST: %s", gotCanon, restCanon)
	}

	// The row, the audit and the intent decision, read back.
	var source string
	var createdBy pgtype.UUID
	var surface *string
	if err := rig.pool.QueryRow(ctx, `SELECT spawn_source::text, created_by, intent_decision->>'surface' FROM sessions WHERE id = $1`, id).Scan(&source, &createdBy, &surface); err != nil {
		t.Fatalf("read the session: %v", err)
	}
	if source != "mcp" || createdBy != flow.member.ID || surface == nil || *surface != "mcp" {
		t.Fatalf("row: source %q created_by %v intent surface %v, want mcp, the member, mcp", source, createdBy, surface)
	}
	var raw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT detail_json FROM audit_log WHERE action = 'session.create' AND resource_id = $1 AND actor_user_id = $2`, id, flow.member.ID).Scan(&raw); err != nil {
		t.Fatalf("read the session.create audit row: %v", err)
	}
	var detail struct {
		SpawnSource string `json:"spawn_source"`
		MCP         struct {
			GrantID  string `json:"grant_id"`
			ClientID string `json:"client_id"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.SpawnSource != "mcp" || detail.MCP.GrantID != rig.grantOf(ctx, t, flow.member.ID) || detail.MCP.ClientID != flow.client.ClientId {
		t.Fatalf("audit detail = %s, want spawn_source mcp and the member's grant and client", raw)
	}
}

// sdkCreateSessionSameKeyRetry is TestOAuth_ProductionRouter's
// CreateSession_SameKeyRetryCreatesOne_SDKClient.
func sdkCreateSessionSameKeyRetry(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	const key = "6f7a8b9c-0d1e-4f2a-9b3c-4d5e6f7a8b9c"

	first, err := callCreate(ctx, flow.session, "retry me", key)
	if err != nil || first.IsError {
		t.Fatalf("first create: res %+v err %v", first, err)
	}
	again, err := callCreate(ctx, flow.session, "retry me", key)
	if err != nil || again.IsError {
		t.Fatalf("retry: res %+v err %v", again, err)
	}
	a, _ := structured(t, first)
	b, _ := structured(t, again)
	if a["id"] == nil || a["id"] != b["id"] {
		t.Fatalf("retry answered session %v, first %v -- want the same one", b["id"], a["id"])
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, flow.member.ID); n != 1 {
		t.Fatalf("the member has %d sessions after a retry, want 1", n)
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM audit_log WHERE action = 'session.create' AND actor_user_id = $1`, flow.member.ID); n != 1 {
		t.Fatalf("session.create audit rows = %d, want 1", n)
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM turns WHERE session_id = $1`, a["id"]); n != 1 {
		t.Fatalf("turns = %d after a retry, want the first create's one", n)
	}
	reused, err := callCreate(ctx, flow.session, "something else", key)
	if err != nil || !reused.IsError || len(reused.Content) != 1 {
		t.Fatalf("the same key with other arguments: res %+v err %v, want isError", reused, err)
	}
	if text, _ := reused.Content[0].(*sdkmcp.TextContent); text == nil || text.Text != "idempotencyKey reused with a different request" {
		t.Fatalf("the same key with other arguments: %+v, want the 409's text", reused.Content[0])
	}
}

// sdkCreateSessionKeyAcrossSources is TestOAuth_ProductionRouter's
// CreateSession_KeyUsedAnotherWayRefused_SDKClient (§43.8): a user's keys are
// one namespace across REST and MCP, but a replay only answers the source
// that started the session. So a key the member used by cookie is refused
// to the member's app, and a key the app used is refused to the member's
// cookie -- each with the same arguments, each with the 409's text, and
// neither starting, auditing or answering anything: the app never gets a
// session whose spawnSource is web, nor the cookie one whose is mcp.
func sdkCreateSessionKeyAcrossSources(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	const refusal = "idempotencyKey already used for a session started another way"
	counts := func() (sessions, audits int) {
		t.Helper()
		return rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, flow.member.ID),
			rig.countOf(ctx, t, `SELECT count(*) FROM audit_log WHERE actor_user_id = $1`, flow.member.ID)
	}

	// By cookie, then over MCP.
	const restKey = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	var rest map[string]any
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", restCreateBody("one key, two ways", restKey), &rest, flow.cookie); status != http.StatusCreated || rest["spawnSource"] != "web" {
		t.Fatalf("REST create by cookie: status %d body %v, want 201 recording web", status, rest)
	}
	sessionsBefore, auditsBefore := counts()
	res, err := callCreate(ctx, flow.session, "one key, two ways", restKey)
	if err != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("the app reusing the cookie's key: res %+v err %v, want isError", res, err)
	}
	if text, _ := res.Content[0].(*sdkmcp.TextContent); text == nil || text.Text != refusal {
		t.Fatalf("the app reusing the cookie's key: %+v, want %q", res.Content[0], refusal)
	}
	if s, a := counts(); s != sessionsBefore || a != auditsBefore {
		t.Fatalf("the refused call wrote %d session(s) and %d audit row(s)", s-sessionsBefore, a-auditsBefore)
	}

	// Over MCP, then by cookie.
	const mcpKey = "6d7e8f9a-0b1c-4d2e-9f3a-4b5c6d7e8f9a"
	first, err := callCreate(ctx, flow.session, "the other way round", mcpKey)
	if err != nil || first.IsError {
		t.Fatalf("create over MCP: res %+v err %v", first, err)
	}
	if got, _ := structured(t, first); got["spawnSource"] != "mcp" {
		t.Fatalf("create over MCP recorded %v, want mcp", got["spawnSource"])
	}
	sessionsBefore, auditsBefore = counts()
	var refused struct {
		Error string `json:"error"`
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", restCreateBody("the other way round", mcpKey), &refused, flow.cookie); status != http.StatusConflict || refused.Error != refusal {
		t.Fatalf("the cookie reusing the app's key: status %d error %q, want 409 %q", status, refused.Error, refusal)
	}
	if s, a := counts(); s != sessionsBefore || a != auditsBefore {
		t.Fatalf("the refused request wrote %d session(s) and %d audit row(s)", s-sessionsBefore, a-auditsBefore)
	}
}

// assertCreateHiddenLikeUnknown checks, for a flow whose grant cannot see
// the write tools, that tools/list and the instructions name none of them,
// that calling narvi_create_session through the SDK fails, that a raw call
// to it -- and to narvi_approve_plan and narvi_stop_session -- answers
// exactly an unknown tool's bytes (the name the caller sent aside), and that
// nothing was written.
func assertCreateHiddenLikeUnknown(ctx context.Context, t *testing.T, rig *oauthRouterRig, flow *sdkFlow) {
	t.Helper()
	writes := map[string]bool{}
	for _, name := range writeToolNames() {
		writes[name] = true
	}
	for _, name := range toolNames(ctx, t, flow.session) {
		if writes[name] {
			t.Fatalf("tools/list names %s under a grant without mcp:write", name)
		}
	}
	init := flow.session.InitializeResult()
	if init == nil {
		t.Fatal("no initialize result")
	}
	for name := range writes {
		if strings.Contains(init.Instructions, name) {
			t.Fatalf("instructions name the write tool %s under a grant without mcp:write: %q", name, init.Instructions)
		}
	}
	if _, err := callCreate(ctx, flow.session, "must not start", "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"); err == nil {
		t.Fatal("calling the hidden write tool succeeded")
	}
	hiddenToken := flow.recorder.lastBearer()
	other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	full := mintBuildBearer(ctx, t, rig.pool, rig.cfg, other.ID)
	call := func(token, tool string) (int, string) {
		arguments, _ := json.Marshal(createArguments("must not start", "8b9c0d1e-2f3a-4b4c-9d5e-6f7a8b9c0d1e"))
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tool, arguments)
		status, _, raw := rig.postMCP(t, "tools/call", tool, body, token)
		return status, string(raw)
	}
	unknownStatus, unknown := call(full, "narvi_does_not_exist")
	for _, tool := range []string{"narvi_create_session", "narvi_approve_plan", "narvi_stop_session"} {
		hiddenStatus, hidden := call(hiddenToken, tool)
		if hiddenStatus != unknownStatus || hidden != strings.ReplaceAll(unknown, "narvi_does_not_exist", tool) {
			t.Fatalf("the hidden write tool %s answers differently from an unknown tool:\n hidden:  %d %s\n unknown: %d %s", tool, hiddenStatus, hidden, unknownStatus, unknown)
		}
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, flow.member.ID); n != 0 {
		t.Fatalf("a grant without mcp:write started %d session(s)", n)
	}
}

// sdkWriteToolsReadGrant is TestOAuth_ProductionRouter's
// WriteTools_ReadGrantSeesNoneAndCannotCall_SDKClient: the member unchecks
// mcp:write on the consent page, so the token holds mcp:read alone -- the
// reads are there, the write is not, and the paragraph still says
// read-only.
func sdkWriteToolsReadGrant(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return []string{"mcp:read"} }
	})
	// The consent page offered both, pre-checked -- owner decision O7: a
	// user who clicks Allow without changing anything grants write too --
	// and said what write means.
	page := flow.driver.lastPage()
	for _, want := range []string{`value="mcp:read" checked`, `value="mcp:write" checked`, "run code in your repositories and spend on models"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the consent page does not show %q", want)
		}
	}
	var tokenScopes []string
	if err := rig.pool.QueryRow(ctx, `SELECT t.scopes FROM mcp_oauth_access_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1`, flow.member.ID).Scan(&tokenScopes); err != nil || strings.Join(tokenScopes, " ") != "mcp:read" {
		t.Fatalf("access token scopes = %v (err %v), want [mcp:read]", tokenScopes, err)
	}
	want := []string{"narvi_get_session", "narvi_get_session_result", "narvi_get_session_status", "narvi_get_session_transcript", "narvi_list_models", "narvi_list_plans", "narvi_list_sessions", "narvi_wait_for_session"}
	if got := toolNames(ctx, t, flow.session); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ListTools under mcp:read = %v, want the reads alone %v", got, want)
	}
	if init := flow.session.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "READ-ONLY") {
		t.Fatalf("instructions under mcp:read do not say read-only: %+v", init)
	}
	assertCreateHiddenLikeUnknown(ctx, t, rig, flow)
}

// sdkWriteToolsScopeless is TestOAuth_ProductionRouter's
// WriteTools_ScopelessGrantSeesNone.
func sdkWriteToolsScopeless(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return nil }
	})
	if got := toolNames(ctx, t, flow.session); len(got) != 0 {
		t.Fatalf("ListTools under a scope-less token = %v, want none", got)
	}
	assertCreateHiddenLikeUnknown(ctx, t, rig, flow)
}

// sdkCreateSessionViewer is TestOAuth_ProductionRouter's
// CreateSession_ViewerRefusedLikeREST_SDKClient: a viewer may connect a
// client and approve mcp:write -- role does not gate discovery -- but the
// token never does more than its user: the create is refused with the
// very text REST's 403 gives the viewer's cookie, and nothing is written.
func sdkCreateSessionViewer(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClientAs(ctx, t, sqlcgen.UserRoleViewer, nil)
	var listed bool
	for _, name := range toolNames(ctx, t, flow.session) {
		listed = listed || name == "narvi_create_session"
	}
	if !listed {
		t.Fatal("a viewer's read+write grant does not list narvi_create_session: role must not gate discovery")
	}
	res, err := callCreate(ctx, flow.session, "a viewer's attempt", "9c0d1e2f-3a4b-4c5d-8e6f-7a8b9c0d1e2f")
	if err != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("a viewer's create: res %+v err %v, want isError", res, err)
	}
	var rest struct {
		Error string `json:"error"`
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", restCreateBody("a viewer's attempt", "ad0e1f2a-3b4c-4d5e-9f6a-7b8c9d0e1f2a"), &rest, flow.cookie); status != http.StatusForbidden {
		t.Fatalf("REST create by the viewer's cookie: status %d, want 403", status)
	}
	if text, _ := res.Content[0].(*sdkmcp.TextContent); text == nil || text.Text != rest.Error {
		t.Fatalf("MCP refusal %+v, REST's %q -- want the same text", res.Content[0], rest.Error)
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, flow.member.ID); n != 0 {
		t.Fatalf("a viewer's grant started %d session(s)", n)
	}
}

// mintScopedBearer is mintBuildBearer with the grant's and the token's
// scopes chosen, on a client of its own, returning the token, the grant's
// id and its client's client_id.
func mintScopedBearer(ctx context.Context, t *testing.T, rig *oauthRouterRig, userID pgtype.UUID, scopes []string) (token, grantID, clientID string) {
	t.Helper()
	client, err := narvipg.NewMCPOAuthClientStore(rig.pool).Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     fmt.Sprintf("narvi_mcp_c_brake_%d", time.Now().UnixNano()),
		Kind:         sqlcgen.McpOauthClientKindPreregistered,
		ClientName:   "Brake Test Client",
		RedirectUris: []string{"http://127.0.0.1/callback"},
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	token, grantID = mintScopedBearerOn(ctx, t, rig, userID, client.ClientID, scopes)
	return token, grantID, client.ClientID
}

// mintScopedBearerOn is mintScopedBearer on the existing client whose
// client_id is clientID -- another user of the same app -- returning the
// token and the grant's id. A user holds one grant per client.
func mintScopedBearerOn(ctx context.Context, t *testing.T, rig *oauthRouterRig, userID pgtype.UUID, clientID string, scopes []string) (token, grantID string) {
	t.Helper()
	ids, err := mcpauth.DeriveIdentifiers(rig.cfg.PublicBaseURL)
	if err != nil {
		t.Fatalf("DeriveIdentifiers: %v", err)
	}
	var clientRowID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `SELECT id FROM mcp_oauth_clients WHERE client_id = $1`, clientID).Scan(&clientRowID); err != nil {
		t.Fatalf("read client %s: %v", clientID, err)
	}
	grants := narvipg.NewMCPOAuthGrantStore(rig.pool)
	grant, err := grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID: userID, ClientID: clientRowID, Scopes: scopes, Resource: ids.Resource,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	raw, err := platform.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	token = "narvi_mcp_at_" + raw
	if _, err := grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID: grant.ID, TokenHash: platform.HashToken(token), Scopes: scopes,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token, grant.ID.String()
}

// rawCall POSTs one tools/call for tool with argumentsJSON under token.
func (r *oauthRouterRig) rawCall(t *testing.T, token, tool, argumentsJSON string) (int, http.Header, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tool, argumentsJSON)
	return r.postMCP(t, "tools/call", tool, body, token)
}

// brakeTestTimeouts keeps every shipped value but the /mcp call brake's
// refill, stretched to an hour: its burst is the shipped one, and no token
// refills while the test spends it, so the refusal is the burst's and the
// Retry-After the interval the router built the brake with.
func brakeTestTimeouts(to *platform.Timeouts) {
	to.MCPCallRateInterval = time.Hour
}

// rateLimitMCPPerGrant is TestOAuth_ProductionRouter's
// RateLimit_MCPPerGrant429: one grant's burst of calls reaches the tools;
// past it /mcp answers 429 with Retry-After and the rate-limited body,
// before any tool runs -- a create refused there starts nothing -- while
// another grant of the same user, and another user's grant of the same
// client (the same app), both from the same address, carry on: the bucket
// is the grant's, not the user's nor the app's. The refusal is logged at
// WARN with the grant and client, never the token, and audited nowhere.
func rateLimitMCPPerGrant(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	warnings := captureWarnings(t)
	member, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	colleague, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	both := []string{"mcp:read", "mcp:write"}
	tokenA, grantA, clientA := mintScopedBearer(ctx, t, rig, member.ID, both)
	tokenB, _, _ := mintScopedBearer(ctx, t, rig, member.ID, both)
	tokenSameApp, _ := mintScopedBearerOn(ctx, t, rig, colleague.ID, clientA, both)
	auditBefore := rig.countOf(ctx, t, `SELECT count(*) FROM audit_log`)

	burst := rig.cfg.Timeouts.MCPCallRateBurst
	if burst != platform.DefaultTimeouts().MCPCallRateBurst {
		t.Fatalf("the router's /mcp burst is %d, want the shipped %d", burst, platform.DefaultTimeouts().MCPCallRateBurst)
	}
	for i := range burst {
		if status, _, raw := rig.rawCall(t, tokenA, "narvi_list_models", `{}`); status != http.StatusOK {
			t.Fatalf("call %d of grant A's burst: status %d body %s", i+1, status, raw)
		}
	}
	arguments, _ := json.Marshal(createArguments("past the brake", "be1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b"))
	status, header, raw := rig.rawCall(t, tokenA, "narvi_create_session", string(arguments))
	retryAfter, _ := strconv.Atoi(header.Get("Retry-After"))
	if status != http.StatusTooManyRequests || string(raw) != `{"error":"rate limited"}` || retryAfter < 3500 || retryAfter > 3600 {
		t.Fatalf("grant A past its burst: status %d Retry-After %q body %s, want 429, about an hour, the rate-limited body", status, header.Get("Retry-After"), raw)
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, member.ID); n != 0 {
		t.Fatalf("a create refused by the /mcp brake started %d session(s)", n)
	}
	if status, _, raw := rig.rawCall(t, tokenB, "narvi_list_models", `{}`); status != http.StatusOK {
		t.Fatalf("grant B of the same user, same address: status %d body %s, want its own bucket", status, raw)
	}
	if status, _, raw := rig.rawCall(t, tokenSameApp, "narvi_list_models", `{}`); status != http.StatusOK {
		t.Fatalf("another user's grant of grant A's client, same address: status %d body %s, want its own bucket", status, raw)
	}
	lines := warnings.matching("mcpauth: rate limited", "grant_id", grantA)
	if len(lines) != 1 || lines[0]["client_id"] != clientA || lines[0]["path"] != "/mcp" {
		t.Fatalf("refusal log lines for grant A = %v, want one naming the path, the grant and the client", lines)
	}
	warnings.mu.Lock()
	logged := warnings.buf.String()
	warnings.mu.Unlock()
	if strings.Contains(logged, tokenA) || strings.Contains(logged, "past the brake") {
		t.Fatal("a refusal log line carries the bearer token or the call's arguments")
	}
	if after := rig.countOf(ctx, t, `SELECT count(*) FROM audit_log`); after != auditBefore {
		t.Fatalf("the refusals wrote %d audit row(s), want none", after-auditBefore)
	}
}

// rateLimitCreateSessionPerGrant is TestOAuth_ProductionRouter's
// RateLimit_CreateSessionPerGrant: one grant starts the shipped burst of
// sessions; the next start is refused with how long to wait, and writes no
// row -- and so is a retry of the last start under its own key, which
// counts as a call because the brake runs before the route can tell it is
// a retry (§43.8); another grant of the same user, and another user's grant
// of the same client, each still start one.
func rateLimitCreateSessionPerGrant(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	member, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	colleague, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	both := []string{"mcp:read", "mcp:write"}
	tokenA, _, clientA := mintScopedBearer(ctx, t, rig, member.ID, both)
	tokenB, _, _ := mintScopedBearer(ctx, t, rig, member.ID, both)
	tokenSameApp, _ := mintScopedBearerOn(ctx, t, rig, colleague.ID, clientA, both)
	burst := rig.cfg.Timeouts.MCPCreateSessionRateBurst
	if burst != platform.DefaultTimeouts().MCPCreateSessionRateBurst || rig.cfg.Timeouts.MCPCreateSessionRateInterval != time.Minute {
		t.Fatalf("the router's create brake is %d per %v, want the shipped values", burst, rig.cfg.Timeouts.MCPCreateSessionRateInterval)
	}
	create := func(token string, i int) (int, map[string]any) {
		t.Helper()
		arguments, _ := json.Marshal(createArguments(fmt.Sprintf("start %d", i), fmt.Sprintf("c0d1e2f3-a4b5-4c6d-8e7f-%012d", i)))
		status, _, raw := rig.rawCall(t, token, "narvi_create_session", string(arguments))
		var env struct {
			Result map[string]any `json:"result"`
		}
		if status == http.StatusOK {
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
		}
		return status, env.Result
	}
	for i := range burst {
		if status, result := create(tokenA, i); status != http.StatusOK || result["isError"] == true {
			t.Fatalf("start %d of grant A's burst: status %d result %v", i+1, status, result)
		}
	}
	// Past the burst: a new start, then a retry of the last one under its
	// own key.
	for _, i := range []int{burst, burst - 1} {
		status, result := create(tokenA, i)
		content, _ := result["content"].([]any)
		var text string
		if len(content) == 1 {
			text, _ = content[0].(map[string]any)["text"].(string)
		}
		var seconds int
		if status != http.StatusOK || result["isError"] != true || !strings.HasPrefix(text, "too many sessions started through this authorization; retry in ") {
			t.Fatalf("grant A past its burst, call %d: status %d result %v, want isError with the brake's refusal", i, status, result)
		}
		if _, err := fmt.Sscanf(strings.TrimPrefix(text, "too many sessions started through this authorization; retry in "), "%d s", &seconds); err != nil || seconds < 1 || seconds > 60 {
			t.Fatalf("refusal %q, want a wait of one to sixty seconds", text)
		}
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, member.ID); n != burst {
		t.Fatalf("grant A started %d sessions, want exactly the burst, %d", n, burst)
	}
	if status, result := create(tokenB, burst+1); status != http.StatusOK || result["isError"] == true {
		t.Fatalf("grant B of the same user: status %d result %v, want its own bucket", status, result)
	}
	if status, result := create(tokenSameApp, burst+2); status != http.StatusOK || result["isError"] == true {
		t.Fatalf("another user's grant of grant A's client: status %d result %v, want its own bucket", status, result)
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, colleague.ID); n != 1 {
		t.Fatalf("the colleague started %d sessions, want 1", n)
	}
}

// rateLimitMCPShippedRefill is TestOAuth_ProductionRouter's
// RateLimit_MCPPerGrantShippedRefill, on a router that keeps every shipped
// value, the /mcp refill included (RateLimit_MCPPerGrant429's router
// stretches it to an hour): calls sent back to back, faster than the
// one-a-second refill, are served until the burst and what refilled
// meanwhile are spent -- at least the burst, at most the burst plus one a
// second elapsed plus one -- then answered 429 with Retry-After 1.
func rateLimitMCPShippedRefill(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	shipped := platform.DefaultTimeouts()
	if got := rig.cfg.Timeouts; got.MCPCallRateBurst != shipped.MCPCallRateBurst || got.MCPCallRateInterval != shipped.MCPCallRateInterval || got.MCPCallRateInterval != time.Second {
		t.Fatalf("the router's /mcp brake is %d per %v, want the shipped %d per %v, a second", got.MCPCallRateBurst, got.MCPCallRateInterval, shipped.MCPCallRateBurst, shipped.MCPCallRateInterval)
	}
	member, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	token, _, _ := mintScopedBearer(ctx, t, rig, member.ID, []string{"mcp:read"})

	start := time.Now()
	served := 0
	for {
		status, header, raw := rig.rawCall(t, token, "narvi_list_models", `{}`)
		if status == http.StatusOK {
			served++
			if served > 20*shipped.MCPCallRateBurst {
				t.Fatalf("%d calls in %v and none refused: calls slower than the refill?", served, time.Since(start))
			}
			continue
		}
		elapsed := time.Since(start)
		if status != http.StatusTooManyRequests || header.Get("Retry-After") != "1" || string(raw) != `{"error":"rate limited"}` {
			t.Fatalf("after %d calls: status %d Retry-After %q body %s, want 429, 1, the rate-limited body", served, status, header.Get("Retry-After"), raw)
		}
		if most := shipped.MCPCallRateBurst + int(elapsed/shipped.MCPCallRateInterval) + 1; served < shipped.MCPCallRateBurst || served > most {
			t.Fatalf("%d calls served in %v before the refusal, want between the burst, %d, and %d", served, elapsed, shipped.MCPCallRateBurst, most)
		}
		t.Logf("%d calls served in %v before the refusal", served, elapsed)
		return
	}
}
