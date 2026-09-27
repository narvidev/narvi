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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
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
		restdtos.SessionActivityActivityAwaitingApproval, restdtos.SessionActivityActivityFinished,
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

	t.Run("a workflow run escalated for review", func(t *testing.T) {
		sess := completedSession(t)
		run, err := rig.workflows.CreateRun(ctx, sess.ID, "request", defID, 1)
		if err != nil {
			t.Fatal(err)
		}
		escalated, err := rig.workflows.EscalateRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := getStatus(t, rig, sess.ID, cookie)
		if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowEscalation ||
			got.Awaiting.Id != run.ID.String() || !got.Awaiting.Since.Equal(escalated.UpdatedAt.Time) {
			t.Fatalf("got activity %q awaiting %+v, want awaiting_approval on the escalated run", got.Activity, got.Awaiting)
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
