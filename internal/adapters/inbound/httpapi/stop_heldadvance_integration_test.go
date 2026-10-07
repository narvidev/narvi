//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/platform"
)

// liveAttempt gives sessionID a workflow run of a custom two-step
// definition whose first attempt is live, and returns the run and the
// attempt.
func (r *stopRig) liveAttempt(ctx context.Context, t *testing.T, sessionID pgtype.UUID) (runID, attemptID pgtype.UUID) {
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
	return run.ID, attempt.ID
}

// heldAdvance gives sessionID a workflow run of a custom two-step
// definition whose first attempt finished ok and whose advance the
// autonomy freeze holds (workflow_advance_holds), as OnTurnCompleted
// leaves one while frozen, and returns the run.
func (r *stopRig) heldAdvance(ctx context.Context, t *testing.T, sessionID pgtype.UUID) pgtype.UUID {
	t.Helper()
	runID, attemptID := r.liveAttempt(ctx, t, sessionID)
	if _, err := r.workflows.FinishStepRun(ctx, attemptID, "completed", "ok"); err != nil {
		t.Fatalf("finish the first attempt: %v", err)
	}
	if held, err := r.workflows.HoldAdvance(ctx, runID, attemptID, sessionID); err != nil || !held {
		t.Fatalf("hold the advance: %v %v", held, err)
	}
	return runID
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

// TestStopSession_DropsAHoldCommittedWhileTheRequestWaitedForTheLock: the
// stop request drops every advance the session holds, whatever its held_at
// (technical plan §40.2, §3.3). The stop route here runs on a pool of its
// own, whose tracer holds it after its transaction began -- the instant
// the request records -- and right before it takes the session's
// actor-epoch lock. Meanwhile a transaction shaped as the session actor's
// end of a workflow attempt's turn while frozen begins, takes the lock,
// finishes the attempt and holds its advance, and commits: the hold's
// held_at, its transaction's start, is later than the request's instant.
// The route then takes the lock, and the hold it reads committed before
// the stop. It goes, and the run ends cancelled, so after the person
// resumes the session and autonomy is unfrozen, the releaser applies
// nothing. A rule of held at or before the request -- the stop timer's --
// would keep the hold, and the advance would start the next step on a
// session the person stopped.
func TestStopSession_DropsAHoldCommittedWhileTheRequestWaitedForTheLock(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, stopRigConfig{noWake: true})
	owner, token := r.user(ctx, t, sqlcgen.UserRoleMember)
	settings := narvipg.NewPlatformSettingsStore(r.pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
	session := r.session(ctx, t, owner.ID, pgtype.UUID{})
	runID, attemptID := r.liveAttempt(ctx, t, session.ID)

	pause := newPauseBeforeQuery("-- name: GetSessionActorEpochForUpdate ")
	_, connStr := httpapi.IntegrationTestPoolAndConnStr(t)
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = pause
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	router := chi.NewRouter()
	router.Route("/api/sessions", func(api chi.Router) {
		api.Use(auth.Middleware(r.userSessions, r.users))
		api.Post("/{sessionID}/stop", httpapi.StopSession(httpapi.StopSessionDeps{
			Pool:             traced,
			Sessions:         narvipg.NewSessionStore(traced),
			Turns:            narvipg.NewTurnStore(traced),
			Timers:           narvipg.NewTimerStore(traced),
			Participants:     narvipg.NewParticipantStore(traced),
			AuditLog:         narvipg.NewAuditLogStore(traced),
			GitHubPRSessions: narvipg.NewGitHubPRSessionStore(traced),
			Timeouts:         r.timeouts,
		}))
	})

	var eg errgroup.Group
	resume := sync.OnceFunc(func() { close(pause.resume) })
	defer func() {
		resume()
		_ = eg.Wait()
	}()
	stopDone := make(chan struct{})
	var stopStatus int
	eg.Go(func() error {
		defer close(stopDone)
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+session.ID.String()+"/stop", http.NoBody)
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		stopStatus = rec.Code
		return nil
	})
	select {
	case <-pause.paused:
	case <-stopDone:
		t.Fatalf("the stop answered %d without reaching the session's lock", stopStatus)
	}

	// The attempt's turn ends while frozen, as the session actor ends it:
	// in a transaction that begins now, after the stop's, and takes the
	// session's lock first.
	actor, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = actor.Rollback(ctx) }()
	if _, err := r.sessions.WithTx(actor).GetActorEpochForUpdate(ctx, session.ID); err != nil {
		t.Fatalf("the actor's lock: %v", err)
	}
	if _, err := r.workflows.WithTx(actor).FinishStepRun(ctx, attemptID, "completed", "ok"); err != nil {
		t.Fatalf("finish the attempt: %v", err)
	}
	if held, err := r.workflows.WithTx(actor).HoldAdvance(ctx, runID, attemptID, session.ID); err != nil || !held {
		t.Fatalf("hold the advance: %v %v", held, err)
	}
	if err := actor.Commit(ctx); err != nil {
		t.Fatalf("commit the actor's transaction: %v", err)
	}
	hold, err := r.workflows.GetAdvanceHold(ctx, runID)
	if err != nil {
		t.Fatalf("read the hold: %v", err)
	}

	resume()
	<-stopDone
	if stopStatus != http.StatusAccepted {
		t.Fatalf("stop: %d, want 202", stopStatus)
	}
	if row := r.sessionRow(ctx, t, session.ID); !row.StopRequestedAt.Valid || !hold.HeldAt.Time.After(row.StopRequestedAt.Time) {
		t.Fatalf("held at %v, the stop requested at %v: want the hold later than the request, the interleaving this test is about", hold.HeldAt.Time, row.StopRequestedAt)
	}
	if status, stepRuns, held := r.heldState(ctx, t, runID); status != "cancelled" || stepRuns != 1 || held {
		t.Errorf("the run when the route answers: %s, %d step runs, held %v; want cancelled, its first attempt alone, no hold", status, stepRuns, held)
	}

	// The person resumes the session, and autonomy is unfrozen.
	r.prompt(t, session.ID, token, "carry on")
	if row := r.sessionRow(ctx, t, session.ID); row.StopRequestedAt.Valid {
		t.Fatal("the person's prompt left the stop standing")
	}
	if _, err := settings.Unfreeze(ctx); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
	releaser, err := workflowengine.NewHeldAdvanceReleaser(r.pool, turnguard.New(r.pool, nil, false), platform.DefaultTimeouts(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := releaser.ReleaseOnce(ctx); err != nil || n != 0 {
		t.Fatalf("ReleaseOnce = %d, %v; want nothing released on a session the person stopped", n, err)
	}
	if status, stepRuns, held := r.heldState(ctx, t, runID); status != "cancelled" || stepRuns != 1 || held {
		t.Fatalf("the run after the resume and the release: %s, %d step runs, held %v; want it cancelled, never advanced", status, stepRuns, held)
	}
}
