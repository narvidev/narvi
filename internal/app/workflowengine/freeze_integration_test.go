//go:build integration

package workflowengine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
	"github.com/narvidev/narvi/internal/domain/loopguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/turn"
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
	e.endAttemptBy(ctx, t, session, stepRunID, turnID, outcome, turn.TriggerComplete)
}

// endAttemptBy is endAttempt with the turn ended by trig: the outcome is
// posted first, as the step-outcome tool posts it during the turn, and
// kept whatever trig implies (FinishStepRun's COALESCE).
func (e freezeEngine) endAttemptBy(ctx context.Context, t *testing.T, session sqlcgen.Session, stepRunID, turnID pgtype.UUID, outcome string, trig turn.Trigger) {
	t.Helper()
	inTx(t, ctx, e.pool, testDeps(e.pool, e.turns, e.workflows), func(deps workflowengine.Deps) {
		if _, err := deps.Workflows.SetStepRunOutcome(ctx, stepRunID, outcome, "test outcome: "+outcome, nil); err != nil {
			t.Fatalf("SetStepRunOutcome(%s): %v", outcome, err)
		}
		workflowengine.OnTurnCompleted(ctx, deps, session, turnID, trig)
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
		trig         turn.Trigger
		wantRun      string
		wantHeld     bool
		wantStepRuns int
	}{
		{name: "ok on the first of two steps advances: held", steps: 2, outcome: "ok", trig: turn.TriggerComplete, wantRun: "running", wantHeld: true, wantStepRuns: 1},
		// turn_deadline's end (timerfired.go) and an undelivered prompt's
		// (dispatch.go) reach OnTurnCompleted with TriggerTimeout: an ok
		// posted before it still advances, and is held the same.
		{name: "ok posted, then the turn timed out: held", steps: 2, outcome: "ok", trig: turn.TriggerTimeout, wantRun: "running", wantHeld: true, wantStepRuns: 1},
		{name: "ok on the last step completes the run", steps: 1, outcome: "ok", trig: turn.TriggerComplete, wantRun: "completed", wantStepRuns: 1},
		{name: "an unrouted needs_fix escalates the run", steps: 2, outcome: "needs_fix", trig: turn.TriggerComplete, wantRun: "needs_review", wantStepRuns: 1},
		{name: "an unrouted blocked escalates the run", steps: 2, outcome: "blocked", trig: turn.TriggerComplete, wantRun: "needs_review", wantStepRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, runID, stepRunID, turnID := e.startCustomRun(ctx, t, "test-frozen-"+tc.name, tc.steps)
			e.endAttemptBy(ctx, t, session, stepRunID, turnID, tc.outcome, tc.trig)

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
	before := workflowAdvanceSkips(t, domainautonomy.SkipFreezeUnreadable)
	completeWithOutcome(t, ctx, deps, session, stepRunID, turnID, "ok")
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("the transaction ending the attempt committed after the freeze could not be read, want it aborted")
	}
	if got := workflowAdvanceSkips(t, domainautonomy.SkipFreezeUnreadable) - before; got != 1 {
		t.Fatalf("workflow_advance skips counted freeze_unreadable = %d, want 1: a read that fails is a counted skip", got)
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
				// Another session's advance, held before this one: a stop
				// reaches its own session's holds alone.
				bystander, bystanderRun := heldRun(t, "test-held-bystander-"+tc.name)
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
				if runStatus, _, _, held := e.state(ctx, t, bystander.ID, bystanderRun); runStatus != "running" || !held {
					t.Fatalf("another session's held run after this stop: %s, held %v; want it running and held", runStatus, held)
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

// TestHeldAdvanceReleaser_DrainsEveryPageOldestFirstWithTheStoredSummary:
// once autonomy is unfrozen, one release tick applies every held advance,
// however many pages they take -- five holds, two to a page -- oldest
// hold first, whatever order their runs were created in; and each next
// attempt's turn is told what the stored outcome summary said, the account
// of what the finished step found, never the generic instruction.
func TestHeldAdvanceReleaser_DrainsEveryPageOldestFirstWithTheStoredSummary(t *testing.T) {
	ctx := context.Background()
	e := newFreezeEngine(t)
	e.freezeAutonomy(ctx, t)

	type held struct {
		run     pgtype.UUID
		summary string
	}
	var holds []held
	for i := range 5 {
		session, runID, stepRunID, turnID := e.startCustomRun(ctx, t, fmt.Sprintf("test-drain-%d", i), 2)
		e.endAttempt(ctx, t, session, stepRunID, turnID, "ok")
		summary := fmt.Sprintf("fix the third finding of run %d", i)
		if _, err := e.pool.Exec(ctx, `UPDATE workflow_step_runs SET outcome_summary = $2 WHERE id = $1`, stepRunID, summary); err != nil {
			t.Fatal(err)
		}
		// The last run created is the oldest hold.
		if _, err := e.pool.Exec(ctx, `UPDATE workflow_advance_holds SET held_at = now() - make_interval(secs => $2::int) WHERE workflow_run_id = $1`, runID, 10*(i+1)); err != nil {
			t.Fatal(err)
		}
		holds = append(holds, held{run: runID, summary: summary})
	}

	e.unfreezeAutonomy(ctx, t)
	r := e.releaser(t)
	r.SetPageSizeForTest(2)
	if n, err := r.ReleaseOnce(ctx); err != nil || n != 5 {
		t.Fatalf("one tick released %d (%v), want all 5 holds, over three pages", n, err)
	}
	if n := countWhere(ctx, t, e.pool, `SELECT count(*) FROM workflow_advance_holds`); n != 0 {
		t.Fatalf("%d holds left after the tick, want none", n)
	}

	// The order the next attempts were created in, oldest hold first.
	var order []pgtype.UUID
	rows, err := e.pool.Query(ctx, `SELECT sr.workflow_run_id FROM workflow_step_runs sr WHERE sr.status = 'running' ORDER BY sr.created_at, sr.id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		order = append(order, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []pgtype.UUID{holds[4].run, holds[3].run, holds[2].run, holds[1].run, holds[0].run}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("next attempts created in run order %v, want oldest hold first %v", order, want)
	}

	for i, h := range holds {
		live, err := e.workflows.GetLiveStepRunForRun(ctx, h.run)
		if err != nil {
			t.Fatalf("run %d's next attempt: %v", i, err)
		}
		next, err := e.turns.Get(ctx, live.TurnID)
		if err != nil {
			t.Fatalf("run %d's next turn: %v", i, err)
		}
		if next.Prompt == nil || *next.Prompt != h.summary {
			t.Fatalf("run %d's next turn is told %v, want the stored summary %q", i, next.Prompt, h.summary)
		}
	}
}

// TestHeldAdvanceReleaser_WaitsForAStopHoldingTheSessionLock: a release
// takes the session's actor-epoch lock first, as a stop request and every
// session actor transaction do. A stop recorded while the release runs --
// its transaction holding the lock, not yet committed, and dropping no
// hold itself, as a replica of the previous release records one -- makes
// the release wait; once it commits, the release sees the stop and cancels
// the run. No next attempt, no turn.
func TestHeldAdvanceReleaser_WaitsForAStopHoldingTheSessionLock(t *testing.T) {
	ctx := context.Background()
	e := newFreezeEngine(t)
	e.freezeAutonomy(ctx, t)
	session, runID, stepRunID, turnID := e.startCustomRun(ctx, t, "test-release-lock", 2)
	e.endAttempt(ctx, t, session, stepRunID, turnID, "ok")
	e.unfreezeAutonomy(ctx, t)

	stop, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop.Rollback(ctx) }()
	if _, err := e.sessions.WithTx(stop).GetActorEpochForUpdate(ctx, session.ID); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	if _, err := e.sessions.WithTx(stop).RequestStop(ctx, session.ID); err != nil {
		t.Fatalf("record the stop: %v", err)
	}

	var released int
	var release errgroup.Group
	done := make(chan struct{})
	release.Go(func() error {
		defer close(done)
		n, err := e.releaser(t).ReleaseOnce(ctx)
		released = n
		return err
	})
	// The release must be waiting on the stop's lock before the stop
	// commits; a release that read the session without waiting has
	// returned, or is blocked further on, by then.
	waiting := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !waiting; time.Sleep(20 * time.Millisecond) {
		select {
		case <-done:
			deadline = time.Now()
		default:
		}
		if err := e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := stop.Commit(ctx); err != nil {
		t.Fatalf("commit the stop: %v", err)
	}
	if err := release.Wait(); err != nil {
		t.Fatalf("ReleaseOnce: %v", err)
	}
	if !waiting {
		t.Fatal("the release never waited on the stop's lock")
	}

	runStatus, stepRuns, turns, held := e.state(ctx, t, session.ID, runID)
	if released != 1 || runStatus != "cancelled" || stepRuns != 1 || turns != 1 || held {
		t.Fatalf("released %d: run %s, %d step runs, %d turns, held %v; want the run cancelled by the stop it waited for, no next attempt, no hold",
			released, runStatus, stepRuns, turns, held)
	}
}

// TestWorkflowAdvance_Frozen_TheBreakersEscalationIsNotHeld: a routed
// needs_fix advance is an advance, so it is held while autonomy is frozen
// and applied once it is not. But one the circuit breaker turns into an
// escalation (§25.9) starts nothing: while frozen it escalates at once --
// the run needs_review, its one notice sent, no hold -- so the person it
// asks to review the run is told during the freeze.
func TestWorkflowAdvance_Frozen_TheBreakersEscalationIsNotHeld(t *testing.T) {
	ctx := context.Background()
	e := newFreezeEngine(t)
	slackThreadSessions := postgres.NewSlackThreadSessionStore(e.pool)
	session := slackOriginSessionWithThread(t, ctx, e.sessions, slackThreadSessions, "C-FROZEN-BREAKER", "1491.1491")
	def := seedAuditFixLoopDefinition(t, ctx, e.pool)
	deps := testDeps(e.pool, e.turns, e.workflows)
	deps.SlackThreadSessions = slackThreadSessions
	runID, auditStepRunID, auditTurnID := startRawRun(t, ctx, e.turns, e.workflows, session, def)
	notices := func() int { return countOutboxRowsOfKind(t, ctx, e.pool, session.ID, "slack_workflow_decision") }
	fixAttempts := func() int64 {
		n, err := e.workflows.CountStepRunsForStepDefinition(ctx, runID, def.fixStepID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	endAudit := func() {
		inTx(t, ctx, e.pool, deps, func(deps workflowengine.Deps) {
			completeWithOutcome(t, ctx, deps, session, auditStepRunID, auditTurnID, "needs_fix")
		})
	}
	loopBack := func() {
		fix, err := e.workflows.GetLiveStepRunForRun(ctx, runID)
		if err != nil || fix.StepDefinitionID != def.fixStepID {
			t.Fatalf("the live attempt %+v (%v), want the fix step's", fix, err)
		}
		inTx(t, ctx, e.pool, deps, func(deps workflowengine.Deps) {
			completeWithOutcome(t, ctx, deps, session, fix.ID, fix.TurnID, "ok")
		})
		audit, err := e.workflows.GetLiveStepRunForRun(ctx, runID)
		if err != nil || audit.StepDefinitionID != def.auditStepID {
			t.Fatalf("the live attempt %+v (%v), want the audit step's", audit, err)
		}
		auditStepRunID, auditTurnID = audit.ID, audit.TurnID
	}

	// The first needs_fix while frozen: an advance, held.
	e.freezeAutonomy(ctx, t)
	endAudit()
	if runStatus, _, _, held := e.state(ctx, t, session.ID, runID); runStatus != "running" || !held || fixAttempts() != 0 {
		t.Fatalf("a first needs_fix while frozen: run %s, held %v, %d fix attempts; want it held, no fix attempt", runStatus, held, fixAttempts())
	}
	e.unfreezeAutonomy(ctx, t)
	if n, err := e.releaser(t).ReleaseOnce(ctx); err != nil || n != 1 {
		t.Fatalf("ReleaseOnce = %d, %v; want the held needs_fix applied", n, err)
	}
	if fixAttempts() != 1 {
		t.Fatalf("fix attempts = %d after the release, want 1", fixAttempts())
	}

	// Loop until the next needs_fix is the one the breaker escalates.
	for loopguard.Evaluate(loopguard.State{AttemptCount: int(fixAttempts())}, loopguard.Config{MaxAttempts: loopguard.DefaultMaxAttempts}).ShouldEscalate == false {
		loopBack()
		endAudit()
		if runStatus, _, _, _ := e.state(ctx, t, session.ID, runID); runStatus != "running" {
			t.Fatalf("the loop ended %s before the breaker's bound, unfrozen", runStatus)
		}
	}
	loopBack()
	attempts := fixAttempts()

	e.freezeAutonomy(ctx, t)
	endAudit()
	runStatus, _, _, held := e.state(ctx, t, session.ID, runID)
	if runStatus != "needs_review" || held || fixAttempts() != attempts || notices() != 1 {
		t.Fatalf("the breaker's escalation while frozen: run %s, held %v, %d fix attempts (had %d), %d notices; want needs_review at once, no hold, no new attempt, one notice",
			runStatus, held, fixAttempts(), attempts, notices())
	}
}
