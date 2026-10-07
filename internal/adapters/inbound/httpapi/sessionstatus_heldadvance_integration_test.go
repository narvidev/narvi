//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// TestGetSessionStatus_HeldWorkflowAdvanceReadsScheduled pins technical
// plan §43.20's promise for the autonomy freeze's held workflow advance
// (§40.2), on the real status route: settled means nothing progresses
// server-side without new input, and a held advance does -- once the
// freeze lifts, the releaser starts the run's next step on the session.
// So with a two-step run whose first step's turn ended while autonomy was
// frozen, the advance held: the plain read reads scheduled, not settled; a
// wait does not end as settled but runs to its bound (timeout), the
// session still scheduled; and once the freeze lifts and the releaser
// applies the advance, the same route reads the next step's turn queued.
// Nobody sent the session anything.
func TestGetSessionStatus_HeldWorkflowAdvanceReadsScheduled(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)
	settings := narvipg.NewPlatformSettingsStore(rig.pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })

	var defID, firstStepID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("status-held-%d", time.Now().UnixNano())).Scan(&defID); err != nil {
		t.Fatalf("insert workflow definition: %v", err)
	}
	if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&firstStepID); err != nil {
		t.Fatalf("insert the first step: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 2, 'agent', '{{prompt}}')`, defID); err != nil {
		t.Fatalf("insert the second step: %v", err)
	}
	run, err := rig.workflows.CreateRun(ctx, sess.ID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	attempt, err := rig.workflows.CreateStepRun(ctx, run.ID, firstStepID)
	if err != nil {
		t.Fatalf("create the first attempt: %v", err)
	}
	stepTurn := createTurn(ctx, t, rig.turns, sess.ID, false)
	if err := rig.workflows.AttachTurn(ctx, attempt.ID, stepTurn.ID); err != nil {
		t.Fatalf("attach the turn: %v", err)
	}
	state := transitionTurn(ctx, t, rig.turns, stepTurn.ID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, stepTurn.ID, state, turn.TriggerStartProcessing)

	// The step's turn ends while autonomy is frozen, the way the session
	// actor ends it: the terminal edge and OnTurnCompleted in one
	// transaction, the freeze bound to it as workflowDeps binds it.
	gate, err := autonomy.NewGate(rig.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		transitionTurn(ctx, t, rig.turns.WithTx(tx), stepTurn.ID, turn.StateProcessing, turn.TriggerComplete)
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
			Guard:               turnguard.New(rig.pool, nil, false).WithTx(tx, nil),
			Origin:              sessionguard.OriginAutomatic,
			Autonomy:            gate.WithTx(tx),
		}, sessionRow, stepTurn.ID, turn.TriggerComplete)
		return nil
	}); err != nil {
		t.Fatalf("end the step's turn: %v", err)
	}
	if _, err := rig.workflows.GetAdvanceHold(ctx, run.ID); err != nil {
		t.Fatalf("the advance was not held: %v", err)
	}

	srv := waitServer(t, rig.pool, testWaiter(2))
	read := func(query string) restdtos.SessionActivity {
		t.Helper()
		code, body, err := getWithCookie(ctx, srv.URL+"/api/sessions/"+sess.ID.String()+"/status"+query, cookie)
		if err != nil || code != http.StatusOK {
			t.Fatalf("GET status%s: %d %s %v", query, code, body, err)
		}
		return decodeActivity(t, body)
	}

	if got := read(""); got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled {
		t.Fatalf("with the advance held: activity %q, settled %v; want scheduled, unsettled", got.Activity, got.Settled)
	}
	start := time.Now()
	got := read("?waitSeconds=1")
	if got.Wait == nil || got.Wait.Reason != restdtos.SessionActivityWaitReasonTimeout || got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled {
		t.Fatalf("a wait with the advance held: %+v (wait %+v); want it to run to its bound, scheduled, never settled", got, got.Wait)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Fatalf("the wait answered after %v, want it to wait its second", took)
	}

	if _, err := settings.Unfreeze(ctx); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
	releaser, err := workflowengine.NewHeldAdvanceReleaser(rig.pool, turnguard.New(rig.pool, nil, false), platform.DefaultTimeouts(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := releaser.ReleaseOnce(ctx); err != nil || n != 1 {
		t.Fatalf("ReleaseOnce = %d, %v; want the held advance released", n, err)
	}
	if got := read(""); got.Activity != restdtos.SessionActivityActivityQueued || got.Settled || got.PendingTurns != 1 {
		t.Fatalf("after the release: activity %q, settled %v, %d pending; want the next step's turn queued", got.Activity, got.Settled, got.PendingTurns)
	}
}
