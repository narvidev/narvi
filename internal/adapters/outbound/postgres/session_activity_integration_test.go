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
	"github.com/narvidev/narvi/internal/app/sessionactor"
)

// TestSessionActivityFacts_OneStatement seeds one session with every kind
// of fact the status route reads -- turns in five states, a sandbox, a
// plan awaiting approval, a workflow step awaiting a decision, and an
// escalated run that a newer run and newer turns have since superseded --
// and proves the single ActivityFacts read returns each one: the
// per-state histogram, the in-flight turn, the newest terminal turn, the
// newest turn, each open gate with the time it opened (the superseded
// escalation is not one; TestSessionActivityFacts_LiveEscalation pins
// when an escalation is), the sandbox status and the session's own row. A
// session with no turn and no gate reads an empty histogram and no ids; an
// unknown session is pgx.ErrNoRows.
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
	deliveryAt := base.Add(16 * time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = $2 WHERE session_id = $1`, sessionID, deliveryAt); err != nil {
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
	// The second run (and the newer turns) supersede the escalation.
	escalatedRun, err := workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := workflows.EscalateRun(ctx, escalatedRun.ID); err != nil {
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

	// Armed timers of both classes, out of name order; two release
	// manifest checks still to come beside one running; and the pull
	// request the session reviews, on a repository that opted in to the
	// automatic re-review with its budget unspent: the statement returns
	// every armed timer, earliest first, the oldest waiting check's enqueue
	// instant, the running check's claim, and that the debounce can fire.
	timers := narvipg.NewTimerStore(pool)
	livenessAt, debounceAt := base.Add(30*time.Minute), base.Add(25*time.Minute)
	for name, at := range map[string]time.Time{"liveness_check": livenessAt, "review_retrigger_debounce": debounceAt} {
		if _, err := timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: sessionID, Name: name, FiresAt: pgtype.Timestamptz{Time: at, Valid: true}}); err != nil {
			t.Fatalf("arm %s: %v", name, err)
		}
	}
	pendingSince, claimedAt := base.Add(5*time.Minute), base.Add(7*time.Minute)
	for i, created := range []time.Time{pendingSince, base.Add(8 * time.Minute)} {
		if _, err := pool.Exec(ctx, `INSERT INTO release_manifest_pending (session_id, owner, repo, pr_number, base_ref, head_ref, created_at) VALUES ($1, 'acme', 'widgets', $2, 'main', 'release/x', $3)`,
			sessionID, 900+i, created); err != nil {
			t.Fatalf("seed release manifest check %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO release_manifest_checks_running (pending_id, session_id, claimed_at) VALUES (gen_random_uuid(), $1, $2)`, sessionID, claimedAt); err != nil {
		t.Fatalf("seed running release manifest check: %v", err)
	}
	repoFullName := fmt.Sprintf("acme/facts-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ($1, 7, $2)`, repoFullName, sessionID); err != nil {
		t.Fatalf("seed github pr session: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, auto_retrigger_review_enabled) VALUES ($1, true)`, repoFullName); err != nil {
		t.Fatalf("seed repo settings: %v", err)
	}

	before := time.Now().Add(-time.Minute)
	facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
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
	if !facts.PrDeliveryStartedAt.Valid || !facts.PrDeliveryStartedAt.Time.Equal(deliveryAt) {
		t.Errorf("pr_delivery_started_at = %v, want %v", facts.PrDeliveryStartedAt, deliveryAt)
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
	if facts.EscalatedRunID.Valid || facts.EscalatedRunSince.Valid {
		t.Errorf("escalated run = (%v, %v), want none: a newer run and newer turns superseded run %v", facts.EscalatedRunID, facts.EscalatedRunSince, escalatedRun.ID)
	}
	if !facts.ObservedAt.Valid || facts.ObservedAt.Time.Before(before) {
		t.Errorf("observed_at = %v, want the statement's own time", facts.ObservedAt)
	}
	if len(facts.ArmedTimerNames) != 2 || len(facts.ArmedTimerFiresAt) != 2 ||
		facts.ArmedTimerNames[0] != "review_retrigger_debounce" || !facts.ArmedTimerFiresAt[0].Time.Equal(debounceAt) ||
		facts.ArmedTimerNames[1] != "liveness_check" || !facts.ArmedTimerFiresAt[1].Time.Equal(livenessAt) {
		t.Errorf("armed timers = %v at %v, want review_retrigger_debounce at %v then liveness_check at %v -- every kind, earliest first", facts.ArmedTimerNames, facts.ArmedTimerFiresAt, debounceAt, livenessAt)
	}
	if !facts.ReleaseCheckPendingSince.Valid || !facts.ReleaseCheckPendingSince.Time.Equal(pendingSince) {
		t.Errorf("release_check_pending_since = %v, want the oldest unclaimed check's %v", facts.ReleaseCheckPendingSince, pendingSince)
	}
	if !facts.ReleaseCheckClaimedAt.Valid || !facts.ReleaseCheckClaimedAt.Time.Equal(claimedAt) {
		t.Errorf("release_check_claimed_at = %v, want the running check's %v", facts.ReleaseCheckClaimedAt, claimedAt)
	}
	if !facts.ReviewRetriggerCanFire {
		t.Error("review_retrigger_can_fire = false, want true: the repository opted in and the budget is unspent")
	}

	t.Run("a session with no turn and no gate", func(t *testing.T) {
		empty := createTestSession(ctx, t, pool)
		facts, err := sessions.ActivityFacts(ctx, empty, sessionactor.ReviewAutoRetriggerBudget)
		if err != nil {
			t.Fatalf("ActivityFacts: %v", err)
		}
		if got := decodeJSONObject(t, facts.TurnCounts); len(got) != 0 {
			t.Errorf("turn_counts = %v, want {}", got)
		}
		if facts.InFlightTurnID.Valid || facts.InFlightTurnStatus != "" || facts.LastRunTurnID.Valid || facts.LastRunStatus != "" || facts.NewestTurnID.Valid ||
			facts.AwaitingPlanID.Valid || facts.AwaitingStepID.Valid || facts.EscalatedRunID.Valid || facts.SandboxStatus != nil || facts.PrDeliveryStartedAt.Valid ||
			len(facts.ArmedTimerNames) != 0 || len(facts.ArmedTimerFiresAt) != 0 || facts.ReleaseCheckPendingSince.Valid || facts.ReleaseCheckClaimedAt.Valid ||
			facts.ReviewRetriggerCanFire {
			t.Errorf("facts = %+v, want no turn, no gate, no sandbox, no delivery, nothing armed", facts)
		}
	})

	t.Run("an unknown session", func(t *testing.T) {
		unknown := pgtype.UUID{Bytes: [16]byte{0xde, 0xad}, Valid: true}
		if _, err := sessions.ActivityFacts(ctx, unknown, sessionactor.ReviewAutoRetriggerBudget); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ActivityFacts(unknown) err = %v, want pgx.ErrNoRows", err)
		}
	})
}

// TestSessionActivityFacts_LiveEscalation pins which workflow escalation
// ActivityFacts reports (technical plan §43.20). A run in needs_review is
// never moved out of it, so each case below is a state that would gate the
// session for good if every such run counted. It is reported only while no
// turn has been CREATED on the session since it escalated -- it is the
// session's newest workflow run, it ran an attempt of its own, and no turn
// other than its own attempts was created at or after the escalation --
// and its definition is not a built-in one. A turn created before the
// escalation never closes it, whether it is queued, running or has ended,
// and whenever it ended. Rows are written the way the workflow engine
// writes them -- a run, one attempt per turn with the turn attached and
// then finished, then the escalation -- with every instant (turns'
// created_at and completed_at, runs' created_at, the escalation's
// updated_at) set explicitly on one timeline, so the order is unambiguous.
func TestSessionActivityFacts_LiveEscalation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	workflows := narvipg.NewWorkflowStore(pool)

	var builtInDef, builtInStep pgtype.UUID
	var builtIn bool
	if err := pool.QueryRow(ctx, `SELECT d.id, d.is_built_in, s.id FROM workflow_definitions d JOIN workflow_step_definitions s ON s.workflow_definition_id = d.id WHERE d.id = $1`,
		builtInRequestDefID).Scan(&builtInDef, &builtIn, &builtInStep); err != nil || !builtIn {
		t.Fatalf("read the built-in request workflow: built-in %v, %v", builtIn, err)
	}
	var customDef, customStep1, customStep2 pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("live-escalation-%d", time.Now().UnixNano())).Scan(&customDef); err != nil {
		t.Fatalf("insert custom workflow definition: %v", err)
	}
	for i, step := range []*pgtype.UUID{&customStep1, &customStep2} {
		if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, $2, 'agent', '{{prompt}}') RETURNING id`,
			customDef, i+1).Scan(step); err != nil {
			t.Fatalf("insert custom step %d: %v", i+1, err)
		}
	}

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	newTurn := func(t *testing.T, sessionID pgtype.UUID, status sqlcgen.TurnStatus, minute int) pgtype.UUID {
		t.Helper()
		row, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: status})
		if err != nil {
			t.Fatalf("create %s turn: %v", status, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE turns SET created_at = $2 WHERE id = $1`, row.ID, base.Add(time.Duration(minute)*time.Minute)); err != nil {
			t.Fatalf("set turn created_at: %v", err)
		}
		return row.ID
	}
	newRun := func(t *testing.T, sessionID, def pgtype.UUID, minute int) pgtype.UUID {
		t.Helper()
		run, err := workflows.CreateRun(ctx, sessionID, "request", def, 1)
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET created_at = $2 WHERE id = $1`, run.ID, base.Add(time.Duration(minute)*time.Minute)); err != nil {
			t.Fatalf("set run created_at: %v", err)
		}
		return run.ID
	}
	// attempt records one finished attempt of step within run, dispatched
	// as turnID.
	attempt := func(t *testing.T, run, step, turnID pgtype.UUID, status, outcome string) {
		t.Helper()
		sr, err := workflows.CreateStepRun(ctx, run, step)
		if err != nil {
			t.Fatalf("create step run: %v", err)
		}
		if err := workflows.AttachTurn(ctx, sr.ID, turnID); err != nil {
			t.Fatalf("attach turn: %v", err)
		}
		if _, err := workflows.FinishStepRun(ctx, sr.ID, status, outcome); err != nil {
			t.Fatalf("finish step run: %v", err)
		}
	}
	// escalate escalates run the way the engine does, at base+at on the
	// same timeline as every turn and run above: the instant the
	// escalation reads as, and the one a turn's creation is ordered
	// against.
	escalate := func(t *testing.T, run pgtype.UUID, at time.Duration) sqlcgen.WorkflowRun {
		t.Helper()
		row, err := workflows.EscalateRun(ctx, run)
		if err != nil {
			t.Fatalf("escalate run: %v", err)
		}
		if err := pool.QueryRow(ctx, `UPDATE workflow_runs SET updated_at = $2 WHERE id = $1 RETURNING updated_at`, run, base.Add(at)).Scan(&row.UpdatedAt); err != nil {
			t.Fatalf("set the escalation's instant: %v", err)
		}
		return row
	}
	// endedAt sets a turn's completed_at to base+at on the same timeline:
	// when a turn ended is not what the rule reads, and these cases show it.
	endedAt := func(t *testing.T, turnID pgtype.UUID, at time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE turns SET completed_at = $2 WHERE id = $1`, turnID, base.Add(at)); err != nil {
			t.Fatalf("set turn completed_at: %v", err)
		}
	}
	// newTurnAt is newTurn at base+at, for instants finer than a minute.
	newTurnAt := func(t *testing.T, sessionID pgtype.UUID, status sqlcgen.TurnStatus, at time.Duration) pgtype.UUID {
		t.Helper()
		id := newTurn(t, sessionID, status, 0)
		if _, err := pool.Exec(ctx, `UPDATE turns SET created_at = $2 WHERE id = $1`, id, base.Add(at)); err != nil {
			t.Fatalf("set turn created_at: %v", err)
		}
		return id
	}
	// escalatedCustomRun is the live case every "superseded" case below
	// starts from: a custom run whose one turn, created at minute 0,
	// failed and escalated the run half a minute later.
	escalatedCustomRun := func(t *testing.T, sessionID pgtype.UUID) sqlcgen.WorkflowRun {
		t.Helper()
		run := newRun(t, sessionID, customDef, 0)
		attempt(t, run, customStep1, newTurn(t, sessionID, sqlcgen.TurnStatusFailed, 0), "failed", "blocked")
		return escalate(t, run, 30*time.Second)
	}

	cases := []struct {
		name string
		// seed writes the case's rows and returns the escalated run, and
		// whether ActivityFacts must report it.
		seed func(t *testing.T, sessionID pgtype.UUID) (sqlcgen.WorkflowRun, bool)
	}{
		{"a custom run whose own turn is the newest is reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			return escalatedCustomRun(t, s), true
		}},
		{"a custom run escalated by its later step's turn is reported (a turn newer than the run's creation is its own)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, customDef, 0)
			attempt(t, run, customStep1, newTurn(t, s, sqlcgen.TurnStatusCompleted, 0), "completed", "ok")
			attempt(t, run, customStep2, newTurn(t, s, sqlcgen.TurnStatusFailed, 1), "failed", "blocked")
			return escalate(t, run, 90*time.Second), true
		}},
		// Review round 2, O2: a decision escalates the run AFTER a turn that
		// is none of its attempts -- one sent while the step awaited that
		// decision, which the engine leaves untracked (no step run, no new
		// run). Nothing has come after the escalation, so it is the
		// session's latest state.
		{"a custom run escalated after an untracked turn is reported (the turn came before the escalation)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, customDef, 0)
			attempt(t, run, customStep1, newTurn(t, s, sqlcgen.TurnStatusCancelled, 0), "cancelled", "blocked")
			newTurn(t, s, sqlcgen.TurnStatusCompleted, 1)
			return escalate(t, run, 2*time.Minute), true
		}},
		{"a turn created after that escalation supersedes it again", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, customDef, 0)
			attempt(t, run, customStep1, newTurn(t, s, sqlcgen.TurnStatusCancelled, 0), "cancelled", "blocked")
			newTurn(t, s, sqlcgen.TurnStatusCompleted, 1)
			escalated := escalate(t, run, 2*time.Minute)
			newTurn(t, s, sqlcgen.TurnStatusPending, 3)
			return escalated, false
		}},
		// Each arm alone: the run's own attempt being the newest turn
		// reports it even with the escalation's instant placed before that
		// turn's creation (clocks aside, its own attempt cannot supersede
		// it); and a turn created before the escalation reports it without
		// being an attempt (the case above).
		{"the run's own attempt as the newest turn is reported, whatever the instants say", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, customDef, 0)
			attempt(t, run, customStep1, newTurn(t, s, sqlcgen.TurnStatusFailed, 1), "failed", "blocked")
			return escalate(t, run, 30*time.Second), true
		}},
		{"a built-in run escalated after an untracked turn is still not reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, builtInDef, 0)
			attempt(t, run, builtInStep, newTurn(t, s, sqlcgen.TurnStatusFailed, 0), "failed", "blocked")
			newTurn(t, s, sqlcgen.TurnStatusCompleted, 1)
			return escalate(t, run, 2*time.Minute), false
		}},
		{"a built-in run whose own turn is the newest is not reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, builtInDef, 0)
			attempt(t, run, builtInStep, newTurn(t, s, sqlcgen.TurnStatusCancelled, 0), "cancelled", "blocked")
			return escalate(t, run, 30*time.Second), false
		}},
		{"a newer run with its own turn supersedes it", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			next := newRun(t, s, customDef, 1)
			attempt(t, next, customStep1, newTurn(t, s, sqlcgen.TurnStatusCompleted, 1), "completed", "ok")
			if _, err := workflows.CompleteRun(ctx, next); err != nil {
				t.Fatalf("complete run: %v", err)
			}
			return escalated, false
		}},
		{"a newer turn with no run of its own supersedes it (an automatic re-review turn, which the engine never tracks, or a plan's implementation turn)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			newTurn(t, s, sqlcgen.TurnStatusPending, 1)
			return escalated, false
		}},
		// Review round 3, P2: a turn queued behind the turn whose end
		// escalates the run (AlwaysQueue: a GitHub mention, a re-review) is
		// created BEFORE the escalation and starts no run. It was sent
		// before anything escalated, so it is no answer to the escalation:
		// the gate stays open while it waits, once it is dispatched after
		// the escalation, and after it has run.
		{"a turn queued behind the escalating turn, still pending, leaves it reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			newTurnAt(t, s, sqlcgen.TurnStatusPending, 15*time.Second)
			return escalated, true
		}},
		{"a turn queued behind the escalating turn, dispatched after it, leaves it reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			newTurnAt(t, s, sqlcgen.TurnStatusDispatched, 15*time.Second)
			return escalated, true
		}},
		{"a turn queued behind the escalating turn, run to completion after it, leaves it reported (round 3's P2)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			queued := newTurnAt(t, s, sqlcgen.TurnStatusCompleted, 15*time.Second)
			endedAt(t, queued, 5*time.Minute)
			return escalated, true
		}},
		{"an older turn cancelled after the escalation leaves it reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			queued := newTurnAt(t, s, sqlcgen.TurnStatusCancelled, 15*time.Second)
			endedAt(t, queued, time.Minute)
			return escalated, true
		}},
		// Review round 4, P5: an untracked turn sent while a step awaited
		// its decision, then /decide escalating the run. Whether that turn
		// ended a moment before the click or a moment after it, it was
		// created before the escalation: both orders read the same.
		{"an untracked turn that ended before the decision escalated the run leaves it reported (round 2's O2)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, customDef, 0)
			attempt(t, run, customStep1, newTurn(t, s, sqlcgen.TurnStatusCompleted, 0), "completed", "blocked")
			untracked := newTurnAt(t, s, sqlcgen.TurnStatusCompleted, 10*time.Second)
			endedAt(t, untracked, 20*time.Second)
			return escalate(t, run, 2*time.Minute), true
		}},
		{"an untracked turn that ended after the decision escalated the run leaves it reported too (round 4's P5)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			run := newRun(t, s, customDef, 0)
			attempt(t, run, customStep1, newTurn(t, s, sqlcgen.TurnStatusCompleted, 0), "completed", "blocked")
			untracked := newTurnAt(t, s, sqlcgen.TurnStatusCompleted, 10*time.Second)
			escalated := escalate(t, run, 2*time.Minute)
			endedAt(t, untracked, 3*time.Minute)
			return escalated, true
		}},
		{"a turn created at the escalation's own instant closes it (created at or after)", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			newTurnAt(t, s, sqlcgen.TurnStatusPending, 30*time.Second)
			return escalated, false
		}},
		{"a newer run supersedes it even before any turn of its own", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			escalated := escalatedCustomRun(t, s)
			newRun(t, s, customDef, 1)
			return escalated, false
		}},
		{"a run with no turn of its own is not reported", func(t *testing.T, s pgtype.UUID) (sqlcgen.WorkflowRun, bool) {
			return escalate(t, newRun(t, s, customDef, 0), 30*time.Second), false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			run, reported := tc.seed(t, sessionID)
			if run.Status != sqlcgen.WorkflowRunStatusNeedsReview {
				t.Fatalf("seeded run status %q, want needs_review", run.Status)
			}
			facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
			if err != nil {
				t.Fatalf("ActivityFacts: %v", err)
			}
			switch {
			case reported && (facts.EscalatedRunID != run.ID || !facts.EscalatedRunSince.Time.Equal(run.UpdatedAt.Time)):
				t.Errorf("escalated run = (%v, %v), want (%v, %v)", facts.EscalatedRunID, facts.EscalatedRunSince.Time, run.ID, run.UpdatedAt.Time)
			case !reported && (facts.EscalatedRunID.Valid || facts.EscalatedRunSince.Valid):
				t.Errorf("escalated run = (%v, %v), want none (run %v is not the session's live escalation)", facts.EscalatedRunID, facts.EscalatedRunSince, run.ID)
			}
		})
	}
}

// TestSessionActivityFacts_ReviewRetriggerCanFire pins
// review_retrigger_can_fire (technical plan §43.20, review round 4's P3):
// true only when the session backs a pull request whose repository opted
// in to the automatic re-review (no repo_settings row reads as off, as the
// fire reads it), whose automatic re-review count is below the budget
// passed in, and whose repository an administrator has not revoked
// (§31.4) -- the fire's three conditions a row can show. The budget is the
// argument, never a literal in the statement.
func TestSessionActivityFacts_ReviewRetriggerCanFire(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	budget := int32(sessionactor.ReviewAutoRetriggerBudget)

	cases := []struct {
		name     string
		prRow    bool
		settings *bool
		count    int32
		budget   int32
		revoked  bool
		want     bool
	}{
		{"not a pull request session", false, nil, 0, budget, false, false},
		{"no repo settings row", true, nil, 0, budget, false, false},
		{"opted out", true, ptrBool(false), 0, budget, false, false},
		{"opted in, nothing spent", true, ptrBool(true), 0, budget, false, true},
		{"opted in, one re-review left", true, ptrBool(true), budget - 1, budget, false, true},
		{"opted in, budget spent", true, ptrBool(true), budget, budget, false, false},
		{"opted in, past the budget", true, ptrBool(true), budget + 3, budget, false, false},
		{"opted in, the budget passed in decides", true, ptrBool(true), 3, 4, false, true},
		{"opted in, a smaller budget passed in is spent", true, ptrBool(true), 4, 4, false, false},
		{"opted in, nothing spent, revoked by an administrator", true, ptrBool(true), 0, budget, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			repo := fmt.Sprintf("acme/can-fire-%d", time.Now().UnixNano())
			if tc.prRow {
				if _, err := pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id, auto_retrigger_count) VALUES ($1, 7, $2, $3)`, repo, sessionID, tc.count); err != nil {
					t.Fatalf("seed github pr session: %v", err)
				}
			}
			if tc.settings != nil {
				if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, auto_retrigger_review_enabled) VALUES ($1, $2)`, repo, *tc.settings); err != nil {
					t.Fatalf("seed repo settings: %v", err)
				}
			}
			if tc.revoked {
				if _, err := narvipg.NewRepoEntitlementRevocationStore(pool).Revoke(ctx, repo, pgtype.UUID{}, "frozen by an administrator"); err != nil {
					t.Fatalf("revoke: %v", err)
				}
			}
			facts, err := sessions.ActivityFacts(ctx, sessionID, tc.budget)
			if err != nil {
				t.Fatalf("ActivityFacts: %v", err)
			}
			if facts.ReviewRetriggerCanFire != tc.want {
				t.Fatalf("review_retrigger_can_fire = %v, want %v", facts.ReviewRetriggerCanFire, tc.want)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

// TestSandboxPRDelivery_StartAndEnd pins the push/PR delivery stamp's own
// writes (technical plan §43.20, migrations/000145): StartPRDelivery stamps
// the database's now() -- the clock ActivityFacts' observed_at reads -- and
// a later start overwrites an earlier one; EndPRDelivery clears it, and is
// a no-op when nothing is outstanding. ActivityFacts reads the stamp in its
// one statement.
func TestSandboxPRDelivery_StartAndEnd(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	sandboxes := narvipg.NewSandboxStore(pool)

	sessionID := createTestSession(ctx, t, pool)
	if _, err := sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	stamp := func() pgtype.Timestamptz {
		t.Helper()
		facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
		if err != nil {
			t.Fatalf("ActivityFacts: %v", err)
		}
		return facts.PrDeliveryStartedAt
	}
	if got := stamp(); got.Valid {
		t.Fatalf("a new sandbox: stamp %v, want none", got)
	}

	if err := sandboxes.StartPRDelivery(ctx, sessionID); err != nil {
		t.Fatalf("StartPRDelivery: %v", err)
	}
	first := stamp()
	facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Valid || first.Time.After(facts.ObservedAt.Time) || facts.ObservedAt.Time.Sub(first.Time) > time.Minute {
		t.Fatalf("started: stamp %v observed %v, want the database's now(), at or just before the read", first, facts.ObservedAt)
	}

	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = pr_delivery_started_at - interval '1 hour' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := sandboxes.StartPRDelivery(ctx, sessionID); err != nil {
		t.Fatalf("StartPRDelivery again: %v", err)
	}
	if again := stamp(); !again.Valid || !again.Time.After(first.Time.Add(-time.Minute)) {
		t.Fatalf("a later start: stamp %v, want it overwritten with now() (not the hour-old one)", again)
	}

	for i := 0; i < 2; i++ {
		if err := sandboxes.EndPRDelivery(ctx, sessionID); err != nil {
			t.Fatalf("EndPRDelivery #%d: %v", i+1, err)
		}
		if got := stamp(); got.Valid {
			t.Fatalf("ended #%d: stamp %v, want none", i+1, got)
		}
	}

	// A respawn replaces the generation the push was sent to: that push
	// can never report back, so its delivery is over.
	if err := sandboxes.StartPRDelivery(ctx, sessionID); err != nil {
		t.Fatalf("StartPRDelivery before the respawn: %v", err)
	}
	if got := stamp(); !got.Valid {
		t.Fatal("before the respawn: no stamp")
	}
	tokenHash := "respawn-token-hash"
	respawned, err := sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &tokenHash})
	if err != nil {
		t.Fatalf("UpsertForSpawn: %v", err)
	}
	if respawned.Gen < 2 || respawned.PrDeliveryStartedAt.Valid {
		t.Fatalf("respawned: gen %d stamp %v, want a new generation and no stamp", respawned.Gen, respawned.PrDeliveryStartedAt)
	}
}
