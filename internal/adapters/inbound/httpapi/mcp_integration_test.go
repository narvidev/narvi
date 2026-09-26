//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// mcpAuditRows returns (action, detail) of every audit row about one
// resource, oldest first.
func mcpAuditRows(ctx context.Context, t *testing.T, r testRig, resourceType, resourceID string) []struct {
	action string
	detail map[string]any
} {
	t.Helper()
	rows, err := r.pool.Query(ctx, `SELECT action, detail_json FROM audit_log WHERE resource_type = $1 AND resource_id = $2 ORDER BY created_at, id`, resourceType, resourceID)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	defer rows.Close()
	var out []struct {
		action string
		detail map[string]any
	}
	for rows.Next() {
		var action string
		var raw []byte
		if err := rows.Scan(&action, &raw); err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, struct {
			action string
			detail map[string]any
		}{action, detail})
	}
	return out
}

// createMCPClientViaAPI registers a client as admin and returns it.
func createMCPClientViaAPI(t *testing.T, r testRig, adminCookie string, body string) restdtos.MCPClient {
	t.Helper()
	var got restdtos.MCPClient
	if status := r.doJSON(t, http.MethodPost, "/api/mcp-clients", []byte(body), &got, adminCookie); status != http.StatusCreated {
		t.Fatalf("POST /api/mcp-clients: status %d, want 201", status)
	}
	return got
}

// grantWithToken stores a grant for userID under clientID plus one live
// access token, returning the grant id and the token's hash.
func grantWithToken(ctx context.Context, t *testing.T, r testRig, userID, clientID pgtype.UUID) (pgtype.UUID, string) {
	t.Helper()
	g, err := r.mcpGrants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID: userID, ClientID: clientID, Scopes: []string{"mcp:read"}, Resource: "http://localhost:8080/mcp",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	hash := platform.HashToken("narvi_mcp_at_" + g.ID.String())
	if _, err := r.mcpGrants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID: g.ID, TokenHash: hash, Scopes: []string{"mcp:read"}, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return g.ID, hash
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestMCPClients_AdminOnly: registering, listing and deleting MCP clients
// is authz.ActionManageIntegrations -- admin only; every other role is
// refused 403 and nothing is written.
func TestMCPClients_AdminOnly(t *testing.T) {
	ctx := context.Background()
	r := newTestRig(t)
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer} {
		_, cookie := createUserWithRole(ctx, t, r, role)
		if s := r.doJSON(t, http.MethodGet, "/api/mcp-clients", nil, nil, cookie); s != http.StatusForbidden {
			t.Errorf("%s GET: status %d, want 403", role, s)
		}
		if s := r.doJSON(t, http.MethodPost, "/api/mcp-clients", []byte(`{"clientName":"X","redirectUris":["https://x.example/cb"]}`), nil, cookie); s != http.StatusForbidden {
			t.Errorf("%s POST: status %d, want 403", role, s)
		}
		if s := r.doJSON(t, http.MethodDelete, "/api/mcp-clients/00000000-0000-0000-0000-000000000000", nil, nil, cookie); s != http.StatusForbidden {
			t.Errorf("%s DELETE: status %d, want 403", role, s)
		}
	}
	clients, err := r.mcpClients.List(ctx)
	if err != nil || len(clients) != 0 {
		t.Fatalf("clients = %v (err %v), want none written", clients, err)
	}
	if s := r.doJSON(t, http.MethodGet, "/api/mcp-clients", nil, nil, ""); s != http.StatusUnauthorized {
		t.Errorf("signed out GET: status %d, want 401", s)
	}
}

// TestMCPClients_CreateValidatesAndAudits: the redirect-URI rule the
// authorization endpoint matches against is enforced at registration
// (so no unusable URI is ever stored), the generated client_id is public
// and prefixed, and the creation is audited.
func TestMCPClients_CreateValidatesAndAudits(t *testing.T) {
	ctx := context.Background()
	r := newTestRig(t)
	_, admin := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)

	for name, body := range map[string]string{
		"http off loopback":     `{"clientName":"X","redirectUris":["http://client.example/cb"]}`,
		"custom scheme":         `{"clientName":"X","redirectUris":["myapp://cb"]}`,
		"fragment":              `{"clientName":"X","redirectUris":["https://x.example/cb#f"]}`,
		"no redirect uris":      `{"clientName":"X","redirectUris":[]}`,
		"null redirect uris":    `{"clientName":"X","redirectUris":null}`,
		"missing redirect uris": `{"clientName":"X"}`,
		"eleven redirect uris":  `{"clientName":"X","redirectUris":["https://a.example/1","https://a.example/2","https://a.example/3","https://a.example/4","https://a.example/5","https://a.example/6","https://a.example/7","https://a.example/8","https://a.example/9","https://a.example/10","https://a.example/11"]}`,
		"blank name":            `{"clientName":"   ","redirectUris":["https://x.example/cb"]}`,
		"control char in name":  `{"clientName":"a\u0007b","redirectUris":["https://x.example/cb"]}`,
		"bidi override in name": `{"clientName":"a\u202eb","redirectUris":["https://x.example/cb"]}`,
		"http client uri":       `{"clientName":"X","redirectUris":["https://x.example/cb"],"clientUri":"http://x.example"}`,
		"not json":              `{`,
	} {
		if s := r.doJSON(t, http.MethodPost, "/api/mcp-clients", []byte(body), nil, admin); s != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, s)
		}
	}

	got := createMCPClientViaAPI(t, r, admin, `{"clientName":"  Editor Plugin  ","redirectUris":["http://127.0.0.1/callback","https://x.example/cb","http://127.0.0.1/callback"],"clientUri":"https://x.example"}`)
	if !strings.HasPrefix(got.ClientId, "narvi_mcp_c_") || got.ClientName != "Editor Plugin" || got.Kind != restdtos.MCPClientKindPreregistered ||
		len(got.RedirectUris) != 2 || got.ClientUri == nil || *got.ClientUri != "https://x.example" || got.DisabledAt != nil {
		t.Fatalf("created client = %+v", got)
	}
	rows := mcpAuditRows(ctx, t, r, "mcp_client", got.Id)
	if len(rows) != 1 || rows[0].action != "mcp_client.created" || rows[0].detail["client_id"] != got.ClientId {
		t.Fatalf("audit rows = %+v, want one mcp_client.created", rows)
	}

	var list restdtos.ListMCPClientsResponse
	if s := r.doJSON(t, http.MethodGet, "/api/mcp-clients", nil, &list, admin); s != http.StatusOK || len(list.Clients) != 1 || list.Clients[0].Id != got.Id {
		t.Fatalf("list: status %d body %+v", s, list)
	}
}

// TestClient_DeleteCascadesGrants: deleting a client removes every
// authorization issued to it -- the token stops resolving -- and audits the
// deletion plus one revocation per authorization it took with it.
func TestClient_DeleteCascadesGrants(t *testing.T) {
	ctx := context.Background()
	r := newTestRig(t)
	_, admin := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)
	member, _ := createUserWithRole(ctx, t, r, sqlcgen.UserRoleMember)
	client := createMCPClientViaAPI(t, r, admin, `{"clientName":"Doomed","redirectUris":["http://127.0.0.1/cb"]}`)
	grantID, tokenHash := grantWithToken(ctx, t, r, member.ID, mustUUID(t, client.Id))

	if s := r.doJSON(t, http.MethodDelete, "/api/mcp-clients/not-a-uuid", nil, nil, admin); s != http.StatusBadRequest {
		t.Fatalf("malformed id: status %d, want 400", s)
	}
	if s := r.doJSON(t, http.MethodDelete, "/api/mcp-clients/"+client.Id, nil, nil, admin); s != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", s)
	}
	if _, err := r.mcpGrants.LookupAccessToken(ctx, tokenHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("token after client delete: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := r.mcpGrants.GetGrant(ctx, grantID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("grant after client delete: err = %v, want pgx.ErrNoRows", err)
	}
	clientRows := mcpAuditRows(ctx, t, r, "mcp_client", client.Id)
	if last := clientRows[len(clientRows)-1]; last.action != "mcp_client.deleted" || last.detail["revoked_authorizations"] != float64(1) {
		t.Fatalf("client audit rows = %+v, want a final mcp_client.deleted counting 1 revocation", clientRows)
	}
	grantRows := mcpAuditRows(ctx, t, r, "mcp_authorization", grantID.String())
	if len(grantRows) != 1 || grantRows[0].action != "mcp_authorization.revoked" || grantRows[0].detail["reason"] != "client_deleted" || grantRows[0].detail["target_user_id"] != member.ID.String() {
		t.Fatalf("grant audit rows = %+v, want one revocation with reason client_deleted", grantRows)
	}
	if s := r.doJSON(t, http.MethodDelete, "/api/mcp-clients/"+client.Id, nil, nil, admin); s != http.StatusNotFound {
		t.Fatalf("second delete: status %d, want 404", s)
	}
}

// TestMCPClients_DisableEnable: disabling and enabling a client is
// authz.ActionManageIntegrations -- admin only, every other role refused
// 403 and nothing changed. Disabling sets disabled_at -- which the bearer
// lookup reads on every call -- deletes nothing (the authorization and its
// token stay), answers the client as it now stands and is audited once;
// enabling clears it. Asking for the state a client is already in is 409
// and writes no audit row; a client that does not exist is 404.
func TestMCPClients_DisableEnable(t *testing.T) {
	ctx := context.Background()
	r := newTestRig(t)
	_, admin := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)
	member, _ := createUserWithRole(ctx, t, r, sqlcgen.UserRoleMember)
	client := createMCPClientViaAPI(t, r, admin, `{"clientName":"Paused Plugin","redirectUris":["http://127.0.0.1/cb"]}`)
	grantID, tokenHash := grantWithToken(ctx, t, r, member.ID, mustUUID(t, client.Id))
	clientDisabled := func() bool {
		t.Helper()
		p, err := r.mcpGrants.LookupAccessToken(ctx, tokenHash)
		if err != nil {
			t.Fatalf("token lookup: %v -- disabling must delete nothing", err)
		}
		return p.ClientDisabled
	}
	disable, enable := "/api/mcp-clients/"+client.Id+"/disable", "/api/mcp-clients/"+client.Id+"/enable"

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer} {
		_, cookie := createUserWithRole(ctx, t, r, role)
		for _, path := range []string{disable, enable} {
			if s := r.doJSON(t, http.MethodPost, path, nil, nil, cookie); s != http.StatusForbidden {
				t.Errorf("%s POST %s: status %d, want 403", role, path, s)
			}
		}
	}
	if s := r.doJSON(t, http.MethodPost, disable, nil, nil, ""); s != http.StatusUnauthorized {
		t.Errorf("signed out: status %d, want 401", s)
	}
	if clientDisabled() {
		t.Fatal("a refused request disabled the client")
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/mcp-clients/not-a-uuid/disable", http.StatusBadRequest},
		{"/api/mcp-clients/00000000-0000-0000-0000-000000000000/disable", http.StatusNotFound},
		{"/api/mcp-clients/00000000-0000-0000-0000-000000000000/enable", http.StatusNotFound},
		{enable, http.StatusConflict},
	} {
		if s := r.doJSON(t, http.MethodPost, tc.path, nil, nil, admin); s != tc.want {
			t.Errorf("POST %s: status %d, want %d", tc.path, s, tc.want)
		}
	}

	var got restdtos.MCPClient
	if s := r.doJSON(t, http.MethodPost, disable, nil, &got, admin); s != http.StatusOK || got.Id != client.Id || got.DisabledAt == nil {
		t.Fatalf("disable: status %d client %+v, want 200 and disabledAt set", s, got)
	}
	if !clientDisabled() {
		t.Fatal("after disable, the bearer lookup does not see the client disabled")
	}
	if _, err := r.mcpGrants.GetGrant(ctx, grantID); err != nil {
		t.Fatalf("grant after disable: %v, want it kept", err)
	}
	if s := r.doJSON(t, http.MethodPost, disable, nil, nil, admin); s != http.StatusConflict {
		t.Fatalf("second disable: status %d, want 409", s)
	}
	var list restdtos.ListMCPClientsResponse
	if s := r.doJSON(t, http.MethodGet, "/api/mcp-clients", nil, &list, admin); s != http.StatusOK || len(list.Clients) != 1 || list.Clients[0].DisabledAt == nil {
		t.Fatalf("list after disable: status %d body %+v, want the client listed disabled", s, list)
	}

	if s := r.doJSON(t, http.MethodPost, enable, nil, &got, admin); s != http.StatusOK || got.DisabledAt != nil {
		t.Fatalf("enable: status %d client %+v, want 200 and disabledAt null", s, got)
	}
	if clientDisabled() {
		t.Fatal("after enable, the bearer lookup still sees the client disabled")
	}
	if s := r.doJSON(t, http.MethodPost, enable, nil, nil, admin); s != http.StatusConflict {
		t.Fatalf("second enable: status %d, want 409", s)
	}

	rows := mcpAuditRows(ctx, t, r, "mcp_client", client.Id)
	var actions []string
	for _, row := range rows {
		actions = append(actions, row.action)
	}
	if strings.Join(actions, ",") != "mcp_client.created,mcp_client.disabled,mcp_client.enabled" {
		t.Fatalf("client audit rows = %v, want created, disabled, enabled -- one per change, none for a refused or repeated request", actions)
	}
	for _, row := range rows[1:] {
		if row.detail["client_id"] != client.ClientId || row.detail["kind"] != "preregistered" {
			t.Fatalf("audit row %s detail = %v, want the client_id and kind", row.action, row.detail)
		}
	}
}

// lockWaitObservationTimeout bounds how long a test waits to SEE a backend
// queued on a lock. It is a failure deadline, not a pacing delay: the poll
// returns on the first observation, normally within a few round trips.
const lockWaitObservationTimeout = 10 * time.Second

// pauseBeforeQuery is a pgx.QueryTracer that stops the first query whose
// SQL contains marker, before it is sent, until resume is closed -- after
// reporting on paused the backend pid it is about to run on. It lets a
// test hold a real handler at an exact point of its transaction.
type pauseBeforeQuery struct {
	marker string
	paused chan uint32
	resume chan struct{}
	once   sync.Once
}

func newPauseBeforeQuery(marker string) *pauseBeforeQuery {
	return &pauseBeforeQuery{marker: marker, paused: make(chan uint32, 1), resume: make(chan struct{})}
}

func (p *pauseBeforeQuery) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, p.marker) {
		p.once.Do(func() {
			p.paused <- conn.PgConn().PID()
			<-p.resume
		})
	}
	return ctx
}

func (p *pauseBeforeQuery) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// waitUntilBlockedBy returns the pid of a backend of this database queued
// behind blocker -- polling back to back, never sleeping -- or 0 if done
// closes first (whatever should have queued finished without waiting).
func waitUntilBlockedBy(ctx context.Context, t *testing.T, pool *pgxpool.Pool, blocker int32, done <-chan struct{}) int32 {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, lockWaitObservationTimeout)
	defer cancel()
	for {
		select {
		case <-done:
			return 0
		default:
		}
		var waiters []int32
		if err := pool.QueryRow(waitCtx, `SELECT coalesce(array_agg(pid), '{}') FROM pg_stat_activity WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, blocker).Scan(&waiters); err != nil {
			t.Fatalf("poll for a backend queued behind pid %d: %v", blocker, err)
		}
		if len(waiters) > 0 {
			return waiters[0]
		}
	}
}

// TestClient_DeleteAuditsGrantAddedByConcurrentConsent: a consent adding a
// grant under a client while an admin deletes that client must never end
// with its grant cascaded away unaudited (technical plan §43.15/§43.18:
// one mcp_authorization.revoked per authorization the deletion took with
// it). The deletion locks the client FOR UPDATE before listing the grants
// it audits and holds that lock until it commits, so:
//
//   - a consent whose grant insert is already in flight (holding the
//     client FOR KEY SHARE through its foreign-key check) is waited out,
//     and its grant is listed, audited and counted;
//   - a consent whose grant insert starts AFTER the deletion took its lock
//     waits for the deletion to commit and then fails its foreign key --
//     which only a lock held for the deletion's whole transaction, not
//     one released at the end of its own statement, guarantees.
//
// The deletion runs the real handler on a pool of its own, whose tracer
// holds it right after the lock, just before it lists the grants.
func TestClient_DeleteAuditsGrantAddedByConcurrentConsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// consentFirst: the consent's grant insert is in flight before the
		// deletion locks the client; otherwise it starts once the deletion
		// holds the lock.
		consentFirst bool
	}{
		{"ConsentInsertInFlightBeforeTheLock", true},
		{"ConsentInsertStartsAfterTheLock", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newTestRig(t)
			_, admin := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)
			existing, _ := createUserWithRole(ctx, t, r, sqlcgen.UserRoleMember)
			consenting, _ := createUserWithRole(ctx, t, r, sqlcgen.UserRoleMember)
			client := createMCPClientViaAPI(t, r, admin, `{"clientName":"Contended","redirectUris":["http://127.0.0.1/cb"]}`)
			clientID := mustUUID(t, client.Id)
			existingGrant, _ := grantWithToken(ctx, t, r, existing.ID, clientID)

			pause := newPauseBeforeQuery("-- name: ListMCPOAuthGrantsForClient ")
			_, connStr := httpapi.IntegrationTestPoolAndConnStr(t)
			cfg, err := pgxpool.ParseConfig(connStr)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.Tracer = pause
			traced, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(traced.Close)
			router := chi.NewRouter()
			router.Route("/api/mcp-clients", func(rt chi.Router) {
				rt.Use(auth.Middleware(r.userSessions, r.users))
				rt.Delete("/{clientID}", httpapi.DeleteMCPClient(traced, narvipg.NewMCPOAuthClientStore(traced), narvipg.NewMCPOAuthGrantStore(traced), narvipg.NewAuditLogStore(traced)))
			})

			// The consent decision's own grant write, in a transaction the
			// test commits or rolls back.
			consent, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var consentPID int32
			if err := consent.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&consentPID); err != nil {
				t.Fatal(err)
			}
			addGrant := func() (sqlcgen.McpOauthGrant, error) {
				return r.mcpGrants.WithTx(consent).UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
					UserID: consenting.ID, ClientID: clientID, Scopes: []string{"mcp:read"}, Resource: "http://localhost:8080/mcp",
					ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
				})
			}

			var eg errgroup.Group
			resume := sync.OnceFunc(func() { close(pause.resume) })
			defer func() {
				resume()
				_ = consent.Rollback(ctx)
				_ = eg.Wait()
			}()
			var status int
			deleteDone := make(chan struct{})
			startDelete := func() {
				eg.Go(func() error {
					defer close(deleteDone)
					req := httptest.NewRequest(http.MethodDelete, "/api/mcp-clients/"+client.Id, nil)
					req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: admin})
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					status = rec.Code
					return nil
				})
			}
			// heldAfterLock returns once the deletion holds the client and
			// is stopped before its grant list.
			heldAfterLock := func() int32 {
				select {
				case pid := <-pause.paused:
					return int32(pid)
				case <-deleteDone:
					t.Fatalf("the deletion finished without reaching its grant list (status %d)", status)
					return 0
				}
			}

			var added sqlcgen.McpOauthGrant
			var addErr error
			if tc.consentFirst {
				if added, addErr = addGrant(); addErr != nil {
					t.Fatalf("consent's grant: %v", addErr)
				}
				startDelete()
				if waitUntilBlockedBy(ctx, t, r.pool, consentPID, deleteDone) == 0 {
					t.Fatalf("the deletion never waited on the consent's lock")
				}
				if err := consent.Commit(ctx); err != nil {
					t.Fatalf("commit consent: %v", err)
				}
				heldAfterLock()
				resume()
			} else {
				startDelete()
				deletionPID := heldAfterLock()
				addDone := make(chan struct{})
				eg.Go(func() error {
					defer close(addDone)
					added, addErr = addGrant()
					return nil
				})
				if waiter := waitUntilBlockedBy(ctx, t, r.pool, deletionPID, addDone); waiter != consentPID {
					t.Errorf("the consent's grant insert, started after the deletion locked the client, did not wait for the deletion's transaction (queued: pid %d, want %d): the lock does not last until the deletion commits", waiter, consentPID)
				}
				resume()
				<-addDone
				var pgErr *pgconn.PgError
				if !errors.As(addErr, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != "mcp_oauth_grants_client_id_fkey" {
					t.Errorf("consent's grant insert after the lock: err = %v, want the client foreign-key violation (23503) once the deletion committed", addErr)
				}
				if addErr == nil {
					// The insert went through: commit it as a real consent
					// would, once the deletion has listed its grants and is
					// queued behind this consent at its DELETE -- the
					// cascade then takes a grant the list never saw.
					waitUntilBlockedBy(ctx, t, r.pool, consentPID, deleteDone)
					if err := consent.Commit(ctx); err != nil {
						t.Fatalf("commit consent: %v", err)
					}
				} else {
					_ = consent.Rollback(ctx)
				}
			}
			if err := eg.Wait(); err != nil {
				t.Fatal(err)
			}
			if status != http.StatusNoContent {
				t.Fatalf("DELETE: status %d, want 204", status)
			}

			committed := map[string]pgtype.UUID{"existing": existingGrant}
			if addErr == nil {
				committed["added by the concurrent consent"] = added.ID
			}
			for name, id := range committed {
				if _, err := r.mcpGrants.GetGrant(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
					t.Errorf("%s grant after the deletion: err = %v, want deleted", name, err)
				}
				rows := mcpAuditRows(ctx, t, r, "mcp_authorization", id.String())
				if len(rows) != 1 || rows[0].action != "mcp_authorization.revoked" || rows[0].detail["reason"] != "client_deleted" {
					t.Errorf("%s grant audit rows = %+v, want one revocation with reason client_deleted", name, rows)
				}
			}
			clientRows := mcpAuditRows(ctx, t, r, "mcp_client", client.Id)
			if last := clientRows[len(clientRows)-1]; last.action != "mcp_client.deleted" || last.detail["revoked_authorizations"] != float64(len(committed)) {
				t.Errorf("client audit rows = %+v, want a final mcp_client.deleted counting %d revocations", clientRows, len(committed))
			}
		})
	}
}

// TestMyMCPAuthorizations_ListAndRevoke: every role lists and revokes its
// OWN authorizations; another user's id is a 404 and leaves it intact; a
// revocation deletes the grant (its token stops resolving) and is audited
// with reason "user"; nothing listed is a secret.
func TestMyMCPAuthorizations_ListAndRevoke(t *testing.T) {
	ctx := context.Background()
	r := newTestRig(t)
	_, admin := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)
	client := createMCPClientViaAPI(t, r, admin, `{"clientName":"Editor Plugin","redirectUris":["http://127.0.0.1/cb"]}`)

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			owner, ownerCookie := createUserWithRole(ctx, t, r, role)
			_, otherCookie := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)
			grantID, tokenHash := grantWithToken(ctx, t, r, owner.ID, mustUUID(t, client.Id))

			var raw json.RawMessage
			if s := r.doJSON(t, http.MethodGet, "/api/me/mcp-authorizations", nil, &raw, ownerCookie); s != http.StatusOK {
				t.Fatalf("list: status %d", s)
			}
			var list restdtos.ListMCPAuthorizationsResponse
			if err := json.Unmarshal(raw, &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Authorizations) != 1 {
				t.Fatalf("list = %+v, want exactly the owner's one authorization", list)
			}
			a := list.Authorizations[0]
			if a.Id != grantID.String() || a.ClientId != client.ClientId || a.ClientName != "Editor Plugin" || a.ClientKind != restdtos.MCPAuthorizationClientKindPreregistered ||
				len(a.Scopes) != 1 || a.Scopes[0] != "mcp:read" || a.LastUsedAt != nil {
				t.Fatalf("authorization = %+v", a)
			}
			if strings.Contains(string(raw), tokenHash) || strings.Contains(string(raw), "narvi_mcp_at_") {
				t.Fatalf("the list leaked token material: %s", raw)
			}
			var otherList restdtos.ListMCPAuthorizationsResponse
			if s := r.doJSON(t, http.MethodGet, "/api/me/mcp-authorizations", nil, &otherList, otherCookie); s != http.StatusOK || len(otherList.Authorizations) != 0 {
				t.Fatalf("another user's list = %+v (status %d), want empty", otherList, s)
			}

			if s := r.doJSON(t, http.MethodDelete, "/api/me/mcp-authorizations/"+grantID.String(), nil, nil, otherCookie); s != http.StatusNotFound {
				t.Fatalf("another user's revoke: status %d, want 404", s)
			}
			if _, err := r.mcpGrants.LookupAccessToken(ctx, tokenHash); err != nil {
				t.Fatalf("token after another user's refused revoke: %v, want still alive", err)
			}
			if s := r.doJSON(t, http.MethodDelete, "/api/me/mcp-authorizations/not-a-uuid", nil, nil, ownerCookie); s != http.StatusBadRequest {
				t.Fatalf("malformed id: status %d, want 400", s)
			}
			if s := r.doJSON(t, http.MethodDelete, "/api/me/mcp-authorizations/"+grantID.String(), nil, nil, ownerCookie); s != http.StatusNoContent {
				t.Fatalf("owner's revoke: status %d, want 204", s)
			}
			if _, err := r.mcpGrants.LookupAccessToken(ctx, tokenHash); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("token after revoke: err = %v, want pgx.ErrNoRows", err)
			}
			rows := mcpAuditRows(ctx, t, r, "mcp_authorization", grantID.String())
			if len(rows) != 1 || rows[0].action != "mcp_authorization.revoked" || rows[0].detail["reason"] != "user" || rows[0].detail["client_id"] != client.ClientId {
				t.Fatalf("audit rows = %+v, want one revocation with reason user", rows)
			}
			if s := r.doJSON(t, http.MethodDelete, "/api/me/mcp-authorizations/"+grantID.String(), nil, nil, ownerCookie); s != http.StatusNotFound {
				t.Fatalf("second revoke: status %d, want 404", s)
			}
		})
	}
}

// TestMemberMCPAuthorizations_AdminListAndRevoke: an administrator lists a
// member's MCP authorizations -- exactly what the member's own list shows,
// never token material -- and revokes one on the member's behalf; every
// other role is refused 403 and nothing is deleted. A grant is revoked only
// through the member it belongs to: the same grant id under any other
// member's path is a 404 that leaves it (and its token) alive. The
// revocation deletes the grant -- its token stops resolving -- and is
// audited as mcp_authorization.revoked, reason "admin", by the
// administrator, naming the member (technical plan §43.18).
func TestMemberMCPAuthorizations_AdminListAndRevoke(t *testing.T) {
	ctx := context.Background()
	r := newTestRig(t)
	admin, adminCookie := createUserWithRole(ctx, t, r, sqlcgen.UserRoleAdmin)
	client := createMCPClientViaAPI(t, r, adminCookie, `{"clientName":"Editor Plugin","redirectUris":["http://127.0.0.1/cb"]}`)
	member, memberCookie := createUserWithRole(ctx, t, r, sqlcgen.UserRoleMember)
	other, _ := createUserWithRole(ctx, t, r, sqlcgen.UserRoleMember)
	grantID, tokenHash := grantWithToken(ctx, t, r, member.ID, mustUUID(t, client.Id))
	listPath := "/api/members/" + member.ID.String() + "/mcp-authorizations"
	revokePath := listPath + "/" + grantID.String()
	alive := func() {
		t.Helper()
		if _, err := r.mcpGrants.LookupAccessToken(ctx, tokenHash); err != nil {
			t.Fatalf("the member's token: %v, want still alive", err)
		}
	}

	t.Run("every role but admin is refused", func(t *testing.T) {
		for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer} {
			_, cookie := createUserWithRole(ctx, t, r, role)
			if s := r.doJSON(t, http.MethodGet, listPath, nil, nil, cookie); s != http.StatusForbidden {
				t.Errorf("%s list: status %d, want 403", role, s)
			}
			if s := r.doJSON(t, http.MethodDelete, revokePath, nil, nil, cookie); s != http.StatusForbidden {
				t.Errorf("%s revoke: status %d, want 403", role, s)
			}
		}
		// The member too: their own grant is theirs to revoke through
		// their own route, never through the administrators'.
		if s := r.doJSON(t, http.MethodDelete, revokePath, nil, nil, memberCookie); s != http.StatusForbidden {
			t.Errorf("the member through the admin route: status %d, want 403", s)
		}
		alive()
	})

	t.Run("the admin sees exactly the member's own list", func(t *testing.T) {
		var raw json.RawMessage
		if s := r.doJSON(t, http.MethodGet, listPath, nil, &raw, adminCookie); s != http.StatusOK {
			t.Fatalf("admin list: status %d", s)
		}
		var mine json.RawMessage
		if s := r.doJSON(t, http.MethodGet, "/api/me/mcp-authorizations", nil, &mine, memberCookie); s != http.StatusOK {
			t.Fatalf("member's own list: status %d", s)
		}
		if string(raw) != string(mine) {
			t.Fatalf("the admin's view differs from the member's own:\n admin:  %s\n member: %s", raw, mine)
		}
		var list restdtos.ListMCPAuthorizationsResponse
		if err := json.Unmarshal(raw, &list); err != nil || len(list.Authorizations) != 1 || list.Authorizations[0].Id != grantID.String() {
			t.Fatalf("admin list = %s (err %v), want the member's one authorization", raw, err)
		}
		if strings.Contains(string(raw), tokenHash) || strings.Contains(string(raw), "narvi_mcp_at_") {
			t.Fatalf("the admin's list leaked token material: %s", raw)
		}
		var empty restdtos.ListMCPAuthorizationsResponse
		if s := r.doJSON(t, http.MethodGet, "/api/members/"+other.ID.String()+"/mcp-authorizations", nil, &empty, adminCookie); s != http.StatusOK || len(empty.Authorizations) != 0 {
			t.Fatalf("another member's list = %+v (status %d), want empty", empty, s)
		}
		if s := r.doJSON(t, http.MethodGet, "/api/members/00000000-0000-0000-0000-000000000000/mcp-authorizations", nil, nil, adminCookie); s != http.StatusNotFound {
			t.Fatalf("an unknown member's list: status %d, want 404", s)
		}
		if s := r.doJSON(t, http.MethodGet, "/api/members/not-a-uuid/mcp-authorizations", nil, nil, adminCookie); s != http.StatusBadRequest {
			t.Fatalf("a malformed member id: status %d, want 400", s)
		}
	})

	t.Run("a grant is revoked only through the member it belongs to", func(t *testing.T) {
		for _, path := range []string{
			"/api/members/" + other.ID.String() + "/mcp-authorizations/" + grantID.String(),
			"/api/members/" + admin.ID.String() + "/mcp-authorizations/" + grantID.String(),
			"/api/members/00000000-0000-0000-0000-000000000000/mcp-authorizations/" + grantID.String(),
		} {
			if s := r.doJSON(t, http.MethodDelete, path, nil, nil, adminCookie); s != http.StatusNotFound {
				t.Errorf("DELETE %s: status %d, want 404", path, s)
			}
		}
		if s := r.doJSON(t, http.MethodDelete, listPath+"/not-a-uuid", nil, nil, adminCookie); s != http.StatusBadRequest {
			t.Errorf("malformed authorization id: status %d, want 400", s)
		}
		alive()
		if rows := mcpAuditRows(ctx, t, r, "mcp_authorization", grantID.String()); len(rows) != 0 {
			t.Fatalf("audit rows after refused revocations = %+v, want none", rows)
		}
	})

	t.Run("the admin revokes it, and it is gone and audited", func(t *testing.T) {
		if s := r.doJSON(t, http.MethodDelete, revokePath, nil, nil, adminCookie); s != http.StatusNoContent {
			t.Fatalf("admin revoke: status %d, want 204", s)
		}
		if _, err := r.mcpGrants.LookupAccessToken(ctx, tokenHash); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("token after the admin's revocation: err = %v, want pgx.ErrNoRows", err)
		}
		var actor pgtype.UUID
		var reason, target, clientID string
		if err := r.pool.QueryRow(ctx, `
			SELECT actor_user_id, detail_json->>'reason', detail_json->>'target_user_id', detail_json->>'client_id'
			FROM audit_log WHERE action = 'mcp_authorization.revoked' AND resource_type = 'mcp_authorization' AND resource_id = $1`,
			grantID.String()).Scan(&actor, &reason, &target, &clientID); err != nil {
			t.Fatalf("read the revocation's audit row: %v", err)
		}
		if actor != admin.ID || reason != "admin" || target != member.ID.String() || clientID != client.ClientId {
			t.Fatalf("audit row: actor %v reason %q target %q client %q; want the admin, admin, the member, the client", actor, reason, target, clientID)
		}
		if s := r.doJSON(t, http.MethodDelete, revokePath, nil, nil, adminCookie); s != http.StatusNotFound {
			t.Fatalf("second revoke: status %d, want 404", s)
		}
		var list restdtos.ListMCPAuthorizationsResponse
		if s := r.doJSON(t, http.MethodGet, listPath, nil, &list, adminCookie); s != http.StatusOK || len(list.Authorizations) != 0 {
			t.Fatalf("admin list after the revocation = %+v (status %d), want empty", list, s)
		}
	})
}
