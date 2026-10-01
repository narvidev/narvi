//go:build integration

package workflowengine_test

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/domain/workflow"
)

// TestOnTurnCompleted_SingleStepLane_OkOutcome_CompletesRun proves the
// common-case path for review/request (§25.8: single step, no edges): a
// real execution_complete (turn.TriggerComplete) derives StepOutcomeOK,
// finishes the step-run 'completed', and -- since NextStep has no further
// step to advance to -- completes the owning run.
func TestOnTurnCompleted_SingleStepLane_OkOutcome_CompletesRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	deps := testDeps(pool, turns, workflows)

	session := newSession(t, ctx, sessions)
	row, _ := startRunAndAttachRealTurn(t, ctx, sessions, turns, workflows, session, "do the thing", nil, false)

	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		workflowengine.OnTurnCompleted(ctx, deps, session, row.turnID, turn.TriggerComplete)
	})

	var status string
	var outcome *string
	var finishedAt *string
	if err := pool.QueryRow(ctx, `SELECT status, outcome_status, finished_at::text FROM workflow_step_runs WHERE id = $1`, row.stepRunID.String()).
		Scan(&status, &outcome, &finishedAt); err != nil {
		t.Fatalf("query step-run: %v", err)
	}
	if status != "completed" {
		t.Errorf("step-run status = %q, want completed", status)
	}
	if outcome == nil || *outcome != "ok" {
		t.Errorf("step-run outcome_status = %v, want ok", outcome)
	}
	if finishedAt == nil {
		t.Error("step-run finished_at is NULL, want set (this attempt is terminal)")
	}

	var runStatus string
	var runFinishedAt *string
	if err := pool.QueryRow(ctx, `SELECT status, finished_at::text FROM workflow_runs WHERE id = $1`, row.runID.String()).
		Scan(&runStatus, &runFinishedAt); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if runStatus != "completed" {
		t.Errorf("run status = %q, want completed (single-step lane, no further step)", runStatus)
	}
	if runFinishedAt == nil {
		t.Error("run finished_at is NULL, want set")
	}
}

// TestOnTurnCompleted_SingleStepLane_FailTrigger_EscalatesRun proves the
// fail-conservative default (§25.4): a turn ending in TriggerFail (no
// posted outcome) derives StepOutcomeBlocked; with no edge wired for
// "blocked" (true of every §25.8 built-in), NextStep escalates the run to
// needs_review -- non-terminal, finished_at stays NULL (migration 000057's
// own "one notice ... stop", never "hold the session hostage" comment).
func TestOnTurnCompleted_SingleStepLane_FailTrigger_EscalatesRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	deps := testDeps(pool, turns, workflows)

	session := newSession(t, ctx, sessions)
	row, _ := startRunAndAttachRealTurn(t, ctx, sessions, turns, workflows, session, "do the thing", nil, false)

	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		workflowengine.OnTurnCompleted(ctx, deps, session, row.turnID, turn.TriggerFail)
	})

	var status string
	var outcome *string
	if err := pool.QueryRow(ctx, `SELECT status, outcome_status FROM workflow_step_runs WHERE id = $1`, row.stepRunID.String()).
		Scan(&status, &outcome); err != nil {
		t.Fatalf("query step-run: %v", err)
	}
	if status != "failed" {
		t.Errorf("step-run status = %q, want failed", status)
	}
	if outcome == nil || *outcome != "blocked" {
		t.Errorf("step-run outcome_status = %v, want blocked", outcome)
	}

	var runStatus string
	var runFinishedAt *string
	if err := pool.QueryRow(ctx, `SELECT status, finished_at::text FROM workflow_runs WHERE id = $1`, row.runID.String()).
		Scan(&runStatus, &runFinishedAt); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if runStatus != "needs_review" {
		t.Errorf("run status = %q, want needs_review (escalated, fail-conservative default)", runStatus)
	}
	if runFinishedAt != nil {
		t.Error("run finished_at is set, want NULL (needs_review is non-terminal, must not freeze the session)")
	}
}

// TestOnTurnCompleted_HITLAfterStep_MarksAwaitingDecision_RunStaysRunning
// proves §25.9's HITL gate: OnTurnCompleted never calls workflow.NextStep
// at all here -- the step-run lands in awaiting_decision (not completed),
// and the RUN's own status is completely untouched (still running), exactly
// where §25.9's own decide endpoint is meant to pick it up.
//
// Exercises a CUSTOM (non-built-in) hitl_after step (seedCustomHITLAfterStep,
// dispatch_integration_test.go), not the built-in plan workflow: migration
// 000088_plan_builtin_passthrough (§25.9's own corrective follow-up, an
// audit-found design incoherence -- see that migration's own header comment
// and docs/TECHNICAL_PLAN.md §25.8) made the built-in plan workflow a
// genuine single-step passthrough carrying no HITL, so classic plan mode
// (§8.1) stays the SOLE plan-approval authority; this test's
// own subject -- the HITLAfter branch in OnTurnCompleted itself -- is
// otherwise completely unchanged and now proven against the shape any
// future custom workflow (e.g. a Phase 7 canvas-authored one) would use.
func TestOnTurnCompleted_HITLAfterStep_MarksAwaitingDecision_RunStaysRunning(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	deps := testDeps(pool, turns, workflows)

	const (
		customDefID  = "10000000-0000-4000-8000-000000000021"
		customStepID = "10000000-0000-4000-8000-000000000022"
	)
	session := seedCustomHITLAfterStep(t, ctx, pool, sessions, customDefID, customStepID, "acme/hitl-completion")

	row, res := startRunAndAttachRealTurn(t, ctx, sessions, turns, workflows, session, "do the hitl-gated thing", nil, false)
	if row.stepDefID.String() != customStepID {
		t.Fatalf("step-run step id = %s, want the custom HITLAfter step %s (test setup assumption)", row.stepDefID.String(), customStepID)
	}
	if res.Prompt != "do the hitl-gated thing" {
		t.Fatalf("res.Prompt = %q, want unchanged (test setup assumption)", res.Prompt)
	}

	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		workflowengine.OnTurnCompleted(ctx, deps, session, row.turnID, turn.TriggerComplete)
	})

	var status string
	var outcome *string
	var finishedAt *string
	if err := pool.QueryRow(ctx, `SELECT status, outcome_status, finished_at::text FROM workflow_step_runs WHERE id = $1`, row.stepRunID.String()).
		Scan(&status, &outcome, &finishedAt); err != nil {
		t.Fatalf("query step-run: %v", err)
	}
	if status != "awaiting_decision" {
		t.Errorf("step-run status = %q, want awaiting_decision", status)
	}
	if outcome == nil || *outcome != "ok" {
		t.Errorf("step-run outcome_status = %v, want ok (the turn itself completed normally)", outcome)
	}
	if finishedAt != nil {
		t.Error("step-run finished_at is set, want NULL (awaiting_decision is still 'live', migration 000057's own doc comment)")
	}

	var runStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1`, row.runID.String()).Scan(&runStatus); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if runStatus != "running" {
		t.Errorf("run status = %q, want running (untouched -- NextStep is never consulted for a HITLAfter-gated step)", runStatus)
	}
}

// TestOnTurnCompleted_PostedOutcomeTakesPrecedenceOverImplicitDerivation
// proves the generic step-outcome-posting tool's own write
// (WorkflowStore.SetStepRunOutcome) is never clobbered by OnTurnCompleted's
// own implicit-from-trigger fallback: an outcome already posted DURING the
// turn (needs_fix, here) survives a subsequent TriggerComplete completion
// unchanged -- the COALESCE discipline both WorkflowStore.
// MarkAwaitingDecision and FinishStepRun share (workflows.sql's own doc
// comment).
func TestOnTurnCompleted_PostedOutcomeTakesPrecedenceOverImplicitDerivation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	deps := testDeps(pool, turns, workflows)

	session := newSession(t, ctx, sessions)
	row, _ := startRunAndAttachRealTurn(t, ctx, sessions, turns, workflows, session, "do the thing", nil, false)

	// Simulate the posting tool having already recorded an outcome mid-turn
	// (a hypothetical non-built-in step's own agent call -- no built-in
	// does this in §25.6, but the persisted-precedence contract must
	// hold regardless of who posted it).
	rowsAffected, err := workflows.SetStepRunOutcome(ctx, row.stepRunID, "needs_fix", "found a fixable issue", nil)
	if err != nil {
		t.Fatalf("SetStepRunOutcome: %v", err)
	}
	if rowsAffected != 1 {
		t.Fatalf("SetStepRunOutcome rowsAffected = %d, want 1", rowsAffected)
	}

	// The turn itself now completes normally -- a naive implementation
	// would derive StepOutcomeOK and overwrite the already-posted
	// needs_fix; the COALESCE in FinishStepRun must prevent that.
	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		workflowengine.OnTurnCompleted(ctx, deps, session, row.turnID, turn.TriggerComplete)
	})

	var status string
	var outcome *string
	var summary *string
	if err := pool.QueryRow(ctx, `SELECT status, outcome_status, outcome_summary FROM workflow_step_runs WHERE id = $1`, row.stepRunID.String()).
		Scan(&status, &outcome, &summary); err != nil {
		t.Fatalf("query step-run: %v", err)
	}
	if status != "completed" {
		t.Errorf("step-run status = %q, want completed", status)
	}
	if outcome == nil || *outcome != "needs_fix" {
		t.Errorf("step-run outcome_status = %v, want needs_fix (the POSTED value, never overwritten by the implicit ok derivation)", outcome)
	}
	if summary == nil || *summary != "found a fixable issue" {
		t.Errorf("step-run outcome_summary = %v, want the posted summary preserved", summary)
	}

	// needs_fix with no wired edge (true of every §25.8 built-in) escalates
	// -- proving NextStep really did consult the POSTED outcome, not a
	// silently-re-derived "ok".
	var runStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1`, row.runID.String()).Scan(&runStatus); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if runStatus != "needs_review" {
		t.Errorf("run status = %q, want needs_review (needs_fix with no wired edge escalates)", runStatus)
	}
}

// TestFinishStepRun_ConcurrentPostBetweenReadAndFinish_AuthoritativeOutcomeOverridesStaleRead
// is the regression test for a stale-read TOCTOU an adversarial review
// confirmed against OnTurnCompleted's pre-fix implementation (completion.go):
// the generic step-outcome-posting tool (WorkflowStore.SetStepRunOutcome,
// written by workflowstepoutcome.go on the POOL, unserialized with
// OnTurnCompleted's own caller-owned transaction) writes the SAME
// workflow_step_runs.outcome_status column OnTurnCompleted itself reads via
// GetLiveStepRunByTurnID near its own top. If that post lands strictly
// between that read and OnTurnCompleted's later FinishStepRun call --
// several DB round-trips later (GetRun, LoadDefinition's own three reads) --
// FinishStepRun's own COALESCE(outcome_status, $3) correctly preserves the
// posted value IN THE ROW, but the pre-fix code discarded FinishStepRun's
// own RETURNING row and called workflow.NextStep with the earlier, by-then
// stale local variable instead -- misrouting the run (e.g. silently
// completing it when the posted outcome was needs_fix).
//
// Real goroutine timing cannot place a write deterministically inside that
// exact window without an artificial hook completion.go itself does not
// have (and should not gain just for a test -- this fix's own scope is
// deliberately narrow), and a flaky race-timing test would be worse than no
// test at all. So rather than calling OnTurnCompleted itself, this test
// drives WorkflowStore's own lower-level methods directly -- the SAME
// methods, called with the SAME arguments, in the SAME order OnTurnCompleted
// uses internally -- deterministically placing the concurrent post exactly
// inside that window instead of hoping real goroutine scheduling lands it
// there. It then proves the fix's own core contract: workflow.NextStep,
// consulted with FinishStepRun's own authoritative RETURNING value, reaches
// a DIFFERENT (and correct) verdict than it would have with the stale
// pre-read value.
func TestFinishStepRun_ConcurrentPostBetweenReadAndFinish_AuthoritativeOutcomeOverridesStaleRead(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)

	session := newSession(t, ctx, sessions)
	row, _ := startRunAndAttachRealTurn(t, ctx, sessions, turns, workflows, session, "do the thing", nil, false)

	// OnTurnCompleted's own initial, lock-free read (completion.go's
	// GetLiveStepRunByTurnID call, queries/workflows.sql's plain "SELECT ...
	// WHERE turn_id = $1 AND status = 'running'", no FOR UPDATE) --
	// outcome_status is still NULL here (nothing posted yet), so a
	// TriggerComplete turn's own implicit derivation (implicitOutcome) is
	// StepOutcomeOK, exactly the "pre-read outcome" local completion.go
	// computes from this same row a few lines later.
	preRead, err := workflows.GetLiveStepRunByTurnID(ctx, row.turnID)
	if err != nil {
		t.Fatalf("GetLiveStepRunByTurnID (simulated initial read): %v", err)
	}
	if preRead.OutcomeStatus != nil {
		t.Fatalf("preRead.OutcomeStatus = %q, want nil (test setup assumption: nothing posted yet)", *preRead.OutcomeStatus)
	}
	staleOutcome := workflow.StepOutcomeOK

	// The race: a concurrent call to the generic step-outcome-posting tool
	// lands strictly between the read above and the FinishStepRun call
	// below -- exactly the window this bug requires.
	rowsAffected, err := workflows.SetStepRunOutcome(ctx, row.stepRunID, "needs_fix", "found a fixable issue mid-turn", nil)
	if err != nil {
		t.Fatalf("SetStepRunOutcome (simulated concurrent post): %v", err)
	}
	if rowsAffected != 1 {
		t.Fatalf("SetStepRunOutcome rowsAffected = %d, want 1 (step-run must still be 'running')", rowsAffected)
	}

	// completion.go's own FinishStepRun call, using the SAME stale pre-read
	// outcome ("ok") as its COALESCE fallback argument -- exactly what
	// OnTurnCompleted's own local `outcome` variable still holds at this
	// point in the real function, unaware of the concurrent post above.
	finished, err := workflows.FinishStepRun(ctx, row.stepRunID, "completed", string(staleOutcome))
	if err != nil {
		t.Fatalf("FinishStepRun: %v", err)
	}

	// The ROW itself is correct regardless of the fix under test here:
	// COALESCE(outcome_status, $3) only ever falls back to $3 when the
	// column was still NULL, so the already-landed concurrent post beats the
	// stale "ok" fallback -- this is what the existing
	// TestOnTurnCompleted_PostedOutcomeTakesPrecedenceOverImplicitDerivation
	// already covers (via a subsequent DB query), just not by inspecting
	// FinishStepRun's own returned row directly, which is what the fix now
	// depends on.
	if finished.OutcomeStatus == nil || *finished.OutcomeStatus != sqlcgen.WorkflowStepOutcomeStatusNeedsFix {
		t.Fatalf("FinishStepRun returned OutcomeStatus = %v, want needs_fix (COALESCE must preserve the concurrent post)", finished.OutcomeStatus)
	}

	// Reassemble the same minimal workflow.Definition LoadDefinition would
	// (unexported, so duplicated here rather than reached across the
	// workflowengine_test package boundary) -- the built-in request lane's
	// single step, no edges (dispatch_integration_test.go's own
	// builtInRequestDefID/builtInRequestStepID; NextStep itself only ever
	// reads Steps[].ID/Order/Edges, so no other field is needed here).
	runRow, err := workflows.GetRun(ctx, row.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	steps, err := workflows.ListStepDefinitions(ctx, runRow.WorkflowDefinitionID)
	if err != nil {
		t.Fatalf("ListStepDefinitions: %v", err)
	}
	edges, err := workflows.ListEdgesForDefinition(ctx, runRow.WorkflowDefinitionID)
	if err != nil {
		t.Fatalf("ListEdgesForDefinition: %v", err)
	}
	edgesByStep := make(map[string][]workflow.Edge, len(edges))
	for _, e := range edges {
		from := e.FromStepID.String()
		edgesByStep[from] = append(edgesByStep[from], workflow.Edge{
			FromStepID: workflow.ID(from),
			OnStatus:   workflow.StepOutcomeStatus(e.OnStatus),
			ToStepID:   workflow.ID(e.ToStepID.String()),
		})
	}
	def := workflow.Definition{ID: workflow.ID(runRow.WorkflowDefinitionID.String())}
	for _, s := range steps {
		def.Steps = append(def.Steps, workflow.StepDefinition{
			ID:    workflow.ID(s.ID.String()),
			Order: int(s.StepOrder),
			Edges: edgesByStep[s.ID.String()],
		})
	}
	stepID := workflow.ID(row.stepDefID.String())

	// The bug, made concrete: NextStep called with the STALE pre-read
	// outcome ("ok", no wired edge for it here so a bare StepOutcomeOK
	// completes) disagrees with NextStep called with FinishStepRun's own
	// authoritative RETURNING value ("needs_fix", no wired edge for it
	// either so it escalates instead). The fixed completion.go must use the
	// LATTER -- this is exactly the distinction this PR's fix makes.
	staleNext, err := workflow.NextStep(def, stepID, staleOutcome)
	if err != nil {
		t.Fatalf("NextStep(stale outcome): %v", err)
	}
	if staleNext.Kind != workflow.NextComplete {
		t.Fatalf("NextStep(stale outcome %q) = %s, want complete (test setup assumption: this is what the PRE-FIX bug would have silently done)", staleOutcome, staleNext.Kind)
	}

	authoritativeOutcome := workflow.StepOutcomeStatus(*finished.OutcomeStatus)
	fixedNext, err := workflow.NextStep(def, stepID, authoritativeOutcome)
	if err != nil {
		t.Fatalf("NextStep(authoritative outcome): %v", err)
	}
	if fixedNext.Kind != workflow.NextEscalate {
		t.Fatalf("NextStep(authoritative outcome %q) = %s, want escalate -- the fix must route on the POSTED needs_fix, never the stale pre-read ok", authoritativeOutcome, fixedNext.Kind)
	}
}
