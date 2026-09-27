//go:build integration

// Integration tests for SessionStore.ActivityFacts (GetSessionActivityFacts,
// technical plan §43.20): every fact a session's live activity is derived
// from, read by one statement.
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestSessionActivityFacts_OneStatement seeds one session with every kind
// of fact the status route reads -- turns in five states, a sandbox, a
// plan awaiting approval, a workflow step awaiting a decision and an
// escalated run -- and proves the single ActivityFacts read returns each
// one: the per-state histogram, the in-flight turn, the newest terminal
// turn, the newest turn, each gate with the time it opened, the sandbox
// status and the session's own row. A session with no turn and no gate
// reads an empty histogram and no ids; an unknown session is
// pgx.ErrNoRows.
func TestSessionActivityFacts_OneStatement(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	workflows := narvipg.NewWorkflowStore(pool)

	sessionID := createTestSession(ctx, t, pool)

	// created_at is set explicitly so the order is unambiguous.
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	createTurn := func(status sqlcgen.TurnStatus, offset time.Duration) sqlcgen.Turn {
		t.Helper()
		row, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: status})
		if err != nil {
			t.Fatalf("create %s turn: %v", status, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE turns SET created_at = $2 WHERE id = $1`, row.ID, base.Add(offset)); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
		return row
	}
	completedAt := base.Add(15 * time.Minute)
	first := createTurn(sqlcgen.TurnStatusCompleted, 0)
	failed := createTurn(sqlcgen.TurnStatusFailed, time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE turns SET completed_at = $2 WHERE id = $1`, failed.ID, completedAt); err != nil {
		t.Fatal(err)
	}
	dispatchedAt := base.Add(20 * time.Minute)
	processing := createTurn(sqlcgen.TurnStatusProcessing, 2*time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_at = $2 WHERE id = $1`, processing.ID, dispatchedAt); err != nil {
		t.Fatal(err)
	}
	createTurn(sqlcgen.TurnStatusPending, 3*time.Minute)
	newest := createTurn(sqlcgen.TurnStatusPending, 4*time.Minute)

	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}

	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sessionID, TurnID: first.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	var defID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("activity-facts-%d", time.Now().UnixNano())).Scan(&defID); err != nil {
		t.Fatalf("insert workflow definition: %v", err)
	}
	var stepDefID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, hitl_after) VALUES ($1, 1, 'agent', '{{prompt}}', true) RETURNING id`, defID).Scan(&stepDefID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	// One run escalated for review, then -- at most one run per session is
	// ever running -- a second, running one whose step awaits a decision.
	escalatedRun, err := workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	escalated, err := workflows.EscalateRun(ctx, escalatedRun.ID)
	if err != nil {
		t.Fatalf("escalate run: %v", err)
	}
	gatedRun, err := workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create second run: %v", err)
	}
	stepRun, err := workflows.CreateStepRun(ctx, gatedRun.ID, stepDefID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	awaitingStep, err := workflows.MarkAwaitingDecision(ctx, stepRun.ID, "ok")
	if err != nil {
		t.Fatalf("mark step awaiting decision: %v", err)
	}

	before := time.Now().Add(-time.Minute)
	facts, err := sessions.ActivityFacts(ctx, sessionID)
	if err != nil {
		t.Fatalf("ActivityFacts: %v", err)
	}

	gotCounts := decodeJSONObject(t, facts.TurnCounts)
	wantCounts := map[string]any{"completed": float64(1), "failed": float64(1), "processing": float64(1), "pending": float64(2)}
	if fmt.Sprint(gotCounts) != fmt.Sprint(wantCounts) {
		t.Errorf("turn_counts = %v, want %v", gotCounts, wantCounts)
	}
	if facts.SessionID != sessionID || facts.SessionStatus != sqlcgen.SessionStatusCreated || facts.Archived {
		t.Errorf("session columns = (%v, %q, archived %v), want (%v, created, false)", facts.SessionID, facts.SessionStatus, facts.Archived, sessionID)
	}
	if facts.SandboxStatus == nil || *facts.SandboxStatus != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox_status = %v, want ready", facts.SandboxStatus)
	}
	if facts.InFlightTurnID != processing.ID || facts.InFlightTurnStatus != "processing" || !facts.InFlightDispatchedAt.Time.Equal(dispatchedAt) {
		t.Errorf("in-flight turn = (%v, %q, %v), want (%v, processing, %v)", facts.InFlightTurnID, facts.InFlightTurnStatus, facts.InFlightDispatchedAt.Time, processing.ID, dispatchedAt)
	}
	if facts.LastRunTurnID != failed.ID || facts.LastRunStatus != "failed" || !facts.LastRunCompletedAt.Time.Equal(completedAt) {
		t.Errorf("last run = (%v, %q, %v), want the newest terminal turn (%v, failed, %v)", facts.LastRunTurnID, facts.LastRunStatus, facts.LastRunCompletedAt.Time, failed.ID, completedAt)
	}
	if facts.NewestTurnID != newest.ID {
		t.Errorf("newest turn = %v, want %v", facts.NewestTurnID, newest.ID)
	}
	if facts.AwaitingPlanID != plan.ID || !facts.AwaitingPlanSince.Time.Equal(plan.CreatedAt.Time) {
		t.Errorf("awaiting plan = (%v, %v), want (%v, %v)", facts.AwaitingPlanID, facts.AwaitingPlanSince.Time, plan.ID, plan.CreatedAt.Time)
	}
	if facts.AwaitingStepID != awaitingStep.ID || !facts.AwaitingStepSince.Time.Equal(awaitingStep.UpdatedAt.Time) {
		t.Errorf("awaiting step = (%v, %v), want (%v, %v)", facts.AwaitingStepID, facts.AwaitingStepSince.Time, awaitingStep.ID, awaitingStep.UpdatedAt.Time)
	}
	if facts.EscalatedRunID != escalated.ID || !facts.EscalatedRunSince.Time.Equal(escalated.UpdatedAt.Time) {
		t.Errorf("escalated run = (%v, %v), want (%v, %v)", facts.EscalatedRunID, facts.EscalatedRunSince.Time, escalated.ID, escalated.UpdatedAt.Time)
	}
	if !facts.ObservedAt.Valid || facts.ObservedAt.Time.Before(before) {
		t.Errorf("observed_at = %v, want the statement's own time", facts.ObservedAt)
	}

	t.Run("a session with no turn and no gate", func(t *testing.T) {
		empty := createTestSession(ctx, t, pool)
		facts, err := sessions.ActivityFacts(ctx, empty)
		if err != nil {
			t.Fatalf("ActivityFacts: %v", err)
		}
		if got := decodeJSONObject(t, facts.TurnCounts); len(got) != 0 {
			t.Errorf("turn_counts = %v, want {}", got)
		}
		if facts.InFlightTurnID.Valid || facts.InFlightTurnStatus != "" || facts.LastRunTurnID.Valid || facts.LastRunStatus != "" || facts.NewestTurnID.Valid ||
			facts.AwaitingPlanID.Valid || facts.AwaitingStepID.Valid || facts.EscalatedRunID.Valid || facts.SandboxStatus != nil {
			t.Errorf("facts = %+v, want no turn, no gate, no sandbox", facts)
		}
	})

	t.Run("an unknown session", func(t *testing.T) {
		unknown := pgtype.UUID{Bytes: [16]byte{0xde, 0xad}, Valid: true}
		if _, err := sessions.ActivityFacts(ctx, unknown); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ActivityFacts(unknown) err = %v, want pgx.ErrNoRows", err)
		}
	})
}
