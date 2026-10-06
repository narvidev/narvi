//go:build integration

package workflowengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins the workflow engine's half of technical plan §40.2 on real
// Postgres: while autonomy is frozen only an advance holds -- completing or
// escalating a run proceeds -- a freeze that cannot be read applies
// nothing, a person's turn on a held run passes through untracked, a
// person's stop drops only the holds it postdates, and the releaser drops a
// hold whose run no longer runs.

// freezeEngine is one test's database and the stores its cases share.
type freezeEngine struct {
	pool      *pgxpool.Pool
	sessions  *postgres.SessionStore
	turns     *postgres.TurnStore
	workflows *postgres.WorkflowStore
}

func newFreezeEngine(t *testing.T) freezeEngine {
	t.Helper()
	pool := newTestPool(t)
	return freezeEngine{pool: pool, sessions: postgres.NewSessionStore(pool), turns: postgres.NewTurnStore(pool), workflows: postgres.NewWorkflowStore(pool)}
}

// freezeAutonomy freezes autonomy, lifting it when the test ends.
func (e freezeEngine) freezeAutonomy(ctx context.Context, t *testing.T) {
	t.Helper()
	settings := postgres.NewPlatformSettingsStore(e.pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
}

func (e freezeEngine) unfreezeAutonomy(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := postgres.NewPlatformSettingsStore(e.pool).Unfreeze(ctx); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
}

// startCustomRun creates a web session, an unbound custom definition of
// steps steps with no edges -- ok advances by Order and completes on the
// last, needs_fix and blocked escalate -- and a run of it whose attempt of
// the first step has a processing turn attached.
func (e freezeEngine) startCustomRun(ctx context.Context, t *testing.T, name string, steps int) (session sqlcgen.Session, runID, stepRunID, turnID pgtype.UUID) {
	t.Helper()
	session = newSession(t, ctx, e.sessions)
	var defID pgtype.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`, name).Scan(&defID); err != nil {
		t.Fatalf("insert the definition: %v", err)
	}
	var first pgtype.UUID
	for i := 0; i < steps; i++ {
		var id pgtype.UUID
		if err := e.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, $2, 'agent', '{{prompt}}') RETURNING id`,
			defID, i+1).Scan(&id); err != nil {
			t.Fatalf("insert step %d: %v", i+1, err)
		}
		if i == 0 {
			first = id
		}
	}
	run, err := e.workflows.CreateRun(ctx, session.ID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create the run: %v", err)
	}
	stepRun, err := e.workflows.CreateStepRun(ctx, run.ID, first)
	if err != nil {
		t.Fatalf("create the attempt: %v", err)
	}
	prompt := "do the first step"
	created, err := e.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create the attempt's turn: %v", err)
	}
	if err := e.workflows.AttachTurn(ctx, stepRun.ID, created.ID); err != nil {
		t.Fatalf("attach the turn: %v", err)
	}
	return session, run.ID, stepRun.ID, created.ID
}

// endAttempt ends the attempt's turn with outcome through OnTurnCompleted,
// in a committed transaction, the freeze bound to it (inTx).
func (e freezeEngine) endAttempt(ctx context.Context, t *testing.T, session sqlcgen.Session, stepRunID, turnID pgtype.UUID, outcome string) {
	t.Helper()
	inTx(t, ctx, e.pool, testDeps(e.pool, e.turns, e.workflows), func(deps workflowengine.Deps) {
		completeWithOutcome(t, ctx, deps, session, stepRunID, turnID, outcome)
	})
}

// state reads the run's status, its step runs, the session's turns, and
// whether the run holds an advance.
func (e freezeEngine) state(ctx context.Context, t *testing.T, sessionID, runID pgtype.UUID) (runStatus string, stepRuns, turns int, held bool) {
	t.Helper()
	runRow, err := e.workflows.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("read the run: %v", err)
	}
	stepRuns = countWhere(ctx, t, e.pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID)
	turns = countWhere(ctx, t, e.pool, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID)
	_, err = e.workflows.GetAdvanceHold(ctx, runID)
	return string(runRow.Status), stepRuns, turns, err == nil
}

func (e freezeEngine) releaser(t *testing.T) *workflowengine.HeldAdvanceReleaser {
	t.Helper()
	r, err := workflowengine.NewHeldAdvanceReleaser(e.pool, turnguard.New(e.pool, nil, false), platform.DefaultTimeouts(), false, false)
	if err != nil {
		t.Fatalf("NewHeldAdvanceReleaser: %v", err)
	}
	return r
}

// TestWorkflowAdvance_Frozen_OnlyTheAdvanceHolds: while autonomy is frozen
// an attempt whose next step would advance holds the advance, while one
// that completes or escalates its run proceeds -- neither starts anything.
func TestWorkflowAdvance_Frozen_OnlyTheAdvanceHolds(t *testing.T) {
	ctx := context.Background()
	e := newFreezeEngine(t)
	e.freezeAutonomy(ctx, t)

	for _, tc := range []struct {
		name         string
		steps        int
		outcome      string
		wantRun      string
		wantHeld     bool
		wantStepRuns int
	}{
		{name: "ok on the first of two steps advances: held", steps: 2, outcome: "ok", wantRun: "running", wantHeld: true, wantStepRuns: 1},
		{name: "ok on the last step completes the run", steps: 1, outcome: "ok", wantRun: "completed", wantStepRuns: 1},
		{name: "an unrouted needs_fix escalates the run", steps: 2, outcome: "needs_fix", wantRun: "needs_review", wantStepRuns: 1},
		{name: "an unrouted blocked escalates the run", steps: 2, outcome: "blocked", wantRun: "needs_review", wantStepRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, runID, stepRunID, turnID := e.startCustomRun(ctx, t, "test-frozen-"+tc.name, tc.steps)
			e.endAttempt(ctx, t, session, stepRunID, turnID, tc.outcome)

			runStatus, stepRuns, turns, held := e.state(ctx, t, session.ID, runID)
			if runStatus != tc.wantRun || held != tc.wantHeld || stepRuns != tc.wantStepRuns || turns != 1 {
				t.Fatalf("run %s, held %v, %d step runs, %d turns; want run %s, held %v, %d step runs, the attempt's turn alone",
					runStatus, held, stepRuns, turns, tc.wantRun, tc.wantHeld, tc.wantStepRuns)
			}
		})
	}
}

// TestWorkflowAdvance_FreezeUnreadable_NothingApplied: a freeze that cannot
// be read is a skip, never a pass (§40.2). The read fails inside the
// transaction ending the attempt, which then cannot commit, so nothing of
// the turn's end lands -- no advance, no hold -- and the end is retried by
// the caller. The releaser releases nothing while the freeze cannot be
// read.
func TestWorkflowAdvance_FreezeUnreadable_NothingApplied(t *testing.T) {
	ctx := context.Background()
	e := newFreezeEngine(t)
	session, runID, stepRunID, turnID := e.startCustomRun(ctx, t, "test-unreadable", 2)
	if _, err := e.pool.Exec(ctx, `ALTER TABLE platform_settings RENAME TO platform_settings_unreadable`); err != nil {
		t.Fatalf("make the freeze unreadable: %v", err)
	}

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deps := testDeps(e.pool, e.turns, e.workflows)
	deps.Workflows = e.workflows.WithTx(tx)
	deps.Turns = e.turns.WithTx(tx)
	deps.SlackThreadSessions = deps.SlackThreadSessions.WithTx(tx)
	deps.LinearAgentSessions = deps.LinearAgentSessions.WithTx(tx)
	deps.GitHubPRSessions = deps.GitHubPRSessions.WithTx(tx)
	deps.Outbox = deps.Outbox.WithTx(tx)
	deps.Guard = turnguard.New(e.pool, nil, false).WithTx(tx, nil)
	deps.Origin = sessionguard.OriginAutomatic
	gate, err := autonomy.NewGate(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	deps.Autonomy = gate.WithTx(tx)
	completeWithOutcome(t, ctx, deps, session, stepRunID, turnID, "ok")
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("the transaction ending the attempt committed after the freeze could not be read, want it aborted")
	}

	runStatus, stepRuns, turns, held := e.state(ctx, t, session.ID, runID)
	attempt, err := e.workflows.GetStepRun(ctx, stepRunID)
	if err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || stepRuns != 1 || turns != 1 || held || attempt.Status != sqlcgen.WorkflowStepRunStatusRunning {
		t.Fatalf("run %s, %d step runs, %d turns, held %v, attempt %s; want nothing applied: the attempt still running, no next attempt, no hold",
			runStatus, stepRuns, turns, held, attempt.Status)
	}

	if _, err := e.pool.Exec(ctx, `INSERT INTO workflow_advance_holds (workflow_run_id, step_run_id, session_id) VALUES ($1, $2, $3)`, runID, stepRunID, session.ID); err != nil {
		t.Fatalf("hold the advance: %v", err)
	}
	if n, err := e.releaser(t).ReleaseOnce(ctx); err == nil || n != 0 {
		t.Fatalf("ReleaseOnce with the freeze unreadable = %d, %v; want an error and nothing released", n, err)
	}
	if _, _, _, held := e.state(ctx, t, session.ID, runID); !held {
		t.Fatal("the hold is gone after a release that could not read the freeze, want it kept")
	}
}

// TestHeldAdvance_PersonsTurnStopAndStaleHold covers what reaches a held
// advance besides the releaser's apply: a person's turn on the session
// passes through untracked and creates no attempt; a person's stop drops
// only the holds it postdates, cancelling their runs; a release that a
// freeze landed on after the tick's own read reads it again in its
// transaction and holds the advance still; and the releaser drops a hold
// whose run no longer runs, starting nothing.
func TestHeldAdvance_PersonsTurnStopAndStaleHold(t *testing.T) {
	ctx := context.Background()
	e := newFreezeEngine(t)
	e.freezeAutonomy(ctx, t)
	heldRun := func(t *testing.T, name string) (sqlcgen.Session, pgtype.UUID) {
		t.Helper()
		session, runID, stepRunID, turnID := e.startCustomRun(ctx, t, name, 2)
		e.endAttempt(ctx, t, session, stepRunID, turnID, "ok")
		if _, _, _, held := e.state(ctx, t, session.ID, runID); !held {
			t.Fatal("the advance was not held")
		}
		// The cases share one database: no hold outlives its case.
		t.Cleanup(func() {
			_, _ = e.pool.Exec(context.Background(), `DELETE FROM workflow_advance_holds WHERE workflow_run_id = $1`, runID)
		})
		return session, runID
	}

	t.Run("a person's turn passes through untracked", func(t *testing.T) {
		session, runID := heldRun(t, "test-held-persons-turn")
		resolution := workflowengine.ResolveStepForNewTurn(ctx, e.workflows, session, "a person's prompt", nil, nil)
		if resolution.Tracked || resolution.Prompt != "a person's prompt" {
			t.Fatalf("resolution = %+v, want the person's prompt passed through untracked", resolution)
		}
		if runStatus, stepRuns, _, held := e.state(ctx, t, session.ID, runID); runStatus != "running" || stepRuns != 1 || !held {
			t.Fatalf("run %s, %d step runs, held %v; want the run and its hold as they were", runStatus, stepRuns, held)
		}
	})

	t.Run("a person's stop drops only the holds it postdates", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			stop        bool
			offset      time.Duration // the stop request's instant, from the hold's
			wantRun     string
			wantDropped bool
		}{
			{name: "a stop after the hold", stop: true, offset: time.Second, wantRun: "cancelled", wantDropped: true},
			{name: "a stop at the hold", stop: true, wantRun: "cancelled", wantDropped: true},
			{name: "a stop before the hold", stop: true, offset: -time.Second, wantRun: "running"},
			{name: "no stop standing", wantRun: "running"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				session, runID := heldRun(t, "test-held-stop-"+tc.name)
				hold, err := e.workflows.GetAdvanceHold(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				stopAt := pgtype.Timestamptz{Time: hold.HeldAt.Time.Add(tc.offset), Valid: tc.stop}
				if err := workflowengine.CancelHeldAdvancesForStop(ctx, e.workflows, session.ID, stopAt); err != nil {
					t.Fatalf("CancelHeldAdvancesForStop: %v", err)
				}
				runStatus, stepRuns, turns, held := e.state(ctx, t, session.ID, runID)
				if runStatus != tc.wantRun || held == tc.wantDropped || stepRuns != 1 || turns != 1 {
					t.Fatalf("run %s, held %v, %d step runs, %d turns; want run %s, dropped %v, nothing started",
						runStatus, held, stepRuns, turns, tc.wantRun, tc.wantDropped)
				}
			})
		}
	})

	t.Run("a freeze that lands after the tick's read holds the advance still", func(t *testing.T) {
		session, runID := heldRun(t, "test-held-mid-tick")
		hold, err := e.workflows.GetAdvanceHold(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		released, err := e.releaser(t).ReleaseHoldForTest(ctx, hold)
		if err != nil || released {
			t.Fatalf("a release while frozen = %v, %v; want the advance held, no error", released, err)
		}
		runStatus, stepRuns, turns, held := e.state(ctx, t, session.ID, runID)
		if runStatus != "running" || !held || stepRuns != 1 || turns != 1 {
			t.Fatalf("run %s, held %v, %d step runs, %d turns; want the hold kept and nothing started", runStatus, held, stepRuns, turns)
		}
	})

	t.Run("the releaser drops a hold whose run no longer runs", func(t *testing.T) {
		session, runID := heldRun(t, "test-held-stale")
		if _, err := e.pool.Exec(ctx, `UPDATE workflow_runs SET status = 'needs_review' WHERE id = $1`, runID); err != nil {
			t.Fatal(err)
		}
		e.unfreezeAutonomy(ctx, t)
		if n, err := e.releaser(t).ReleaseOnce(ctx); err != nil || n != 0 {
			t.Fatalf("ReleaseOnce = %d, %v; want nothing applied", n, err)
		}
		runStatus, stepRuns, turns, held := e.state(ctx, t, session.ID, runID)
		if runStatus != "needs_review" || held || stepRuns != 1 || turns != 1 {
			t.Fatalf("run %s, held %v, %d step runs, %d turns; want the stale hold dropped and the run untouched, nothing started",
				runStatus, held, stepRuns, turns)
		}
	})
}
