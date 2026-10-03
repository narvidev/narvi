//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/framecut"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves §8.1's ("plan mode, web", §8.1/§12.2 item 3) two new
// REST endpoints -- POST .../plans/:planId/approve and .../reject -- and
// their shared authorization stopgap (planauthz.go's canActOnPlan).

// createUserWithRole is createAuthenticatedUser's own role-parameterized
// twin -- this file's own authorization tests need a real admin/maintainer/
// viewer, not just createAuthenticatedUser's hardcoded member.
func createUserWithRole(ctx context.Context, t *testing.T, r testRig, role sqlcgen.UserRole) (sqlcgen.User, string) {
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

// createSessionForUser creates a session owned (created_by) by ownerID,
// optionally with buildModelID set -- this file's own tests need a real
// owner to exercise canActOnPlan's "own session" branch, and a real
// build_model_id to prove ApprovePlan's new turn actually carries it.
//
// buildEffort (H2 fix, §29.8) is a trailing, variadic *string -- mirroring
// this codebase's own established idiom for adding one new optional value
// to a widely-called constructor without touching every existing call site
// (e.g. internal/adapters/outbound/opencode.New's own trailing
// `capabilityRestricted ...bool`, adapter.go). Every one of this helper's
// ~50 pre-existing callers omits it entirely and keeps compiling and
// behaving identically (sessions.build_effort left NULL); only
// TestApprovePlan_Owner_HappyPath (this file, proving H2's own fix) passes
// one explicitly.
func createSessionForUser(ctx context.Context, t *testing.T, r testRig, ownerID pgtype.UUID, buildModelID *string, buildEffort ...*string) sqlcgen.Session {
	t.Helper()
	var effort *string
	if len(buildEffort) > 0 {
		effort = buildEffort[0]
	}
	row, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource:  sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:    ownerID,
		BuildModelID: buildModelID,
		BuildEffort:  effort,
	})
	if err != nil {
		t.Fatalf("create test session for user: %v", err)
	}
	return row
}

// seedAwaitingApprovalPlan seeds a producing turn (Completed, plan_mode
// true) and an awaiting_approval plans row atop it, at the given version
// -- a direct DB seed (bypassing the actor pipeline entirely), matching
// internal/app/sessionactor's own established "surgical direct DB seed"
// precedent (createProcessingTurn) exactly: these tests exist to prove
// ApprovePlan/RejectPlan's OWN behavior reacting to an existing plan row,
// not planrecord.go's own creation logic (already covered by
// planrecord_integration_test.go).
func seedAwaitingApprovalPlan(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, version int32) sqlcgen.Plan {
	t.Helper()
	turn, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusCompleted,
		PlanMode:  true,
	})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := r.plans.Create(ctx, sqlcgen.CreatePlanParams{
		SessionID: sessionID,
		TurnID:    turn.ID,
		Version:   version,
		Status:    sqlcgen.PlanStatusAwaitingApproval,
	})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}
	return plan
}

// --- ApprovePlan: happy path ---

// TestApprovePlan_Owner_HappyPath proves the session's own creator can
// approve its awaiting plan: 200, the plan flips to approved with
// decided_by/decided_at set, and a new Pending, plan_mode=false turn is
// enqueued carrying the session's own build_model_id.
func TestApprovePlan_Owner_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)

	buildModel := "anthropic/claude-sonnet-5"
	buildEffort := "high"
	session := createSessionForUser(ctx, t, rig, owner.ID, &buildModel, &buildEffort)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	var got planActionResponseForTest
	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Status != "approved" {
		t.Errorf("Status = %q, want %q", got.Status, "approved")
	}
	if got.TurnID == nil || *got.TurnID == "" {
		t.Fatal("TurnID is nil/empty, want the new implementation turn's id")
	}

	var dbStatus sqlcgen.PlanStatus
	var decidedBy pgtype.UUID
	var decidedAt pgtype.Timestamptz
	if err := rig.pool.QueryRow(ctx,
		`SELECT status, decided_by, decided_at FROM plans WHERE id = $1`, plan.ID,
	).Scan(&dbStatus, &decidedBy, &decidedAt); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusApproved {
		t.Errorf("db status = %q, want %q", dbStatus, sqlcgen.PlanStatusApproved)
	}
	if !decidedBy.Valid || decidedBy != owner.ID {
		t.Errorf("decided_by = %v, want %v", decidedBy, owner.ID)
	}
	if !decidedAt.Valid {
		t.Error("decided_at is NULL, want set")
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	// The seeded producing turn (Completed) + the new implementation turn.
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2", len(turns))
	}
	var implTurn *sqlcgen.Turn
	for i := range turns {
		if turns[i].ID.String() == *got.TurnID {
			implTurn = &turns[i]
		}
	}
	if implTurn == nil {
		t.Fatal("the new implementation turn id from the response was not found among the session's own turns")
	}
	if implTurn.Status != sqlcgen.TurnStatusPending {
		t.Errorf("new turn status = %q, want %q", implTurn.Status, sqlcgen.TurnStatusPending)
	}
	if implTurn.PlanMode {
		t.Error("new turn PlanMode = true, want false (this is the IMPLEMENTATION turn)")
	}
	if implTurn.ModelID == nil || *implTurn.ModelID != buildModel {
		t.Errorf("new turn ModelID = %v, want %q (the session's own build_model_id)", implTurn.ModelID, buildModel)
	}
	// H2 (adversarial review, §29.8): sessions.build_effort must land on
	// this turn's own effort exactly like build_model_id -> ModelID does
	// immediately above -- before the fix, decideplan.go's own
	// CreateTurnParams literal omitted Effort entirely, so this turn's
	// effort was always NULL regardless of the session's build_effort.
	if implTurn.Effort == nil || *implTurn.Effort != buildEffort {
		t.Errorf("new turn Effort = %v, want %q (the session's own build_effort)", implTurn.Effort, buildEffort)
	}
	if implTurn.Prompt == nil || *implTurn.Prompt == "" {
		t.Error("new turn Prompt is nil/empty, want the fixed implementation prompt")
	}
}

// TestApprovePlan_ConcurrentDoubleApprove_ExactlyOneWins proves "first
// verdict wins" is a real, race-safe DB guarantee, not an application
// convention: firing two concurrent approve requests at the SAME
// awaiting_approval plan must produce exactly one 200 and one 409 --
// mirroring §5.1/§8.3's own errgroup-based concurrency-test pattern
// (internal/app/sessionactor/registry_integration_test.go).
func TestApprovePlan_ConcurrentDoubleApprove_ExactlyOneWins(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	path := "/api/sessions/" + session.ID.String() + "/plans/" + plan.ID.String() + "/approve"

	var eg errgroup.Group
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		eg.Go(func() error {
			statuses <- rig.doJSON(t, http.MethodPost, path, []byte{}, nil, token)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(statuses)

	var ok, conflict int
	for s := range statuses {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d among concurrent approve responses", s)
		}
	}
	if ok != 1 {
		t.Errorf("ok = %d, want exactly 1", ok)
	}
	if conflict != 1 {
		t.Errorf("conflict = %d, want exactly 1", conflict)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	// The seeded producing turn + exactly ONE implementation turn -- the
	// loser must never have inserted a second one.
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want exactly 2 (the losing approve must not have created a second implementation turn)", len(turns))
	}
}

// TestApprovePlan_AlreadyDecided_Returns409 proves an approve against a
// plan that is already approved/rejected/superseded gets 409, not a
// silent success or a 500.
func TestApprovePlan_AlreadyDecided_Returns409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	if _, err := rig.pool.Exec(ctx, `UPDATE plans SET status = 'rejected' WHERE id = $1`, plan.ID); err != nil {
		t.Fatalf("seed already-decided plan: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d", status, http.StatusConflict)
	}
}

// TestApprovePlan_OpenTurnInFlight_Returns409 proves ApprovePlan's own
// hasOpenTurn gate (planapprove.go's top doc comment): a plan can be
// genuinely 'awaiting_approval' in the DB while a "request changes" turn
// for it is already Pending/Dispatched/Processing (that turn hasn't
// completed yet, so it hasn't superseded this plan row yet either) --
// approving it in that window must be refused with 409, not silently
// succeed and enqueue a second (implementation) turn behind the in-flight
// revision's back.
func TestApprovePlan_OpenTurnInFlight_Returns409(t *testing.T) {
	tests := []struct {
		name   string
		status sqlcgen.TurnStatus
	}{
		{name: "revision turn pending", status: sqlcgen.TurnStatusPending},
		{name: "revision turn dispatched", status: sqlcgen.TurnStatusDispatched},
		{name: "revision turn processing", status: sqlcgen.TurnStatusProcessing},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			owner, token := rig.createAuthenticatedUser(ctx, t)
			session := createSessionForUser(ctx, t, rig, owner.ID, nil)
			plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

			// Simulate a "request changes" turn already in flight for this
			// SAME session -- v1 is still 'awaiting_approval' at this point
			// (recordPlanIfNeeded only supersedes it once THIS turn actually
			// completes), exactly the window this gate must close.
			if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
				SessionID: session.ID,
				Status:    tc.status,
				PlanMode:  true,
			}); err != nil {
				t.Fatalf("seed in-flight revision turn: %v", err)
			}

			status := rig.doJSON(t, http.MethodPost,
				"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
			if status != http.StatusConflict {
				t.Errorf("status = %d, want %d", status, http.StatusConflict)
			}

			var dbStatus sqlcgen.PlanStatus
			if err := rig.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
				t.Fatalf("query plan row: %v", err)
			}
			if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
				t.Errorf("db status = %q, want %q (a 409 must never change plan state)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
			}

			turns, err := rig.turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}
			// The seeded producing turn + the seeded in-flight revision turn
			// -- NO implementation turn must have been created.
			if len(turns) != 2 {
				t.Errorf("len(turns) = %d, want exactly 2 (no implementation turn must be created while another turn is in flight)", len(turns))
			}
		})
	}
}

// --- RejectPlan: happy path ---

// TestRejectPlan_Owner_HappyPath proves reject flips the plan to rejected
// and creates NO new turn.
func TestRejectPlan_Owner_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	var got planActionResponseForTest
	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/reject", []byte{}, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Status != "rejected" {
		t.Errorf("Status = %q, want %q", got.Status, "rejected")
	}
	if got.TurnID != nil {
		t.Errorf("TurnID = %v, want nil (reject never dispatches a new turn)", *got.TurnID)
	}

	var dbStatus sqlcgen.PlanStatus
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusRejected {
		t.Errorf("db status = %q, want %q", dbStatus, sqlcgen.PlanStatusRejected)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn, no new one)", len(turns))
	}
}

// --- Observability: REST success log line (audit-fix batch, M2 part 2) ---

// captureDefaultLoggerJSON temporarily replaces slog.Default() with a JSON
// handler writing into a *bytes.Buffer, restoring the original on cleanup
// -- mirrors internal/app/outboxworker/builder_integration_test.go's own
// established "capture slog.Default() into a buffer" convention exactly
// (that package's own TestPumpOnce_AttemptLogsCorrelationIDAndSessionID),
// since platform.Logger(ctx) (what every httpapi handler actually calls)
// is itself built on top of slog.Default().
func captureDefaultLoggerJSON(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })
	return &buf
}

// findLogEntry scans buf's own newline-delimited JSON log lines for the
// first one whose "msg" field equals wantMsg -- fails the test if none is
// found.
func findLogEntry(t *testing.T, buf *bytes.Buffer, wantMsg string) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == wantMsg {
			return entry
		}
	}
	t.Fatalf("no log line with msg %q found; full log output:\n%s", wantMsg, buf.String())
	return nil
}

// TestApprovePlan_HappyPath_LogsDecidedPlanSuccessLine proves the audit-fix
// batch's own M2 part 2 fix: ApprovePlan (a REST handler that calls
// DecidePlanOnTx directly, never DecidePlan's own pool-based wrapper --
// previously the ONLY place that logged this line) now logs an equivalent
// "httpapi: decided plan" success line after its own commit, with the same
// field shape (plan_id/session_id/verdict/won/final_status) DecidePlan's
// wrapper already uses.
func TestApprovePlan_HappyPath_LogsDecidedPlanSuccessLine(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	buf := captureDefaultLoggerJSON(t)

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	entry := findLogEntry(t, buf, "httpapi: decided plan")
	if got, _ := entry["plan_id"].(string); got != plan.ID.String() {
		t.Errorf("plan_id = %q, want %q", got, plan.ID.String())
	}
	if got, _ := entry["session_id"].(string); got != session.ID.String() {
		t.Errorf("session_id = %q, want %q", got, session.ID.String())
	}
	if got, _ := entry["verdict"].(string); got != "approve" {
		t.Errorf("verdict = %q, want %q", got, "approve")
	}
	if got, ok := entry["won"].(bool); !ok || !got {
		t.Errorf("won = %v, want true", entry["won"])
	}
	if got, _ := entry["final_status"].(string); got != "approved" {
		t.Errorf("final_status = %q, want %q", got, "approved")
	}
}

// TestRejectPlan_HappyPath_LogsDecidedPlanSuccessLine is the reject twin of
// the approve case above.
func TestRejectPlan_HappyPath_LogsDecidedPlanSuccessLine(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	buf := captureDefaultLoggerJSON(t)

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/reject", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	entry := findLogEntry(t, buf, "httpapi: decided plan")
	if got, _ := entry["plan_id"].(string); got != plan.ID.String() {
		t.Errorf("plan_id = %q, want %q", got, plan.ID.String())
	}
	if got, _ := entry["verdict"].(string); got != "reject" {
		t.Errorf("verdict = %q, want %q", got, "reject")
	}
	if got, ok := entry["won"].(bool); !ok || !got {
		t.Errorf("won = %v, want true", entry["won"])
	}
	if got, _ := entry["final_status"].(string); got != "rejected" {
		t.Errorf("final_status = %q, want %q", got, "rejected")
	}
}

// --- Authorization ---

// TestApprovePlan_NonOwnerNonParticipantMember_Returns403 proves canActOnPlan's
// own negative case: a plain member who neither created nor joined the
// session cannot approve its plan.
func TestApprovePlan_NonOwnerNonParticipantMember_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, outsiderToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, outsiderToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}

	var dbStatus sqlcgen.PlanStatus
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (a 403 must never change plan state)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}
}

// TestRejectPlan_NonOwnerNonParticipantMember_Returns403 is the reject
// twin of the approve case above.
func TestRejectPlan_NonOwnerNonParticipantMember_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, outsiderToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/reject", []byte{}, nil, outsiderToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestApprovePlan_Maintainer_NotOwnerOrParticipant_Allowed proves §13.3's
// own "approve any plan" row: a maintainer who neither created nor joined
// the session can still approve it.
func TestApprovePlan_Maintainer_NotOwnerOrParticipant_Allowed(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, maintainerToken)
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d (maintainer may approve any plan)", status, http.StatusOK)
	}
}

// TestApprovePlan_Viewer_NotOwnerOrParticipant_Returns403 proves the
// viewer role never gets an implicit pass -- §13.3: "viewer: —".
func TestApprovePlan_Viewer_NotOwnerOrParticipant_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, viewerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleViewer)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, viewerToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// --- Request changes: reuse of the existing /turns endpoint ---

// TestRequestChanges_ViaExistingTurnsEndpoint_SupersedesAndCreatesV2
// proves the design decision this Step's own brief calls for explicitly:
// "Request changes" reuses the EXISTING POST .../turns endpoint AS-IS
// (planMode=true, feedback text as prompt) rather than a dedicated new
// endpoint. A session already has an awaiting_approval plan v1; a new
// turn is submitted through that same existing endpoint with
// planMode=true; once THAT turn completes (driven through the real
// sessionactor Actor, mirroring internal/app/sessionactor's own
// established direct-completion-drive precedent), v1 is superseded and a
// v2 plan is created.
func TestRequestChanges_ViaExistingTurnsEndpoint_SupersedesAndCreatesV2(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	v1 := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	if _, err := narvipg.NewSandboxStore(rig.pool).Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Submit the "request changes" turn via the EXISTING /turns endpoint
	// (turn.go's own CreateTurn, reused as-is -- not a new endpoint).
	body := []byte(`{"prompt": "keep the env fallback", "modelId": null,"effort":null, "planMode": true}`)
	var createdTurn restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", body, &createdTurn, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if createdTurn.Status != restdtos.CreateTurnResponseStatusPending {
		t.Fatalf("created turn status = %q, want %q", createdTurn.Status, restdtos.CreateTurnResponseStatusPending)
	}

	var turnID pgtype.UUID
	if err := turnID.Scan(createdTurn.Id); err != nil {
		t.Fatalf("scan turn id: %v", err)
	}

	// Drive the new turn to Processing then completion the SAME way
	// internal/app/sessionactor's own tests do (a direct DB seed of the
	// pre-completion state, then a real execution_complete through the
	// real Actor) -- this rig's own registry has a nil provider/commander
	// (newTestRig's own doc comment), so the real spawn/dispatch network
	// path is deliberately not exercised here; only completeProcessingTurn's
	// own reaction to a genuine execution_complete is under test.
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET status = 'processing' WHERE id = $1`, turnID); err != nil {
		t.Fatalf("seed turn as processing: %v", err)
	}

	actor, err := rig.registry.GetOrSpawn(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	evt := sessionactor.SandboxEvent{
		Type:  "execution_complete",
		Gen:   1,
		Raw:   executionCompleteRawForTest(t, session.ID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
		Reply: reply,
	}
	if err := actor.Send(ctx, evt); err != nil {
		t.Fatalf("Send execution_complete: %v", err)
	}
	select {
	case <-reply:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SandboxEventOutcome")
	}

	var v1Status sqlcgen.PlanStatus
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, v1.ID).Scan(&v1Status); err != nil {
		t.Fatalf("query v1 plan row: %v", err)
	}
	if v1Status != sqlcgen.PlanStatusSuperseded {
		t.Errorf("v1 status = %q, want %q", v1Status, sqlcgen.PlanStatusSuperseded)
	}

	rows, err := rig.pool.Query(ctx, `SELECT version, status, turn_id FROM plans WHERE session_id = $1 ORDER BY version`, session.ID)
	if err != nil {
		t.Fatalf("query plans: %v", err)
	}
	defer rows.Close()
	var versions []int32
	var v2TurnID pgtype.UUID
	var v2Status sqlcgen.PlanStatus
	for rows.Next() {
		var version int32
		var status sqlcgen.PlanStatus
		var tid pgtype.UUID
		if err := rows.Scan(&version, &status, &tid); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		versions = append(versions, version)
		if version == 2 {
			v2Status = status
			v2TurnID = tid
		}
	}
	if len(versions) != 2 {
		t.Fatalf("plan row count = %d, want 2 (v1 superseded + v2 awaiting_approval)", len(versions))
	}
	if v2Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("v2 status = %q, want %q", v2Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if v2TurnID != turnID {
		t.Errorf("v2 turn_id = %v, want %v (the turn submitted via /turns)", v2TurnID, turnID)
	}
}

// executionCompleteRawForTest mirrors internal/app/sessionactor's own
// unexported executionCompleteRaw helper -- duplicated here (this file's
// own package, httpapi_test, cannot reach that unexported test helper
// across the package boundary), matching this codebase's own established
// per-package test-helper-duplication precedent (see turn_integration_
// test.go's own fakeTurnCommander doc comment for the identical
// rationale).
func executionCompleteRawForTest(t *testing.T, sessionID string, gen int, outcome sandboxws.ExecutionCompleteOutcome) []byte {
	t.Helper()
	evt := sandboxws.ExecutionComplete{
		Type:      "execution_complete",
		MessageId: "msg-" + sessionID,
		SessionId: sessionID,
		Gen:       gen,
		AckId:     "execution_complete:msg-" + sessionID,
		Outcome:   outcome,
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal execution_complete: %v", err)
	}
	return raw
}

// planActionResponseForTest mirrors restdtos.PlanActionResponse's own wire
// shape (contracts/rest/v1/dtos.schema.json) -- an audit-fix batch (M3/L14/
// L16) promoted planapprove.go's own former hand-written, unexported
// planActionResponse struct into that generated /contracts type, but this
// local mirror is kept anyway, purely as a test-side convenience: a plain
// string Status field here (no restdtos.PlanActionResponseStatus's own
// strict enum UnmarshalJSON to fight with when asserting on a response),
// mirroring members_integration_test.go's own identical "keep a local
// mirror even after the real DTO is promoted" precedent (see that file's
// own "Local wire-shape mirrors" doc comment) rather than switching every
// call site here to decode into restdtos.PlanActionResponse directly.
type planActionResponseForTest struct {
	PlanID string  `json:"planId"`
	Status string  `json:"status"`
	TurnID *string `json:"turnId"`
}

// --- ApprovePlan: a plan whose text was cut on its way from the sandbox ---

// TestApprovePlan_CutPlan_409WithTheReason proves the cut gate
// (ErrPlanCut, decideplan.go): a plan whose final text is a frame the
// sandbox-agent cut (technical plan §6.1) -- here keeping a complete
// plan-steps block at its start -- is refused with 409 and the reason, and
// nothing changes: the plan stays awaiting approval, with no snapshot, no
// implementation turn and no audit row. Reject stays open on it.
func TestApprovePlan_CutPlan_409WithTheReason(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	const total = 40960
	kept := len(cutPlanBlock) + 12
	cutText := cutPlanBlock + "Then wire t" + "h" + cutMarker(kept, total)
	plan := seedDispatchedPlan(ctx, t, rig, session.ID, 1, func() {
		seedCutTokenFrame(ctx, t, rig, session.ID, "prt_plan", "prt_plan", "", nil)
		seedCutTokenFrame(ctx, t, rig, session.ID, "prt_plan#cut", "prt_plan", cutText, map[string]int{"kept": kept, "total": total})
	})

	var body struct {
		Error string `json:"error"`
	}
	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, &body, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
	if want := framecut.Reason(&framecut.Cut{Kept: kept, Total: total}); body.Error != want {
		t.Errorf("error = %q, want the reason %q", body.Error, want)
	}
	if got := planStatusOf(ctx, t, rig, plan.ID); got != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("plan status = %q, want %q (a refused approval changes nothing)", got, sqlcgen.PlanStatusAwaitingApproval)
	}
	if _, err := rig.planDocuments.GetByPlanID(ctx, plan.ID); err == nil {
		t.Error("a plan_documents snapshot was written for a refused approval")
	}
	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("len(turns) = %d, want 1: no implementation turn for a refused approval", len(turns))
	}
	var audits int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE resource_id = $1`, plan.ID.String()).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 0 {
		t.Errorf("audit rows for the plan = %d, want 0", audits)
	}

	var rejected planActionResponseForTest
	if status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/reject", []byte{}, &rejected, token); status != http.StatusOK || rejected.Status != "rejected" {
		t.Errorf("reject after the refused approval: status %d %q, want 200 rejected", status, rejected.Status)
	}
}

// TestApprovePlan_ReadsTheCutFromTheFrameNeverTheText covers the two parts
// of the rule a reader learns a cut by -- only from `cut`, never from the
// text -- through the approve route: a part ending in a marker-shaped line
// and carrying no `cut` is the model's own text and is approved, its
// snapshot that text with its structured steps; and a part stored whole
// and then cut (a replay through a connection that read less) reads as
// the whole text, and is approved with the whole text snapshotted.
func TestApprovePlan_ReadsTheCutFromTheFrameNeverTheText(t *testing.T) {
	const whole = "The plan.\n\n" + cutPlanBlock + "Then the tests.\n"
	tests := []struct {
		name         string
		frames       func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID)
		wantSnapshot string
	}{
		{
			name: "a marker-shaped line without cut is text",
			frames: func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
				seedCutTokenFrame(ctx, t, rig, sessionID, "prt_plan", "prt_plan", "", nil)
				seedCutTokenFrame(ctx, t, rig, sessionID, "prt_plan#1", "prt_plan", whole+cutMarker(12, 40960), nil)
			},
			wantSnapshot: whole + cutMarker(12, 40960),
		},
		{
			name: "a cut of the stored whole text reads whole",
			frames: func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
				seedCutTokenFrame(ctx, t, rig, sessionID, "prt_plan", "prt_plan", "", nil)
				seedCutTokenFrame(ctx, t, rig, sessionID, "prt_plan#whole", "prt_plan", whole, nil)
				seedCutTokenFrame(ctx, t, rig, sessionID, "prt_plan#cut", "prt_plan", whole[:9]+cutMarker(9, len(whole)), map[string]int{"kept": 9, "total": len(whole)})
			},
			wantSnapshot: whole,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			owner, token := rig.createAuthenticatedUser(ctx, t)
			session := createSessionForUser(ctx, t, rig, owner.ID, nil)
			plan := seedDispatchedPlan(ctx, t, rig, session.ID, 1, func() { tt.frames(ctx, t, rig, session.ID) })

			var got planActionResponseForTest
			if status := rig.doJSON(t, http.MethodPost,
				"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, &got, token); status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
			if err != nil {
				t.Fatalf("GetByPlanID: %v -- the approval must have snapshotted the plan", err)
			}
			if doc.Content == nil || *doc.Content != tt.wantSnapshot {
				t.Errorf("snapshot content = %v, want %q", doc.Content, tt.wantSnapshot)
			}
			if doc.StructuredSteps == nil {
				t.Error("snapshot structured_steps is NULL, want the plan's steps")
			}
		})
	}
}

// TestApprovePlan_LaterTurnsFramesStayOutOfThePlan pins the upper bound of
// the window the approval reads a plan's text in (sessionactor.
// ReadPlanFinal, ending at the next dispatched turn): a request for changes
// that failed without producing a plan leaves v1 awaiting approval behind a
// later dispatched turn whose frames are in the log. Approving v1 reads v1's
// own text -- for the cut gate and for the snapshot -- never the later
// turn's, whole or cut.
func TestApprovePlan_LaterTurnsFramesStayOutOfThePlan(t *testing.T) {
	const v1Text = "The plan v1 proposed.\n\n" + cutPlanBlock
	tests := []struct {
		name      string
		laterText string
		laterCut  any
	}{
		{name: "the later turn's text is whole", laterText: "The failed revision's own partial text."},
		{name: "the later turn's text is cut", laterText: "The failed" + cutMarker(10, 40960), laterCut: map[string]int{"kept": 10, "total": 40960}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			owner, token := rig.createAuthenticatedUser(ctx, t)
			session := createSessionForUser(ctx, t, rig, owner.ID, nil)
			plan := seedDispatchedPlan(ctx, t, rig, session.ID, 1, func() {
				seedCutTokenFrame(ctx, t, rig, session.ID, "prt_v1", "prt_v1", v1Text, nil)
			})

			revision, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
			if err != nil {
				t.Fatalf("create the revision turn: %v", err)
			}
			dispatchTurn(ctx, t, rig, session.ID, revision.ID)
			seedCutTokenFrame(ctx, t, rig, session.ID, "prt_v2", "prt_v2", "", nil)
			seedCutTokenFrame(ctx, t, rig, session.ID, "prt_v2#2", "prt_v2", tt.laterText, tt.laterCut)
			if _, err := rig.pool.Exec(ctx, `UPDATE turns SET status = 'failed' WHERE id = $1`, revision.ID); err != nil {
				t.Fatalf("fail the revision turn: %v", err)
			}

			var got planActionResponseForTest
			if status := rig.doJSON(t, http.MethodPost,
				"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, &got, token); status != http.StatusOK {
				t.Fatalf("approve v1 = %d, want 200: the later turn's frames are not v1's", status)
			}
			doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
			if err != nil {
				t.Fatalf("GetByPlanID: %v", err)
			}
			if doc.Content == nil || *doc.Content != v1Text {
				t.Errorf("snapshot content = %v, want v1's own text %q", doc.Content, v1Text)
			}
			if doc.StructuredSteps == nil {
				t.Error("snapshot structured_steps is NULL, want v1's steps")
			}
		})
	}
}
