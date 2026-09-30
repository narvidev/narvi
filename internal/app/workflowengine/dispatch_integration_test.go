//go:build integration

package workflowengine_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// The seeded built-in request definition/step ids (migration 000057's own
// header comment) -- mirrors internal/adapters/outbound/postgres's own
// workflow_seed_integration_test.go identical constants (unreachable from
// this external test package). No built-in plan constant is needed here:
// as of migration 000088_plan_builtin_passthrough (§25.9's own
// corrective follow-up, §25.8/§25.9), the built-in plan workflow is a
// single-step passthrough carrying no HITL, so this package's own
// HITLAfter-specific tests below exercise a CUSTOM (non-built-in) step
// instead (seedCustomHITLAfterStep).
const (
	builtInRequestDefID  = "00000000-0000-4000-8000-000000000002"
	builtInRequestStepID = "00000000-0000-4000-8000-000000000021"
)

// liveRow bundles the (workflow_run_id, step_run_id, step_definition_id)
// triple TestOnTurnCompleted's own subtests need after starting a run.
type liveRow struct {
	sessionID      pgtype.UUID
	runID          pgtype.UUID
	stepRunID      pgtype.UUID
	stepDefID      pgtype.UUID
	turnID         pgtype.UUID
	workflowsStore *postgres.WorkflowStore
}

// testDeps builds a workflowengine.Deps for OnTurnCompleted's own tests
// (completion_integration_test.go) -- turns/workflows are the SAME
// instances the calling test already constructed (so bookkeeping reads/
// writes land through the identical store the test itself asserts
// against); the notification-destination stores are constructed fresh from
// pool (cheap, side-effect-free wrappers, mirroring every other
// *_store.go's own constructor) -- every test session in that file is
// 'web'-origin (newSession's own fixed SpawnSourceWeb below), so
// enqueueWorkflowNotice's own top check short-circuits before ever
// touching any of the three.
func testDeps(pool *pgxpool.Pool, turns *postgres.TurnStore, workflows *postgres.WorkflowStore) workflowengine.Deps {
	return workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}
}

// newSession creates a bare session (no repos, no intent_decision --
// zero-config) directly via SessionStore, exactly like
// internal/adapters/inbound/httpapi's own turnCoreTestRig.newFixtureSession.
func newSession(t *testing.T, ctx context.Context, sessions *postgres.SessionStore) sqlcgen.Session {
	t.Helper()
	s, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create fixture session: %v", err)
	}
	return s
}

// startRunAndAttachRealTurn mirrors createTurnLocked's own two-call
// sequence (ResolveStepForNewTurn, then insert the turn, then AttachTurn)
// exactly, so OnTurnCompleted's own tests exercise a run/step-run/turn
// triple shaped exactly like production traffic produces -- returns the
// resolution's own prompt/modelID too, for callers that want to assert on
// them directly (the repo-override test).
func startRunAndAttachRealTurn(t *testing.T, ctx context.Context, sessions *postgres.SessionStore, turns *postgres.TurnStore, workflows *postgres.WorkflowStore, sessionRow sqlcgen.Session, prompt string, modelID *string, planMode bool) (liveRow, workflowengine.Resolution) {
	t.Helper()

	// effort is always nil here -- no existing caller of this helper
	// exercises effort-specific behavior (that gets its own, faster,
	// DB-free unit test: applyStep's ModelID/Effort override logic is pure,
	// see dispatch_test.go's TestApplyStep_EffortOverride). Mirrors
	// modelID's own "nil unless a test cares" convention exactly.
	res := workflowengine.ResolveStepForNewTurn(ctx, workflows, sessionRow, prompt, modelID, nil)
	if !res.Tracked {
		t.Fatalf("ResolveStepForNewTurn: Tracked = false, want true for a fresh session")
	}

	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionRow.ID,
		Status:    sqlcgen.TurnStatusProcessing,
		Prompt:    &res.Prompt,
		ModelID:   res.ModelID,
		Effort:    res.Effort,
		PlanMode:  planMode,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	workflowengine.AttachTurn(ctx, workflows, res, created.ID)

	// Re-fetch by the now-attached turn id (GetLiveStepRunByTurnID, the
	// SAME lookup OnTurnCompleted itself uses) -- res.StepRunID alone has
	// no direct "fetch by step-run id" method on WorkflowStore, and this
	// exercises the real reverse lookup path instead of inventing one.
	stepRun, err := workflows.GetLiveStepRunByTurnID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get live step run by turn id: %v", err)
	}

	return liveRow{
		sessionID:      sessionRow.ID,
		runID:          stepRun.WorkflowRunID,
		stepRunID:      stepRun.ID,
		stepDefID:      stepRun.StepDefinitionID,
		turnID:         created.ID,
		workflowsStore: workflows,
	}, res
}

// seedCustomHITLAfterStep seeds a custom (non-built-in), single-step
// workflow definition whose one step carries hitl_after = true, bound as a
// repo override for the 'request' lane, plus a fresh session naming EXACTLY
// that one repo -- so ResolveStepForNewTurn's own repo-override resolution
// (resolveBinding, definition.go) deterministically routes new turns
// through it, mirroring TestResolveStepForNewTurn_RepoOverrideBinding_
// UsesOverrideNotGlobal's own seeding shape one field further (hitl_after
// instead of a ModelID override).
//
// Exists so this package's own HITLAfter-specific tests no longer depend on
// the built-in PLAN workflow's own shape: migration
// 000088_plan_builtin_passthrough (§25.9's own corrective follow-up, an
// audit-found design incoherence -- see that migration's own header comment
// and docs/TECHNICAL_PLAN.md §25.8) made the built-in plan workflow a
// genuine single-step passthrough, identical to review/request, so classic
// plan mode (§8.1) stays the SOLE plan-approval authority and
// no built-in carries hitl_after any longer. The HITLAfter mechanism itself
// (OnTurnCompleted's own HITLAfter branch, completion.go) is completely
// unchanged by that migration and remains available to any future custom
// workflow definition -- e.g. one authored via the Phase 7 canvas editor,
// §25.12 -- which is exactly the shape this helper seeds directly.
func seedCustomHITLAfterStep(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, defID, stepID, repoFullName string) sqlcgen.Session {
	t.Helper()

	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_definitions (id, lane, name, is_built_in, version) VALUES ($1, 'request', $2, false, 1)`,
		defID, "custom-hitl-"+defID); err != nil {
		t.Fatalf("seed custom HITL definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_step_definitions (id, workflow_definition_id, step_order, kind, prompt_template, hitl_after)
		VALUES ($1, $2, 1, 'agent', '{{prompt}}', true)`,
		stepID, defID); err != nil {
		t.Fatalf("seed custom HITL step: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version)
		VALUES ('request', $1, $2, 1)`, repoFullName, defID); err != nil {
		t.Fatalf("seed repo override binding: %v", err)
	}

	repos := []byte(`[{"name":"repo","url":"https://github.com/` + repoFullName + `.git","branch":null}]`)
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, Repos: repos})
	if err != nil {
		t.Fatalf("create session with repo: %v", err)
	}
	return session
}

func TestResolveStepForNewTurn_ZeroConfigRequestLane_StartsNewRunAndTracksFirstStep(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)

	session := newSession(t, ctx, sessions)

	res := workflowengine.ResolveStepForNewTurn(ctx, workflows, session, "hello there", nil, nil)
	if !res.Tracked {
		t.Fatal("Tracked = false, want true")
	}
	if res.Prompt != "hello there" {
		t.Errorf("Prompt = %q, want %q (built-in request step is pure passthrough)", res.Prompt, "hello there")
	}
	if res.ModelID != nil {
		t.Errorf("ModelID = %v, want nil (caller passed nil, built-in step ModelID is nil -- inherit)", res.ModelID)
	}

	runRow, err := workflows.GetRunningRunForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetRunningRunForSession: %v", err)
	}
	if string(runRow.Lane) != "request" {
		t.Errorf("run lane = %q, want %q", runRow.Lane, "request")
	}
	if runRow.WorkflowDefinitionID.String() != builtInRequestDefID {
		t.Errorf("run definition id = %s, want the built-in request definition %s", runRow.WorkflowDefinitionID.String(), builtInRequestDefID)
	}
	if string(runRow.Status) != "running" {
		t.Errorf("run status = %q, want running", runRow.Status)
	}

	stepRun, err := workflows.GetLiveStepRunForRun(ctx, runRow.ID)
	if err != nil {
		t.Fatalf("GetLiveStepRunForRun: %v", err)
	}
	if stepRun.StepDefinitionID.String() != builtInRequestStepID {
		t.Errorf("step-run step id = %s, want the built-in request step %s", stepRun.StepDefinitionID.String(), builtInRequestStepID)
	}
	if stepRun.TurnID.Valid {
		t.Error("step-run turn_id is already set before AttachTurn ran, want NULL")
	}

	// workflow_step_runs.turn_id is a real FK (REFERENCES turns(id)) -- a
	// real turn row is required here, not an invented UUID.
	createdTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID,
		Status:    sqlcgen.TurnStatusPending,
		Prompt:    &res.Prompt,
		ModelID:   res.ModelID,
	})
	if err != nil {
		t.Fatalf("create real turn for FK: %v", err)
	}
	workflowengine.AttachTurn(ctx, workflows, res, createdTurn.ID)

	stepRun2, err := workflows.GetLiveStepRunForRun(ctx, runRow.ID)
	if err != nil {
		t.Fatalf("GetLiveStepRunForRun (after attach): %v", err)
	}
	if !stepRun2.TurnID.Valid || stepRun2.TurnID != createdTurn.ID {
		t.Errorf("step-run turn_id after AttachTurn = %+v, want %+v", stepRun2.TurnID, createdTurn.ID)
	}
}

// TestResolveStepForNewTurn_RepoOverrideBinding_UsesOverrideNotGlobal
// proves §25.7's per-step model/provider binding AND §25.4's repo-override
// resolution together, in the one scenario this Step's own zero-config
// characterization test deliberately does NOT cover (a real, non-built-in
// definition bound to a specific repo): a session naming exactly one repo
// whose "owner/repo" has its own workflow_bindings row must dispatch
// through THAT definition's own step -- a non-passthrough PromptTemplate
// and a non-nil ModelID -- not the global built-in.
func TestResolveStepForNewTurn_RepoOverrideBinding_UsesOverrideNotGlobal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	workflows := postgres.NewWorkflowStore(pool)

	const (
		customDefID  = "10000000-0000-4000-8000-000000000001"
		customStepID = "10000000-0000-4000-8000-000000000002"
	)
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_definitions (id, lane, name, is_built_in, version) VALUES ($1, 'request', 'custom-request', false, 1)`,
		customDefID); err != nil {
		t.Fatalf("seed custom definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_step_definitions (id, workflow_definition_id, step_order, kind, model_id, prompt_template)
		VALUES ($1, $2, 1, 'agent', 'openai/gpt-5.5-codex', 'OVERRIDE: {{prompt}}')`,
		customStepID, customDefID); err != nil {
		t.Fatalf("seed custom step: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version)
		VALUES ('request', 'acme/widgets', $1, 1)`, customDefID); err != nil {
		t.Fatalf("seed repo override binding: %v", err)
	}

	repos := []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`)
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, Repos: repos})
	if err != nil {
		t.Fatalf("create session with repos: %v", err)
	}

	res := workflowengine.ResolveStepForNewTurn(ctx, workflows, session, "fix the bug", nil, nil)
	if !res.Tracked {
		t.Fatal("Tracked = false, want true")
	}
	if res.Prompt != "OVERRIDE: fix the bug" {
		t.Errorf("Prompt = %q, want the custom step's own rendered template", res.Prompt)
	}
	if res.ModelID == nil || *res.ModelID != "openai/gpt-5.5-codex" {
		t.Errorf("ModelID = %v, want %q (the custom step's own per-step override, §25.7)", res.ModelID, "openai/gpt-5.5-codex")
	}

	runRow, err := workflows.GetRunningRunForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetRunningRunForSession: %v", err)
	}
	if runRow.WorkflowDefinitionID.String() != customDefID {
		t.Errorf("run definition id = %s, want the repo-override custom definition %s, not the global built-in", runRow.WorkflowDefinitionID.String(), customDefID)
	}
}

// TestResolveStepForNewTurn_MultiRepoSession_FallsBackToGlobalBinding
// proves repoFullNameFromSessionRepos' own documented ambiguous-multi-repo
// carve-out end to end: even with a repo-specific override seeded for ONE
// of a session's two repos, a multi-repo session resolves the GLOBAL
// binding, never guessing which repo's override should apply.
func TestResolveStepForNewTurn_MultiRepoSession_FallsBackToGlobalBinding(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	workflows := postgres.NewWorkflowStore(pool)

	const customDefID = "10000000-0000-4000-8000-000000000003"
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_definitions (id, lane, name, is_built_in, version) VALUES ($1, 'request', 'custom-request-2', false, 1)`,
		customDefID); err != nil {
		t.Fatalf("seed custom definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version)
		VALUES ('request', 'acme/widgets', $1, 1)`, customDefID); err != nil {
		t.Fatalf("seed repo override binding: %v", err)
	}

	repos := []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null},{"name":"other","url":"https://github.com/acme/other.git","branch":null}]`)
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, Repos: repos})
	if err != nil {
		t.Fatalf("create session with repos: %v", err)
	}

	res := workflowengine.ResolveStepForNewTurn(ctx, workflows, session, "do it", nil, nil)
	if !res.Tracked {
		t.Fatal("Tracked = false, want true")
	}
	if res.Prompt != "do it" {
		t.Errorf("Prompt = %q, want the caller's own text unchanged (global built-in is passthrough)", res.Prompt)
	}

	runRow, err := workflows.GetRunningRunForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetRunningRunForSession: %v", err)
	}
	if runRow.WorkflowDefinitionID.String() != builtInRequestDefID {
		t.Errorf("run definition id = %s, want the GLOBAL built-in %s (multi-repo session, ambiguous, never guesses)", runRow.WorkflowDefinitionID.String(), builtInRequestDefID)
	}
}

// TestResolveStepForNewTurn_LiveAwaitingDecisionStep_ResolvesButDoesNotTrack
// proves case 2 of ResolveStepForNewTurn's own doc comment: once a run's
// live step-run is 'awaiting_decision' (a HITLAfter gate), a SECOND call
// for the same session (simulating a "revise" turn) resolves that SAME
// step's template/model but creates NO new workflow_step_runs row -- never
// violating workflow_step_runs_one_live_per_run.
//
// Exercises a CUSTOM (non-built-in) hitl_after step (seedCustomHITLAfterStep)
// rather than the built-in plan workflow: migration
// 000088_plan_builtin_passthrough (§25.9's own corrective follow-up)
// removed hitl_after from every built-in, so this is now the only way to
// reach case 2 at all -- see seedCustomHITLAfterStep's own doc comment for
// the full "why".
func TestResolveStepForNewTurn_LiveAwaitingDecisionStep_ResolvesButDoesNotTrack(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)

	const (
		customDefID  = "10000000-0000-4000-8000-000000000004"
		customStepID = "10000000-0000-4000-8000-000000000005"
	)
	session := seedCustomHITLAfterStep(t, ctx, pool, sessions, customDefID, customStepID, "acme/hitl-resolve")

	row, _ := startRunAndAttachRealTurn(t, ctx, sessions, turns, workflows, session, "draft something", nil, false)
	inTx(t, ctx, pool, testDeps(pool, turns, workflows), func(deps workflowengine.Deps) {
		workflowengine.OnTurnCompleted(ctx, deps, session, row.turnID, turn.TriggerComplete)
	})

	stepRun, err := workflows.GetLiveStepRunForRun(ctx, row.runID)
	if err != nil {
		t.Fatalf("get live step run after completion: %v", err)
	}
	if string(stepRun.Status) != "awaiting_decision" {
		t.Fatalf("step-run status = %q, want awaiting_decision (test setup assumption)", stepRun.Status)
	}

	res2 := workflowengine.ResolveStepForNewTurn(ctx, workflows, session, "revise: drop the retry", nil, nil)
	if res2.Tracked {
		t.Error("Tracked = true, want false (an awaiting_decision live step must not get a second attempt created by this Step's engine)")
	}
	if res2.Prompt != "revise: drop the retry" {
		t.Errorf("Prompt = %q, want the caller's own text unchanged (the custom step's own passthrough template)", res2.Prompt)
	}

	var liveCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1 AND status IN ('running','awaiting_decision')`,
		row.runID.String()).Scan(&liveCount); err != nil {
		t.Fatalf("count live step-runs: %v", err)
	}
	if liveCount != 1 {
		t.Errorf("live step-run count = %d, want exactly 1 (no second attempt created)", liveCount)
	}
}
