//go:build integration

// Integration tests for the operator action behind technical plan §40.2's
// freeze (autonomyfreeze.go): an administrator freezes autonomy with a
// reason and lifts the freeze, through /api/autonomy, each change audited
// in its own transaction, against a real Postgres instance and this
// package's testRig. The decision inbox's banner, its held rows and a
// person's Merge click while frozen are pinned beside them.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// autonomyAuditRows returns the detail of every audit_log row with action
// on the platform/autonomy resource, oldest first, and its actor.
func autonomyAuditRows(ctx context.Context, t *testing.T, rig testRig, action string) ([]map[string]any, []pgtype.UUID) {
	t.Helper()
	rows, err := rig.pool.Query(ctx, `SELECT detail_json, actor_user_id FROM audit_log WHERE action = $1 AND resource_type = 'platform' AND resource_id = 'autonomy' ORDER BY id`, action)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	var raws [][]byte
	var actors []pgtype.UUID
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

// frozenInDB reads platform_settings directly: the row every site reads.
func frozenInDB(ctx context.Context, t *testing.T, rig testRig) bool {
	t.Helper()
	frozen, err := narvipg.NewPlatformSettingsStore(rig.pool).AutonomyFrozen(ctx)
	if err != nil {
		t.Fatalf("read the freeze: %v", err)
	}
	return frozen
}

// TestFreezeAutonomy_AdminFreezesAndUnfreezes_Audited: an administrator's
// freeze stores the reason, trimmed, answers the freeze naming who, when
// and why, is what GET reads and what every site reads, and writes one
// autonomy.frozen row by the admin on the platform/autonomy resource; the
// unfreeze answers autonomy no longer frozen.
func TestFreezeAutonomy_AdminFreezesAndUnfreezes_Audited(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var before restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodGet, "/api/autonomy", nil, &before, token); status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if before.Frozen || before.FrozenAt != nil || before.FrozenByUserId != nil || before.FrozenByDisplayName != nil || before.Reason != nil {
		t.Errorf("GET before freezing = %+v, want not frozen, every freeze field null", before)
	}

	var got restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"  an incident: hold every automatic action  "}`), &got, token); status != http.StatusOK {
		t.Fatalf("freeze status = %d, want 200", status)
	}
	if !got.Frozen || got.FrozenAt == nil || got.FrozenByUserId == nil || *got.FrozenByUserId != admin.ID.String() ||
		got.FrozenByDisplayName == nil || *got.FrozenByDisplayName != admin.DisplayName || got.Reason == nil || *got.Reason != "an incident: hold every automatic action" {
		t.Errorf("freeze response = %+v, want frozen by %s with the trimmed reason", got, admin.ID.String())
	}
	if !frozenInDB(ctx, t, rig) {
		t.Fatal("platform_settings is not frozen after the freeze")
	}
	var read restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodGet, "/api/autonomy", nil, &read, token); status != http.StatusOK || !read.Frozen || read.Reason == nil || *read.Reason != "an incident: hold every automatic action" {
		t.Errorf("GET after freezing: status %d, %+v; want the freeze", status, read)
	}
	details, actors := autonomyAuditRows(ctx, t, rig, "autonomy.frozen")
	if len(details) != 1 || details[0]["reason"] != "an incident: hold every automatic action" || actors[0] != admin.ID {
		t.Errorf("autonomy.frozen rows = %v by %v, want one with the reason, by the admin", details, actors)
	}

	var lifted restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/unfreeze", nil, &lifted, token); status != http.StatusOK {
		t.Fatalf("unfreeze status = %d, want 200", status)
	}
	if lifted.Frozen || lifted.FrozenAt != nil || lifted.Reason != nil {
		t.Errorf("unfreeze response = %+v, want not frozen", lifted)
	}
	if frozenInDB(ctx, t, rig) {
		t.Error("platform_settings is still frozen after the unfreeze")
	}
}

// TestFreezeAutonomy_AdminOnly: freezing and unfreezing are admin only
// (authz.ActionManageAutonomyFreeze, §13.3) -- 403 for a maintainer, a
// member and a viewer, each way, and a refused request writes nothing.
func TestFreezeAutonomy_AdminOnly(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		_, token := createUserWithRole(ctx, t, rig, role)
		if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"not mine to freeze"}`), nil, token); status != http.StatusForbidden {
			t.Errorf("freeze as %s: status = %d, want 403", role, status)
		}
		if frozenInDB(ctx, t, rig) {
			t.Fatalf("a refused freeze as %s froze autonomy", role)
		}
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"the admin's freeze"}`), nil, adminToken); status != http.StatusOK {
		t.Fatalf("admin freeze status = %d, want 200", status)
	}
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		_, token := createUserWithRole(ctx, t, rig, role)
		if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/unfreeze", nil, nil, token); status != http.StatusForbidden {
			t.Errorf("unfreeze as %s: status = %d, want 403", role, status)
		}
		if !frozenInDB(ctx, t, rig) {
			t.Fatalf("a refused unfreeze as %s lifted the freeze", role)
		}
	}
	if details, _ := autonomyAuditRows(ctx, t, rig, "autonomy.frozen"); len(details) != 1 {
		t.Errorf("%d autonomy.frozen rows, want the admin's alone", len(details))
	}
	if details, _ := autonomyAuditRows(ctx, t, rig, "autonomy.unfrozen"); len(details) != 0 {
		t.Errorf("%d autonomy.unfrozen rows after refused unfreezes, want none", len(details))
	}
}

// TestFreezeAutonomy_RequiresReason: the reason is required, trimmed, at
// most 500 characters, and holds no NUL character (which a Postgres TEXT
// column would refuse with a 500) -- each refusal says which, and none
// writes anything; a 500-character reason is accepted.
func TestFreezeAutonomy_RequiresReason(t *testing.T) {
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
		{"a NUL character", `{"reason":"frozen \u0000 for an incident"}`, "reason must not contain a NUL byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]string
			if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(tc.body), &body, token); status != http.StatusBadRequest || body["error"] != tc.want {
				t.Errorf("status = %d body = %v, want 400 %q", status, body, tc.want)
			}
			if frozenInDB(ctx, t, rig) {
				t.Fatal("a refused freeze froze autonomy")
			}
		})
	}
	if details, _ := autonomyAuditRows(ctx, t, rig, "autonomy.frozen"); len(details) != 0 {
		t.Fatalf("%d autonomy.frozen rows after refused freezes, want none", len(details))
	}
	var accepted restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"`+strings.Repeat("é", 500)+`"}`), &accepted, token); status != http.StatusOK || !accepted.Frozen {
		t.Errorf("a 500-character reason: status = %d, frozen %v; want 200 frozen", status, accepted.Frozen)
	}
}

// TestFreezeAutonomy_SecondFreeze409KeepsFirst: a second freeze is refused
// and keeps the first freeze's who, when and why, with no second audit
// row.
func TestFreezeAutonomy_SecondFreeze409KeepsFirst(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	first, firstToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	_, secondToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var firstFreeze restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"the first reason"}`), &firstFreeze, firstToken); status != http.StatusOK {
		t.Fatalf("first freeze status = %d, want 200", status)
	}
	var body map[string]string
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"the second reason"}`), &body, secondToken); status != http.StatusConflict || body["error"] != "autonomy is already frozen" {
		t.Fatalf("second freeze: status = %d body = %v, want 409 already frozen", status, body)
	}
	var read restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodGet, "/api/autonomy", nil, &read, secondToken); status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", status)
	}
	if read.Reason == nil || *read.Reason != "the first reason" || read.FrozenByUserId == nil || *read.FrozenByUserId != first.ID.String() ||
		read.FrozenAt == nil || firstFreeze.FrozenAt == nil || !read.FrozenAt.Equal(*firstFreeze.FrozenAt) {
		t.Errorf("GET after a second freeze = %+v, want the first freeze kept", read)
	}
	if details, _ := autonomyAuditRows(ctx, t, rig, "autonomy.frozen"); len(details) != 1 {
		t.Errorf("%d autonomy.frozen rows, want the first alone", len(details))
	}
}

// failAuditInserts makes every audit_log insert of action fail, for the
// rest of the test: the same-transaction proof's injected failure.
func failAuditInserts(ctx context.Context, t *testing.T, rig testRig, action string) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION autonomy_freeze_test_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`,
		`CREATE TRIGGER autonomy_freeze_test_fail_audit BEFORE INSERT ON audit_log FOR EACH ROW WHEN (NEW.action = '` + action + `') EXECUTE FUNCTION autonomy_freeze_test_fail_audit()`,
	} {
		if _, err := rig.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject the audit failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = rig.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS autonomy_freeze_test_fail_audit ON audit_log`)
		_, _ = rig.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS autonomy_freeze_test_fail_audit()`)
	})
}

// TestFreezeAutonomy_AuditInTheSameTransaction: the change and its audit
// row commit together (§13.3). An audit insert that fails fails the
// request and leaves the freeze as it was -- a freeze is never set, nor
// lifted, without its row.
func TestFreezeAutonomy_AuditInTheSameTransaction(t *testing.T) {
	t.Run("freeze", func(t *testing.T) {
		rig := newTestRig(t)
		ctx := context.Background()
		_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
		failAuditInserts(ctx, t, rig, "autonomy.frozen")

		if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"an incident"}`), nil, token); status != http.StatusInternalServerError {
			t.Fatalf("freeze with a failing audit: status = %d, want 500", status)
		}
		if frozenInDB(ctx, t, rig) {
			t.Fatal("autonomy is frozen although its autonomy.frozen row was never written")
		}
	})
	t.Run("unfreeze", func(t *testing.T) {
		rig := newTestRig(t)
		ctx := context.Background()
		_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
		if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"an incident"}`), nil, token); status != http.StatusOK {
			t.Fatalf("freeze status = %d, want 200", status)
		}
		failAuditInserts(ctx, t, rig, "autonomy.unfrozen")

		if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/unfreeze", nil, nil, token); status != http.StatusInternalServerError {
			t.Fatalf("unfreeze with a failing audit: status = %d, want 500", status)
		}
		if !frozenInDB(ctx, t, rig) {
			t.Fatal("the freeze was lifted although its autonomy.unfrozen row was never written")
		}
	})
}

// TestUnfreezeAutonomy_Idle409: lifting a freeze when nothing is frozen is
// refused, and audits nothing.
func TestUnfreezeAutonomy_Idle409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var body map[string]string
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/unfreeze", nil, &body, token); status != http.StatusConflict || body["error"] != "autonomy is not frozen" {
		t.Fatalf("unfreeze: status = %d body = %v, want 409 not frozen", status, body)
	}
	if details, _ := autonomyAuditRows(ctx, t, rig, "autonomy.unfrozen"); len(details) != 0 {
		t.Errorf("%d autonomy.unfrozen rows, want none", len(details))
	}
}

// TestUnfreezeAutonomy_AuditNamesWhatWasLifted: the autonomy.unfrozen row
// is the unfreezing admin's, and names the freeze it lifted -- when it was
// set, by whom, why -- and how long it held.
func TestUnfreezeAutonomy_AuditNamesWhatWasLifted(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	freezer, freezerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	lifter, lifterToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"a bad deploy"}`), nil, freezerToken); status != http.StatusOK {
		t.Fatalf("freeze status = %d, want 200", status)
	}
	// The freeze has held for an hour and a half.
	if _, err := rig.pool.Exec(ctx, `UPDATE platform_settings SET autonomy_frozen_at = now() - interval '90 minutes' WHERE id = 1`); err != nil {
		t.Fatalf("age the freeze: %v", err)
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/unfreeze", nil, nil, lifterToken); status != http.StatusOK {
		t.Fatalf("unfreeze status = %d, want 200", status)
	}

	details, actors := autonomyAuditRows(ctx, t, rig, "autonomy.unfrozen")
	if len(details) != 1 || actors[0] != lifter.ID {
		t.Fatalf("autonomy.unfrozen rows = %v by %v, want one, by the unfreezing admin", details, actors)
	}
	d := details[0]
	if d["reason"] != "a bad deploy" || d["frozen_by"] != freezer.ID.String() || d["frozen_at"] == nil {
		t.Errorf("unfrozen detail = %v, want what was lifted: reason, frozen_by %s, frozen_at", d, freezer.ID.String())
	}
	seconds, ok := d["frozen_seconds"].(float64)
	if !ok || seconds < 90*60-60 || seconds > 90*60+60 {
		t.Errorf("frozen_seconds = %v, want about 5400 (90 minutes)", d["frozen_seconds"])
	}
}

// TestUnfreezeAutonomy_FrozenSecondsOnTheDatabaseClock: how long a freeze
// held is measured on the database's clock at both ends, as the audit
// rows' own created_at is, never on a replica's. Here the database's clock
// runs an hour behind this process's -- now() resolves, through the
// routes' pool's search_path, to a function an hour behind pg_catalog's --
// so a duration taken against this process's clock would read about 3600
// seconds for a freeze held a moment.
func TestUnfreezeAutonomy_FrozenSecondsOnTheDatabaseClock(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	_, connStr := httpapi.IntegrationTestPoolAndConnStr(t)

	for _, stmt := range []string{
		`CREATE SCHEMA autonomy_test_skew`,
		`CREATE FUNCTION autonomy_test_skew.now() RETURNS timestamptz LANGUAGE sql STABLE AS $$ SELECT pg_catalog.now() - interval '1 hour' $$`,
	} {
		if _, err := rig.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("skew the database clock: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = rig.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS autonomy_test_skew CASCADE`) })
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["search_path"] = "autonomy_test_skew, pg_catalog, public"
	skewed, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the skewed pool: %v", err)
	}
	t.Cleanup(skewed.Close)

	settings := narvipg.NewPlatformSettingsStore(skewed)
	router := chi.NewRouter()
	router.Route("/api/autonomy", func(r chi.Router) {
		r.Use(auth.Middleware(narvipg.NewUserSessionStore(skewed), narvipg.NewUserStore(skewed)))
		r.Post("/freeze", httpapi.PostFreezeAutonomy(skewed, settings, narvipg.NewAuditLogStore(skewed)))
		r.Post("/unfreeze", httpapi.PostUnfreezeAutonomy(skewed, settings, narvipg.NewAuditLogStore(skewed)))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	post := func(path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	resp := post("/api/autonomy/freeze", `{"reason":"an incident on a skewed clock"}`)
	var frozen restdtos.AutonomyFreeze
	if err := json.NewDecoder(resp.Body).Decode(&frozen); err != nil {
		t.Fatalf("decode the freeze: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || frozen.FrozenAt == nil || time.Since(*frozen.FrozenAt) < 50*time.Minute {
		t.Fatalf("freeze: status %d, frozen at %v; want 200, stamped about an hour behind this process's clock", resp.StatusCode, frozen.FrozenAt)
	}
	resp = post("/api/autonomy/unfreeze", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unfreeze: status %d, want 200", resp.StatusCode)
	}

	details, _ := autonomyAuditRows(ctx, t, rig, "autonomy.unfrozen")
	if len(details) != 1 {
		t.Fatalf("autonomy.unfrozen rows = %v, want one", details)
	}
	if seconds, ok := details[0]["frozen_seconds"].(float64); !ok || seconds < 0 || seconds > 60 {
		t.Errorf("frozen_seconds = %v, want the moment the freeze held on the database's clock, not an hour measured against this process's", details[0]["frozen_seconds"])
	}
}

// TestGetAutonomyFreeze_EveryRoleReads: every signed-in role reads the
// freeze in force, the same body -- the banner every role's inbox shows --
// and an unauthenticated request is refused.
func TestGetAutonomyFreeze_EveryRoleReads(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	if status := rig.doJSON(t, http.MethodPost, "/api/autonomy/freeze", []byte(`{"reason":"an incident"}`), nil, adminToken); status != http.StatusOK {
		t.Fatalf("freeze status = %d, want 200", status)
	}

	var want restdtos.AutonomyFreeze
	if status := rig.doJSON(t, http.MethodGet, "/api/autonomy", nil, &want, adminToken); status != http.StatusOK || !want.Frozen {
		t.Fatalf("GET as admin: status %d, %+v; want 200 frozen", status, want)
	}
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		_, token := createUserWithRole(ctx, t, rig, role)
		var got restdtos.AutonomyFreeze
		if status := rig.doJSON(t, http.MethodGet, "/api/autonomy", nil, &got, token); status != http.StatusOK {
			t.Fatalf("GET as %s: status = %d, want 200", role, status)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("GET as %s = %s, want what the admin reads, %s", role, gotJSON, wantJSON)
		}
	}
	if status := rig.doJSON(t, http.MethodGet, "/api/autonomy", nil, nil, ""); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET: status = %d, want 401", status)
	}
}

// TestFreeze_MergeClickMerges is §40.2's "a human command still works" at
// the decision inbox: while autonomy is frozen, a ready_to_merge pull
// request in a repository with auto-merge armed is listed, marked held,
// under the banner -- and a person's Merge click on it merges it, the
// freeze never read on that path.
func TestFreeze_MergeClickMerges(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1310"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{{
			Owner: "acme", Repo: "widgets", Number: 1310, Title: "low risk", HTMLURL: htmlURL,
			HeadSHA: "headsha1310", BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, Assignees: []ports.PRPerson{{ExternalID: "9310", Login: "octocat"}},
			CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now(),
		}},
		mergeSHA: "merged-while-frozen",
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9310")
	rig.markPlatformAuthored(ctx, t, user.ID, htmlURL)
	rig.seedAutoApprovedVerdict(ctx, t, "acme/widgets", 1310, "headsha1310")
	repoSettings := narvipg.NewRepoSettingsStore(rig.pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, "acme/widgets", true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}
	if _, err := repoSettings.UpsertAutoMergeToggle(ctx, "acme/widgets", true); err != nil {
		t.Fatalf("arm auto-merge: %v", err)
	}
	admin, _ := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleAdmin)
	if _, err := narvipg.NewPlatformSettingsStore(rig.pool).Freeze(ctx, admin.ID, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}

	var inbox restdtos.ListDecisionInboxResponse
	if status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, &inbox, token); status != http.StatusOK {
		t.Fatalf("GET inbox status = %d, want 200", status)
	}
	if !inbox.AutonomyFreeze.Frozen || inbox.AutonomyFreezeUnread || inbox.AutonomyFreeze.Reason == nil || *inbox.AutonomyFreeze.Reason != "an incident: hold every automatic action" {
		t.Fatalf("inbox freeze = %+v (unread %v), want the freeze for the banner", inbox.AutonomyFreeze, inbox.AutonomyFreezeUnread)
	}
	if inbox.HeldWorkflowAdvances == nil {
		t.Error("heldWorkflowAdvances is null, want an empty list")
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Kind != restdtos.DecisionInboxItemKindReadyToMerge || !inbox.Items[0].HeldByFreeze {
		t.Fatalf("inbox items = %+v, want the pull request listed ready_to_merge and held", inbox.Items)
	}

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: "acme/widgets", PrNumber: 1310})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got restdtos.MergePullRequestResponse
	if status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, &got, token); status != http.StatusOK {
		t.Fatalf("merge while frozen: status = %d, want 200: a person's merge is never held", status)
	}
	if !got.Merged || len(fakeSCM.mergeCalls) != 1 {
		t.Errorf("merge response = %+v, MergePR calls %d; want one merge", got, len(fakeSCM.mergeCalls))
	}
}
