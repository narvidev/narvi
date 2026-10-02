//go:build integration

// Integration tests for the operator action behind technical plan §31.4's
// "Un-entitlement" (repoentitlement.go): an administrator revokes a
// repository's eligibility for new sessions and restores it, through the
// admin-only routes under /api/repos/{owner}/{repo}/entitlement, against a
// real Postgres instance and this package's testRig, which makes
// acme/widgets and narvidev/narvi known.
package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// createSessionBody is a web session-creation request naming repoFullName.
func createSessionBody(repoFullName string) []byte {
	return []byte(fmt.Sprintf(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"widgets","url":"https://github.com/%s","branch":null}],"modelId":null,"effort":null,"planMode":false}`, repoFullName))
}

// auditRows returns the detail of every audit_log row for repoFullName with
// action, oldest first, and its actor.
func auditRows(ctx context.Context, t *testing.T, rig testRig, action, repoFullName string) ([]map[string]any, []pgtype.UUID) {
	t.Helper()
	rows, err := rig.pool.Query(ctx, `SELECT detail_json, actor_user_id FROM audit_log WHERE action = $1 AND resource_type = 'repo' AND resource_id = $2 ORDER BY id`, action, repoFullName)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	var details []map[string]any
	var actors []pgtype.UUID
	var raws [][]byte
	for rows.Next() {
		var raw []byte
		var actor pgtype.UUID
		if err := rows.Scan(&raw, &actor); err != nil {
			rows.Close()
			t.Fatalf("scan audit_log: %v", err)
		}
		raws = append(raws, raw)
		actors = append(actors, actor)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("audit_log rows: %v", err)
	}
	for _, raw := range raws {
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode detail_json %s: %v", raw, err)
		}
		details = append(details, detail)
	}
	return details, actors
}

// TestCreateSession_RevokedRepo_403DistinctFromNotEntitled is §31.4's
// end-to-end proof: an administrator revokes a known repository through the
// route, a member's session creation on it is refused with the revocation's
// own words, a repository the deployment does not know is still refused
// with "not entitled" -- two different refusals -- and the restore lets the
// same request create its session.
func TestCreateSession_RevokedRepo_403DistinctFromNotEntitled(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	member, memberToken := rig.createAuthenticatedUser(ctx, t)

	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"credentials rotated"}`), nil, adminToken); status != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", status)
	}

	var revokedBody map[string]string
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", createSessionBody("acme/widgets"), &revokedBody, memberToken); status != http.StatusForbidden {
		t.Fatalf("create on the revoked repo: status = %d, want 403 (body %v)", status, revokedBody)
	}
	if got, want := revokedBody["error"], "repository entitlement revoked by an administrator: acme/widgets"; got != want {
		t.Errorf("create on the revoked repo: error = %q, want %q", got, want)
	}

	var unknownBody map[string]string
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", createSessionBody("acme/never-seen"), &unknownBody, memberToken); status != http.StatusForbidden {
		t.Fatalf("create on an unknown repo: status = %d, want 403 (body %v)", status, unknownBody)
	}
	if got, want := unknownBody["error"], "repository not entitled: acme/never-seen"; got != want {
		t.Errorf("create on an unknown repo: error = %q, want %q", got, want)
	}
	if count := rig.sessionCountForUser(ctx, t, member.ID); count != 0 {
		t.Fatalf("sessions for the member = %d, want none while refused", count)
	}

	details, _ := auditRows(ctx, t, rig, "session.repo_entitlement_denied", "acme/widgets")
	if len(details) != 1 || details[0]["reason"] != "revoked" {
		t.Errorf("denial audit rows for acme/widgets = %v, want one with reason revoked", details)
	}
	details, _ = auditRows(ctx, t, rig, "session.repo_entitlement_denied", "acme/never-seen")
	if len(details) != 1 || details[0]["reason"] != "unknown" {
		t.Errorf("denial audit rows for acme/never-seen = %v, want one with reason unknown", details)
	}

	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/restore", nil, nil, adminToken); status != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", status)
	}
	var created restdtos.Session
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions", createSessionBody("acme/widgets"), &created, memberToken); status != http.StatusCreated {
		t.Fatalf("create after the restore: status = %d, want 201", status)
	}
	if count := rig.sessionCountForUser(ctx, t, member.ID); count != 1 {
		t.Errorf("sessions for the member = %d, want the one created after the restore", count)
	}
}

// TestRepoEntitlementRoutes_MemberAndMaintainerDenied: all three routes are
// admin only (authz.ActionManageRepoEntitlement, §13.3), the status read
// included, for every other role -- and a denied revoke writes nothing.
func TestRepoEntitlementRoutes_MemberAndMaintainerDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		_, token := createUserWithRole(ctx, t, rig, role)
		for _, route := range []struct {
			method, path string
			body         []byte
		}{
			{http.MethodGet, "/api/repos/acme/widgets/entitlement", nil},
			{http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"not mine to revoke"}`)},
			{http.MethodPost, "/api/repos/acme/widgets/entitlement/restore", nil},
		} {
			if status := rig.doJSON(t, route.method, route.path, route.body, nil, token); status != http.StatusForbidden {
				t.Errorf("%s %s as %s: status = %d, want 403", route.method, route.path, role, status)
			}
		}
	}
	var n int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM repo_entitlement_revocations`).Scan(&n); err != nil {
		t.Fatalf("count revocations: %v", err)
	}
	if n != 0 {
		t.Errorf("%d revocations after denied requests, want none", n)
	}
}

// TestRepoEntitlementRoutes_UnknownRepo404: a repository this deployment
// has never seen a pull-request session for cannot be revoked, restored or
// read -- 404 "repo not found", like every repository-scoped route.
func TestRepoEntitlementRoutes_UnknownRepo404(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	for _, route := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/api/repos/acme/never-seen/entitlement", nil},
		{http.MethodPost, "/api/repos/acme/never-seen/entitlement/revoke", []byte(`{"reason":"pre-emptive"}`)},
		{http.MethodPost, "/api/repos/acme/never-seen/entitlement/restore", nil},
	} {
		var body map[string]string
		if status := rig.doJSON(t, route.method, route.path, route.body, &body, token); status != http.StatusNotFound || body["error"] != "repo not found" {
			t.Errorf("%s %s: status = %d body = %v, want 404 repo not found", route.method, route.path, status, body)
		}
	}
}

// TestPostRevokeRepoEntitlement_AdminRevokes_AuditedWithReason: an admin's
// revoke stores the reason, trimmed, answers the repository's entitlement
// naming who, when and why, writes one repo_entitlement.revoked audit row in
// the same transaction, and the status read returns the same.
func TestPostRevokeRepoEntitlement_AdminRevokes_AuditedWithReason(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var before restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/widgets/entitlement", nil, &before, token); status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if before.Revoked || before.RevokedAt != nil || before.RevokedByUserId != nil || before.RevokedByDisplayName != nil || before.Reason != nil || before.RepoFullName != "acme/widgets" {
		t.Errorf("GET before revoking = %+v, want acme/widgets not revoked, every revocation field null", before)
	}

	var got restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"  credentials leaked  "}`), &got, token); status != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", status)
	}
	if !got.Revoked || got.RevokedAt == nil || got.RevokedByUserId == nil || *got.RevokedByUserId != admin.ID.String() ||
		got.RevokedByDisplayName == nil || *got.RevokedByDisplayName != admin.DisplayName || got.Reason == nil || *got.Reason != "credentials leaked" {
		t.Errorf("revoke response = %+v, want revoked by %s with the trimmed reason", got, admin.ID.String())
	}

	var read restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/widgets/entitlement", nil, &read, token); status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if !read.Revoked || read.Reason == nil || *read.Reason != "credentials leaked" {
		t.Errorf("GET after revoking = %+v, want the revocation", read)
	}

	details, actors := auditRows(ctx, t, rig, "repo_entitlement.revoked", "acme/widgets")
	if len(details) != 1 || details[0]["reason"] != "credentials leaked" || actors[0] != admin.ID {
		t.Errorf("repo_entitlement.revoked rows = %v by %v, want one with the reason, by the admin", details, actors)
	}
}

// TestPostRevokeRepoEntitlement_BlankReason400: the reason is required,
// trimmed, at most 500 characters, and holds no NUL character (which a
// Postgres TEXT column would refuse with a 500) -- each refusal says which,
// and none writes anything.
func TestPostRevokeRepoEntitlement_BlankReason400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	for _, tc := range []struct {
		name, body, want string
	}{
		{"no body field", `{}`, "reason is required"},
		{"null", `{"reason":null}`, "reason is required"},
		{"empty", `{"reason":""}`, "reason is required"},
		{"white space", `{"reason":" \t\n "}`, "reason is required"},
		{"501 characters", `{"reason":"` + strings.Repeat("é", 501) + `"}`, "reason must be at most 500 characters"},
		{"malformed", `not json`, "malformed request body"},
		{"a NUL character", `{"reason":"frozen \u0000 for an audit"}`, "reason must not contain a NUL byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]string
			if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(tc.body), &body, token); status != http.StatusBadRequest || body["error"] != tc.want {
				t.Errorf("status = %d body = %v, want 400 %q", status, body, tc.want)
			}
		})
	}
	var accepted restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"`+strings.Repeat("é", 500)+`"}`), &accepted, token); status != http.StatusOK {
		t.Errorf("a 500-character reason: status = %d, want 200", status)
	}
	details, _ := auditRows(ctx, t, rig, "repo_entitlement.revoked", "acme/widgets")
	if len(details) != 1 {
		t.Errorf("%d revoke audit rows, want only the accepted one", len(details))
	}
}

// TestPostRevokeRepoEntitlement_AlreadyRevoked409: a second revoke is
// refused and keeps the first revocation's who, when and why, with no second
// audit row.
func TestPostRevokeRepoEntitlement_AlreadyRevoked409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	first, firstToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	_, secondToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"the first reason"}`), nil, firstToken); status != http.StatusOK {
		t.Fatalf("first revoke status = %d, want 200", status)
	}
	var body map[string]string
	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"the second reason"}`), &body, secondToken); status != http.StatusConflict || body["error"] != "repository entitlement is already revoked" {
		t.Fatalf("second revoke: status = %d body = %v, want 409 already revoked", status, body)
	}
	var read restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/widgets/entitlement", nil, &read, firstToken); status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if read.Reason == nil || *read.Reason != "the first reason" || read.RevokedByUserId == nil || *read.RevokedByUserId != first.ID.String() {
		t.Errorf("GET after a second revoke = %+v, want the first revocation kept", read)
	}
	if details, _ := auditRows(ctx, t, rig, "repo_entitlement.revoked", "acme/widgets"); len(details) != 1 {
		t.Errorf("%d revoke audit rows, want the first alone", len(details))
	}
}

// TestPostRestoreRepoEntitlement_AdminRestores_Audited: an admin's restore
// lifts the revocation, answers the repository no longer revoked, and
// writes one repo_entitlement.restored row naming what was lifted -- when,
// by whom, why -- by the restoring admin.
func TestPostRestoreRepoEntitlement_AdminRestores_Audited(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	revoker, revokerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	restorer, restorerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var revoked restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"audit pending"}`), &revoked, revokerToken); status != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", status)
	}

	var got restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/restore", nil, &got, restorerToken); status != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", status)
	}
	if got.Revoked || got.Reason != nil || got.RevokedAt != nil || got.RepoFullName != "acme/widgets" {
		t.Errorf("restore response = %+v, want acme/widgets no longer revoked", got)
	}

	details, actors := auditRows(ctx, t, rig, "repo_entitlement.restored", "acme/widgets")
	if len(details) != 1 || actors[0] != restorer.ID {
		t.Fatalf("repo_entitlement.restored rows = %v by %v, want one, by the restoring admin", details, actors)
	}
	if details[0]["reason"] != "audit pending" || details[0]["revoked_by"] != revoker.ID.String() || details[0]["revoked_at"] == nil {
		t.Errorf("restored detail = %v, want what was lifted: reason, revoked_by %s, revoked_at", details[0], revoker.ID.String())
	}
	var n int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM repo_entitlement_revocations`).Scan(&n); err != nil {
		t.Fatalf("count revocations: %v", err)
	}
	if n != 0 {
		t.Errorf("%d revocations after the restore, want none", n)
	}
}

// TestPostRestoreRepoEntitlement_NotRevoked409: restoring a repository that
// is not revoked is refused, and audits nothing.
func TestPostRestoreRepoEntitlement_NotRevoked409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var body map[string]string
	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/restore", nil, &body, token); status != http.StatusConflict || body["error"] != "repository entitlement is not revoked" {
		t.Fatalf("restore: status = %d body = %v, want 409 not revoked", status, body)
	}
	if details, _ := auditRows(ctx, t, rig, "repo_entitlement.restored", "acme/widgets"); len(details) != 0 {
		t.Errorf("%d restore audit rows, want none", len(details))
	}
}

// TestRepoSettingsRoutes_StillReachableForRevokedRepo: the repository-scoped
// admin routes scope on whether the deployment knows the repository, never
// on whether it is revoked (confirmRepoKnown), so a revoked repository's
// settings stay readable and writable -- and its entitlement stays
// reachable, which is how it is restored.
func TestRepoSettingsRoutes_StillReachableForRevokedRepo(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	if status := rig.doJSON(t, http.MethodPost, "/api/repos/acme/widgets/entitlement/revoke", []byte(`{"reason":"frozen"}`), nil, token); status != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", status)
	}

	var settings restdtos.RepoSettings
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/widgets/settings", nil, &settings, token); status != http.StatusOK || settings.RepoFullName != "acme/widgets" {
		t.Errorf("GET settings of a revoked repo: status = %d, want 200", status)
	}
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/acme/widgets/settings", []byte(`{"blockOnHighRisk":true,"sentinelAutofixEnabled":false}`), nil, token); status != http.StatusOK {
		t.Errorf("PUT settings of a revoked repo: status = %d, want 200", status)
	}
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/widgets/review-analytics", nil, nil, token); status != http.StatusOK {
		t.Errorf("GET review analytics of a revoked repo: status = %d, want 200", status)
	}
	var entitlement restdtos.RepoEntitlement
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/widgets/entitlement", nil, &entitlement, token); status != http.StatusOK || !entitlement.Revoked {
		t.Errorf("GET entitlement of a revoked repo: status = %d revoked = %v, want 200 revoked", status, entitlement.Revoked)
	}
}
