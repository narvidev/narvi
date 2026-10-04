//go:build integration

package workflowengine_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/workflowengine"
)

// TestOnTurnWithdrawn runs a withdrawn attempt -- a request dropped before
// it ran -- through the engine on a step whose blocked self edge
// OnTurnCompleted would follow (refusal_integration_test.go's control
// case): the run ends cancelled and its attempt cancelled, no edge is
// followed, so no fresh attempt or turn is queued, and no notice is posted;
// a turn the engine does not track is left alone.
func TestOnTurnWithdrawn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	slackThreadSessions := postgres.NewSlackThreadSessionStore(pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: slackThreadSessions,
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}
	count := func(t *testing.T, sql string, arg pgtype.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, arg).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	t.Run("a withdrawn attempt cancels its run and follows no edge", func(t *testing.T) {
		session := slackOriginSessionWithThread(t, ctx, sessions, slackThreadSessions, "C-WITHDRAWN", "2.0001")
		definitionID, stepID := seedBlockedSelfEdgeDefinition(t, ctx, pool, "test-withdrawn-blocked-self-edge")
		run, err := workflows.CreateRun(ctx, session.ID, "request", definitionID, 1)
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		stepRun, err := workflows.CreateStepRun(ctx, run.ID, stepID)
		if err != nil {
			t.Fatalf("create step run: %v", err)
		}
		created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusFailed})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		if err := workflows.AttachTurn(ctx, stepRun.ID, created.ID); err != nil {
			t.Fatalf("attach turn: %v", err)
		}

		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnWithdrawn(ctx, deps.Workflows, created.ID)
		})

		got, err := workflows.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if got.Status != sqlcgen.WorkflowRunStatusCancelled {
			t.Errorf("run status = %s, want cancelled", got.Status)
		}
		var stepStatus string
		if err := pool.QueryRow(ctx, `SELECT status::text FROM workflow_step_runs WHERE id = $1`, stepRun.ID).Scan(&stepStatus); err != nil {
			t.Fatalf("read the step run: %v", err)
		}
		if stepStatus != "cancelled" {
			t.Errorf("step run status = %s, want cancelled", stepStatus)
		}
		if n := count(t, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID); n != 1 {
			t.Errorf("step runs = %d, want the withdrawn attempt alone: an edge was followed", n)
		}
		if n := count(t, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID); n != 1 {
			t.Errorf("turns = %d, want the withdrawn one alone: a turn was queued", n)
		}
		if n := count(t, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID); n != 0 {
			t.Errorf("outbox rows = %d, want none: the caller has told the person", n)
		}
	})

	t.Run("a turn the engine does not track is left alone", func(t *testing.T) {
		session := slackOriginSessionWithThread(t, ctx, sessions, slackThreadSessions, "C-WITHDRAWN", "2.0002")
		created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusFailed})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnWithdrawn(ctx, deps.Workflows, created.ID)
		})
		if n := count(t, `SELECT count(*) FROM workflow_runs WHERE session_id = $1`, session.ID); n != 0 {
			t.Errorf("workflow runs = %d, want none for an untracked turn", n)
		}
		if n := count(t, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID); n != 0 {
			t.Errorf("outbox rows = %d, want none for an untracked turn", n)
		}
	})
}
