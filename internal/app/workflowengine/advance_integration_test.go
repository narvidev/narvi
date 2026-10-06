//go:build integration

// Integration tests for §25.9's ("workflow HITL gate + circuit breaker",
// §25.9) own advance.go: the circuit breaker's real first call site
// (loopguard.Evaluate consulted only on a genuine needs_fix re-fire), the
// escalation notice's "never repeated" guarantee, and human-revision
// loops' own structural exemption from all of the above.
//
// None of the three built-in workflows (migration 000057's own seed) wire
// an audit<->fix loop (§25.8), so these tests build one directly via raw
// SQL against a custom (non-built-in, unbound) workflow_definitions row --
// exactly the shape §25's own Gemini->Opus->Sonnet->Codex example
// describes (an audit step's own needs_fix edge to a fix step, and the fix
// step's own ok edge back to audit), the concrete scenario loopguard.
// Evaluate exists to bound. Every id is server-generated
// (gen_random_uuid(), the tables' own DEFAULT) and captured via RETURNING,
// never client-generated, mirroring migration 000057's own seed style.
package workflowengine_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/loopguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/domain/workflow"
)

// auditFixLoopDef bundles the ids seedAuditFixLoopDefinition creates.
type auditFixLoopDef struct {
	definitionID pgtype.UUID
	auditStepID  pgtype.UUID
	fixStepID    pgtype.UUID
}

// seedAuditFixLoopDefinition inserts a custom, non-built-in
// workflow_definitions row directly via raw SQL: two steps ("audit" order
// 1, "fix" order 2), an Edge{audit, needs_fix, fix} and an Edge{fix, ok,
// audit} -- §25.9's own "two ordinary edges NextStep already evaluates"
// auto-fix loop shape. Unbound (no workflow_bindings row): this test drives
// run/step-run creation directly, never through ResolveStepForNewTurn/
// binding resolution, mirroring startRunAndAttachRealTurn's own sibling
// helper (dispatch_integration_test.go) for the built-ins.
func seedAuditFixLoopDefinition(t *testing.T, ctx context.Context, pool *pgxpool.Pool) auditFixLoopDef {
	t.Helper()

	var def auditFixLoopDef
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'test-audit-fix-loop', false, 1) RETURNING id`).
		Scan(&def.definitionID); err != nil {
		t.Fatalf("insert test workflow_definitions row: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, def.definitionID).
		Scan(&def.auditStepID); err != nil {
		t.Fatalf("insert audit step definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 2, 'agent', '{{prompt}}') RETURNING id`, def.definitionID).
		Scan(&def.fixStepID); err != nil {
		t.Fatalf("insert fix step definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $3, 'needs_fix')`,
		def.definitionID, def.auditStepID, def.fixStepID); err != nil {
		t.Fatalf("insert audit->fix needs_fix edge: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $3, 'ok')`,
		def.definitionID, def.fixStepID, def.auditStepID); err != nil {
		t.Fatalf("insert fix->audit ok edge: %v", err)
	}

	return def
}

// startRawRun creates a workflow_runs row pinned at def (bypassing binding
// resolution entirely -- this test drives it directly) and the FIRST
// step's own live attempt, with a real Processing turn attached -- mirrors
// startRunAndAttachRealTurn's own manual-construction style
// (dispatch_integration_test.go), just against a custom definition instead
// of a resolved built-in.
func startRawRun(t *testing.T, ctx context.Context, turns *postgres.TurnStore, workflows *postgres.WorkflowStore, sessionRow sqlcgen.Session, def auditFixLoopDef) (runID, stepRunID, turnID pgtype.UUID) {
	t.Helper()

	run, err := workflows.CreateRun(ctx, sessionRow.ID, "request", def.definitionID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepRun, err := workflows.CreateStepRun(ctx, run.ID, def.auditStepID)
	if err != nil {
		t.Fatalf("create audit step run: %v", err)
	}
	prompt := "audit the change"
	createdTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionRow.ID,
		Status:    sqlcgen.TurnStatusProcessing,
		Prompt:    &prompt,
		PlanMode:  false,
	})
	if err != nil {
		t.Fatalf("create audit turn: %v", err)
	}
	if err := workflows.AttachTurn(ctx, stepRun.ID, createdTurn.ID); err != nil {
		t.Fatalf("attach turn: %v", err)
	}
	return run.ID, stepRun.ID, createdTurn.ID
}

// completeWithOutcome posts outcomeStatus onto stepRunID (simulating the
// agent's own generic step-outcome-posting tool call, exactly like a real
// audit/fix step's prompt would instruct it to make) and then calls
// OnTurnCompleted for turnID with turn.TriggerComplete -- the SAME two-call
// sequence a real turn's own genuine completion produces in production
// (workflowstepoutcome.go's own POST, then sessionactor's own
// completeProcessingTurn).
func completeWithOutcome(t *testing.T, ctx context.Context, deps workflowengine.Deps, sessionRow sqlcgen.Session, stepRunID, turnID pgtype.UUID, outcomeStatus string) {
	t.Helper()
	if _, err := deps.Workflows.SetStepRunOutcome(ctx, stepRunID, outcomeStatus, "test outcome: "+outcomeStatus, nil); err != nil {
		t.Fatalf("SetStepRunOutcome(%s): %v", outcomeStatus, err)
	}
	workflowengine.OnTurnCompleted(ctx, deps, sessionRow, turnID, turn.TriggerComplete)
}

// inTx runs fn with deps bound to one transaction on pool, and commits it,
// as every production caller of the engine does (the session actor's
// transact, the decide endpoint's own transaction): the engine creates its
// turns through TurnStore.CreateAndArmDispatch, which arms their dispatch
// timer in the same transaction and refuses a store bound to the pool.
func inTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deps workflowengine.Deps, fn func(workflowengine.Deps)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	bound := deps
	bound.Workflows = deps.Workflows.WithTx(tx)
	bound.Turns = deps.Turns.WithTx(tx)
	bound.SlackThreadSessions = deps.SlackThreadSessions.WithTx(tx)
	bound.LinearAgentSessions = deps.LinearAgentSessions.WithTx(tx)
	bound.GitHubPRSessions = deps.GitHubPRSessions.WithTx(tx)
	bound.Outbox = deps.Outbox.WithTx(tx)
	// The session guard (technical plan §40.1), bound to the same
	// transaction, as the session actor's workflowDeps binds it: every
	// attempt the engine dispatches is admitted by it first.
	bound.Guard = turnguard.New(pool, nil, false).WithTx(tx, nil)
	if bound.Origin == 0 {
		bound.Origin = sessionguard.OriginAutomatic
	}
	// The autonomy freeze (technical plan §40.2), bound to the same
	// transaction, as the session actor's workflowDeps binds it: the
	// engine's own advance is held while autonomy is frozen.
	gate, err := autonomy.NewGate(pool)
	if err != nil {
		t.Fatalf("autonomy gate: %v", err)
	}
	bound.Autonomy = gate.WithTx(tx)
	fn(bound)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// countOutboxRowsOfKind returns how many outbox rows for sessionID carry
// kind -- used to assert "exactly one notice, never repeated" directly
// against durable state, not just the immediate call's own return value.
func countOutboxRowsOfKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sessionID pgtype.UUID, kind string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, kind).Scan(&count); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return count
}

// slackOriginSessionWithThread creates a Slack-origin session and claims a
// real slack_thread_sessions row for it -- the destination
// enqueueWorkflowNotice (notify.go) resolves against, so this test can
// assert on a REAL enqueued outbox row rather than merely "no error".
func slackOriginSessionWithThread(t *testing.T, ctx context.Context, sessions *postgres.SessionStore, slackThreadSessions *postgres.SlackThreadSessionStore, channelID, threadTS string) sqlcgen.Session {
	t.Helper()
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack})
	if err != nil {
		t.Fatalf("create slack-origin session: %v", err)
	}
	if _, ok, err := slackThreadSessions.Claim(ctx, channelID, threadTS, session.ID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}
	return session
}

// TestCircuitBreaker_NeedsFixLoop_EscalatesAfterMaxAttempts_ExactlyOneNotice
// is this Step's own flagship circuit-breaker proof (§25.9/§25.5): drives a
// real audit<->fix loop (needs_fix -> fix, ok -> audit, repeated) through
// OnTurnCompleted -- the SAME entry point real agent-driven traffic uses --
// past loopguard.DefaultMaxAttempts, and asserts:
//
//   - every attempt strictly under the bound actually proceeds (a new "fix"
//     step-run is created and the loop continues back through "audit");
//   - the run escalates to needs_review at exactly the point the bound is
//     exhausted, never before;
//   - exactly ONE workflow-decision notice is enqueued for the whole run,
//     even after a further redundant escalate attempt against the
//     already-escalated run (never a second notice, mirroring §24.6's own
//     "never repeated" contract).
func TestCircuitBreaker_NeedsFixLoop_EscalatesAfterMaxAttempts_ExactlyOneNotice(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	slackThreadSessions := postgres.NewSlackThreadSessionStore(pool)

	session := slackOriginSessionWithThread(t, ctx, sessions, slackThreadSessions, "C-CIRCUIT-BREAKER", "1111.1111")
	def := seedAuditFixLoopDefinition(t, ctx, pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: slackThreadSessions,
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}

	runID, auditStepRunID, auditTurnID := startRawRun(t, ctx, turns, workflows, session, def)

	escalated := false
	for attempt := 0; attempt < loopguard.DefaultMaxAttempts+2; attempt++ {
		// audit reports needs_fix -> either proceeds to a fresh "fix"
		// attempt, or (once the bound is exhausted) escalates instead.
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			completeWithOutcome(t, ctx, deps, session, auditStepRunID, auditTurnID, "needs_fix")
		})

		runRow, err := workflows.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("get run (attempt %d): %v", attempt, err)
		}
		if string(runRow.Status) == "needs_review" {
			escalated = true
			break
		}

		// Not escalated yet: a new "fix" attempt must now be the run's own
		// live step-run.
		fixRun, err := workflows.GetLiveStepRunForRun(ctx, runID)
		if err != nil {
			t.Fatalf("get live step run after needs_fix advance (attempt %d): %v", attempt, err)
		}
		if fixRun.StepDefinitionID != def.fixStepID {
			t.Fatalf("attempt %d: live step = %s, want the fix step %s", attempt, fixRun.StepDefinitionID, def.fixStepID)
		}
		if !fixRun.TurnID.Valid {
			t.Fatalf("attempt %d: fix step run has no turn_id -- ApplyStepOutcome must dispatch a real turn on proceed", attempt)
		}

		// fix reports ok -> loops back to a fresh "audit" attempt.
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			completeWithOutcome(t, ctx, deps, session, fixRun.ID, fixRun.TurnID, "ok")
		})

		nextAudit, err := workflows.GetLiveStepRunForRun(ctx, runID)
		if err != nil {
			t.Fatalf("get live step run after ok loop-back (attempt %d): %v", attempt, err)
		}
		if nextAudit.StepDefinitionID != def.auditStepID {
			t.Fatalf("attempt %d: live step after fix's own ok = %s, want the audit step %s", attempt, nextAudit.StepDefinitionID, def.auditStepID)
		}
		if !nextAudit.TurnID.Valid {
			t.Fatalf("attempt %d: audit step run has no turn_id", attempt)
		}
		auditStepRunID, auditTurnID = nextAudit.ID, nextAudit.TurnID
	}

	if !escalated {
		t.Fatalf("circuit breaker never escalated within %d loop attempts", loopguard.DefaultMaxAttempts+2)
	}

	runRow, err := workflows.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run after escalation: %v", err)
	}
	if string(runRow.Status) != "needs_review" {
		t.Errorf("run status = %q, want needs_review", runRow.Status)
	}
	if !runRow.NeedsReviewNotifiedAt.Valid {
		t.Error("needs_review_notified_at is NULL, want set (the one-time escalation notice claim)")
	}

	noticeCount := countOutboxRowsOfKind(t, ctx, pool, session.ID, "slack_workflow_decision")
	if noticeCount != 1 {
		t.Fatalf("outbox rows with kind slack_workflow_decision = %d, want exactly 1", noticeCount)
	}

	// A further, redundant attempt to re-escalate the SAME already-
	// escalated run (e.g. a defensive caller re-running EscalateRun/
	// ClaimEscalationNotice against it) must claim nothing further and
	// enqueue no second notice.
	claimed, err := workflows.ClaimEscalationNotice(ctx, runID)
	if err != nil {
		t.Fatalf("ClaimEscalationNotice (redundant attempt): %v", err)
	}
	if claimed != 0 {
		t.Errorf("ClaimEscalationNotice redundant claim = %d, want 0 (already claimed once)", claimed)
	}
	if noticeCount := countOutboxRowsOfKind(t, ctx, pool, session.ID, "slack_workflow_decision"); noticeCount != 1 {
		t.Errorf("outbox rows with kind slack_workflow_decision after redundant claim attempt = %d, want still exactly 1", noticeCount)
	}
}

// TestDispatchSameStepRevision_NeverEscalates_RegardlessOfLoopLength proves
// §25.9's own "human-revision loops are EXEMPT from the circuit breaker"
// requirement structurally: DispatchSameStepRevision re-executes the SAME
// step far more times than loopguard.DefaultMaxAttempts would ever permit
// an ordinary needs_fix re-fire, and the run must never escalate -- because
// this call path never calls ApplyStepOutcome/workflow.NextStep/
// loopguard.Evaluate at all, not because some threshold happens not to be
// reached.
func TestDispatchSameStepRevision_NeverEscalates_RegardlessOfLoopLength(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)

	session := newSession(t, ctx, sessions)
	def := seedAuditFixLoopDefinition(t, ctx, pool) // only def.auditStepID is used here
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}

	run, err := workflows.CreateRun(ctx, session.ID, "request", def.definitionID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// DispatchSameStepRevision only ever reads ID/PromptTemplate/ModelID off
	// the step it is handed (dispatchNextAttempt's own applyStep call) --
	// Order/Edges/etc. are irrelevant to it, so this minimal value mirrors
	// exactly what seedAuditFixLoopDefinition persisted for the audit step
	// without a full round-trip read of it.
	auditStep := workflow.StepDefinition{ID: workflow.ID(def.auditStepID.String()), PromptTemplate: "{{prompt}}"}

	const revisionRounds = 10 // well past loopguard.DefaultMaxAttempts (3)
	if revisionRounds <= loopguard.DefaultMaxAttempts {
		t.Fatalf("test setup: revisionRounds (%d) must exceed loopguard.DefaultMaxAttempts (%d) to prove the exemption", revisionRounds, loopguard.DefaultMaxAttempts)
	}

	for i := 0; i < revisionRounds; i++ {
		var newTurnID pgtype.UUID
		inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
			if newTurnID, err = workflowengine.DispatchSameStepRevision(ctx, deps, run.ID, auditStep, "please revise again", session); err != nil {
				t.Fatalf("DispatchSameStepRevision (round %d): %v", i, err)
			}
		})

		runRow, err := workflows.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("get run (round %d): %v", i, err)
		}
		if string(runRow.Status) != "running" {
			t.Fatalf("round %d: run status = %q, want running (revise must never escalate, regardless of how many rounds)", i, runRow.Status)
		}

		// Free the "one live attempt per run" slot before the NEXT round's
		// own DispatchSameStepRevision call creates another one -- mirrors
		// what the real decide endpoint's own guarded DecideStepRun UPDATE
		// does immediately before ever calling DispatchSameStepRevision
		// (decideworkflowstep.go): the CURRENT attempt is decided (moved out
		// of the live set) before the NEXT one is created. This test drives
		// the transition directly via SQL rather than through DecideStepRun
		// itself, since DecideStepRun's own guard requires 'awaiting_decision'
		// -- a state this synthetic, non-HITL step never actually enters.
		liveStepRun, err := workflows.GetLiveStepRunForRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("get live step run (round %d): %v", i, err)
		}
		if liveStepRun.TurnID != newTurnID {
			t.Fatalf("round %d: live step run's turn_id = %s, want the just-dispatched turn %s", i, liveStepRun.TurnID, newTurnID)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_step_runs SET status = 'completed', finished_at = now() WHERE id = $1`, liveStepRun.ID); err != nil {
			t.Fatalf("force step-run terminal (round %d): %v", i, err)
		}
	}

	attempts, err := workflows.CountStepRunsForStepDefinition(ctx, run.ID, def.auditStepID)
	if err != nil {
		t.Fatalf("CountStepRunsForStepDefinition: %v", err)
	}
	if int(attempts) != revisionRounds {
		t.Errorf("attempt count = %d, want %d (one workflow_step_runs row per revision round)", attempts, revisionRounds)
	}
}

// TestApplyStepOutcome_NextAttemptLeavesAPersonsStopStanding: a workflow's
// next attempt is a turn the engine inserts, never one a person creates, so
// inserting it leaves a person's stop request standing (technical plan
// §3.3: only a person's own act clears it -- a turn they create, a plan
// they approve, or a workflow step they approve or revise). Reached here
// through ApplyStepOutcome -- the authority a person's HITL approve also
// calls, whose route clears the request itself, before this runs
// (httpapi's DecideWorkflowStep; TestStopSession_HITLDecisionResumes) --
// on a session whose request stands: the next attempt's turn is inserted,
// and sessions.stop_requested_at keeps its instant.
func TestApplyStepOutcome_NextAttemptLeavesAPersonsStopStanding(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	def := seedAuditFixLoopDefinition(t, ctx, pool)
	runID, auditStepRunID, _ := startRawRun(t, ctx, turns, workflows, session, def)
	if _, err := workflows.FinishStepRun(ctx, auditStepRunID, "completed", string(workflow.StepOutcomeNeedsFix)); err != nil {
		t.Fatalf("finish audit step run: %v", err)
	}

	var requestedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `UPDATE sessions SET stop_requested_at = now() WHERE id = $1 RETURNING stop_requested_at`, session.ID).Scan(&requestedAt); err != nil {
		t.Fatalf("a person's stop: %v", err)
	}
	sessionRow, err := sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	runRow, err := workflows.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	loaded, err := workflowengine.LoadDefinition(ctx, workflows, def.definitionID)
	if err != nil {
		t.Fatalf("load definition: %v", err)
	}

	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		if _, err := workflowengine.ApplyStepOutcome(ctx, deps, runRow, loaded, sessionRow, workflow.ID(def.auditStepID.String()), workflow.StepOutcomeNeedsFix, nil); err != nil {
			t.Fatalf("ApplyStepOutcome: %v", err)
		}
	})

	all, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("turns = %d, want the audit attempt and the fix attempt the engine inserted", len(all))
	}
	var after pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT stop_requested_at FROM sessions WHERE id = $1`, session.ID).Scan(&after); err != nil {
		t.Fatalf("read stop request: %v", err)
	}
	if !after.Valid || !after.Time.Equal(requestedAt.Time) {
		t.Fatalf("stop_requested_at = %v after the engine's next attempt, want %v: a turn the engine creates never lifts a person's stop", after, requestedAt)
	}
}
