//go:build integration

package workflowengine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// seedBlockedSelfEdgeDefinition inserts a custom one-step definition whose
// step carries a self edge on `blocked` -- a retry loop wired explicitly,
// the shape an implicit "blocked" would re-fire.
func seedBlockedSelfEdgeDefinition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) (definitionID, stepID pgtype.UUID) {
	t.Helper()
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`, name).Scan(&definitionID); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, model_id) VALUES ($1, 1, 'agent', '{{prompt}}', 'openai/gpt-5.4') RETURNING id`, definitionID).Scan(&stepID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $2, 'blocked')`, definitionID, stepID); err != nil {
		t.Fatalf("insert blocked self edge: %v", err)
	}
	return definitionID, stepID
}

// TestOnTurnRefused runs a refused attempt through the engine on a step
// whose blocked self edge OnTurnCompleted would follow (the control case):
// the refusal escalates the run once, with a notice naming it, and queues
// nothing; a person's standing stop cancels the run instead; a turn the
// engine does not track is left alone.
func TestOnTurnRefused(t *testing.T) {
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
	const reason = "personal_link_only: the model openai/gpt-5.4 is available here only through a person's own openai link"

	startRun := func(t *testing.T, thread string) (sqlcgen.Session, pgtype.UUID, pgtype.UUID) {
		t.Helper()
		session := slackOriginSessionWithThread(t, ctx, sessions, slackThreadSessions, "C-REFUSAL", thread)
		definitionID, stepID := seedBlockedSelfEdgeDefinition(t, ctx, pool, "test-blocked-self-edge-"+thread)
		run, err := workflows.CreateRun(ctx, session.ID, "request", definitionID, 1)
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		stepRun, err := workflows.CreateStepRun(ctx, run.ID, stepID)
		if err != nil {
			t.Fatalf("create step run: %v", err)
		}
		model := "openai/gpt-5.4"
		created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusFailed, Prompt: &model, ModelID: &model})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		if err := workflows.AttachTurn(ctx, stepRun.ID, created.ID); err != nil {
			t.Fatalf("attach turn: %v", err)
		}
		return session, run.ID, created.ID
	}
	stepRuns := func(t *testing.T, runID pgtype.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count step runs: %v", err)
		}
		return n
	}

	t.Run("control: a completion read as blocked follows the self edge", func(t *testing.T) {
		session, runID, turnID := startRun(t, "1.0001")
		// In a transaction, as every production caller runs the engine: the
		// re-queued attempt's turn is created with its dispatch timer, which
		// needs one (TurnStore.CreateAndArmDispatch).
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnCompleted(ctx, deps, session, turnID, turn.TriggerAbandon)
		})
		if n := stepRuns(t, runID); n != 2 {
			t.Fatalf("step runs = %d, want 2: the edge re-queued the step", n)
		}
		live, err := workflows.GetLiveStepRunForRun(ctx, runID)
		if err != nil {
			t.Fatalf("get the re-queued attempt: %v", err)
		}
		if !live.TurnID.Valid || live.TurnID == turnID {
			t.Fatalf("re-queued attempt's turn_id = %v, want a new turn: the edge queued no turn", live.TurnID)
		}
		var timers int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'dispatch'`, session.ID).Scan(&timers); err != nil {
			t.Fatalf("count dispatch timers: %v", err)
		}
		if timers != 1 {
			t.Fatalf("dispatch timers = %d, want 1: the re-queued turn was created without its dispatch timer", timers)
		}
	})

	t.Run("a refusal escalates once and follows no edge", func(t *testing.T) {
		session, runID, turnID := startRun(t, "1.0002")
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnRefused(ctx, deps, session, turnID, reason)
		})
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnRefused(ctx, deps, session, turnID, reason)
		})

		run, err := workflows.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status != sqlcgen.WorkflowRunStatusNeedsReview {
			t.Errorf("run status = %s, want needs_review", run.Status)
		}
		if n := stepRuns(t, runID); n != 1 {
			t.Errorf("step runs = %d, want the refused attempt alone", n)
		}
		var count int
		var notice string
		if err := pool.QueryRow(ctx, `SELECT count(*), max(payload->>'text') FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSlackWorkflowDecision)).Scan(&count, &notice); err != nil {
			t.Fatalf("read notices: %v", err)
		}
		if count != 1 || !strings.Contains(notice, reason) || !strings.Contains(notice, "refused before it ran") {
			t.Errorf("notices = %d, text %q; want exactly one, naming the refusal", count, notice)
		}
	})

	t.Run("a person's standing stop cancels the run", func(t *testing.T) {
		session, runID, turnID := startRun(t, "1.0003")
		session.StopRequestedAt = pgtype.Timestamptz{Valid: true, Time: session.CreatedAt.Time}
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnRefused(ctx, deps, session, turnID, reason)
		})
		run, err := workflows.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status != sqlcgen.WorkflowRunStatusCancelled || stepRuns(t, runID) != 1 {
			t.Errorf("run status = %s with %d step runs, want cancelled with the refused attempt alone", run.Status, stepRuns(t, runID))
		}
	})

	t.Run("a turn the engine does not track is left alone", func(t *testing.T) {
		session := slackOriginSessionWithThread(t, ctx, sessions, slackThreadSessions, "C-REFUSAL", "1.0004")
		created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusFailed})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			workflowengine.OnTurnRefused(ctx, deps, session, created.ID, reason)
		})
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID).Scan(&n); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		if n != 0 {
			t.Errorf("outbox rows = %d, want none for an untracked turn", n)
		}
	})
}
