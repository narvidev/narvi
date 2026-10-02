//go:build integration

// This file proves what POST /api/sessions decides about a create beyond its
// body (technical plan §43.1/§43.8/§43.18): the source a session records
// (mcp exactly when an MCP grant is on the request context), a create
// replayed under the same idempotency key, the MCP stamp on an audit row,
// and the mcp label on the gates' refusals of a create over MCP. It is in
// package httpapi to reach createRequestSHA256, which the
// race below needs to plant a winning row the handler will recognise, and
// it drives the real handler directly with the context each principal
// arrives with -- the real cookie middleware for the cookie case.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// createSourceRepo is the repository every create here names; the rig
// makes it known to the deployment, as the entitlement gate requires.
const createSourceRepo = "acme/widgets"

// createSourceRig is the real POST /api/sessions handler over the shared
// pool, with the stores a test reads back through.
type createSourceRig struct {
	pool         *pgxpool.Pool
	sessions     *narvipg.SessionStore
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	userSessions *narvipg.UserSessionStore
	auditLog     *narvipg.AuditLogStore
	// handler is the route in the open rollout mode; handlerIn builds it,
	// over the same stores and registry, in another.
	handler   http.HandlerFunc
	handlerIn func(mode platform.RolloutMode) http.HandlerFunc
}

func newCreateSourceRig(t *testing.T) *createSourceRig {
	t.Helper()
	ctx := context.Background()
	pool, connStr := IntegrationTestPoolAndConnStr(t)
	// Every live session actor holds a connection until its registry shuts
	// down (sharedPoolMaxConns' own doc comment), and every create here
	// with a prompt spawns one, so the registry gets a pool of its own on
	// the same database; the handler's own transactions stay on the shared
	// one. Closed after the registry shuts down (cleanups run last first),
	// and both before the shared database is reset.
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
	// nil provider and commander, as httpapi_test's own rig: these tests
	// assert what the create writes, not what dispatch then does.
	registry, err := sessionactor.NewRegistry(ctx, actorPool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, createSourceRepo, 1); err != nil {
		t.Fatalf("make %s known: %v", createSourceRepo, err)
	}
	r := &createSourceRig{
		pool:         pool,
		sessions:     narvipg.NewSessionStore(pool),
		users:        narvipg.NewUserStore(pool),
		identities:   narvipg.NewIdentityStore(pool),
		userSessions: narvipg.NewUserSessionStore(pool),
		auditLog:     narvipg.NewAuditLogStore(pool),
	}
	turns, environments, repoSettings := narvipg.NewTurnStore(pool), narvipg.NewEnvironmentStore(pool), narvipg.NewRepoSettingsStore(pool)
	r.handlerIn = func(mode platform.RolloutMode) http.HandlerFunc {
		return CreateSession(pool, r.sessions, turns, environments, r.auditLog, registry, nil, false, mode, repoSettings, prSessions)
	}
	r.handler = r.handlerIn(platform.RolloutModeOpen)
	return r
}

// newUser creates a user of role with a sign-in identity and a live
// session cookie, returning the row, the principal a context carries for
// it, and the cookie's value.
func (r *createSourceRig) newUser(ctx context.Context, t *testing.T, role sqlcgen.UserRole) (sqlcgen.User, platform.AuthenticatedUser, string) {
	t.Helper()
	externalID := fmt.Sprintf("create-source-%s-%d", role, time.Now().UnixNano())
	email := externalID + "@example.com"
	user, err := r.users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: "Create Source", Role: role})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := r.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID: user.ID, TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	return user, platform.AuthenticatedUser{ID: user.ID.String(), Role: string(role), Email: email}, token
}

// principal is how one create request arrives: with a session cookie
// through the real cookie middleware, or with a context principal and,
// optionally, an MCP grant (what auth.RequireMCPBearer attaches).
type principal struct {
	cookie string
	user   *platform.AuthenticatedUser
	grant  *platform.MCPGrant
}

// post sends body to the handler as p and returns the recorded response.
func (r *createSourceRig) post(t *testing.T, p principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	return r.postTo(t, r.handler, p, body)
}

// postTo is post to handler.
func (r *createSourceRig) postTo(t *testing.T, handler http.HandlerFunc, p principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	ctx := req.Context()
	if p.user != nil {
		ctx = platform.WithUser(ctx, *p.user)
	}
	if p.grant != nil {
		ctx = platform.WithMCPGrant(ctx, *p.grant)
	}
	req = req.WithContext(ctx)
	var h http.Handler = handler
	if p.cookie != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: p.cookie})
		h = auth.Middleware(r.userSessions, r.users)(handler)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createBody is a create request naming createSourceRepo, with prompt and
// spawnSource as given, and idempotencyKey when key is not empty.
func createBody(spawnSource, prompt, key string) string {
	body := fmt.Sprintf(`{"spawnSource":%q,"title":null,"prompt":%q,"repos":[{"name":"widgets","url":"https://github.com/%s","branch":null}],"modelId":null,"effort":null,"planMode":false`, spawnSource, prompt, createSourceRepo)
	if key != "" {
		body += fmt.Sprintf(`,"idempotencyKey":%q`, key)
	}
	return body + "}"
}

// sessionFrom decodes a create's answer.
func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) restdtos.Session {
	t.Helper()
	var s restdtos.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode session from %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return s
}

// countRows runs a count query.
func (r *createSourceRig) countRows(ctx context.Context, t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// createAudit returns the session.create audit row's detail for sessionID.
func (r *createSourceRig) createAudit(ctx context.Context, t *testing.T, sessionID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := r.pool.QueryRow(ctx, `SELECT detail_json FROM audit_log WHERE action = 'session.create' AND resource_id = $1`, sessionID).Scan(&raw); err != nil {
		t.Fatalf("read session.create audit for %s: %v", sessionID, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode audit detail %s: %v", raw, err)
	}
	return detail
}

// TestCreateSession_RecordsMcpOnlyWithAGrant is §43.1's source rule: a
// create made under an MCP grant records mcp; a create by cookie, and one
// by a principal with no grant (standing in for a future bearer that
// attaches a user and nothing else), record web -- the source comes from
// the grant, never from the body nor from the kind of credential. A body
// whose spawnSource is not web is refused 400 either way, writing nothing.
// The audit row says the same source, and carries the grant only when
// there is one (§43.18).
func TestCreateSession_RecordsMcpOnlyWithAGrant(t *testing.T) {
	ctx := context.Background()
	rig := newCreateSourceRig(t)
	user, principalUser, cookie := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
	grant := platform.MCPGrant{GrantID: "4b0e5c7a-1f0e-4f1d-9d6c-2a3b4c5d6e7f", ClientID: "narvi_mcp_c_source_test", Scopes: []string{"mcp:read", "mcp:write"}}

	tests := []struct {
		name       string
		principal  principal
		body       string
		wantStatus int
		wantSource string
		wantStamp  bool
	}{
		{"an MCP grant records mcp", principal{user: &principalUser, grant: &grant}, createBody("web", "over mcp", ""), http.StatusCreated, "mcp", true},
		{"a cookie records web", principal{cookie: cookie}, createBody("web", "by cookie", ""), http.StatusCreated, "web", false},
		{"a user with no grant records web", principal{user: &principalUser}, createBody("web", "a bearer with no grant", ""), http.StatusCreated, "web", false},
		{"a non-web body under a grant is refused", principal{user: &principalUser, grant: &grant}, createBody("slack", "forged", ""), http.StatusBadRequest, "", false},
		{"a non-web body with no grant is refused", principal{user: &principalUser}, createBody("linear", "forged", ""), http.StatusBadRequest, "", false},
		{"a non-web body by cookie is refused", principal{cookie: cookie}, createBody("github", "forged", ""), http.StatusBadRequest, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, user.ID)
			rec := rig.post(t, tc.principal, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			if tc.wantStatus != http.StatusCreated {
				if after := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, user.ID); after != before {
					t.Fatalf("a refused create wrote %d session(s)", after-before)
				}
				return
			}
			got := sessionFrom(t, rec)
			if string(got.SpawnSource) != tc.wantSource {
				t.Errorf("answer spawnSource = %q, want %q", got.SpawnSource, tc.wantSource)
			}
			var stored string
			if err := rig.pool.QueryRow(ctx, `SELECT spawn_source::text FROM sessions WHERE id = $1`, got.Id).Scan(&stored); err != nil || stored != tc.wantSource {
				t.Errorf("stored spawn_source = %q (err %v), want %q", stored, err, tc.wantSource)
			}
			if got.CreatedBy == nil || *got.CreatedBy != user.ID.String() {
				t.Errorf("createdBy = %v, want the user", got.CreatedBy)
			}
			detail := rig.createAudit(ctx, t, got.Id)
			if detail["spawn_source"] != tc.wantSource {
				t.Errorf("audit spawn_source = %v, want %q", detail["spawn_source"], tc.wantSource)
			}
			stamp, stamped := detail["mcp"].(map[string]any)
			if stamped != tc.wantStamp {
				t.Fatalf("audit detail %v: stamped %v, want %v", detail, stamped, tc.wantStamp)
			}
			if stamped && (stamp["grant_id"] != grant.GrantID || stamp["client_id"] != grant.ClientID || len(stamp) != 2) {
				t.Errorf("audit detail.mcp = %v, want exactly the grant and client ids", stamp)
			}
		})
	}
}

// TestCreateSession_IdempotencyKey_Table is §43.8's replay, step by step on
// one user's key: the first create answers 201; the same request again --
// however its JSON is spelled, its defaults written out or left out, its
// key in capitals -- answers 200 with that session and writes no session,
// turn or audit row; the same key with a different request is refused 409;
// the same key and request over MCP is refused 409 too, since the session
// records web and a replay only answers the source that started it;
// another user's same key is independent; a key that is not a UUID in its
// 8-4-4-4-12 form is refused 400, even one naming the same 16 bytes; no key
// creates each time, as before; and a user who may no longer create is
// refused 403 before the key is even looked up.
func TestCreateSession_IdempotencyKey_Table(t *testing.T) {
	ctx := context.Background()
	rig := newCreateSourceRig(t)
	userA, principalA, _ := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
	userB, principalB, _ := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
	grantA := platform.MCPGrant{GrantID: "2d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a", ClientID: "narvi_mcp_c_idempotency", Scopes: []string{"mcp:read", "mcp:write"}}
	asA, asB, asAOverMCP := principal{user: &principalA}, principal{user: &principalB}, principal{user: &principalA, grant: &grantA}
	const key = "0f6c1d2e-3a4b-4c5d-8e9f-a0b1c2d3e4f5"
	const notUUID = "idempotencyKey: must be a UUID, as 8-4-4-4-12 hexadecimal digits"
	first := createBody("web", "fix the flaky test", key)
	// The same request with its keys in another order and extra spaces.
	respelled := fmt.Sprintf(`{ "planMode": false, "idempotencyKey": %q, "effort": null, "modelId": null, "repos": [ {"branch": null, "url": "https://github.com/%s", "name": "widgets"} ], "prompt": "fix the flaky test", "title": null, "spawnSource": "web" }`, key, createSourceRepo)
	// The same request with every optional field written out as the value
	// the route reads like its absence.
	spelledOut := strings.TrimSuffix(first, "}") + `,"docker":false,"pathScope":[],"mockConfig":null,"buildModelId":null,"buildEffort":null,"epistemicCheckEnabled":null,"egressPolicy":null}`

	var firstID string
	steps := []struct {
		name       string
		who        principal
		body       string
		wantStatus int
		// wantFirst: the answer is the first create's session.
		wantFirst bool
		wantError string
	}{
		{"the first create", asA, first, http.StatusCreated, false, ""},
		{"the same request again", asA, first, http.StatusOK, true, ""},
		{"the same request spelled differently", asA, respelled, http.StatusOK, true, ""},
		{"the same request with its defaults written out", asA, spelledOut, http.StatusOK, true, ""},
		{"the same request with the key in capitals", asA, createBody("web", "fix the flaky test", strings.ToUpper(key)), http.StatusOK, true, ""},
		{"the same key with a different request", asA, createBody("web", "a different prompt", key), http.StatusConflict, false, "idempotencyKey reused with a different request"},
		{"the same key and request over MCP", asAOverMCP, first, http.StatusConflict, false, "idempotencyKey already used for a session started another way"},
		{"another user's same key", asB, first, http.StatusCreated, false, ""},
		{"a key that is not a UUID", asA, createBody("web", "fix the flaky test", "not-a-uuid"), http.StatusBadRequest, false, notUUID},
		{"the key without its hyphens", asA, createBody("web", "fix the flaky test", strings.ReplaceAll(key, "-", "")), http.StatusBadRequest, false, notUUID},
		{"the key with other separators", asA, createBody("web", "fix the flaky test", strings.ReplaceAll(key, "-", "x")), http.StatusBadRequest, false, notUUID},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			rec := rig.post(t, step.who, step.body)
			if rec.Code != step.wantStatus {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), step.wantStatus)
			}
			if step.wantError != "" {
				var e struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error != step.wantError {
					t.Fatalf("body %s, want error %q", rec.Body.String(), step.wantError)
				}
				return
			}
			got := sessionFrom(t, rec)
			if step.name == "the first create" {
				firstID = got.Id
			}
			if (got.Id == firstID) != (step.wantFirst || step.name == "the first create") {
				t.Fatalf("answered session %s, first create's %s -- want same %v", got.Id, firstID, step.wantFirst)
			}
		})
	}
	// One session for A under the key, one for B, and the replays wrote
	// nothing: one audit row and one turn for A's session.
	if n := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, userA.ID); n != 1 {
		t.Fatalf("user A has %d sessions, want 1", n)
	}
	if n := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1 AND create_idempotency_key = $2`, userB.ID, key); n != 1 {
		t.Fatalf("user B has %d sessions under the key, want 1", n)
	}
	if n := rig.countRows(ctx, t, `SELECT count(*) FROM audit_log WHERE action = 'session.create' AND resource_id = $1`, firstID); n != 1 {
		t.Fatalf("session.create audit rows for the first session = %d, want 1", n)
	}
	if n := rig.countRows(ctx, t, `SELECT count(*) FROM turns WHERE session_id = $1`, firstID); n != 1 {
		t.Fatalf("turns for the first session = %d, want 1", n)
	}
	if n := rig.countRows(ctx, t, `SELECT count(*) FROM audit_log WHERE actor_user_id = $1 AND detail_json ? 'mcp'`, userA.ID); n != 0 {
		t.Fatalf("the refused replay over MCP wrote %d audit row(s)", n)
	}

	t.Run("no key creates each time", func(t *testing.T) {
		a := rig.post(t, principal{user: &principalA}, createBody("web", "no key", ""))
		b := rig.post(t, principal{user: &principalA}, createBody("web", "no key", ""))
		if a.Code != http.StatusCreated || b.Code != http.StatusCreated || sessionFrom(t, a).Id == sessionFrom(t, b).Id {
			t.Fatalf("two keyless creates: %d %d, want two new sessions", a.Code, b.Code)
		}
	})

	t.Run("authorization runs before the lookup", func(t *testing.T) {
		if _, err := rig.pool.Exec(ctx, `UPDATE users SET role = 'viewer' WHERE id = $1`, userA.ID); err != nil {
			t.Fatal(err)
		}
		viewer := principalA
		viewer.Role = string(sqlcgen.UserRoleViewer)
		if rec := rig.post(t, principal{user: &viewer}, first); rec.Code != http.StatusForbidden {
			t.Fatalf("a viewer replaying their own key: status %d body %s, want 403, not their session", rec.Code, rec.Body.String())
		}
	})
}

// TestCreateSession_IdempotencyKey_ConcurrentReplaysCreateOne: two creates
// with one key that both find no row both insert; the unique index makes
// the second wait for the first and fail with 23505, and the handler then
// reads the winner back -- one row whatever the timing. The first subtest
// forces that path: a transaction holding the winning insert open while
// the request's own insert waits behind it; committed, the request answers
// the winner (same hash and source) or 409 (a different hash, or a winner
// that records another source). The last fires many requests at once.
func TestCreateSession_IdempotencyKey_ConcurrentReplaysCreateOne(t *testing.T) {
	ctx := context.Background()
	rig := newCreateSourceRig(t)

	blockedOnTheIndex := func(t *testing.T, winnerSource sqlcgen.SessionSpawnSource, winnerPrompt string, wantStatus int, wantError string) {
		t.Helper()
		user, who, _ := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
		keyText := fmt.Sprintf("7d2f0a3b-%04x-4c1d-9e8f-0a1b2c3d4e5f", time.Now().UnixNano()&0xffff)
		var key pgtype.UUID
		if err := key.Scan(keyText); err != nil {
			t.Fatal(err)
		}
		// The winner, planted as the handler would write it: its request's
		// own hash, from the same decode the handler runs.
		var winnerReq restdtos.CreateSessionRequest
		if err := json.Unmarshal([]byte(createBody("web", winnerPrompt, keyText)), &winnerReq); err != nil {
			t.Fatal(err)
		}
		hash, err := createRequestSHA256(winnerReq)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := rig.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		winner, err := rig.sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{
			SpawnSource: winnerSource, CreatedBy: user.ID,
			Repos:                []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`),
			CreateIdempotencyKey: key, CreateRequestSha256: hash,
		})
		if err != nil {
			t.Fatalf("plant the winner: %v", err)
		}

		var rec *httptest.ResponseRecorder
		var g errgroup.Group
		g.Go(func() error {
			rec = rig.post(t, principal{user: &who}, createBody("web", "fix it", keyText))
			return nil
		})
		// Wait until the request's own session insert is waiting on the
		// winner's index entry.
		deadline := time.Now().Add(20 * time.Second)
		for {
			n := rig.countRows(ctx, t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO sessions%'`)
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the request's insert never waited on the winner")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the winner: %v", err)
		}
		_ = g.Wait()

		if rec.Code != wantStatus {
			t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), wantStatus)
		}
		if wantError != "" && !strings.Contains(rec.Body.String(), wantError) {
			t.Fatalf("body %s, want error %q", rec.Body.String(), wantError)
		}
		if wantStatus == http.StatusOK {
			if got := sessionFrom(t, rec); got.Id != winner.ID.String() {
				t.Fatalf("answered %s, want the winner %s", got.Id, winner.ID.String())
			}
		}
		if n := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, user.ID); n != 1 {
			t.Fatalf("user has %d sessions, want the winner alone", n)
		}
	}
	t.Run("blocked on the index, the same request reads the winner", func(t *testing.T) {
		blockedOnTheIndex(t, sqlcgen.SessionSpawnSourceWeb, "fix it", http.StatusOK, "")
	})
	t.Run("blocked on the index, a different request is refused", func(t *testing.T) {
		blockedOnTheIndex(t, sqlcgen.SessionSpawnSourceWeb, "something else", http.StatusConflict, "idempotencyKey reused with a different request")
	})
	t.Run("blocked on the index, a winner started over MCP is refused", func(t *testing.T) {
		blockedOnTheIndex(t, sqlcgen.SessionSpawnSourceMcp, "fix it", http.StatusConflict, "idempotencyKey already used for a session started another way")
	})

	t.Run("many at once create one", func(t *testing.T) {
		user, who, _ := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
		const key = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
		const n = 8
		var mu sync.Mutex
		ids := map[string]int{}
		statuses := map[int]int{}
		var g errgroup.Group
		for range n {
			g.Go(func() error {
				rec := rig.post(t, principal{user: &who}, createBody("web", "at once", key))
				mu.Lock()
				defer mu.Unlock()
				statuses[rec.Code]++
				if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
					var s restdtos.Session
					if json.Unmarshal(rec.Body.Bytes(), &s) == nil {
						ids[s.Id]++
					}
				}
				return nil
			})
		}
		_ = g.Wait()
		if statuses[http.StatusCreated] != 1 || statuses[http.StatusOK] != n-1 || len(ids) != 1 {
			t.Fatalf("statuses %v, sessions answered %v: want one 201, the rest 200, all one session", statuses, ids)
		}
		if got := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, user.ID); got != 1 {
			t.Fatalf("user has %d sessions, want 1", got)
		}
	})
}

// TestAuditRecord_StampsMCPGrant is §43.18's audit stamp, through the one
// audit writer against a real audit_log row: a change made under an MCP
// grant carries detail.mcp = {grant_id, client_id} beside the caller's own
// detail, a change without one carries no mcp key, and the caller's map is
// never modified.
func TestAuditRecord_StampsMCPGrant(t *testing.T) {
	ctx := context.Background()
	rig := newCreateSourceRig(t)
	user, _, _ := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
	grant := platform.MCPGrant{GrantID: "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", ClientID: "narvi_mcp_c_audit", Scopes: []string{"mcp:write"}}

	detail := map[string]any{"spawn_source": "mcp", "note": "kept"}
	original := maps.Clone(detail)
	record := func(ctx context.Context, resourceID string) map[string]any {
		t.Helper()
		if err := recordAuditLog(ctx, rig.auditLog, user.ID, "test.stamp", "test", resourceID, detail); err != nil {
			t.Fatalf("record: %v", err)
		}
		var raw []byte
		if err := rig.pool.QueryRow(ctx, `SELECT detail_json FROM audit_log WHERE action = 'test.stamp' AND resource_id = $1`, resourceID).Scan(&raw); err != nil {
			t.Fatalf("read row: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	stamped := record(platform.WithMCPGrant(ctx, grant), fmt.Sprintf("stamped-%d", time.Now().UnixNano()))
	mcp, ok := stamped["mcp"].(map[string]any)
	if !ok || mcp["grant_id"] != grant.GrantID || mcp["client_id"] != grant.ClientID || len(mcp) != 2 {
		t.Fatalf("detail under a grant = %v, want mcp = {grant_id, client_id}", stamped)
	}
	if stamped["spawn_source"] != "mcp" || stamped["note"] != "kept" || len(stamped) != 3 {
		t.Fatalf("detail under a grant = %v, want the caller's keys beside the stamp", stamped)
	}
	if !maps.Equal(detail, original) {
		t.Fatalf("the caller's map was modified: %v, was %v", detail, original)
	}

	plain := record(ctx, fmt.Sprintf("plain-%d", time.Now().UnixNano()))
	if _, ok := plain["mcp"]; ok || len(plain) != 2 {
		t.Fatalf("detail without a grant = %v, want the caller's keys alone", plain)
	}
}

// gateMetrics is a reader on the global OTel meter provider, set once per
// test process. The gates' counters are built from the global provider
// (sync.OnceValue in rolloutgate.go and repoentitlementgate.go), which
// hands every instrument made before a provider is set over to the first
// one set, so this reader sees them whichever test used them first.
var gateMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
})

// countsBySource is the counter name's value per spawn_source label, as
// the meter named scope reports it.
func countsBySource(ctx context.Context, t *testing.T, scope, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := gateMetrics().Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is a %T, want an int64 sum", name, m.Data)
			}
			for _, point := range sum.DataPoints {
				source, _ := point.Attributes.Value("spawn_source")
				counts[source.AsString()] += point.Value
			}
		}
	}
	return counts
}

// lockedLog is a JSON log sink safe to write from any goroutine.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// entries is every line logged with msg.
func (l *lockedLog) entries(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			found = append(found, entry)
		}
	}
	return found
}

// TestCreateSession_McpRefusalsCarryTheMcpLabel is §43.1's label on the
// gates' refusals: a create over MCP refused by the entitlement gate (a
// repository this deployment does not know) or by the rollout gate (a
// known repository not enrolled, in cohort mode) is logged, counted and --
// for the entitlement gate, which audits its denials -- audited as mcp, the
// source the session would have recorded, never as the web the body says;
// the denial row also carries the grant (§43.18). It is written through the
// pool before the create's transaction opens, so the stamp comes from the
// request's context alone. Not parallel: it swaps the default logger.
func TestCreateSession_McpRefusalsCarryTheMcpLabel(t *testing.T) {
	ctx := context.Background()
	rig := newCreateSourceRig(t)
	user, principalUser, _ := rig.newUser(ctx, t, sqlcgen.UserRoleMember)
	grant := platform.MCPGrant{GrantID: "3e4f5a6b-7c8d-4e9f-8a0b-1c2d3e4f5a6b", ClientID: "narvi_mcp_c_refusals", Scopes: []string{"mcp:read", "mcp:write"}}
	overMCP := principal{user: &principalUser, grant: &grant}

	logs := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// refused posts body over MCP to handler and checks the refusal: a 403
	// with message, no session, the log line msg labelled mcp alone, and
	// the counter name of meter scope one higher under mcp and nowhere
	// else.
	refused := func(t *testing.T, handler http.HandlerFunc, body, message, msg, scope, name string) {
		t.Helper()
		before := countsBySource(ctx, t, scope, name)
		linesBefore := len(logs.entries(t, msg))
		rec := rig.postTo(t, handler, overMCP, body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), message) {
			t.Fatalf("status %d body %s, want 403 %q", rec.Code, rec.Body.String(), message)
		}
		if n := rig.countRows(ctx, t, `SELECT count(*) FROM sessions WHERE created_by = $1`, user.ID); n != 0 {
			t.Fatalf("a refused create wrote %d session(s)", n)
		}
		after := countsBySource(ctx, t, scope, name)
		sources := map[string]bool{"mcp": true}
		for source := range before {
			sources[source] = true
		}
		for source := range after {
			sources[source] = true
		}
		for source := range sources {
			want := before[source]
			if source == "mcp" {
				want++
			}
			if after[source] != want {
				t.Errorf("%s{spawn_source=%s} = %d, was %d: want the refusal counted once, under mcp alone", name, source, after[source], before[source])
			}
		}
		lines := logs.entries(t, msg)
		if len(lines) != linesBefore+1 || lines[len(lines)-1]["spawn_source"] != "mcp" {
			t.Fatalf("log lines %q = %v, want one more, labelled mcp", msg, lines)
		}
	}

	t.Run("the entitlement gate", func(t *testing.T) {
		const unknown = "unknown-org/probe-repo"
		body := strings.ReplaceAll(createBody("web", "not entitled", ""), createSourceRepo, unknown)
		refused(t, rig.handler, body, "repository not entitled: "+unknown,
			"httpapi: repo entitlement gate: session creation refused, repo not entitled",
			repoEntitlementGateMeterName, "session_repo_entitlement_denied_total")

		var raw []byte
		if err := rig.pool.QueryRow(ctx, `SELECT detail_json FROM audit_log WHERE action = 'session.repo_entitlement_denied' AND actor_user_id = $1 AND resource_id = $2`, user.ID, unknown).Scan(&raw); err != nil {
			t.Fatalf("read the denial audit row: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatal(err)
		}
		stamp, _ := detail["mcp"].(map[string]any)
		if detail["spawn_source"] != "mcp" || detail["reason"] != "unknown" || stamp["grant_id"] != grant.GrantID || stamp["client_id"] != grant.ClientID || len(stamp) != 2 || len(detail) != 3 {
			t.Fatalf("denial audit detail = %s, want spawn_source mcp, reason unknown (§31.4) and exactly the grant and client", raw)
		}
	})

	t.Run("the rollout gate", func(t *testing.T) {
		refused(t, rig.handlerIn(platform.RolloutModeCohort), createBody("web", "not enrolled", ""), "repository not enrolled: "+createSourceRepo,
			"httpapi: rollout gate: session creation refused, repo not enrolled",
			rolloutGateMeterName, "session_rollout_refused_total")
	})
}
