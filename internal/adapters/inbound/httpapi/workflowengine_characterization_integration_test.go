//go:build integration

// This file is §25.6's ("workflow execution engine", §25.6) own
// NON-NEGOTIABLE exit criterion: the characterization test proving the new
// engine-mediated dispatch path (createTurnLocked, now wired through
// internal/app/workflowengine.ResolveStepForNewTurn) produces a BYTE-FOR-
// BYTE/FIELD-FOR-FIELD identical sandboxws.Prompt to what the OLD, direct
// (pre-existing) dispatch path would have produced, for the zero-config
// case (no custom WorkflowBinding for a repo, so the seeded global
// built-in applies) -- for every lane this Step actually flips onto the
// engine (review, request, and the plan lane's own first turn; see
// dispatch.go's own doc comment in internal/app/workflowengine for why
// plan mode's SECOND turn, created by decideplan.go's own untouched
// approve path, is a documented, deliberate gap outside this proof's own
// scope).
//
// Method: call the REAL, production CreateTurnCore (this package's own
// turn.go, unchanged in signature by this Step) to dispatch a turn through
// today's actual code path -- which now happens to be engine-mediated --
// then build the wire payload from the RESULTING turn row via
// sessionactor.BuildPromptPayload (exported by this Step specifically so
// this test can call the exact same function real dispatch uses, rather
// than a re-derived approximation of it -- see that function's own doc
// comment). Separately, build the SAME wire payload from a hand-
// constructed turn row simulating exactly what createTurnLocked inserted
// BEFORE this Step existed (Prompt/ModelID set to the caller's own raw
// values, verbatim, no engine involved at all). The two are compared
// field-by-field, with the one field expected to legitimately differ
// (MessageId, a fresh nonce BuildPromptPayload mints on every call, by
// design) explicitly excluded from the comparison.
//
// Every test below ALSO asserts the engine genuinely ran (a workflow_runs/
// workflow_step_runs row was actually created, attached to the real turn)
// -- proving the equivalence is real ("the engine ran and its built-in
// transform happens to be a no-op"), not an accidental pass because some
// bug silently short-circuited engine resolution entirely (this package's
// own fail-open contract, internal/app/workflowengine/doc.go, means a
// bug WOULD still produce identical prompt/model output -- only the
// workflow_runs/workflow_step_runs assertions below can tell the
// difference).
package httpapi

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
)

// referenceSandboxRow is the SAME fixed, fake sandbox row every comparison
// below builds both its "old" and "new" sandboxws.Prompt against --
// BuildPromptPayload only reads sandboxRow.Gen, so a real (persisted)
// sandbox row is unnecessary for this comparison; a plain struct literal
// is sufficient and keeps this test independent of the spawn/dispatch
// machinery entirely.
var referenceSandboxRow = narvipg.Sandbox{Gen: 3}

// promptJSONWithoutMessageID unmarshals raw as a sandboxws.Prompt and
// zeroes its own MessageId -- the ONE field BuildPromptPayload mints fresh
// (uuid.NewString()) on every single call, by design, so two calls against
// the identical logical turn NEVER produce equal MessageId values even
// when every other field is byte-for-byte identical. Comparing structs
// (not raw bytes) also makes this comparison robust to JSON key ordering,
// which encoding/json does not guarantee is stable across two independent
// Marshal calls of otherwise-equal Go values.
func promptJSONWithoutMessageID(t *testing.T, raw json.RawMessage) sandboxws.Prompt {
	t.Helper()
	var p sandboxws.Prompt
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal sandboxws.Prompt: %v (payload: %s)", err, raw)
	}
	p.MessageId = ""
	return p
}

// assertIdenticalPrompts is the actual field-for-field equivalence
// assertion every test below runs. reflect.DeepEqual, NOT ==/!=: Model/
// Effort/ConversationId are all pointer-shaped fields (sandboxws.
// PromptModel etc.), and the old/new Prompt values are built from two
// INDEPENDENTLY allocated *string values holding the identical text --
// Go's own == on a struct containing pointer fields compares pointer
// IDENTITY, not the pointed-to value, which would make this assertion
// spuriously fail on every single call even when the two payloads are
// genuinely, semantically identical. reflect.DeepEqual follows pointers,
// exactly the comparison this test actually needs.
func assertIdenticalPrompts(t *testing.T, oldRaw, newRaw json.RawMessage, context string) {
	t.Helper()
	oldPrompt := promptJSONWithoutMessageID(t, oldRaw)
	newPrompt := promptJSONWithoutMessageID(t, newRaw)
	if !reflect.DeepEqual(oldPrompt, newPrompt) {
		t.Errorf("sandboxws.Prompt mismatch (%s):\n old = %s\n new = %s", context, oldRaw, newRaw)
	}
}

// assertLiveWorkflowStepRun proves the engine genuinely tracked turnID:
// exactly one workflow_step_runs row exists whose turn_id matches, its
// owning workflow_runs row belongs to sessionID, is on wantLane, and is
// bound to the seeded built-in definition for that lane (§25.4's global
// binding, guaranteed present by migration 000057's own seed -- this is
// the zero-config case, so no repo override exists to shadow it).
func (r *turnCoreTestRig) assertLiveWorkflowStepRun(t *testing.T, ctx context.Context, sessionID, turnID pgtype.UUID, wantLane, wantBuiltInDefID string) {
	t.Helper()

	var runID, lane, defID string
	if err := r.pool.QueryRow(ctx, `
		SELECT wr.id, wr.lane, wr.workflow_definition_id
		FROM workflow_step_runs wsr
		JOIN workflow_runs wr ON wr.id = wsr.workflow_run_id
		WHERE wsr.turn_id = $1`, turnID.String()).Scan(&runID, &lane, &defID); err != nil {
		t.Fatalf("query workflow_step_runs/workflow_runs for turn %s: %v (the engine did not track this turn at all)", turnID.String(), err)
	}

	var runSessionID string
	if err := r.pool.QueryRow(ctx, `SELECT session_id FROM workflow_runs WHERE id = $1`, runID).Scan(&runSessionID); err != nil {
		t.Fatalf("query workflow_runs.session_id: %v", err)
	}
	if runSessionID != sessionID.String() {
		t.Errorf("workflow_runs.session_id = %s, want %s", runSessionID, sessionID.String())
	}
	if lane != wantLane {
		t.Errorf("workflow_runs.lane = %q, want %q", lane, wantLane)
	}
	if defID != wantBuiltInDefID {
		t.Errorf("workflow_runs.workflow_definition_id = %s, want the seeded built-in %s (zero-config: no repo override exists)", defID, wantBuiltInDefID)
	}
}

// The three seeded built-in definition ids (migration 000057's own header
// comment) -- mirrors workflow_seed_integration_test.go's own identical
// constants (that file lives in package postgres_test, unreachable from
// here).
const (
	builtInReviewDefinitionID  = "00000000-0000-4000-8000-000000000001"
	builtInRequestDefinitionID = "00000000-0000-4000-8000-000000000002"
	builtInPlanDefinitionID    = "00000000-0000-4000-8000-000000000003"
)

// TestCharacterization_RequestLane_ZeroConfig_IdenticalPromptJSON is the
// flagship proof for the request lane -- an ordinary chat session with no
// classified intent at all (the single most common real-world shape: a
// brand new session's SECOND turn, since the very first is created by
// CreateSessionOnTx, a documented gap -- see internal/app/workflowengine/
// dispatch.go's own doc comment). resolveLane's own fail-open default
// (empty target/mode -> LaneRequest) must resolve here, and the built-in
// request workflow's single passthrough step must reproduce the caller's
// own prompt/modelID byte-for-byte.
func TestCharacterization_RequestLane_ZeroConfig_IdenticalPromptJSON(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	inputPrompt := "please refactor the widget loader"
	inputModelID := "anthropic/claude-sonnet-5"

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, inputPrompt, &inputModelID, false, false, pgtype.UUID{}, RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}

	// Zero-config equivalence, field for field: the engine's own
	// passthrough transform must be the identity function here.
	if created.Prompt == nil || *created.Prompt != inputPrompt {
		t.Errorf("created.Prompt = %v, want %q (byte-for-byte unchanged)", created.Prompt, inputPrompt)
	}
	if created.ModelID == nil || *created.ModelID != inputModelID {
		t.Errorf("created.ModelID = %v, want %q (byte-for-byte unchanged, ModelID: nil on the built-in step means inherit)", created.ModelID, inputModelID)
	}

	rig.assertLiveWorkflowStepRun(t, ctx, session.ID, created.ID, "request", builtInRequestDefinitionID)

	sessionRow, err := rig.sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	oldTurn := narvipg.Turn{Prompt: &inputPrompt, ModelID: &inputModelID, PlanMode: false}
	oldRaw, err := sessionactor.BuildPromptPayload(session.ID.String(), sessionRow, referenceSandboxRow, oldTurn)
	if err != nil {
		t.Fatalf("BuildPromptPayload (old/reference): %v", err)
	}
	newRaw, err := sessionactor.BuildPromptPayload(session.ID.String(), sessionRow, referenceSandboxRow, created)
	if err != nil {
		t.Fatalf("BuildPromptPayload (new/actual): %v", err)
	}

	assertIdenticalPrompts(t, oldRaw, newRaw, "request lane, zero-config")
}

// TestCharacterization_ReviewLane_ZeroConfig_IdenticalPromptJSON proves the
// SAME equivalence for a session already classified into the review lane
// (sessions.intent_decision.target = "review") -- the built-in review
// workflow's single step is ALSO ModelID nil/passthrough (§25.8: "prompt =
// today's unchanged text, no HITL"), so this must be an identity transform
// too, even though it resolves a genuinely different WorkflowDefinition
// than the request-lane test above.
func TestCharacterization_ReviewLane_ZeroConfig_IdenticalPromptJSON(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	if _, err := rig.sessions.UpdateIntentDecisionIfNull(ctx, session.ID,
		[]byte(`{"surface":"github","source":"classifier","target":"review","mode":"build","decided_at":"2026-01-01T00:00:00Z","decided_at_stage":"create"}`)); err != nil {
		t.Fatalf("seed intent_decision: %v", err)
	}

	// review.RenderTurnPrompt has ALREADY built this exact text upstream,
	// before ever reaching CreateTurnCore (github/coalesce.go's own real
	// review-session dispatch) -- from createTurnLocked's own point of
	// view, this is simply "the caller's own prompt", identical in kind to
	// the request-lane test above.
	inputPrompt := "## Review request\n\nPlease review PR #42 in acme/widgets."

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, inputPrompt, nil, false, false, pgtype.UUID{}, AlwaysQueue)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}

	if created.Prompt == nil || *created.Prompt != inputPrompt {
		t.Errorf("created.Prompt = %v, want %q (byte-for-byte unchanged)", created.Prompt, inputPrompt)
	}
	if created.ModelID != nil {
		t.Errorf("created.ModelID = %v, want nil (caller passed nil, built-in review step's own ModelID is nil -- inherit, no override)", created.ModelID)
	}

	rig.assertLiveWorkflowStepRun(t, ctx, session.ID, created.ID, "review", builtInReviewDefinitionID)

	sessionRow, err := rig.sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	oldTurn := narvipg.Turn{Prompt: &inputPrompt, ModelID: nil, PlanMode: false}
	oldRaw, err := sessionactor.BuildPromptPayload(session.ID.String(), sessionRow, referenceSandboxRow, oldTurn)
	if err != nil {
		t.Fatalf("BuildPromptPayload (old/reference): %v", err)
	}
	newRaw, err := sessionactor.BuildPromptPayload(session.ID.String(), sessionRow, referenceSandboxRow, created)
	if err != nil {
		t.Fatalf("BuildPromptPayload (new/actual): %v", err)
	}

	assertIdenticalPrompts(t, oldRaw, newRaw, "review lane, zero-config")
}

// TestCharacterization_PlanLane_FirstTurn_ZeroConfig_IdenticalPromptJSON
// proves the SAME equivalence for the plan lane's own FIRST turn (mode =
// "plan", planMode=true on the turn itself) -- the built-in plan
// workflow's step 1 is ALSO ModelID nil/passthrough (§25.8), so this must
// be an identity transform too. This is deliberately scoped to the FIRST
// turn only: plan mode's transitional handling (§25.6/§25.8) leaves the
// SECOND turn -- created by decideplan.go's own DecidePlanOnTx, a
// completely separate insert this Step's engine wiring never touches --
// out of THIS proof's own scope, exactly as documented in
// internal/app/workflowengine/dispatch.go's own top comment: decideplan.go
// sets ModelID: sessionRow.BuildModelID directly, unaffected by
// createTurnLocked either way, so there is no equivalent "old vs new" pair
// to compare there in the first place -- that code path is simply
// unchanged by this Step, which this test's own sibling
// (TestCharacterization_PlanLane_ApprovalTurn_UnaffectedByEngine below)
// confirms directly.
func TestCharacterization_PlanLane_FirstTurn_ZeroConfig_IdenticalPromptJSON(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	if _, err := rig.sessions.UpdateIntentDecisionIfNull(ctx, session.ID,
		[]byte(`{"surface":"web","source":"explicit","target":"request","mode":"plan","decided_at":"2026-01-01T00:00:00Z","decided_at_stage":"create"}`)); err != nil {
		t.Fatalf("seed intent_decision: %v", err)
	}

	inputPrompt := "draft a plan for adding dark mode"

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, inputPrompt, nil, true, false, pgtype.UUID{}, RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}
	if !created.PlanMode {
		t.Fatal("created.PlanMode = false, want true")
	}

	if created.Prompt == nil || *created.Prompt != inputPrompt {
		t.Errorf("created.Prompt = %v, want %q (byte-for-byte unchanged)", created.Prompt, inputPrompt)
	}
	if created.ModelID != nil {
		t.Errorf("created.ModelID = %v, want nil (built-in plan step 1's own ModelID is nil -- inherit, no override)", created.ModelID)
	}

	rig.assertLiveWorkflowStepRun(t, ctx, session.ID, created.ID, "plan", builtInPlanDefinitionID)

	sessionRow, err := rig.sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	oldTurn := narvipg.Turn{Prompt: &inputPrompt, ModelID: nil, PlanMode: true}
	oldRaw, err := sessionactor.BuildPromptPayload(session.ID.String(), sessionRow, referenceSandboxRow, oldTurn)
	if err != nil {
		t.Fatalf("BuildPromptPayload (old/reference): %v", err)
	}
	newRaw, err := sessionactor.BuildPromptPayload(session.ID.String(), sessionRow, referenceSandboxRow, created)
	if err != nil {
		t.Fatalf("BuildPromptPayload (new/actual): %v", err)
	}

	assertIdenticalPrompts(t, oldRaw, newRaw, "plan lane, first turn, zero-config")
}

// TestCharacterization_PlanLane_ApprovalTurn_UnaffectedByEngine documents
// and proves the plan lane's own transitional-handling boundary directly
// (see the previous test's own doc comment): DecidePlanOnTx's approve path
// (decideplan.go) inserts the implementation turn via its OWN direct
// turns.Create call, completely bypassing createTurnLocked/CreateTurnCore
// -- so it is, and remains, entirely untouched by this Step's engine
// wiring. No workflow_step_runs row is ever attached to that turn.
func TestCharacterization_PlanLane_ApprovalTurn_UnaffectedByEngine(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	plan := rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	outbox := postgres.NewOutboxStore(rig.pool, false)
	linearAgentSessions := postgres.NewLinearAgentSessionStore(rig.pool)
	events := postgres.NewEventStore(rig.pool)
	planDocuments := postgres.NewPlanDocumentStore(rig.pool)
	outcome, err := DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, events, planDocuments, outbox, linearAgentSessions, rig.auditLog, rig.registry, session.ID, plan.ID, PlanVerdictApprove, pgtype.UUID{}, false)
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if !outcome.Won || outcome.TurnID == nil {
		t.Fatalf("outcome = %+v, want Won=true with a real TurnID", outcome)
	}

	var turnID pgtype.UUID
	if err := turnID.Scan(*outcome.TurnID); err != nil {
		t.Fatalf("parse outcome.TurnID: %v", err)
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE turn_id = $1`, turnID.String()).Scan(&count); err != nil {
		t.Fatalf("count workflow_step_runs for approval turn: %v", err)
	}
	if count != 0 {
		t.Errorf("workflow_step_runs count for decideplan.go's own approval turn = %d, want 0 (this Step's engine must never touch that code path)", count)
	}
}
