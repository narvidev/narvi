//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
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
