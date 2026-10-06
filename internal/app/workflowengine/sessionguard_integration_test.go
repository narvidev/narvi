//go:build integration

package workflowengine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/workflow"
)

// This file is the workflow engine's half of technical plan §40.1's spend
// cap: the engine's next attempt is admitted by the session guard before
// its step run is created, an automatic advance refused escalates the run
// with the guard's text as its one notice, and a person's revision refused
// is answered with the refusal.

// capWorkflowSession is a Slack-origin session naming repo, capped at
// limit, its thread mapped.
func capWorkflowSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, limit, thread string) sqlcgen.Session {
	t.Helper()
	session, err := postgres.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceSlack,
		Repos:       []byte(`[{"url": "https://github.com/` + repo + `.git"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := postgres.NewSlackThreadSessionStore(pool).Claim(ctx, "C-GUARD", thread, session.ID); err != nil || !ok {
		t.Fatalf("claim slack thread: %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repo, limit); err != nil {
		t.Fatal(err)
	}
	return session
}

func countWhere(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// refusedAdvance runs an audit step whose turn took a Slack-origin session
// capped at $1.00 to $1.50 to its needs_fix end, through OnTurnCompleted,
// so the engine's automatic advance to the fix step meets the session
// guard. notifiedBefore claims the run's one escalation notice first, as
// an earlier escalation would have. It returns the session, the run and
// the refusal the guard makes.
func refusedAdvance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, thread string, notifiedBefore bool) (sqlcgen.Session, pgtype.UUID, sessionguard.Refusal) {
	t.Helper()
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	session := capWorkflowSession(ctx, t, pool, repo, "1.00", thread)
	def := seedAuditFixLoopDefinition(t, ctx, pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}
	runID, auditStepRunID, auditTurnID := startRawRun(t, ctx, turns, workflows, session, def)
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_at = now(), cost_usd = 1.5 WHERE id = $1`, auditTurnID); err != nil {
		t.Fatal(err)
	}
	if notifiedBefore {
		if claimed, err := workflows.ClaimEscalationNotice(ctx, runID); err != nil || claimed != 1 {
			t.Fatalf("claim the run's notice beforehand: %d %v", claimed, err)
		}
	}
	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		completeWithOutcome(t, ctx, deps, session, auditStepRunID, auditTurnID, "needs_fix")
	})
	return session, runID, sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: session.ID.Bytes, Cap: 1_000_000, Spent: 1_500_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}}
}

// TestWorkflowAdvance_AtSpendCap_EscalatesWithoutATurn: the engine's
// advance past a step that took the session past its cap is refused by the
// session guard before the next step run exists: no turn and no orphan step
// run are created, and the run escalates to needs_review -- the session
// waits on a person, nothing failed.
func TestWorkflowAdvance_AtSpendCap_EscalatesWithoutATurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	session, runID, _ := refusedAdvance(ctx, t, pool, "acme/workflow-cap", "2222.0001", false)

	runRow, err := postgres.NewWorkflowStore(pool).GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if string(runRow.Status) != "needs_review" {
		t.Fatalf("run status = %q, want needs_review: a refused advance escalates", runRow.Status)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID); n != 1 {
		t.Fatalf("step runs = %d, want the audit step's alone: the refused attempt's step run is never created", n)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID); n != 1 {
		t.Fatalf("turns = %d, want the audit turn alone", n)
	}
}

// TestSpendCap_WorkflowEscalationCarriesTheOneNotice: the crossing an
// automatic advance meets is told once. The run's escalation notice
// carries the guard's text, the crossing's warning is recorded, and no
// separate guard notice is sent; when the run was already notified of an
// earlier escalation, the guard's own notice goes out instead.
func TestSpendCap_WorkflowEscalationCarriesTheOneNotice(t *testing.T) {
	ctx := context.Background()

	t.Run("the escalation's notice carries the guard's text", func(t *testing.T) {
		pool := newTestPool(t)
		session, _, refusal := refusedAdvance(ctx, t, pool, "acme/workflow-notice", "2222.0002", false)
		var payload []byte
		if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = 'slack_workflow_decision'`, session.ID).Scan(&payload); err != nil {
			t.Fatalf("the escalation notice: %v", err)
		}
		var notice struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatal(err)
		}
		if notice.Text != sessionguard.Text(refusal) {
			t.Fatalf("escalation notice = %q, want the guard's text", notice.Text)
		}
		if n := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = 'slack_session_guard'`, session.ID); n != 0 {
			t.Fatalf("guard notices = %d, want none: the escalation carries the one notice", n)
		}
		if n := countWhere(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, session.ID, turnguard.WarningMessageID(refusal)); n != 1 {
			t.Fatalf("warnings at the crossing's id = %d, want 1", n)
		}
	})

	t.Run("an already-notified run's crossing gets the guard's notice", func(t *testing.T) {
		pool := newTestPool(t)
		session, _, _ := refusedAdvance(ctx, t, pool, "acme/workflow-notified", "2222.0003", true)
		escalations := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = 'slack_workflow_decision'`, session.ID)
		guards := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = 'slack_session_guard'`, session.ID)
		if escalations != 0 || guards != 1 {
			t.Fatalf("escalation notices %d, guard notices %d; want the guard's one notice", escalations, guards)
		}
	})
}

// TestDispatchSameStepRevision_AtSpendCap_ReturnsTheRefusal: a person's
// revision of a step is refused with the guard's refusal as its error, and
// no step run or turn is created -- the decide endpoint answers 409 and
// rolls back.
func TestDispatchSameStepRevision_AtSpendCap_ReturnsTheRefusal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	session := capWorkflowSession(ctx, t, pool, "acme/workflow-revise", "1.00", "2222.0004")
	def := seedAuditFixLoopDefinition(t, ctx, pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
		Origin:              sessionguard.OriginPerson,
	}
	runID, _, auditTurnID := startRawRun(t, ctx, turns, workflows, session, def)
	if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 2 WHERE id = $1`, auditTurnID); err != nil {
		t.Fatal(err)
	}
	loaded, err := workflowengine.LoadDefinition(ctx, workflows, def.definitionID)
	if err != nil {
		t.Fatal(err)
	}
	var step workflow.StepDefinition
	for _, s := range loaded.Steps {
		if s.ID == workflow.ID(def.auditStepID.String()) {
			step = s
		}
	}
	var revisionErr error
	var turnID pgtype.UUID
	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		turnID, revisionErr = workflowengine.DispatchSameStepRevision(ctx, deps, runID, step, "try again", session)
	})
	if _, ok := sessionguard.AsRefusal(revisionErr); !ok || turnID.Valid {
		t.Fatalf("revision past the cap = %v (turn %v), want the guard's refusal", revisionErr, turnID.Valid)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID); n != 1 {
		t.Fatalf("step runs = %d, want 1: a refused revision creates none", n)
	}
}
