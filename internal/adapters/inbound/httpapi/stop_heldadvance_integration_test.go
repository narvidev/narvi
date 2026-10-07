//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/platform"
)

// heldAdvance gives sessionID a workflow run of a custom two-step
// definition whose first attempt finished ok and whose advance the
// autonomy freeze holds (workflow_advance_holds), as OnTurnCompleted
// leaves one while frozen, and returns the run.
func (r *stopRig) heldAdvance(ctx context.Context, t *testing.T, sessionID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var defID, firstStepID pgtype.UUID
	if err := r.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("stop-held-%d", time.Now().UnixNano())).Scan(&defID); err != nil {
		t.Fatalf("insert workflow definition: %v", err)
	}
	if err := r.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&firstStepID); err != nil {
		t.Fatalf("insert the first step: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 2, 'agent', '{{prompt}}')`, defID); err != nil {
		t.Fatalf("insert the second step: %v", err)
	}
	run, err := r.workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	attempt, err := r.workflows.CreateStepRun(ctx, run.ID, firstStepID)
	if err != nil {
		t.Fatalf("create the first attempt: %v", err)
	}
	if _, err := r.workflows.FinishStepRun(ctx, attempt.ID, "completed", "ok"); err != nil {
		t.Fatalf("finish the first attempt: %v", err)
	}
	if held, err := r.workflows.HoldAdvance(ctx, run.ID, attempt.ID, sessionID); err != nil || !held {
		t.Fatalf("hold the advance: %v %v", held, err)
	}
	return run.ID
}

// heldState reads a run's status, its step runs, and whether it holds an
// advance.
func (r *stopRig) heldState(ctx context.Context, t *testing.T, runID pgtype.UUID) (status string, stepRuns int, held bool) {
	t.Helper()
	run, err := r.workflows.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID).Scan(&stepRuns); err != nil {
		t.Fatal(err)
	}
	_, err = r.workflows.GetAdvanceHold(ctx, runID)
	return string(run.Status), stepRuns, err == nil
}

// TestStopSession_DropsHeldWorkflowAdvancesInItsOwnTransaction: a person's
// stop reaches a workflow advance the autonomy freeze holds (technical plan
// §40.2, §3.3) in the stop request's own transaction, not later through the
// actor -- with no actor woken here at all, the hold is gone and its run
// cancelled the moment the route answers. So a resume that commits right
// after it -- the person's next prompt, which clears the session's stop
// request -- cannot revive it: once autonomy is unfrozen, the releaser
// applies nothing on that session. Another session's held advance, held
// before the stop, is left alone, and is released.
func TestStopSession_DropsHeldWorkflowAdvancesInItsOwnTransaction(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, stopRigConfig{noWake: true})
	owner, token := r.user(ctx, t, sqlcgen.UserRoleMember)
	settings := narvipg.NewPlatformSettingsStore(r.pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })

	other := r.session(ctx, t, owner.ID, pgtype.UUID{})
	otherRun := r.heldAdvance(ctx, t, other.ID)
	stopped := r.session(ctx, t, owner.ID, pgtype.UUID{})
	stoppedRun := r.heldAdvance(ctx, t, stopped.ID)

	if status, _ := r.stop(t, stopped.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: %d, want 202", status)
	}
	if status, stepRuns, held := r.heldState(ctx, t, stoppedRun); status != "cancelled" || stepRuns != 1 || held {
		t.Fatalf("the stopped session's run when the route answers: %s, %d step runs, held %v; want cancelled, its first attempt alone, no hold", status, stepRuns, held)
	}
	if status, _, held := r.heldState(ctx, t, otherRun); status != "running" || !held {
		t.Fatalf("another session's run after the stop: %s, held %v; want it running and held", status, held)
	}

	// The person resumes the session at once.
	r.prompt(t, stopped.ID, token, "carry on")
	if row := r.sessionRow(ctx, t, stopped.ID); row.StopRequestedAt.Valid {
		t.Fatal("the person's prompt left the stop standing")
	}

	if _, err := settings.Unfreeze(ctx); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
	releaser, err := workflowengine.NewHeldAdvanceReleaser(r.pool, turnguard.New(r.pool, nil, false), platform.DefaultTimeouts(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := releaser.ReleaseOnce(ctx); err != nil || n != 1 {
		t.Fatalf("ReleaseOnce = %d, %v; want the other session's advance alone released", n, err)
	}
	if status, stepRuns, held := r.heldState(ctx, t, stoppedRun); status != "cancelled" || stepRuns != 1 || held {
		t.Fatalf("the stopped session's run after the resume and the release: %s, %d step runs, held %v; want it still cancelled, never advanced", status, stepRuns, held)
	}
	if status, stepRuns, held := r.heldState(ctx, t, otherRun); status != "running" || stepRuns != 2 || held {
		t.Fatalf("the other session's run after the release: %s, %d step runs, held %v; want its next attempt", status, stepRuns, held)
	}
}
