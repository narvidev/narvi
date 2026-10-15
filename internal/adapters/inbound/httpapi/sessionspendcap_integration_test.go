//go:build integration

// Integration tests for the audited write behind technical plan §40.1's
// spend cap (sessionspendcap.go): an administrator sets, raises or clears
// the session spend cap of a repository or an automation, each write and
// its session_spend_cap.set audit row in one transaction, and a raise lets
// a session the cap refused take its next turn -- against a real Postgres
// instance and this package's testRig.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// spendCapAuditRows returns the detail and the actor of every
// session_spend_cap.set row on the resource, oldest first (audit_log.id is
// a random UUID, so its order is created_at's: each write is its own
// transaction).
func spendCapAuditRows(ctx context.Context, t *testing.T, rig testRig, resourceType, resourceID string) ([]map[string]any, []pgtype.UUID) {
	t.Helper()
	rows, err := rig.pool.Query(ctx, `SELECT detail_json, actor_user_id FROM audit_log WHERE action = 'session_spend_cap.set' AND resource_type = $1 AND resource_id = $2 ORDER BY created_at, id`, resourceType, resourceID)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	var raws [][]byte
	var actors []pgtype.UUID
	var scanErr error
	for rows.Next() {
		var raw []byte
		var actor pgtype.UUID
		if scanErr = rows.Scan(&raw, &actor); scanErr != nil {
			break
		}
		raws = append(raws, raw)
		actors = append(actors, actor)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan audit_log: %v", scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit_log rows: %v", err)
	}
	details := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode detail_json %s: %v", raw, err)
		}
		details = append(details, detail)
	}
	return details, actors
}

// allSpendCapAuditRows counts every session_spend_cap.set row.
func allSpendCapAuditRows(ctx context.Context, t *testing.T, rig testRig) int {
	t.Helper()
	var n int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'session_spend_cap.set'`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// repoCapInDB reads repo's stored cap as the database prints it: "" when
// the repository has no settings row, "NULL" when it has no cap.
func repoCapInDB(ctx context.Context, t *testing.T, rig testRig, repo string) string {
	t.Helper()
	var text string
	err := rig.pool.QueryRow(ctx, `SELECT COALESCE(session_spend_cap_usd::text, 'NULL') FROM repo_settings WHERE repo_full_name = $1`, repo).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the repository's cap: %v", err)
	}
	return text
}

// automationCapInDB reads id's stored cap as the database prints it,
// "NULL" when it has none.
func automationCapInDB(ctx context.Context, t *testing.T, rig testRig, id pgtype.UUID) string {
	t.Helper()
	var text string
	if err := rig.pool.QueryRow(ctx, `SELECT COALESCE(session_spend_cap_usd::text, 'NULL') FROM automations WHERE id = $1`, id).Scan(&text); err != nil {
		t.Fatalf("read the automation's cap: %v", err)
	}
	return text
}

// createCapTestAutomation stores an automation with the cap limit ("" for
// none) and returns its id.
func createCapTestAutomation(ctx context.Context, t *testing.T, rig testRig, name, limit string) pgtype.UUID {
	t.Helper()
	var value any
	if limit != "" {
		value = limit
	}
	var id pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `INSERT INTO automations (name, repos, session_spend_cap_usd)
		VALUES ($1, '[{"name": "widgets", "url": "https://github.com/acme/widgets"}]'::jsonb, $2::numeric) RETURNING id`, name, value).Scan(&id); err != nil {
		t.Fatalf("create automation: %v", err)
	}
	return id
}

// usd dereferences a nullable wire amount for a comparison: -1 for null.
func usd(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

// capBody is a session spend cap write's body.
func capBody(capUSD string) []byte {
	return []byte(`{"capUsd": ` + capUSD + `}`)
}

// awaitDispatchEvaluated waits until the dispatch evaluation a create
// triggers has run -- it deletes the session's dispatch timer first.
func awaitDispatchEvaluated(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var timers int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'dispatch'`, sessionID).Scan(&timers); err != nil {
			t.Fatal(err)
		}
		if timers == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the dispatch evaluation never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertAdmittedAndQueued posts a prompt to sessionID and checks it is
// created (201), and still queued after its dispatch evaluation -- the
// raised cap admits the dispatch too, not only the insert.
func assertAdmittedAndQueued(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, token string) {
	t.Helper()
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+sessionID.String()+"/turns", turnBody("go on"), &created, token); status != http.StatusCreated || created.Status != "pending" {
		t.Fatalf("after the raise: status %d, %+v; want 201 and a pending turn", status, created)
	}
	awaitDispatchEvaluated(ctx, t, rig, sessionID)
	var turnStatus string
	var endReason *string
	if err := rig.pool.QueryRow(ctx, `SELECT status::text, end_reason FROM turns WHERE id = $1::uuid`, created.ID).Scan(&turnStatus, &endReason); err != nil {
		t.Fatal(err)
	}
	if turnStatus != "pending" || endReason != nil {
		t.Fatalf("the admitted turn after its dispatch evaluation: %s (end reason %v), want still pending", turnStatus, endReason)
	}
}

// TestSpendCap_AuditedRaiseReadmitsTheNextTurn: a prompt the cap refused is
// taken once an administrator raises the repository's cap through PUT
// .../session-spend-cap. The write answers the repository's settings with
// the new cap, stores it, and records one session_spend_cap.set row by the
// administrator naming the cap it replaced and the cap it set.
func TestSpendCap_AuditedRaiseReadmitsTheNextTurn(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	owner, ownerToken := rig.createAuthenticatedUser(ctx, t)
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	seed := spendCapSeed{repo: "acme/audited-raise", cap: "2.00", spent: "2.500000"}
	seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)
	rig.markRepoKnown(ctx, t, seed.repo)

	var refused refusalBody
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("go on"), &refused, ownerToken); status != http.StatusConflict || refused.Reason != "spend_cap" {
		t.Fatalf("at the cap: status %d, body %+v; want a 409 with reason spend_cap", status, refused)
	}

	var settings restdtos.RepoSettings
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+seed.repo+"/session-spend-cap", capBody("5"), &settings, adminToken); status != http.StatusOK {
		t.Fatalf("raise: status %d, want 200", status)
	}
	if settings.RepoFullName != seed.repo || usd(settings.SessionSpendCapUsd) != 5 {
		t.Fatalf("raise answered %s with cap %v, want %s with 5", settings.RepoFullName, usd(settings.SessionSpendCapUsd), seed.repo)
	}
	if got := repoCapInDB(ctx, t, rig, seed.repo); got != "5.00" {
		t.Fatalf("stored cap = %q, want 5.00", got)
	}
	details, actors := spendCapAuditRows(ctx, t, rig, "repo", seed.repo)
	if len(details) != 1 || actors[0] != admin.ID || details[0]["previous_usd"] != 2.0 || details[0]["cap_usd"] != 5.0 {
		t.Fatalf("session_spend_cap.set rows = %v by %v, want one by the admin from 2 to 5", details, actors)
	}

	assertAdmittedAndQueued(ctx, t, rig, session.ID, ownerToken)
}

// TestSpendCap_AutomationRaiseReadmitsTheNextTurn: a session an automation
// created is held to the automation's cap, which takes precedence over its
// repository's even above it. Raising the automation's cap through PUT
// /api/automations/{id}/session-spend-cap admits the next turn, although
// the repository's stricter cap is still below the spend.
func TestSpendCap_AutomationRaiseReadmitsTheNextTurn(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	owner, ownerToken := rig.createAuthenticatedUser(ctx, t)
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	seedSpendCap(ctx, t, rig.pool, session.ID, spendCapSeed{repo: "acme/automation-raise", cap: "0.50", spent: "1.500000"}, false)
	automationID := createCapTestAutomation(ctx, t, rig, "nightly", "1.00")
	if _, err := rig.pool.Exec(ctx, `WITH inv AS (
			INSERT INTO automation_invocations (automation_id, targets, total_runs) VALUES ($1, '[]'::jsonb, 1) RETURNING id)
		INSERT INTO automation_runs (invocation_id, automation_id, target, session_id)
		SELECT inv.id, $1, '{}'::jsonb, $2 FROM inv`, automationID, session.ID); err != nil {
		t.Fatalf("create the automation run: %v", err)
	}

	var refused refusalBody
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("go on"), &refused, ownerToken); status != http.StatusConflict || refused.Reason != "spend_cap" {
		t.Fatalf("at the automation's cap: status %d, body %+v; want a 409 with reason spend_cap", status, refused)
	}

	var automation restdtos.Automation
	if status := rig.doJSON(t, http.MethodPut, "/api/automations/"+automationID.String()+"/session-spend-cap", capBody("3"), &automation, adminToken); status != http.StatusOK {
		t.Fatalf("raise: status %d, want 200", status)
	}
	if automation.Id != automationID.String() || usd(automation.SessionSpendCapUsd) != 3 {
		t.Fatalf("raise answered %s with cap %v, want %s with 3", automation.Id, usd(automation.SessionSpendCapUsd), automationID.String())
	}
	details, actors := spendCapAuditRows(ctx, t, rig, "automation", automationID.String())
	if len(details) != 1 || actors[0] != admin.ID || details[0]["previous_usd"] != 1.0 || details[0]["cap_usd"] != 3.0 {
		t.Fatalf("session_spend_cap.set rows = %v by %v, want one by the admin from 1 to 3", details, actors)
	}

	assertAdmittedAndQueued(ctx, t, rig, session.ID, ownerToken)
}

// TestPutSessionSpendCap_AdminOnly: setting a repository's cap is admin only
// (authz.ActionSetSessionSpendCap, §13.3) -- 403 for a maintainer, a member
// and a viewer, and a refused request writes nothing, audit included.
func TestPutSessionSpendCap_AdminOnly(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	const repo = "acme/cap-admin-only"
	rig.markRepoKnown(ctx, t, repo)

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		_, token := createUserWithRole(ctx, t, rig, role)
		if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("50"), nil, token); status != http.StatusForbidden {
			t.Errorf("as %s: status = %d, want 403", role, status)
		}
	}
	if got := repoCapInDB(ctx, t, rig, repo); got != "" {
		t.Fatalf("refused writes left a settings row with cap %q", got)
	}
	if n := allSpendCapAuditRows(ctx, t, rig); n != 0 {
		t.Fatalf("%d session_spend_cap.set rows after refused writes, want none", n)
	}
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("50"), nil, adminToken); status != http.StatusOK {
		t.Fatalf("as admin: status = %d, want 200", status)
	}
}

// TestPutAutomationSessionSpendCap_AdminOnly: setting an automation's cap is
// admin only too -- stricter than managing the automation, so a maintainer,
// who may create and pause it, is refused, as are a member and a viewer.
func TestPutAutomationSessionSpendCap_AdminOnly(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	automationID := createCapTestAutomation(ctx, t, rig, "admin-only", "")

	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	if status := rig.doJSON(t, http.MethodPost, "/api/automations/"+automationID.String()+"/pause", nil, nil, maintainerToken); status != http.StatusOK {
		t.Fatalf("a maintainer pausing the automation: status = %d, want 200 (the maintainer manages automations)", status)
	}
	for _, tc := range []struct {
		role  sqlcgen.UserRole
		token string
	}{
		{sqlcgen.UserRoleMaintainer, maintainerToken},
		{sqlcgen.UserRoleMember, ""},
		{sqlcgen.UserRoleViewer, ""},
	} {
		token := tc.token
		if token == "" {
			_, token = createUserWithRole(ctx, t, rig, tc.role)
		}
		if status := rig.doJSON(t, http.MethodPut, "/api/automations/"+automationID.String()+"/session-spend-cap", capBody("50"), nil, token); status != http.StatusForbidden {
			t.Errorf("as %s: status = %d, want 403", tc.role, status)
		}
	}
	if got := automationCapInDB(ctx, t, rig, automationID); got != "NULL" {
		t.Fatalf("refused writes set the cap to %s", got)
	}
	if n := allSpendCapAuditRows(ctx, t, rig); n != 0 {
		t.Fatalf("%d session_spend_cap.set rows after refused writes, want none", n)
	}
}

// TestPutSessionSpendCap_ZeroOrLessRefused: a cap of zero or less permits
// nothing, so it is refused when written (§37) with a 400 that says what a
// cap must be, on both routes, and nothing is written.
func TestPutSessionSpendCap_ZeroOrLessRefused(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	const repo = "acme/cap-zero"
	rig.markRepoKnown(ctx, t, repo)
	automationID := createCapTestAutomation(ctx, t, rig, "zero", "5.00")

	for _, value := range []string{"0", "0.00", "-0", "-0.01", "-1"} {
		t.Run(value, func(t *testing.T) {
			for _, path := range []string{"/api/repos/" + repo + "/session-spend-cap", "/api/automations/" + automationID.String() + "/session-spend-cap"} {
				var body map[string]string
				if status := rig.doJSON(t, http.MethodPut, path, capBody(value), &body, token); status != http.StatusBadRequest || body["error"] != "capUsd must be above zero, with at most two decimal places and no larger than 99999999.99, or null to clear the cap" {
					t.Errorf("%s: status = %d body = %v, want 400 naming what a cap must be", path, status, body)
				}
			}
		})
	}
	if got := repoCapInDB(ctx, t, rig, repo); got != "" {
		t.Errorf("refused writes left a settings row with cap %q", got)
	}
	if got := automationCapInDB(ctx, t, rig, automationID); got != "5.00" {
		t.Errorf("refused writes changed the automation's cap to %s", got)
	}
	if n := allSpendCapAuditRows(ctx, t, rig); n != 0 {
		t.Errorf("%d session_spend_cap.set rows after refused writes, want none", n)
	}
}

// TestPutSessionSpendCap_TooManyDecimalsRefused: a cap NUMERIC(10, 2) would
// round, or cannot hold, is refused rather than silently changed.
func TestPutSessionSpendCap_TooManyDecimalsRefused(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	const repo = "acme/cap-decimals"
	rig.markRepoKnown(ctx, t, repo)

	for _, value := range []string{"1.005", "0.001", "24.999", "100000000", "1e9"} {
		t.Run(value, func(t *testing.T) {
			var body map[string]string
			if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody(value), &body, token); status != http.StatusBadRequest || body["error"] != "capUsd must be above zero, with at most two decimal places and no larger than 99999999.99, or null to clear the cap" {
				t.Errorf("status = %d body = %v, want 400", status, body)
			}
		})
	}
	if got := repoCapInDB(ctx, t, rig, repo); got != "" {
		t.Errorf("refused writes left a settings row with cap %q", got)
	}
	var settings restdtos.RepoSettings
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("99999999.99"), &settings, token); status != http.StatusOK || usd(settings.SessionSpendCapUsd) != 99_999_999.99 {
		t.Fatalf("the largest cap: status %d, cap %v; want 200 and 99999999.99", status, usd(settings.SessionSpendCapUsd))
	}
	if got := repoCapInDB(ctx, t, rig, repo); got != "99999999.99" {
		t.Fatalf("stored cap = %q, want 99999999.99", got)
	}
}

// TestPutSessionSpendCap_MalformedBodyRefused: capUsd is required -- a body
// that leaves it out, or a bare null, clears nothing -- and must be a
// number or null.
func TestPutSessionSpendCap_MalformedBodyRefused(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	const repo = "acme/cap-malformed"
	rig.markRepoKnown(ctx, t, repo)
	if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 5)`, repo); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"no capUsd", `{}`, "capUsd is required: a number of dollars, or null to clear the cap"},
		{"another key", `{"cap": null}`, "capUsd is required: a number of dollars, or null to clear the cap"},
		{"a bare null", `null`, "malformed request body"},
		{"not JSON", `not json`, "malformed request body"},
		{"a string", `{"capUsd": "5"}`, "capUsd must be above zero, with at most two decimal places and no larger than 99999999.99, or null to clear the cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]string
			if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", []byte(tc.body), &body, token); status != http.StatusBadRequest || body["error"] != tc.want {
				t.Errorf("status = %d body = %v, want 400 %q", status, body, tc.want)
			}
		})
	}
	if got := repoCapInDB(ctx, t, rig, repo); got != "5.00" {
		t.Errorf("refused writes changed the cap to %s", got)
	}
}

// TestPutSessionSpendCap_NullClears: null clears a repository's cap -- the
// only spelling of "no cap" -- and is audited like any write; the
// repository's settings read the cap set and then none.
func TestPutSessionSpendCap_NullClears(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	const repo = "acme/cap-clear"
	rig.markRepoKnown(ctx, t, repo)

	var set restdtos.RepoSettings
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("25.5"), &set, token); status != http.StatusOK || usd(set.SessionSpendCapUsd) != 25.5 {
		t.Fatalf("set: status %d, cap %v; want 200 and 25.5", status, usd(set.SessionSpendCapUsd))
	}
	var read restdtos.RepoSettings
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/"+repo+"/settings", nil, &read, token); status != http.StatusOK || usd(read.SessionSpendCapUsd) != 25.5 {
		t.Fatalf("GET after set: status %d, cap %v; want 25.5", status, usd(read.SessionSpendCapUsd))
	}

	var cleared restdtos.RepoSettings
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("null"), &cleared, token); status != http.StatusOK || cleared.SessionSpendCapUsd != nil {
		t.Fatalf("clear: status %d, cap %v; want 200 and null", status, usd(cleared.SessionSpendCapUsd))
	}
	if got := repoCapInDB(ctx, t, rig, repo); got != "NULL" {
		t.Fatalf("stored cap after clearing = %q, want NULL", got)
	}
	read = restdtos.RepoSettings{}
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/"+repo+"/settings", nil, &read, token); status != http.StatusOK || read.SessionSpendCapUsd != nil {
		t.Fatalf("GET after clear: status %d, cap %v; want null", status, usd(read.SessionSpendCapUsd))
	}
	details, _ := spendCapAuditRows(ctx, t, rig, "repo", repo)
	if len(details) != 2 ||
		details[0]["previous_usd"] != nil || details[0]["cap_usd"] != 25.5 ||
		details[1]["previous_usd"] != 25.5 || details[1]["cap_usd"] != nil {
		t.Fatalf("session_spend_cap.set rows = %v, want none to 25.5, then 25.5 to none", details)
	}
}

// TestPutSessionSpendCap_UnknownRepo404: a repository this deployment has
// never seen a pull-request session for answers 404 before anything is
// read or written, as every repository-scoped route does.
func TestPutSessionSpendCap_UnknownRepo404(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var body map[string]string
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/acme/never-onboarded/session-spend-cap", capBody("10"), &body, token); status != http.StatusNotFound || body["error"] != "repo not found" {
		t.Fatalf("status = %d body = %v, want 404 repo not found", status, body)
	}
	if got := repoCapInDB(ctx, t, rig, "acme/never-onboarded"); got != "" {
		t.Fatalf("an unknown repository got a settings row with cap %q", got)
	}
	if n := allSpendCapAuditRows(ctx, t, rig); n != 0 {
		t.Fatalf("%d session_spend_cap.set rows, want none", n)
	}
}

// TestPutAutomationSessionSpendCap_SetsClearsAndAudits: an administrator
// sets an automation's cap -- the answer, GET and the list all read it --
// and clears it, each write audited on the automation, naming the cap it
// replaced.
func TestPutAutomationSessionSpendCap_SetsClearsAndAudits(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	admin, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	automationID := createCapTestAutomation(ctx, t, rig, "sets-and-clears", "")
	path := "/api/automations/" + automationID.String()

	var set restdtos.Automation
	if status := rig.doJSON(t, http.MethodPut, path+"/session-spend-cap", capBody("12.5"), &set, token); status != http.StatusOK || usd(set.SessionSpendCapUsd) != 12.5 || set.Name != "sets-and-clears" {
		t.Fatalf("set: status %d, %s cap %v; want 200 and 12.5", status, set.Name, usd(set.SessionSpendCapUsd))
	}
	if got := automationCapInDB(ctx, t, rig, automationID); got != "12.50" {
		t.Fatalf("stored cap = %s, want 12.50", got)
	}
	var got restdtos.Automation
	if status := rig.doJSON(t, http.MethodGet, path, nil, &got, token); status != http.StatusOK || usd(got.SessionSpendCapUsd) != 12.5 {
		t.Fatalf("GET: status %d, cap %v; want 12.5", status, usd(got.SessionSpendCapUsd))
	}
	var list restdtos.ListAutomationsResponse
	if status := rig.doJSON(t, http.MethodGet, "/api/automations", nil, &list, token); status != http.StatusOK || len(list.Automations) != 1 || usd(list.Automations[0].SessionSpendCapUsd) != 12.5 {
		t.Fatalf("list: status %d, %+v; want the automation with cap 12.5", status, list.Automations)
	}

	var cleared restdtos.Automation
	if status := rig.doJSON(t, http.MethodPut, path+"/session-spend-cap", capBody("null"), &cleared, token); status != http.StatusOK || cleared.SessionSpendCapUsd != nil {
		t.Fatalf("clear: status %d, cap %v; want 200 and null", status, usd(cleared.SessionSpendCapUsd))
	}
	if got := automationCapInDB(ctx, t, rig, automationID); got != "NULL" {
		t.Fatalf("stored cap after clearing = %s, want NULL", got)
	}
	details, actors := spendCapAuditRows(ctx, t, rig, "automation", automationID.String())
	if len(details) != 2 || actors[0] != admin.ID || actors[1] != admin.ID ||
		details[0]["previous_usd"] != nil || details[0]["cap_usd"] != 12.5 ||
		details[1]["previous_usd"] != 12.5 || details[1]["cap_usd"] != nil {
		t.Fatalf("session_spend_cap.set rows = %v by %v, want none to 12.5, then 12.5 to none, by the admin", details, actors)
	}
}

// TestPutAutomationSessionSpendCap_UnknownAutomation404: an automation that
// does not exist answers 404, and a malformed id 400, auditing nothing.
func TestPutAutomationSessionSpendCap_UnknownAutomation404(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var body map[string]string
	if status := rig.doJSON(t, http.MethodPut, "/api/automations/00000000-0000-0000-0000-000000000001/session-spend-cap", capBody("10"), &body, token); status != http.StatusNotFound || body["error"] != "automation not found" {
		t.Fatalf("unknown automation: status = %d body = %v, want 404", status, body)
	}
	if status := rig.doJSON(t, http.MethodPut, "/api/automations/not-a-uuid/session-spend-cap", capBody("10"), nil, token); status != http.StatusBadRequest {
		t.Fatalf("malformed id: status = %d, want 400", status)
	}
	if n := allSpendCapAuditRows(ctx, t, rig); n != 0 {
		t.Fatalf("%d session_spend_cap.set rows, want none", n)
	}
}

// failSpendCapAudits makes every session_spend_cap.set audit insert fail,
// for the rest of the test.
func failSpendCapAudits(ctx context.Context, t *testing.T, rig testRig) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION spend_cap_test_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`,
		`CREATE TRIGGER spend_cap_test_fail_audit BEFORE INSERT ON audit_log FOR EACH ROW WHEN (NEW.action = 'session_spend_cap.set') EXECUTE FUNCTION spend_cap_test_fail_audit()`,
	} {
		if _, err := rig.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject the audit failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = rig.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS spend_cap_test_fail_audit ON audit_log`)
		_, _ = rig.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS spend_cap_test_fail_audit()`)
	})
}

// failSpendCapCommits makes every transaction that leaves a repository or
// an automation with a cap of 50 fail at its commit, for the rest of the
// test: a deferred constraint trigger, checked once the write and its audit
// row are both in.
func failSpendCapCommits(ctx context.Context, t *testing.T, rig testRig) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION spend_cap_test_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.session_spend_cap_usd = 50 THEN RAISE EXCEPTION 'injected commit failure'; END IF; RETURN NULL; END $$`,
		`CREATE CONSTRAINT TRIGGER spend_cap_test_fail_commit AFTER INSERT OR UPDATE ON repo_settings DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION spend_cap_test_fail_commit()`,
		`CREATE CONSTRAINT TRIGGER spend_cap_test_fail_commit AFTER INSERT OR UPDATE ON automations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION spend_cap_test_fail_commit()`,
	} {
		if _, err := rig.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject the commit failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = rig.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS spend_cap_test_fail_commit ON repo_settings`)
		_, _ = rig.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS spend_cap_test_fail_commit ON automations`)
		_, _ = rig.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS spend_cap_test_fail_commit()`)
	})
}

// TestSessionSpendCap_AuditInTheSameTransaction: a cap and its audit row
// commit together (§13.3), on both routes. An audit insert that fails
// fails the request and leaves the cap as it was -- a cap is never raised
// without its row -- and a write that fails at its commit leaves no audit
// row -- the log never records a raise that did not happen.
func TestSessionSpendCap_AuditInTheSameTransaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(context.Context, *testing.T, testRig)
	}{
		{"an audit that fails leaves the cap", failSpendCapAudits},
		{"a write that fails at commit leaves no audit row", failSpendCapCommits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newTestRig(t)
			_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
			const repo = "acme/cap-audit-tx"
			rig.markRepoKnown(ctx, t, repo)
			if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 5)`, repo); err != nil {
				t.Fatal(err)
			}
			automationID := createCapTestAutomation(ctx, t, rig, "audit-tx", "5.00")
			tc.inject(ctx, t, rig)

			if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("50"), nil, token); status != http.StatusInternalServerError {
				t.Errorf("repository raise: status = %d, want 500", status)
			}
			if got := repoCapInDB(ctx, t, rig, repo); got != "5.00" {
				t.Errorf("the repository's cap is %s after a failed raise, want 5.00", got)
			}
			if status := rig.doJSON(t, http.MethodPut, "/api/automations/"+automationID.String()+"/session-spend-cap", capBody("50"), nil, token); status != http.StatusInternalServerError {
				t.Errorf("automation raise: status = %d, want 500", status)
			}
			if got := automationCapInDB(ctx, t, rig, automationID); got != "5.00" {
				t.Errorf("the automation's cap is %s after a failed raise, want 5.00", got)
			}
			if n := allSpendCapAuditRows(ctx, t, rig); n != 0 {
				t.Errorf("%d session_spend_cap.set rows after two failed raises, want none", n)
			}
		})
	}
}

// putCapRaw is a session spend cap write made off the test goroutine, where
// doJSON's t.Fatal cannot run: its status, or the error that kept it from
// one.
func putCapRaw(ctx context.Context, rig testRig, path, token string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rig.server.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// TestSessionSpendCap_ConcurrentWriteAuditsTheCapItReplaced: a write locks
// the row before it reads the cap it replaces, so a write that begins while
// another transaction is changing the cap waits for it and audits the cap
// that transaction committed -- on a repository whose row exists, on one
// whose row that transaction is creating, and on an automation. Read
// without the lock, it would audit the cap from before.
func TestSessionSpendCap_ConcurrentWriteAuditsTheCapItReplaced(t *testing.T) {
	const repo = "acme/cap-concurrent"
	for _, tc := range []struct {
		name       string
		automation bool
		seed       string
		holder     string
		lockQuery  string
	}{
		{"a repository whose row exists", false,
			`INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 3)`,
			`UPDATE repo_settings SET session_spend_cap_usd = 7 WHERE repo_full_name = $1`, "LockRepoSessionSpendCap"},
		{"a repository's first write", false, "",
			`INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 7)`, "LockRepoSessionSpendCap"},
		{"an automation", true, "",
			`UPDATE automations SET session_spend_cap_usd = 7 WHERE id = $1`, "LockAutomationForUpdate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newTestRig(t)
			_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
			path, resourceType, resourceID := "/api/repos/"+repo+"/session-spend-cap", "repo", repo
			var holderArg any = repo
			readCap := func() string { return repoCapInDB(ctx, t, rig, repo) }
			if tc.automation {
				id := createCapTestAutomation(ctx, t, rig, "concurrent", "3.00")
				path, resourceType, resourceID = "/api/automations/"+id.String()+"/session-spend-cap", "automation", id.String()
				holderArg = id
				readCap = func() string { return automationCapInDB(ctx, t, rig, id) }
			} else {
				rig.markRepoKnown(ctx, t, repo)
			}
			if tc.seed != "" {
				if _, err := rig.pool.Exec(ctx, tc.seed, repo); err != nil {
					t.Fatal(err)
				}
			}

			holder, err := rig.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, tc.holder, holderArg); err != nil {
				t.Fatal(err)
			}

			var status int
			var g errgroup.Group
			g.Go(func() error {
				var err error
				status, err = putCapRaw(ctx, rig, path, token, capBody("9"))
				return err
			})
			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting int
				if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, tc.lockQuery).Scan(&waiting); err != nil {
					_ = holder.Rollback(ctx)
					_ = g.Wait()
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					_ = holder.Rollback(ctx)
					_ = g.Wait()
					t.Fatal("the write never waited for the row's lock")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := g.Wait(); err != nil {
				t.Fatal(err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if got := readCap(); got != "9.00" {
				t.Fatalf("stored cap = %s, want 9.00", got)
			}
			details, _ := spendCapAuditRows(ctx, t, rig, resourceType, resourceID)
			if len(details) != 1 || details[0]["previous_usd"] != 7.0 || details[0]["cap_usd"] != 9.0 {
				t.Fatalf("session_spend_cap.set rows = %v, want one from 7 -- the cap committed ahead of it -- to 9", details)
			}
		})
	}
}

// TestRepoSettingsResponses_CarryTheSessionSpendCap: every route that
// answers RepoSettings renders the repository's cap, so no settings write
// reads as having cleared it.
func TestRepoSettingsResponses_CarryTheSessionSpendCap(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	const repo = "acme/cap-everywhere"
	rig.markRepoKnown(ctx, t, repo)
	if status := rig.doJSON(t, http.MethodPut, "/api/repos/"+repo+"/session-spend-cap", capBody("42"), nil, token); status != http.StatusOK {
		t.Fatalf("set the cap: status %d", status)
	}

	for _, tc := range []struct {
		method, route, body string
	}{
		{http.MethodGet, "settings", ""},
		{http.MethodPut, "settings", `{"blockOnHighRisk": true}`},
		{http.MethodPut, "auto-approval-settings", `{"maxAutoApproveFilesChanged": 15, "sensitiveBlastRadiusTags": null}`},
		{http.MethodPut, "auto-merge", `{"enabled": true}`},
		{http.MethodPut, "auto-retrigger-review", `{"enabled": true}`},
		{http.MethodPut, "description-autofix", `{"enabled": true}`},
		{http.MethodPut, "review-depth", `{"mode": "auto", "deepPaths": null}`},
		{http.MethodPut, "review-cost-budget", `{"lightUsd": 1, "deepUsd": null}`},
	} {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.route), func(t *testing.T) {
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			var settings restdtos.RepoSettings
			if status := rig.doJSON(t, tc.method, "/api/repos/"+repo+"/"+tc.route, body, &settings, token); status != http.StatusOK || usd(settings.SessionSpendCapUsd) != 42 {
				t.Errorf("status %d, cap %v; want 200 and 42", status, usd(settings.SessionSpendCapUsd))
			}
		})
	}
}
