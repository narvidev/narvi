//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestRun_StartsTheHeldAdvanceReleaser proves App.Run starts the workflow
// engine's held-advance releaser (technical plan §40.2) -- nothing else
// releases what the freeze held, and with no releaser running every held
// run would stay running with no live attempt after the unfreeze. The
// releaser releases once as it starts, so on a database holding one stale
// hold (its run no longer running, so its release starts nothing on the
// session and spawns no sandbox) with autonomy not frozen, the hold is gone
// within seconds of Run starting, its run left as it was. Run then shuts
// down cleanly. Without the releaser's goroutine in Run, the hold stays.
func TestRun_StartsTheHeldAdvanceReleaser(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)
	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	ctx := context.Background()

	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create the session: %v", err)
	}
	var defID, stepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'run-releaser', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("insert the definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert the step: %v", err)
	}
	var runID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_runs (session_id, lane, workflow_definition_id, definition_version, status) VALUES ($1, 'request', $2, 1, 'needs_review') RETURNING id`, session.ID, defID).Scan(&runID); err != nil {
		t.Fatalf("insert the run: %v", err)
	}
	var stepRunID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_runs (workflow_run_id, step_definition_id, status, outcome_status, finished_at) VALUES ($1, $2, 'completed', 'ok', now()) RETURNING id`, runID, stepID).Scan(&stepRunID); err != nil {
		t.Fatalf("insert the attempt: %v", err)
	}
	if held, err := narvipg.NewWorkflowStore(pool).HoldAdvance(ctx, runID, stepRunID, session.ID); err != nil || !held {
		t.Fatalf("hold the advance: %v %v", held, err)
	}

	app, err := Build(ctx, cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	var running errgroup.Group
	running.Go(func() error { return app.Run(runCtx, "127.0.0.1:0") })
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		stopRun()
		return running.Wait()
	}
	t.Cleanup(func() { _ = stop() })

	waitFor(t, 15*time.Second, "the releaser App.Run starts to drop the stale hold", func() bool {
		_, err := narvipg.NewWorkflowStore(pool).GetAdvanceHold(ctx, runID)
		return errors.Is(err, pgx.ErrNoRows)
	})
	run, err := narvipg.NewWorkflowStore(pool).GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sqlcgen.WorkflowRunStatusNeedsReview {
		t.Fatalf("the stale hold's run is %s after the release, want it left needs_review", run.Status)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
