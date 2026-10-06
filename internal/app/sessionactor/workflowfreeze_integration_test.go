//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §40.2 at the workflow engine's automatic
// advance, through the real session actor on real Postgres: a step's turn
// that ends while autonomy is frozen finishes its attempt and holds the
// advance in a row -- no next attempt, no turn, no step run, the run still
// running -- and the releaser applies it exactly once after the freeze
// lifts, unless a person stopped the session meanwhile. A freeze severs no
// turn in flight, and is read before the spend cap (§40.1), so a frozen
// advance records no crossing.

// workflowFreezeFixture is a pull request's review session on a ready
// sandbox at gen 1, its repository bound to a custom two-step review
// workflow -- first, then second on ok, by Order -- whose first attempt is
// the review turn in flight.
type workflowFreezeFixture struct {
	pool         *pgxpool.Pool
	sessionID    pgtype.UUID
	repoFullName string
	secondStepID pgtype.UUID
	run          sqlcgen.WorkflowRun
	firstAttempt sqlcgen.WorkflowStepRun
	turn         sqlcgen.Turn
	workflows    *narvipg.WorkflowStore
	turns        *narvipg.TurnStore
}

func newWorkflowFreezeFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32) *workflowFreezeFixture {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       reposJSONForTest(t, "widgets", "https://github.com/"+repoFullName+".git", ""),
	})
	if err != nil {
		t.Fatalf("create the review session: %v", err)
	}
	claimPullRequest(ctx, t, pool, repoFullName, prNumber, session.ID)
	seedReadySandbox(ctx, t, pool, session.ID)

	var defID, firstStepID, secondStepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('review', $1, false, 1) RETURNING id`,
		"test-freeze-"+repoFullName).Scan(&defID); err != nil {
		t.Fatalf("insert the definition: %v", err)
	}
	for i, id := range []*pgtype.UUID{&firstStepID, &secondStepID} {
		if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, $2, 'agent', '{{prompt}}') RETURNING id`,
			defID, i+1).Scan(id); err != nil {
			t.Fatalf("insert step %d: %v", i+1, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version) VALUES ('review', $1, $2, 1)`,
		repoFullName, defID); err != nil {
		t.Fatalf("bind the definition to the repository: %v", err)
	}

	workflows := narvipg.NewWorkflowStore(pool)
	run, err := workflows.CreateRun(ctx, session.ID, "review", defID, 1)
	if err != nil {
		t.Fatalf("create the run: %v", err)
	}
	attempt, err := workflows.CreateStepRun(ctx, run.ID, firstStepID)
	if err != nil {
		t.Fatalf("create the first attempt: %v", err)
	}
	turns := narvipg.NewTurnStore(pool)
	prompt, head := "review this pull request", "sha-under-review"
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true,
	})
	if err != nil {
		t.Fatalf("create the first attempt's turn: %v", err)
	}
	gen := int32(1)
	watermark, err := narvipg.NewEventStore(pool).MaxEventIDForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("read the watermark: %v", err)
	}
	messageID := uuid.NewString()
	processing, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen, DispatchedEventID: &watermark, DispatchedMessageID: &messageID,
	})
	if err != nil {
		t.Fatalf("move the first attempt's turn to processing: %v", err)
	}
	if err := workflows.AttachTurn(ctx, attempt.ID, processing.ID); err != nil {
		t.Fatalf("attach the turn: %v", err)
	}
	// The package's tests share one database: no hold outlives its test.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_advance_holds WHERE session_id = $1`, session.ID)
	})
	return &workflowFreezeFixture{
		pool: pool, sessionID: session.ID, repoFullName: repoFullName, secondStepID: secondStepID,
		run: run, firstAttempt: attempt, turn: processing, workflows: workflows, turns: turns,
	}
}

// completeFirstAttempt ends the first attempt's turn with a real
// execution_complete through the actor, and waits for it to be completed.
func (f *workflowFreezeFixture) completeFirstAttempt(ctx context.Context, t *testing.T, rig *holdRig) {
	t.Helper()
	raw := executionCompleteRaw(t, f.sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted)
	var evt struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("decode execution_complete: %v", err)
	}
	if outcome := sendSandboxEventForTest(ctx, t, rig.actor, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: evt.MessageID, Raw: raw}); !outcome.Persisted {
		t.Fatal("execution_complete not persisted")
	}
	waitUntil(t, 5*time.Second, func() bool {
		got, err := f.turns.Get(ctx, f.turn.ID)
		return err == nil && turn.IsTerminal(turn.State(got.Status))
	})
	if got, _ := f.turns.Get(ctx, f.turn.ID); got.Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("the first attempt's turn ended %s, want completed", got.Status)
	}
}

// finishSnapshot answers the post-turn snapshot the first attempt's end
// sent the sandbox with its snapshot_ready, so the sandbox is ready again
// for the next turn.
func (f *workflowFreezeFixture) finishSnapshot(ctx context.Context, t *testing.T, rig *holdRig) {
	t.Helper()
	var snapshotCmd sandboxws.Snapshot
	waitUntil(t, 5*time.Second, func() bool {
		rig.commander.mu.Lock()
		defer rig.commander.mu.Unlock()
		for _, p := range rig.commander.payloads {
			var cmd sandboxws.Snapshot
			if json.Unmarshal(p, &cmd) == nil && cmd.Type == "snapshot" {
				snapshotCmd = cmd
				return true
			}
		}
		return false
	})
	raw := json.RawMessage(`{"type":"snapshot_ready","messageId":"sr-` + snapshotCmd.MessageId + `","sessionId":"` + f.sessionID.String() +
		`","gen":1,"ackId":"snapshot_ready:sr-` + snapshotCmd.MessageId + `","snapshotId":"snap-held-workflow","commandMessageId":"` + snapshotCmd.MessageId + `"}`)
	if outcome := sendSandboxEventForTest(ctx, t, rig.actor, SandboxEvent{Type: "snapshot_ready", Gen: 1, MessageID: "sr-" + snapshotCmd.MessageId, Raw: raw}); !outcome.Persisted {
		t.Fatal("snapshot_ready not persisted")
	}
	waitUntil(t, 5*time.Second, func() bool {
		var status string
		return f.pool.QueryRow(ctx, `SELECT status::text FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&status) == nil && status == string(sqlcgen.SandboxStatusReady)
	})
}

// state reads the run's status, its step runs, the session's turns, and
// the run's hold (the step run it holds the advance past, if any).
func (f *workflowFreezeFixture) state(ctx context.Context, t *testing.T) (runStatus string, stepRuns, turns int, heldPast pgtype.UUID, held bool) {
	t.Helper()
	runRow, err := f.workflows.GetRun(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read the run: %v", err)
	}
	stepRuns = countRows(ctx, t, f.pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, f.run.ID)
	turns = countRows(ctx, t, f.pool, `SELECT count(*) FROM turns WHERE session_id = $1`, f.sessionID)
	hold, err := f.workflows.GetAdvanceHold(ctx, f.run.ID)
	if err == nil {
		heldPast, held = hold.StepRunID, true
	}
	return string(runRow.Status), stepRuns, turns, heldPast, held
}

// assertHeld checks the advance past the first attempt is held: the
// attempt completed with its outcome stored, the run still running with no
// other step run, no turn but the attempt's, and one hold, past it.
func (f *workflowFreezeFixture) assertHeld(ctx context.Context, t *testing.T) {
	t.Helper()
	attempt, err := f.workflows.GetStepRun(ctx, f.firstAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != sqlcgen.WorkflowStepRunStatusCompleted || attempt.OutcomeStatus == nil || *attempt.OutcomeStatus != sqlcgen.WorkflowStepOutcomeStatusOk {
		t.Fatalf("the first attempt is %s with outcome %v, want completed ok: the attempt is finished as always", attempt.Status, attempt.OutcomeStatus)
	}
	runStatus, stepRuns, turns, heldPast, held := f.state(ctx, t)
	if runStatus != "running" || stepRuns != 1 || turns != 1 || !held || heldPast != f.firstAttempt.ID {
		t.Fatalf("run %s, %d step runs, %d turns, held %v past %v; want the run running, the first attempt alone, its turn alone, the advance held past it",
			runStatus, stepRuns, turns, held, heldPast)
	}
}

// newReleaser is the held-advance releaser on pool, as the control plane
// builds it.
func newReleaser(t *testing.T, pool *pgxpool.Pool) *workflowengine.HeldAdvanceReleaser {
	t.Helper()
	r, err := workflowengine.NewHeldAdvanceReleaser(pool, turnguard.New(pool, nil, false), platform.DefaultTimeouts(), false, false)
	if err != nil {
		t.Fatalf("NewHeldAdvanceReleaser: %v", err)
	}
	return r
}

// workflowAdvanceSkips reads autonomy_freeze_skip_total{site=
// workflow_advance, reason} from this binary's one meter provider.
func workflowAdvanceSkips(t *testing.T, reason domainautonomy.SkipReason) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("site", string(domainautonomy.SiteWorkflowAdvance)), attribute.String("reason", string(reason)))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "autonomy_freeze_skip_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("autonomy_freeze_skip_total is %T, want a Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// TestWorkflowAdvance_Frozen_HeldNoTurn: a step's turn that ends while
// autonomy is frozen finishes its attempt, with its outcome, and holds the
// advance in a row: no next attempt, no turn, nothing sent to the sandbox,
// no notice, the run still running -- and the skip is counted.
func TestWorkflowAdvance_Frozen_HeldNoTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newWorkflowFreezeFixture(ctx, t, pool, "acme/frozen-workflow", 1491)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
	freezeAutonomyForActorTest(ctx, t, pool)
	before := workflowAdvanceSkips(t, domainautonomy.SkipFrozen)

	f.completeFirstAttempt(ctx, t, rig)

	f.assertHeld(ctx, t)
	if got := workflowAdvanceSkips(t, domainautonomy.SkipFrozen) - before; got != 1 {
		t.Fatalf("workflow_advance skips counted = %d, want 1", got)
	}
	if n := promptCount(rig.commander); n != 0 {
		t.Fatalf("prompts sent = %d, want none while frozen", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, f.sessionID, string(ports.NotificationKindGitHubWorkflowDecision)); n != 0 {
		t.Fatalf("workflow notices = %d, want none: a held advance tells nobody", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`, f.sessionID, TimerDispatch); n != 0 {
		t.Fatalf("dispatch timers = %d, want none: nothing was queued", n)
	}
}

// TestWorkflowAdvance_Unfreeze_ReleasedOnce: once autonomy is unfrozen, two
// releasers racing over the held advance apply it once: one next attempt,
// of the second step, with one turn, the hold gone -- and the timer pump
// dispatches that turn with no other trigger. A later release finds
// nothing.
func TestWorkflowAdvance_Unfreeze_ReleasedOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newWorkflowFreezeFixture(ctx, t, pool, "acme/released-workflow", 1492)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
	freezeAutonomyForActorTest(ctx, t, pool)
	f.completeFirstAttempt(ctx, t, rig)
	f.finishSnapshot(ctx, t, rig)
	f.assertHeld(ctx, t)

	// Still frozen: nothing is released.
	if n, err := newReleaser(t, pool).ReleaseOnce(ctx); err != nil || n != 0 {
		t.Fatalf("ReleaseOnce while frozen = %d, %v; want nothing released", n, err)
	}
	unfreezeAutonomyForActorTest(ctx, t, pool)

	releasers := []*workflowengine.HeldAdvanceReleaser{newReleaser(t, pool), newReleaser(t, pool)}
	released := make([]int, len(releasers))
	var g errgroup.Group
	for i, r := range releasers {
		g.Go(func() error {
			n, err := r.ReleaseOnce(ctx)
			released[i] = n
			return err
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("ReleaseOnce: %v", err)
	}
	if total := released[0] + released[1]; total != 1 {
		t.Fatalf("released %v, want the one advance released once", released)
	}

	runStatus, stepRuns, turns, _, held := f.state(ctx, t)
	if runStatus != "running" || stepRuns != 2 || turns != 2 || held {
		t.Fatalf("after the release: run %s, %d step runs, %d turns, held %v; want one next attempt with one turn, the hold gone", runStatus, stepRuns, turns, held)
	}
	live, err := f.workflows.GetLiveStepRunForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("the next attempt: %v", err)
	}
	if live.StepDefinitionID != f.secondStepID || !live.TurnID.Valid {
		t.Fatalf("the next attempt is of step %v with turn %v, want the second step's, with its turn", live.StepDefinitionID, live.TurnID)
	}
	if n, err := newReleaser(t, pool).ReleaseOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a later ReleaseOnce = %d, %v; want nothing left", n, err)
	}
	if timer, ok := dispatchTimer(ctx, t, pool, f.sessionID); !ok || timer.FiresAt.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("dispatch timer after the release: present %v, due %v; want it armed due at once with the next attempt's turn", ok, timer.FiresAt.Time)
	}

	if err := rig.registry.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return promptCount(rig.commander) == 1 })
	next, err := f.turns.Get(ctx, live.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("the released attempt's turn is %s, want dispatched by the timer pump", next.Status)
	}
}

// TestWorkflowAdvance_StopWhileHeld_RunCancelled: a person's stop reaches a
// held advance whichever comes first. The stop timer drops the hold and
// cancels the run, so the advance is never applied, even once the person
// resumes the session and autonomy is unfrozen; and a releaser that meets
// the standing stop first cancels the run itself. Either way: no next
// attempt, no turn, the first attempt kept as it finished.
func TestWorkflowAdvance_StopWhileHeld_RunCancelled(t *testing.T) {
	ctx := context.Background()
	for i, tc := range []struct {
		name         string
		stopTimer    bool
		wantReleased int
	}{
		{name: "the stop timer drops the hold, then the person resumes", stopTimer: true, wantReleased: 0},
		{name: "the releaser meets the standing stop", stopTimer: false, wantReleased: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			f := newWorkflowFreezeFixture(ctx, t, pool, fmt.Sprintf("acme/stopped-held-workflow-%d", i), int32(1493+i))
			rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
			freezeAutonomyForActorTest(ctx, t, pool)
			f.completeFirstAttempt(ctx, t, rig)
			f.assertHeld(ctx, t)

			requestStop(ctx, t, pool, f.sessionID)
			if tc.stopTimer {
				if err := rig.actor.Send(ctx, TimerFired{Name: TimerStop}); err != nil {
					t.Fatalf("Send TimerFired: %v", err)
				}
				waitUntil(t, 5*time.Second, func() bool {
					runStatus, _, _, _, held := f.state(ctx, t)
					return runStatus == "cancelled" && !held
				})
				if _, err := narvipg.NewSessionStore(pool).ClearStopRequest(ctx, f.sessionID); err != nil {
					t.Fatalf("resume the session: %v", err)
				}
			}
			unfreezeAutonomyForActorTest(ctx, t, pool)
			if n, err := newReleaser(t, pool).ReleaseOnce(ctx); err != nil || n != tc.wantReleased {
				t.Fatalf("ReleaseOnce = %d, %v; want %d", n, err, tc.wantReleased)
			}

			runStatus, stepRuns, turns, _, held := f.state(ctx, t)
			if runStatus != "cancelled" || stepRuns != 1 || turns != 1 || held {
				t.Fatalf("run %s, %d step runs, %d turns, held %v; want the run cancelled with its first attempt alone, no turn queued, no hold",
					runStatus, stepRuns, turns, held)
			}
			attempt, err := f.workflows.GetStepRun(ctx, f.firstAttempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Status != sqlcgen.WorkflowStepRunStatusCompleted {
				t.Fatalf("the first attempt is %s, want kept completed", attempt.Status)
			}
		})
	}
}

// TestFreeze_ProcessingWorkflowTurnCompletesAndOnlyTheAdvanceHolds: a
// freeze severs no workflow turn in flight (§32.8, inherited). Frozen while
// the first attempt's turn runs, nothing is sent to the sandbox, the
// turn's deadline is untouched and the sandbox's gen is not retired; the
// turn's own execution_complete completes it and its attempt, and only the
// advance past it holds.
func TestFreeze_ProcessingWorkflowTurnCompletesAndOnlyTheAdvanceHolds(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newWorkflowFreezeFixture(ctx, t, pool, "acme/frozen-mid-workflow", 1495)
	timers := narvipg.NewTimerStore(pool)
	deadline, err := timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: f.sessionID, Name: TimerTurnDeadline,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().TurnDeadline), Valid: true},
	})
	if err != nil {
		t.Fatalf("arm the turn's deadline: %v", err)
	}
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
	freezeAutonomyForActorTest(ctx, t, pool)
	sendEnsureDispatched(ctx, t, rig.actor)

	if got, err := f.turns.Get(ctx, f.turn.ID); err != nil || got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("the running turn after the freeze: %v (%v), want still processing", got.Status, err)
	}
	if n := rig.commander.callCount(); n != 0 {
		t.Fatalf("%d commands sent to the sandbox after the freeze, want none: no stop", n)
	}
	after, err := timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerTurnDeadline})
	if err != nil {
		t.Fatalf("read the turn's deadline: %v", err)
	}
	if !after.FiresAt.Time.Equal(deadline.FiresAt.Time) || !after.ArmedAt.Time.Equal(deadline.ArmedAt.Time) {
		t.Fatalf("turn_deadline moved from %v to %v after the freeze, want untouched", deadline.FiresAt.Time, after.FiresAt.Time)
	}
	var status string
	var gen int32
	var retired *int32
	if err := pool.QueryRow(ctx, `SELECT status::text, gen, stop_retire_gen FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&status, &gen, &retired); err != nil {
		t.Fatalf("read the sandbox: %v", err)
	}
	if status != string(sqlcgen.SandboxStatusReady) || gen != 1 || retired != nil {
		t.Fatalf("sandbox %s gen %d retired %v after the freeze, want ready gen 1 not retired", status, gen, retired)
	}

	f.completeFirstAttempt(ctx, t, rig)
	f.assertHeld(ctx, t)
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND status IN ('failed', 'cancelled')`, f.sessionID); n != 0 {
		t.Fatalf("%d turns failed or cancelled, want none", n)
	}
}

// TestWorkflowAdvance_FrozenAtSpendCap_TheFreezeHoldsFirst pins the order
// of the two checks at the advance, as at the automatic re-review: the
// freeze (§40.2) is read before the session guard (§40.1) is asked. A
// session past its cap whose step ends while frozen holds the advance and
// records no crossing -- no warning, no notice, no escalation. Once
// unfrozen, the released advance meets the guard as any advance does: the
// run escalates with one notice carrying the guard's text, and the
// crossing's one warning is recorded.
func TestWorkflowAdvance_FrozenAtSpendCap_TheFreezeHoldsFirst(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newWorkflowFreezeFixture(ctx, t, pool, "acme/frozen-workflow-at-cap", 1496)
	setRepoCap(ctx, t, pool, f.repoFullName, "1.00")
	spendOn(ctx, t, pool, f.sessionID, "2.00")
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
	freezeAutonomyForActorTest(ctx, t, pool)
	workflowNotices := func() int {
		return countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, f.sessionID, string(ports.NotificationKindGitHubWorkflowDecision))
	}

	f.completeFirstAttempt(ctx, t, rig)
	runStatus, stepRuns, _, _, held := f.state(ctx, t)
	if runStatus != "running" || stepRuns != 1 || !held {
		t.Fatalf("frozen at the cap: run %s, %d step runs, held %v; want the advance held, the run running", runStatus, stepRuns, held)
	}
	if warnings, notices := guardRecorded(ctx, t, pool, f.sessionID); warnings != 0 || notices != 0 || workflowNotices() != 0 {
		t.Fatalf("frozen at the cap: guard warnings %d, guard notices %d, workflow notices %d; want none: the freeze held the advance before the guard was asked",
			warnings, notices, workflowNotices())
	}

	unfreezeAutonomyForActorTest(ctx, t, pool)
	if n, err := newReleaser(t, pool).ReleaseOnce(ctx); err != nil || n != 1 {
		t.Fatalf("ReleaseOnce = %d, %v; want the one advance released", n, err)
	}
	runStatus, stepRuns, turns, _, held := f.state(ctx, t)
	if runStatus != "needs_review" || stepRuns != 1 || turns != 2 || held {
		// turns: the first attempt's, and the spend spendOn stored.
		t.Fatalf("released at the cap: run %s, %d step runs, %d turns, held %v; want the run escalated, no next attempt, no turn",
			runStatus, stepRuns, turns, held)
	}
	if warnings, notices := guardRecorded(ctx, t, pool, f.sessionID); warnings != 1 || notices != 0 {
		t.Fatalf("released at the cap: guard warnings %d, guard notices %d; want the crossing's one warning, told by the run's escalation", warnings, notices)
	}
	var notice string
	if err := pool.QueryRow(ctx, `SELECT payload->>'text' FROM outbox WHERE session_id = $1 AND kind = $2`, f.sessionID, string(ports.NotificationKindGitHubWorkflowDecision)).Scan(&notice); err != nil {
		t.Fatalf("the run's escalation notice: %v", err)
	}
	for _, part := range []string{"This workflow run (" + f.run.ID.String() + ") now needs your review", "spend cap of $1.00, set on repository " + f.repoFullName} {
		if !strings.Contains(notice, part) {
			t.Fatalf("escalation notice %q does not say %q", notice, part)
		}
	}
}
