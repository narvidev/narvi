//go:build integration

// Integration tests for GET /api/sessions/{sessionID}/status (technical
// plan §43.20) on real Postgres: the live activity derived from the turn
// queue and the human gates in one snapshot -- never from sessions.status.
package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// getStatus reads the status route as the cookie's user, decoding through
// the generated DTO (whose own UnmarshalJSON enforces every required
// field and enum).
func getStatus(t *testing.T, r testRig, sessionID pgtype.UUID, cookie string) restdtos.SessionActivity {
	t.Helper()
	var got restdtos.SessionActivity
	if status := r.doJSON(t, http.MethodGet, "/api/sessions/"+sessionID.String()+"/status", nil, &got, cookie); status != http.StatusOK {
		t.Fatalf("GET status: %d, want 200", status)
	}
	return got
}

// transitionTurn moves a turn along one legal edge of the turn machine
// (turn.Transition, the only authority on edges), stamping dispatched_at
// or completed_at the way the session actor does, on q (the pool or a
// transaction).
func transitionTurn(ctx context.Context, t *testing.T, turns *narvipg.TurnStore, id pgtype.UUID, from turn.State, trig turn.Trigger) turn.State {
	t.Helper()
	to, err := turn.Transition(from, trig)
	if err != nil {
		t.Fatalf("turn.Transition(%s, %s): %v", from, trig, err)
	}
	arg := sqlcgen.UpdateTurnStatusParams{ID: id, Status: sqlcgen.TurnStatus(to)}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if to == turn.StateDispatched {
		arg.DispatchedAt = now
	}
	if turn.IsTerminal(to) {
		arg.CompletedAt = now
	}
	if _, err := turns.UpdateStatus(ctx, arg); err != nil {
		t.Fatalf("persist turn %s -> %s: %v", from, to, err)
	}
	return to
}

func createTurn(ctx context.Context, t *testing.T, turns *narvipg.TurnStore, sessionID pgtype.UUID, planMode bool) sqlcgen.Turn {
	t.Helper()
	row, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, PlanMode: planMode})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return row
}

// TestGetSessionStatus_FollowUpQueuedUnderCompletedRow is the lag the
// status route exists for (technical plan §43.20): the session row says
// "completed" -- the outcome of its first turn -- while a follow-up turn is
// queued, then dispatched, then processing. The row never changes through
// any of it; the status route answers queued, then running, and finished
// only once the follow-up is really terminal. Never idle, never finished
// before that.
func TestGetSessionStatus_FollowUpQueuedUnderCompletedRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)

	first := createTurn(ctx, t, rig.turns, sess.ID, false)
	state := transitionTurn(ctx, t, rig.turns, first.ID, turn.StatePending, turn.TriggerDispatch)
	state = transitionTurn(ctx, t, rig.turns, first.ID, state, turn.TriggerStartProcessing)
	transitionTurn(ctx, t, rig.turns, first.ID, state, turn.TriggerComplete)
	if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sess.ID, Status: sqlcgen.SessionStatusCompleted}); err != nil {
		t.Fatalf("derive the first turn's outcome onto the row: %v", err)
	}

	rowSays := func() restdtos.SessionStatus {
		t.Helper()
		var row restdtos.Session
		if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+sess.ID.String(), nil, &row, cookie); status != http.StatusOK {
			t.Fatalf("GET session: %d", status)
		}
		return row.Status
	}
	notSettled := func(stage string, got restdtos.SessionActivity) {
		t.Helper()
		if got.Settled || got.Activity == restdtos.SessionActivityActivityIdle || got.Activity == restdtos.SessionActivityActivityFinished {
			t.Fatalf("%s: activity %q settled %v -- a non-empty queue read as idle or finished", stage, got.Activity, got.Settled)
		}
		if row := rowSays(); row != restdtos.SessionStatusCompleted {
			t.Fatalf("%s: the session row says %q, want it still saying completed (this test is about that lag)", stage, row)
		}
	}

	if got := getStatus(t, rig, sess.ID, cookie); got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled {
		t.Fatalf("before the follow-up: activity %q settled %v, want finished and settled", got.Activity, got.Settled)
	}

	followUp := createTurn(ctx, t, rig.turns, sess.ID, false)
	got := getStatus(t, rig, sess.ID, cookie)
	notSettled("queued", got)
	if got.Activity != restdtos.SessionActivityActivityQueued || got.PendingTurns != 1 || got.InFlightTurn != nil {
		t.Fatalf("queued: activity %q pendingTurns %d inFlightTurn %+v, want queued, 1, none", got.Activity, got.PendingTurns, got.InFlightTurn)
	}
	if got.LastRun == nil || got.LastRun.TurnId != first.ID.String() || got.LastRun.Outcome != restdtos.SessionActivityLastRunOutcomeCompleted {
		t.Fatalf("queued: lastRun %+v, want the first turn, completed", got.LastRun)
	}
	if got.SandboxStatus != nil || got.SuggestedDelaySeconds != 15 {
		t.Fatalf("queued with no sandbox: sandboxStatus %+v delay %d, want none and the cold-start 15", got.SandboxStatus, got.SuggestedDelaySeconds)
	}

	state = transitionTurn(ctx, t, rig.turns, followUp.ID, turn.StatePending, turn.TriggerDispatch)
	got = getStatus(t, rig, sess.ID, cookie)
	notSettled("dispatched", got)
	if got.Activity != restdtos.SessionActivityActivityRunning || got.PendingTurns != 0 || got.InFlightTurn == nil ||
		got.InFlightTurn.TurnId != followUp.ID.String() || got.InFlightTurn.State != restdtos.SessionActivityInFlightTurnStateDispatched || got.InFlightTurn.DispatchedAt == nil {
		t.Fatalf("dispatched: %+v (inFlightTurn %+v), want running with the follow-up dispatched", got, got.InFlightTurn)
	}
	if got.SuggestedDelaySeconds != 10 {
		t.Fatalf("running: delay %d, want 10", got.SuggestedDelaySeconds)
	}

	state = transitionTurn(ctx, t, rig.turns, followUp.ID, state, turn.TriggerStartProcessing)
	got = getStatus(t, rig, sess.ID, cookie)
	notSettled("processing", got)
	if got.Activity != restdtos.SessionActivityActivityRunning || got.InFlightTurn == nil || got.InFlightTurn.State != restdtos.SessionActivityInFlightTurnStateProcessing {
		t.Fatalf("processing: activity %q inFlightTurn %+v, want running, processing", got.Activity, got.InFlightTurn)
	}

	transitionTurn(ctx, t, rig.turns, followUp.ID, state, turn.TriggerComplete)
	got = getStatus(t, rig, sess.ID, cookie)
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.InFlightTurn != nil || got.PendingTurns != 0 {
		t.Fatalf("terminal: %+v, want finished and settled", got)
	}
	if got.LastRun == nil || got.LastRun.TurnId != followUp.ID.String() || got.LastRun.FinishedAt == nil || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("terminal: lastRun %+v delay %d, want the follow-up with its finish time, and 300", got.LastRun, got.SuggestedDelaySeconds)
	}
}

// pollUntil runs readers against the status route until stop is closed,
// counting every activity observed. Each reader returns its error rather
// than failing the test itself: it runs off the test goroutine.
func pollUntil(ctx context.Context, g *errgroup.Group, rig testRig, sessionID pgtype.UUID, cookie string, stop <-chan struct{}, seen map[restdtos.SessionActivityActivity]*atomic.Int64) {
	url := rig.server.URL + "/api/sessions/" + sessionID.String() + "/status"
	read := func() (restdtos.SessionActivity, error) {
		var got restdtos.SessionActivity
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return got, err
		}
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return got, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return got, fmt.Errorf("GET status: %d", resp.StatusCode)
		}
		return got, json.NewDecoder(resp.Body).Decode(&got)
	}
	for i := 0; i < 4; i++ {
		g.Go(func() error {
			for {
				select {
				case <-stop:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				got, err := read()
				if err != nil {
					return err
				}
				counter, ok := seen[got.Activity]
				if !ok {
					return fmt.Errorf("unexpected activity %q", got.Activity)
				}
				counter.Add(1)
			}
		})
	}
}

func newSeen() map[restdtos.SessionActivityActivity]*atomic.Int64 {
	seen := map[restdtos.SessionActivityActivity]*atomic.Int64{}
	for _, a := range []restdtos.SessionActivityActivity{
		restdtos.SessionActivityActivityIdle, restdtos.SessionActivityActivityQueued, restdtos.SessionActivityActivityRunning,
		restdtos.SessionActivityActivityDelivering, restdtos.SessionActivityActivityScheduled, restdtos.SessionActivityActivityAwaitingApproval,
		restdtos.SessionActivityActivityFinished,
	} {
		seen[a] = &atomic.Int64{}
	}
	return seen
}

// inTx runs fn in one transaction.
func inTx(ctx context.Context, t *testing.T, rig testRig, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return pgx.BeginFunc(ctx, rig.pool, fn)
}

// createTurnThroughCore creates a turn the way every ingress does
// (httpapi.CreateTurnCore, RejectIfOpen), so the workflow engine resolves
// it and tracks it as an attempt of a workflow run.
func createTurnThroughCore(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	created, wasCreated, cerr := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry,
		sessionID, "carry on", nil, false, false, pgtype.UUID{}, httpapi.RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: %d %s", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("CreateTurnCore created no turn")
	}
	return created
}

// endTurnThroughEngine runs a pending turn to its end the way the session
// actor does: dispatched, then processing, then -- in ONE transaction, as
// completeProcessingTurn (pushpr.go), handleTurnDeadlineTimer
// (timerfired.go) and failDispatchedTurn (dispatch.go) each do -- the
// terminal edge trig takes from processing, followed by
// workflowengine.OnTurnCompleted.
func endTurnThroughEngine(ctx context.Context, t *testing.T, rig testRig, sessionID, turnID pgtype.UUID, trig turn.Trigger) {
	t.Helper()
	state := transitionTurn(ctx, t, rig.turns, turnID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, turnID, state, turn.TriggerStartProcessing)
	endProcessingTurnThroughEngine(ctx, t, rig, sessionID, turnID, trig)
}

// endProcessingTurnThroughEngine is endTurnThroughEngine's last step alone,
// for a turn already processing: the terminal edge and OnTurnCompleted in
// one transaction.
func endProcessingTurnThroughEngine(ctx context.Context, t *testing.T, rig testRig, sessionID, turnID pgtype.UUID, trig turn.Trigger) {
	t.Helper()
	if err := inTx(ctx, t, rig, func(tx pgx.Tx) error {
		transitionTurn(ctx, t, rig.turns.WithTx(tx), turnID, turn.StateProcessing, trig)
		sessionRow, err := rig.sessions.WithTx(tx).Get(ctx, sessionID)
		if err != nil {
			return err
		}
		workflowengine.OnTurnCompleted(ctx, workflowengine.Deps{
			Workflows:           rig.workflows.WithTx(tx),
			Turns:               rig.turns.WithTx(tx),
			SlackThreadSessions: narvipg.NewSlackThreadSessionStore(rig.pool).WithTx(tx),
			LinearAgentSessions: rig.linearAgentSessions.WithTx(tx),
			GitHubPRSessions:    narvipg.NewGitHubPRSessionStore(rig.pool).WithTx(tx),
			Outbox:              rig.outbox.WithTx(tx),
		}, sessionRow, turnID, trig)
		return nil
	}); err != nil {
		t.Fatalf("end turn %v via %s: %v", turnID, trig, err)
	}
}

// sessionRuns is the session's workflow runs, oldest first.
func sessionRuns(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) []sqlcgen.WorkflowRun {
	t.Helper()
	runs, err := rig.workflows.ListRunsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list workflow runs: %v", err)
	}
	slices.Reverse(runs)
	return runs
}

// customWorkflowSession binds a custom request-lane workflow -- one step,
// no edge, no human gate, the shape of a duplicated built-in -- to a
// repository of its own (a repo override, so no other test's global
// binding changes), and returns a session on that repository, whose turns
// the engine runs under it, and the workflow's id.
func customWorkflowSession(ctx context.Context, t *testing.T, rig testRig, ownerID pgtype.UUID) (sqlcgen.Session, pgtype.UUID) {
	t.Helper()
	return customWorkflowSessionGated(ctx, t, rig, ownerID, false)
}

// customWorkflowSessionGated is customWorkflowSession whose one step can
// carry a human gate after it (hitlAfter): its turn's end then parks the
// step awaiting a decision, which POST .../decide settles.
func customWorkflowSessionGated(ctx context.Context, t *testing.T, rig testRig, ownerID pgtype.UUID, hitlAfter bool) (sqlcgen.Session, pgtype.UUID) {
	t.Helper()
	repo := fmt.Sprintf("example/status-escalation-%d", time.Now().UnixNano())
	var defID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`, repo).Scan(&defID); err != nil {
		t.Fatalf("insert custom workflow definition: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, hitl_after) VALUES ($1, 1, 'agent', '{{prompt}}', $2)`, defID, hitlAfter); err != nil {
		t.Fatalf("insert custom step definition: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version) VALUES ('request', $1, $2, 1)`, repo, defID); err != nil {
		t.Fatalf("bind the custom workflow to %s: %v", repo, err)
	}
	sess, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   ownerID,
		Repos:       []byte(`[{"name":"repo","url":"https://github.com/` + repo + `.git","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session on %s: %v", repo, err)
	}
	return sess, defID
}

// TestGetSessionStatus_PlanCompletionNeverObservedAsFinished_Race: a
// writer loops through the plan-mode cycle the way the application
// commits it -- a plan-mode turn completing AND its plan inserted
// awaiting approval in one transaction (sessionactor's recordPlanIfNeeded),
// the plan approved AND the implementation turn created in one transaction
// (DecidePlanOnTx), then that turn dispatched and processing -- while
// readers poll the status route. No committed state is ever "every turn
// terminal, nothing awaiting", so a reader that sees one snapshot never
// reads finished (or idle).
func TestGetSessionStatus_PlanCompletionNeverObservedAsFinished_Race(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)

	current := createTurn(ctx, t, rig.turns, sess.ID, true)
	state := transitionTurn(ctx, t, rig.turns, current.ID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, current.ID, state, turn.TriggerStartProcessing)

	seen := newSeen()
	stop := make(chan struct{})
	var readers errgroup.Group
	pollUntil(ctx, &readers, rig, sess.ID, cookie, stop, seen)

	writerErr := func() error {
		defer close(stop)
		for i := 1; i <= 60; i++ {
			var planID pgtype.UUID
			if err := inTx(ctx, t, rig, func(tx pgx.Tx) error {
				transitionTurn(ctx, t, rig.turns.WithTx(tx), current.ID, turn.StateProcessing, turn.TriggerComplete)
				plan, err := rig.plans.WithTx(tx).Create(ctx, sqlcgen.CreatePlanParams{SessionID: sess.ID, TurnID: current.ID, Version: int32(i), Status: sqlcgen.PlanStatusAwaitingApproval})
				planID = plan.ID
				return err
			}); err != nil {
				return fmt.Errorf("complete plan turn + insert plan: %w", err)
			}
			if err := inTx(ctx, t, rig, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE plans SET status = 'approved', decided_at = now() WHERE id = $1`, planID); err != nil {
					return err
				}
				next, err := rig.turns.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending, PlanMode: true})
				current = next
				return err
			}); err != nil {
				return fmt.Errorf("approve plan + create implementation turn: %w", err)
			}
			state := transitionTurn(ctx, t, rig.turns, current.ID, turn.StatePending, turn.TriggerDispatch)
			transitionTurn(ctx, t, rig.turns, current.ID, state, turn.TriggerStartProcessing)
		}
		return nil
	}()
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	if err := readers.Wait(); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if n := seen[restdtos.SessionActivityActivityFinished].Load() + seen[restdtos.SessionActivityActivityIdle].Load(); n != 0 {
		t.Fatalf("observed finished or idle %d times while the plan cycle always had work or a plan pending", n)
	}
	total := int64(0)
	for _, c := range seen {
		total += c.Load()
	}
	if total == 0 || seen[restdtos.SessionActivityActivityAwaitingApproval].Load() == 0 {
		t.Fatalf("readers observed %d snapshots (awaiting_approval %d): the race was not exercised", total, seen[restdtos.SessionActivityActivityAwaitingApproval].Load())
	}
	t.Logf("snapshots observed: running %d, queued %d, awaiting_approval %d", seen[restdtos.SessionActivityActivityRunning].Load(), seen[restdtos.SessionActivityActivityQueued].Load(), seen[restdtos.SessionActivityActivityAwaitingApproval].Load())
}

// TestGetSessionStatus_WorkflowAdvanceNeverObservedAsFinished_Race: the
// same property for a workflow with a human gate after each step -- a
// step's turn completing AND the step marked awaiting a decision in one
// transaction (the workflow engine's OnTurnCompleted runs inside the
// completing turn's transaction), then the decision AND the next step's
// run and turn in one transaction (DecideWorkflowStep's approve path).
func TestGetSessionStatus_WorkflowAdvanceNeverObservedAsFinished_Race(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)

	var defID, stepDefID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("status-race-%d", time.Now().UnixNano())).Scan(&defID); err != nil {
		t.Fatalf("insert workflow definition: %v", err)
	}
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, hitl_after) VALUES ($1, 1, 'agent', '{{prompt}}', true) RETURNING id`, defID).Scan(&stepDefID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	run, err := rig.workflows.CreateRun(ctx, sess.ID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepRun, err := rig.workflows.CreateStepRun(ctx, run.ID, stepDefID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	current := createTurn(ctx, t, rig.turns, sess.ID, false)
	state := transitionTurn(ctx, t, rig.turns, current.ID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, current.ID, state, turn.TriggerStartProcessing)

	seen := newSeen()
	stop := make(chan struct{})
	var readers errgroup.Group
	pollUntil(ctx, &readers, rig, sess.ID, cookie, stop, seen)

	writerErr := func() error {
		defer close(stop)
		for i := 0; i < 60; i++ {
			if err := inTx(ctx, t, rig, func(tx pgx.Tx) error {
				transitionTurn(ctx, t, rig.turns.WithTx(tx), current.ID, turn.StateProcessing, turn.TriggerComplete)
				_, err := rig.workflows.WithTx(tx).MarkAwaitingDecision(ctx, stepRun.ID, "ok")
				return err
			}); err != nil {
				return fmt.Errorf("complete step turn + open the gate: %w", err)
			}
			if err := inTx(ctx, t, rig, func(tx pgx.Tx) error {
				if n, err := rig.workflows.WithTx(tx).DecideStepRun(ctx, stepRun.ID, "approve", nil, user.ID); err != nil || n != 1 {
					return fmt.Errorf("decide: %d rows, %w", n, err)
				}
				next, err := rig.workflows.WithTx(tx).CreateStepRun(ctx, run.ID, stepDefID)
				if err != nil {
					return err
				}
				stepRun = next
				nextTurn, err := rig.turns.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending})
				current = nextTurn
				return err
			}); err != nil {
				return fmt.Errorf("decide + next step's turn: %w", err)
			}
			state := transitionTurn(ctx, t, rig.turns, current.ID, turn.StatePending, turn.TriggerDispatch)
			transitionTurn(ctx, t, rig.turns, current.ID, state, turn.TriggerStartProcessing)
		}
		return nil
	}()
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	if err := readers.Wait(); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if n := seen[restdtos.SessionActivityActivityFinished].Load() + seen[restdtos.SessionActivityActivityIdle].Load(); n != 0 {
		t.Fatalf("observed finished or idle %d times while the workflow always had work or a gate pending", n)
	}
	if seen[restdtos.SessionActivityActivityAwaitingApproval].Load() == 0 {
		t.Fatalf("readers never observed the gate: the race was not exercised")
	}
}

// TestGetSessionStatus_Gates covers the rest of the route: its 400 and
// 404, an idle session, each kind of human gate, when lastRun carries a
// failure reason, and the delay while queued behind a cold start versus on
// a warm sandbox.
func TestGetSessionStatus_Gates(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	t.Run("malformed id is 400, unknown session is 404", func(t *testing.T) {
		if status := rig.doJSON(t, http.MethodGet, "/api/sessions/not-a-uuid/status", nil, nil, cookie); status != http.StatusBadRequest {
			t.Fatalf("malformed id: %d, want 400", status)
		}
		if status := rig.doJSON(t, http.MethodGet, "/api/sessions/00000000-0000-0000-0000-000000000000/status", nil, nil, cookie); status != http.StatusNotFound {
			t.Fatalf("unknown session: %d, want 404", status)
		}
	})

	t.Run("no turn at all is idle", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		got := getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityIdle || !got.Settled || got.PendingTurns != 0 || got.InFlightTurn != nil || got.Awaiting != nil || got.LastRun != nil || got.SuggestedDelaySeconds != 300 {
			t.Fatalf("got %+v, want idle, settled, nothing else, 300", got)
		}
		if got.SessionId != sess.ID.String() || got.Archived || got.ObservedAt.IsZero() {
			t.Fatalf("got %+v, want this session, not archived, observed", got)
		}
	})

	completedSession := func(t *testing.T) sqlcgen.Session {
		t.Helper()
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		tr := createTurn(ctx, t, rig.turns, sess.ID, true)
		state := transitionTurn(ctx, t, rig.turns, tr.ID, turn.StatePending, turn.TriggerDispatch)
		state = transitionTurn(ctx, t, rig.turns, tr.ID, state, turn.TriggerStartProcessing)
		transitionTurn(ctx, t, rig.turns, tr.ID, state, turn.TriggerComplete)
		return sess
	}

	t.Run("a plan awaiting approval", func(t *testing.T) {
		sess := completedSession(t)
		var turnID pgtype.UUID
		if err := rig.pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1`, sess.ID).Scan(&turnID); err != nil {
			t.Fatal(err)
		}
		plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sess.ID, TurnID: turnID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
		if err != nil {
			t.Fatal(err)
		}
		got := getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || !got.Settled || got.SuggestedDelaySeconds != 60 {
			t.Fatalf("got activity %q settled %v delay %d, want awaiting_approval, settled, 60", got.Activity, got.Settled, got.SuggestedDelaySeconds)
		}
		if got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindPlan || got.Awaiting.Id != plan.ID.String() || !got.Awaiting.Since.Equal(plan.CreatedAt.Time) {
			t.Fatalf("awaiting = %+v, want the plan since its creation", got.Awaiting)
		}

		// A follow-up queued beside the open plan: queued wins, the gate is
		// still reported.
		createTurn(ctx, t, rig.turns, sess.ID, false)
		got = getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityQueued || got.Settled || got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindPlan {
			t.Fatalf("with a queued follow-up: activity %q settled %v awaiting %+v, want queued, unsettled, the plan still reported", got.Activity, got.Settled, got.Awaiting)
		}
	})

	var defID, stepDefID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("status-gates-%d", time.Now().UnixNano())).Scan(&defID); err != nil {
		t.Fatalf("insert workflow definition: %v", err)
	}
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, hitl_after) VALUES ($1, 1, 'agent', '{{prompt}}', true) RETURNING id`, defID).Scan(&stepDefID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}

	t.Run("a workflow step awaiting a decision", func(t *testing.T) {
		sess := completedSession(t)
		run, err := rig.workflows.CreateRun(ctx, sess.ID, "request", defID, 1)
		if err != nil {
			t.Fatal(err)
		}
		step, err := rig.workflows.CreateStepRun(ctx, run.ID, stepDefID)
		if err != nil {
			t.Fatal(err)
		}
		awaiting, err := rig.workflows.MarkAwaitingDecision(ctx, step.ID, "ok")
		if err != nil {
			t.Fatal(err)
		}
		got := getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowStep ||
			got.Awaiting.Id != step.ID.String() || !got.Awaiting.Since.Equal(awaiting.UpdatedAt.Time) {
			t.Fatalf("got activity %q awaiting %+v, want awaiting_approval on the step since it was marked", got.Activity, got.Awaiting)
		}
	})

	// Review round 2, O4: a plan and a workflow step can be open at once
	// (a custom plan-lane workflow with a human gate after its step: the
	// turn's completion records the plan and parks the step in one
	// transaction). The plan is reported first, as the schema says.
	t.Run("a plan and a workflow step open together: the plan is reported", func(t *testing.T) {
		sess := completedSession(t)
		var turnID pgtype.UUID
		if err := rig.pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1`, sess.ID).Scan(&turnID); err != nil {
			t.Fatal(err)
		}
		plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sess.ID, TurnID: turnID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
		if err != nil {
			t.Fatal(err)
		}
		run, err := rig.workflows.CreateRun(ctx, sess.ID, "request", defID, 1)
		if err != nil {
			t.Fatal(err)
		}
		step, err := rig.workflows.CreateStepRun(ctx, run.ID, stepDefID)
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.workflows.AttachTurn(ctx, step.ID, turnID); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.workflows.MarkAwaitingDecision(ctx, step.ID, "ok"); err != nil {
			t.Fatal(err)
		}
		got := getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || got.Awaiting == nil ||
			got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindPlan || got.Awaiting.Id != plan.ID.String() {
			t.Fatalf("got activity %q awaiting %+v, want awaiting_approval on the plan %v, reported before the step %v", got.Activity, got.Awaiting, plan.ID, step.ID)
		}
	})

	// A custom workflow's run escalated by its failed turn gates the session
	// while it is the latest thing that happened -- and no longer once any
	// newer turn exists. The newer turn here starts no run of its own (as a
	// plan's implementation turn, or an automatic re-review turn the engine
	// never tracks, does not), so the escalated run stays the session's
	// newest run throughout: it is the newer TURN that supersedes it. A turn
	// queued BEFORE the escalation that runs after it is
	// TestGetSessionStatus_TurnQueuedBehindTheEscalatingTurnClosesItOnceItRuns.
	// TestGetSessionStatus_EscalatedTurnNeverGatesForGood covers a follow-up
	// that starts a run of its own, and the built-in workflow.
	t.Run("a workflow run escalated for review, until a newer turn supersedes it", func(t *testing.T) {
		sess, customDef := customWorkflowSession(ctx, t, rig, user.ID)
		first := createTurnThroughCore(ctx, t, rig, sess.ID)
		endTurnThroughEngine(ctx, t, rig, sess.ID, first.ID, turn.TriggerFail)
		runs := sessionRuns(ctx, t, rig, sess.ID)
		if len(runs) != 1 || runs[0].WorkflowDefinitionID != customDef || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview {
			t.Fatalf("runs = %+v, want one run of the custom workflow, escalated", runs)
		}
		escalated := runs[0]

		got := getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || !got.Settled || got.SuggestedDelaySeconds != 60 || got.Awaiting == nil ||
			got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowEscalation || got.Awaiting.Id != escalated.ID.String() || !got.Awaiting.Since.Equal(escalated.UpdatedAt.Time) {
			t.Errorf("escalated: activity %q settled %v delay %d awaiting %+v, want awaiting_approval, settled, 60, on run %v since it escalated", got.Activity, got.Settled, got.SuggestedDelaySeconds, got.Awaiting, escalated.ID)
		}

		newer := createTurn(ctx, t, rig.turns, sess.ID, false)
		got = getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityQueued || got.Awaiting != nil {
			t.Errorf("a newer turn queued: activity %q awaiting %+v, want queued and no gate", got.Activity, got.Awaiting)
		}
		state := transitionTurn(ctx, t, rig.turns, newer.ID, turn.StatePending, turn.TriggerDispatch)
		state = transitionTurn(ctx, t, rig.turns, newer.ID, state, turn.TriggerStartProcessing)
		transitionTurn(ctx, t, rig.turns, newer.ID, state, turn.TriggerComplete)
		if runs := sessionRuns(ctx, t, rig, sess.ID); len(runs) != 1 || runs[0].ID != escalated.ID || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview {
			t.Fatalf("runs = %+v, want the escalated run still the only one, still in needs_review", runs)
		}
		got = getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.Awaiting != nil || got.SuggestedDelaySeconds != 300 {
			t.Errorf("the newer turn completed: activity %q settled %v awaiting %+v delay %d, want finished, settled, no gate, 300", got.Activity, got.Settled, got.Awaiting, got.SuggestedDelaySeconds)
		}
	})

	t.Run("lastRun carries the failure reason only when it can describe nothing else", func(t *testing.T) {
		timeout := sqlcgen.SessionFailureReasonTimeout
		cases := []struct {
			name       string
			rowStatus  sqlcgen.SessionStatus
			followUp   bool
			wantReason *string
		}{
			{"newest turn failed, row failed/timeout -> timeout", sqlcgen.SessionStatusFailed, false, ptr("timeout")},
			{"row outcome is not this run's -> null", sqlcgen.SessionStatusCancelled, false, nil},
			{"a newer turn is queued -> null", sqlcgen.SessionStatusFailed, true, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				sess := createSessionForUser(ctx, t, rig, user.ID, nil)
				tr := createTurn(ctx, t, rig.turns, sess.ID, false)
				state := transitionTurn(ctx, t, rig.turns, tr.ID, turn.StatePending, turn.TriggerDispatch)
				state = transitionTurn(ctx, t, rig.turns, tr.ID, state, turn.TriggerStartProcessing)
				transitionTurn(ctx, t, rig.turns, tr.ID, state, turn.TriggerTimeout)
				if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sess.ID, Status: tc.rowStatus, FailureReason: &timeout}); err != nil {
					t.Fatal(err)
				}
				if tc.followUp {
					createTurn(ctx, t, rig.turns, sess.ID, false)
				}
				got := getStatus(t, rig, sess.ID, cookie)
				if got.LastRun == nil || got.LastRun.TurnId != tr.ID.String() || got.LastRun.Outcome != restdtos.SessionActivityLastRunOutcomeFailed {
					t.Fatalf("lastRun = %+v, want the failed turn", got.LastRun)
				}
				switch {
				case tc.wantReason == nil && got.LastRun.FailureReason != nil:
					t.Fatalf("failureReason = %v, want null", got.LastRun.FailureReason.Value)
				case tc.wantReason != nil && (got.LastRun.FailureReason == nil || got.LastRun.FailureReason.Value != *tc.wantReason):
					t.Fatalf("failureReason = %+v, want %q", got.LastRun.FailureReason, *tc.wantReason)
				}
			})
		}
	})

	t.Run("queued: cold start versus a warm sandbox", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		createTurn(ctx, t, rig.turns, sess.ID, false)
		if _, err := rig.sandboxes.Create(ctx, sess.ID); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			sandbox string
			delay   int
		}{{"booting", 15}, {"ready", 5}, {"stopped", 15}} {
			if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = $2 WHERE session_id = $1`, sess.ID, tc.sandbox); err != nil {
				t.Fatal(err)
			}
			got := getStatus(t, rig, sess.ID, cookie)
			if got.Activity != restdtos.SessionActivityActivityQueued || got.SandboxStatus == nil || got.SandboxStatus.Value != tc.sandbox || got.SuggestedDelaySeconds != tc.delay {
				t.Fatalf("sandbox %s: activity %q sandboxStatus %+v delay %d, want queued, %s, %d", tc.sandbox, got.Activity, got.SandboxStatus, got.SuggestedDelaySeconds, tc.sandbox, tc.delay)
			}
		}
	})

	t.Run("every role reads another user's session", func(t *testing.T) {
		sess := completedSession(t)
		for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleViewer, sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleAdmin} {
			_, other := createUserWithRole(ctx, t, rig, role)
			if got := getStatus(t, rig, sess.ID, other); got.Activity != restdtos.SessionActivityActivityFinished {
				t.Fatalf("%s: activity %q, want finished", role, got.Activity)
			}
		}
	})
}

// TestGetSessionStatus_EscalatedTurnNeverGatesForGood: a workflow run in
// needs_review is never moved out of it, and every follow-up turn is an
// attempt of a workflow run -- the built-in single-step one by default --
// that a failed, timed-out, abandoned or stopped turn escalates. Driven
// through the production paths (CreateTurnCore; turn.Transition and
// workflowengine.OnTurnCompleted in one terminal transaction), for each way
// a turn ends short of completing, under the built-in and under a custom
// workflow:
//
//   - right after the turn ends, the built-in run's escalation reads
//     finished (it never gates: technical plan §43.20) and the custom run's
//     reads awaiting_approval on that run -- each pinned;
//   - a follow-up turn supersedes it at once, while it is still queued;
//   - once the follow-up completes the session reads finished, the
//     escalated run still parked in needs_review beside the completed one.
func TestGetSessionStatus_EscalatedTurnNeverGatesForGood(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	builtIn := mustUUID(t, builtInRequestDefID)

	ends := []struct {
		name    string
		trigger turn.Trigger
		outcome restdtos.SessionActivityLastRunOutcome
	}{
		// pushpr.go: an execution_complete reporting cancelled (the user's Stop).
		{"stopped by the user", turn.TriggerCancel, restdtos.SessionActivityLastRunOutcomeCancelled},
		// pushpr.go: an execution_complete reporting failed.
		{"failed", turn.TriggerFail, restdtos.SessionActivityLastRunOutcomeFailed},
		// timerfired.go: the turn deadline expired.
		{"timed out", turn.TriggerTimeout, restdtos.SessionActivityLastRunOutcomeFailed},
		// dispatch.go's failDispatchedTurn: the prompt never reached the
		// sandbox, and the actor ends the turn on the deadline's own edge.
		{"abandoned at dispatch", turn.TriggerTimeout, restdtos.SessionActivityLastRunOutcomeFailed},
	}
	for _, custom := range []bool{false, true} {
		for _, end := range ends {
			name := "built-in workflow/" + end.name
			if custom {
				name = "custom workflow/" + end.name
			}
			t.Run(name, func(t *testing.T) {
				sess, wantDef := createSessionForUser(ctx, t, rig, user.ID, nil), builtIn
				if custom {
					sess, wantDef = customWorkflowSession(ctx, t, rig, user.ID)
				}

				first := createTurnThroughCore(ctx, t, rig, sess.ID)
				endTurnThroughEngine(ctx, t, rig, sess.ID, first.ID, end.trigger)
				runs := sessionRuns(ctx, t, rig, sess.ID)
				if len(runs) != 1 || runs[0].WorkflowDefinitionID != wantDef || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview {
					t.Fatalf("after the first turn: runs %+v, want one run of %v, escalated", runs, wantDef)
				}
				escalated := runs[0]

				got := getStatus(t, rig, sess.ID, cookie)
				if got.LastRun == nil || got.LastRun.TurnId != first.ID.String() || got.LastRun.Outcome != end.outcome {
					t.Errorf("after the first turn: lastRun %+v, want turn %v, %s", got.LastRun, first.ID, end.outcome)
				}
				if custom {
					if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || !got.Settled || got.SuggestedDelaySeconds != 60 || got.Awaiting == nil ||
						got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowEscalation || got.Awaiting.Id != escalated.ID.String() || !got.Awaiting.Since.Equal(escalated.UpdatedAt.Time) {
						t.Errorf("custom escalation, the latest state: activity %q settled %v delay %d awaiting %+v, want awaiting_approval, settled, 60, on run %v", got.Activity, got.Settled, got.SuggestedDelaySeconds, got.Awaiting, escalated.ID)
					}
				} else if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 || got.Awaiting != nil {
					t.Errorf("built-in escalation, the latest state: activity %q settled %v delay %d awaiting %+v, want finished, settled, 300, no gate", got.Activity, got.Settled, got.SuggestedDelaySeconds, got.Awaiting)
				}

				followUp := createTurnThroughCore(ctx, t, rig, sess.ID)
				if got := getStatus(t, rig, sess.ID, cookie); got.Activity != restdtos.SessionActivityActivityQueued || got.Awaiting != nil {
					t.Errorf("follow-up queued: activity %q awaiting %+v, want queued and no gate", got.Activity, got.Awaiting)
				}

				endTurnThroughEngine(ctx, t, rig, sess.ID, followUp.ID, turn.TriggerComplete)
				runs = sessionRuns(ctx, t, rig, sess.ID)
				if len(runs) != 2 || runs[0].ID != escalated.ID || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview ||
					runs[1].WorkflowDefinitionID != wantDef || runs[1].Status != sqlcgen.WorkflowRunStatusCompleted {
					t.Fatalf("after the follow-up: runs %+v, want [the escalated run still in needs_review, a completed run of %v]", runs, wantDef)
				}
				got = getStatus(t, rig, sess.ID, cookie)
				if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 || got.Awaiting != nil ||
					got.LastRun == nil || got.LastRun.TurnId != followUp.ID.String() || got.LastRun.Outcome != restdtos.SessionActivityLastRunOutcomeCompleted {
					t.Errorf("after the follow-up: activity %q settled %v delay %d awaiting %+v lastRun %+v, want finished, settled, 300, no gate, the follow-up completed",
						got.Activity, got.Settled, got.SuggestedDelaySeconds, got.Awaiting, got.LastRun)
				}
			})
		}
	}
}

// TestGetSessionStatus_EscalationAfterAnUntrackedTurn is review round 2's
// O2, through the production paths: a custom workflow whose one step has a
// human gate after it; its turn is stopped, so the step awaits a decision
// on a blocked outcome; a second turn is sent while it waits -- the engine
// leaves that turn untracked, an attempt of no run -- and completes; then a
// person approves the step, and with no edge for a blocked outcome the
// decision escalates the run (/decide's own transaction). The escalation
// is then the session's latest state and its only open hand-off, so the
// status reads awaiting_approval on it -- not finished -- until a newer
// turn supersedes it. The control has no untracked turn.
func TestGetSessionStatus_EscalationAfterAnUntrackedTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	for _, untracked := range []bool{false, true} {
		name := "escalated by a decision, no turn in between (the control)"
		if untracked {
			name = "escalated by a decision after an untracked turn"
		}
		t.Run(name, func(t *testing.T) {
			sess, _ := customWorkflowSessionGated(ctx, t, rig, user.ID, true)
			first := createTurnThroughCore(ctx, t, rig, sess.ID)
			endTurnThroughEngine(ctx, t, rig, sess.ID, first.ID, turn.TriggerCancel)
			runs := sessionRuns(ctx, t, rig, sess.ID)
			if len(runs) != 1 || runs[0].Status != sqlcgen.WorkflowRunStatusRunning {
				t.Fatalf("after the first turn: runs %+v, want one run, still running (its step awaits a decision)", runs)
			}
			run := runs[0]
			steps, err := rig.workflows.ListStepRunsForRun(ctx, run.ID)
			if err != nil || len(steps) != 1 || steps[0].Status != sqlcgen.WorkflowStepRunStatusAwaitingDecision {
				t.Fatalf("after the first turn: step runs %+v (%v), want one awaiting a decision", steps, err)
			}
			stepRun := steps[0]
			if got := getStatus(t, rig, sess.ID, cookie); got.Activity != restdtos.SessionActivityActivityAwaitingApproval ||
				got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowStep {
				t.Fatalf("the step awaits a decision: activity %q awaiting %+v, want awaiting_approval on the step", got.Activity, got.Awaiting)
			}

			if untracked {
				second := createTurnThroughCore(ctx, t, rig, sess.ID)
				if runs := sessionRuns(ctx, t, rig, sess.ID); len(runs) != 1 {
					t.Fatalf("a turn sent while the step awaits a decision started a run: %+v", runs)
				}
				var attempts int
				if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE turn_id = $1`, second.ID).Scan(&attempts); err != nil || attempts != 0 {
					t.Fatalf("the second turn is an attempt of %d step runs (%v), want none: it must be untracked", attempts, err)
				}
				endTurnThroughEngine(ctx, t, rig, sess.ID, second.ID, turn.TriggerComplete)
				if got := getStatus(t, rig, sess.ID, cookie); got.Activity != restdtos.SessionActivityActivityAwaitingApproval ||
					got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowStep {
					t.Fatalf("the untracked turn completed: activity %q awaiting %+v, want the step still awaiting its decision", got.Activity, got.Awaiting)
				}
			}

			var decided restdtos.WorkflowStepDecideResponse
			if status := rig.doJSON(t, http.MethodPost, decidePath(run.ID, stepRun.ID), []byte(`{"verdict":"approve","text":null}`), &decided, cookie); status != http.StatusOK {
				t.Fatalf("decide approve: %d, want 200", status)
			}
			runs = sessionRuns(ctx, t, rig, sess.ID)
			if len(runs) != 1 || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview {
				t.Fatalf("after the decision: runs %+v, want the one run escalated to needs_review", runs)
			}
			escalated := runs[0]

			got := getStatus(t, rig, sess.ID, cookie)
			if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || !got.Settled || got.SuggestedDelaySeconds != 60 || got.Awaiting == nil ||
				got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowEscalation || got.Awaiting.Id != escalated.ID.String() || !got.Awaiting.Since.Equal(escalated.UpdatedAt.Time) {
				t.Fatalf("escalated by the decision: activity %q settled %v delay %d awaiting %+v, want awaiting_approval, settled, 60, on run %v since it escalated",
					got.Activity, got.Settled, got.SuggestedDelaySeconds, got.Awaiting, escalated.ID)
			}

			// New work supersedes it, as for any escalation.
			followUp := createTurnThroughCore(ctx, t, rig, sess.ID)
			if got := getStatus(t, rig, sess.ID, cookie); got.Activity != restdtos.SessionActivityActivityQueued || got.Awaiting != nil {
				t.Fatalf("follow-up queued: activity %q awaiting %+v, want queued and no gate", got.Activity, got.Awaiting)
			}
			endTurnThroughEngine(ctx, t, rig, sess.ID, followUp.ID, turn.TriggerComplete)
			got = getStatus(t, rig, sess.ID, cookie)
			if got.Activity == restdtos.SessionActivityActivityAwaitingApproval && got.Awaiting != nil && got.Awaiting.Kind == restdtos.SessionActivityAwaitingKindWorkflowEscalation {
				t.Fatalf("after the follow-up: the escalation of run %v still gates: %+v", escalated.ID, got.Awaiting)
			}
		})
	}
}

// TestGetSessionStatus_EscalationNeverObservedAsFinished_Race is review
// round 2's O3: the escalation gate's own one-snapshot property. A writer
// loops through a custom workflow's escalations the way the application
// commits them -- a turn's failure AND its run's escalation in one
// transaction (turn.Transition, then workflowengine.OnTurnCompleted), then
// the follow-up turn AND the fresh run it starts in one transaction
// (CreateTurnCore) -- while readers poll the status route. No committed
// state is ever "every turn terminal, nothing awaiting", so a reader that
// sees one snapshot never reads finished (or idle). Read in two statements,
// the turns could be seen before the follow-up commits and the escalation
// after it (closed by the newer run), or the escalation before the failure
// commits and the turns after it -- a false finished either way.
func TestGetSessionStatus_EscalationNeverObservedAsFinished_Race(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess, _ := customWorkflowSession(ctx, t, rig, user.ID)

	current := createTurnThroughCore(ctx, t, rig, sess.ID)
	state := transitionTurn(ctx, t, rig.turns, current.ID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, current.ID, state, turn.TriggerStartProcessing)

	seen := newSeen()
	stop := make(chan struct{})
	var readers errgroup.Group
	pollUntil(ctx, &readers, rig, sess.ID, cookie, stop, seen)

	const cycles = 60
	writerErr := func() error {
		defer close(stop)
		for i := 0; i < cycles; i++ {
			if err := inTx(ctx, t, rig, func(tx pgx.Tx) error {
				to, err := turn.Transition(turn.StateProcessing, turn.TriggerFail)
				if err != nil {
					return err
				}
				if _, err := rig.turns.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
					ID: current.ID, Status: sqlcgen.TurnStatus(to), CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
				}); err != nil {
					return err
				}
				sessionRow, err := rig.sessions.WithTx(tx).Get(ctx, sess.ID)
				if err != nil {
					return err
				}
				workflowengine.OnTurnCompleted(ctx, workflowengine.Deps{
					Workflows:           rig.workflows.WithTx(tx),
					Turns:               rig.turns.WithTx(tx),
					SlackThreadSessions: narvipg.NewSlackThreadSessionStore(rig.pool).WithTx(tx),
					LinearAgentSessions: rig.linearAgentSessions.WithTx(tx),
					GitHubPRSessions:    narvipg.NewGitHubPRSessionStore(rig.pool).WithTx(tx),
					Outbox:              rig.outbox.WithTx(tx),
				}, sessionRow, current.ID, turn.TriggerFail)
				return nil
			}); err != nil {
				return fmt.Errorf("fail the turn + escalate its run: %w", err)
			}
			created, wasCreated, cerr := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry,
				sess.ID, "carry on", nil, false, false, pgtype.UUID{}, httpapi.RejectIfOpen)
			if cerr != nil || !wasCreated {
				return fmt.Errorf("follow-up through CreateTurnCore: created %v, %v", wasCreated, cerr)
			}
			current = created
			for _, trig := range []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing} {
				row, err := rig.turns.Get(ctx, current.ID)
				if err != nil {
					return err
				}
				to, err := turn.Transition(turn.State(row.Status), trig)
				if err != nil {
					return err
				}
				arg := sqlcgen.UpdateTurnStatusParams{ID: current.ID, Status: sqlcgen.TurnStatus(to)}
				if to == turn.StateDispatched {
					arg.DispatchedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
				}
				if _, err := rig.turns.UpdateStatus(ctx, arg); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	if err := readers.Wait(); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if n := seen[restdtos.SessionActivityActivityFinished].Load() + seen[restdtos.SessionActivityActivityIdle].Load(); n != 0 {
		t.Fatalf("observed finished or idle %d times while every failed turn's escalation stayed open until its follow-up was queued", n)
	}
	if seen[restdtos.SessionActivityActivityAwaitingApproval].Load() == 0 {
		t.Fatalf("readers never observed an escalation: the race was not exercised")
	}
	runs := sessionRuns(ctx, t, rig, sess.ID)
	escalatedRuns := 0
	for _, r := range runs {
		if r.Status == sqlcgen.WorkflowRunStatusNeedsReview {
			escalatedRuns++
		}
	}
	if len(runs) != cycles+1 || escalatedRuns != cycles {
		t.Fatalf("runs %d (%d escalated), want %d with %d escalated: each failure escalated its run and each follow-up started one", len(runs), escalatedRuns, cycles+1, cycles)
	}
	t.Logf("snapshots observed: running %d, queued %d, awaiting_approval %d", seen[restdtos.SessionActivityActivityRunning].Load(), seen[restdtos.SessionActivityActivityQueued].Load(), seen[restdtos.SessionActivityActivityAwaitingApproval].Load())
}
