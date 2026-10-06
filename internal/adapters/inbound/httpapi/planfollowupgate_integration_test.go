//go:build integration

// Integration tests for §23's own plan_followup classification block
// inside createTurnLocked (turn.go, §23.1/§23.2/§23.3) -- the classify-
// then-consult sequencing that replaces the interim awaiting-plan gate's
// unconditional "always decline" for the unprefixed (planMode == false)
// case. Lives in package httpapi (not httpapi_test), exactly like
// turncore_integration_test.go, since createTurnLocked itself is
// unexported and this file drives it via the exported CreateTurnCore
// wrapper against the SAME turnCoreTestRig.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
)

// fakePlanFollowupLLM is a minimal ports.LLM fake -- this package has no
// pre-existing one (confirmed by search), so this mirrors
// internal/app/intentclassifier/classifier_test.go's own fakeLLM shape
// (unreachable from here: unexported, different package) rather than
// reusing it directly. response/err are mutually exclusive, exactly like
// that fake; calls counts every Complete invocation so a test can assert
// the classifier was (or, tellingly, was NOT) actually called.
type fakePlanFollowupLLM struct {
	response json.RawMessage
	err      error
	calls    int
}

func (f *fakePlanFollowupLLM) Complete(_ context.Context, _ ports.CompletionRequest) (ports.CompletionResponse, error) {
	f.calls++
	if f.err != nil {
		return ports.CompletionResponse{}, f.err
	}
	return ports.CompletionResponse{Raw: f.response}, nil
}

// planFollowupResponse builds a fakePlanFollowupLLM.response payload
// shaped like the real plan_followup structured-output schema
// (schema_planfollowup.go's own planFollowupStructuredOutput) -- a plain
// map literal, since that type is unexported in a different package.
func planFollowupResponse(target, confidence string) json.RawMessage {
	raw, err := json.Marshal(map[string]string{
		"target":     target,
		"confidence": confidence,
		"reasoning":  "test fixture",
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// Each test below builds its own *intentclassifier.Service inline via
// intentclassifier.New(llm, "anthropic", "claude-haiku-4-5",
// narvipg.NewPromptTemplateStore(rig.pool), nil, nil) -- a REAL,
// pool-scoped postgres.PromptTemplateStore so GetTemplate reads the ACTUAL
// seeded row migrations/000074_plan_followup.up.sql inserts, proving
// §23's own template wiring end to end, not just the fake LLM response
// mapping; only the LLM call itself is faked. provider/model strings are
// never actually sent anywhere (llm is a fake), so any non-empty values
// do; sessions (DecisionStore) is nil-safe and unused by
// ClassifyPlanFollowup.

// --- actual tests ---

// TestCreateTurnCore_PlanFollowup_ConfidentAmend_PromotesToRevisionTurn
// proves §23's own central claim: a confident "amend" classification
// unblocks dispatch and promotes the turn to a REAL plan-revision turn
// (plan_mode=true), exactly like a revise:-prefixed reply already is --
// answer_only persists false (the classifier's positive signal), never
// true (a true verdict, by construction, never reaches a persisted row --
// see migrations/000074's own doc comment).
func TestCreateTurnCore_PlanFollowup_ConfidentAmend_PromotesToRevisionTurn(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	llm := &fakePlanFollowupLLM{response: planFollowupResponse(intentdomain.TargetAmend, intentdomain.ConfidenceHigh)}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "actually let's do it differently", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	if cerr != nil {
		t.Fatalf("cerr = %+v, want nil (a confident amend verdict must unblock dispatch)", cerr)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}
	if !created.PlanMode {
		t.Error("created.PlanMode = false, want true -- a confident amend verdict must promote this turn to a real plan-revision turn")
	}
	if created.AnswerOnly == nil {
		t.Fatal("created.AnswerOnly = nil, want a pointer to false")
	}
	if *created.AnswerOnly {
		t.Error("created.AnswerOnly = true, want false (the classifier's positive amend signal)")
	}
	if llm.calls != 1 {
		t.Errorf("llm.calls = %d, want 1 (the classifier must actually have been invoked)", llm.calls)
	}
}

// TestCreateTurnCore_PlanFollowup_ConfidentAnswer_StillBlocked proves a
// confident "answer" classification still declines exactly like the
// pre-existing gate always did: 409, ErrPlanAwaitingApproval, no turn row
// inserted at all.
func TestCreateTurnCore_PlanFollowup_ConfidentAnswer_StillBlocked(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	llm := &fakePlanFollowupLLM{response: planFollowupResponse(intentdomain.TargetAnswer, intentdomain.ConfidenceHigh)}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "yes, the staging one", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	assertAwaitingApprovalDecline(t, wasCreated, cerr)
	if llm.calls != 1 {
		t.Errorf("llm.calls = %d, want 1 (the classifier must actually have been invoked)", llm.calls)
	}
}

// TestCreateTurnCore_PlanFollowup_LowConfidence_FailsOpenToBlocked proves
// §23.3's own fail-open floor for the "classifier ran but was unconfident"
// case -- a low-confidence "amend" reading must still decline, never
// dispatch, exactly like a genuine "answer" would.
func TestCreateTurnCore_PlanFollowup_LowConfidence_FailsOpenToBlocked(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	llm := &fakePlanFollowupLLM{response: planFollowupResponse(intentdomain.TargetAmend, intentdomain.ConfidenceLow)}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "maybe? not sure", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	assertAwaitingApprovalDecline(t, wasCreated, cerr)
}

// TestCreateTurnCore_PlanFollowup_ClassifierError_FailsOpenToBlocked
// proves §23.3's own literal words: "A classifier failure must never let a
// build turn dispatch against an unapproved plan, under any failure
// mode." A raw LLM error must still decline, exactly like every other
// failure mode.
func TestCreateTurnCore_PlanFollowup_ClassifierError_FailsOpenToBlocked(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	llm := &fakePlanFollowupLLM{err: &ports.LLMError{Code: ports.CodeAPIError, Provider: "anthropic"}}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "let's change the approach", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	assertAwaitingApprovalDecline(t, wasCreated, cerr)
}

// TestCreateTurnCore_PlanFollowup_NilClassifier_FailsOpenToBlocked proves
// a nil intentSvc (never true in production wiring, but every pre-existing
// test in this package passes it) degrades to EXACTLY the pre-existing
// "always decline" behavior -- never a panic, never a silent dispatch.
func TestCreateTurnCore_PlanFollowup_NilClassifier_FailsOpenToBlocked(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "let's change the approach", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	assertAwaitingApprovalDecline(t, wasCreated, cerr)
}

// TestCreateTurnCore_PlanFollowup_PlanModeTrue_NeverClassifies proves the
// revise: prefix (and any other planMode==true caller, e.g. Slack's
// Request-changes modal) stays a deterministic override that bypasses
// classification ENTIRELY (§23 intro) -- the classifier must never even be
// called when planMode is already true, regardless of whether a plan is
// awaiting_approval.
func TestCreateTurnCore_PlanFollowup_PlanModeTrue_NeverClassifies(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	llm := &fakePlanFollowupLLM{response: planFollowupResponse(intentdomain.TargetAnswer, intentdomain.ConfidenceHigh)}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "revise: drop the retry", nil, true, false, pgtype.UUID{}, RejectIfOpen)

	if cerr != nil {
		t.Fatalf("cerr = %+v, want nil (planMode=true must always bypass the awaiting-plan gate)", cerr)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}
	if llm.calls != 0 {
		t.Errorf("llm.calls = %d, want 0 -- planMode=true must bypass classification entirely, never call the classifier at all", llm.calls)
	}
	if created.AnswerOnly != nil {
		t.Errorf("created.AnswerOnly = %v, want nil -- classification never ran for this turn", *created.AnswerOnly)
	}
}

// TestCreateTurnCore_PlanFollowup_NoAwaitingPlan_NeverClassifies proves
// the classifier is truly gated on "a plan exists and is
// awaiting_approval" (§23.1) -- an ordinary turn on a session with NO
// awaiting-approval plan at all must never invoke the classifier, since
// there is nothing here for it to classify against.
func TestCreateTurnCore_PlanFollowup_NoAwaitingPlan_NeverClassifies(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	// Deliberately no seedAwaitingApprovalPlan call -- this session has no
	// plan at all.

	llm := &fakePlanFollowupLLM{response: planFollowupResponse(intentdomain.TargetAnswer, intentdomain.ConfidenceHigh)}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "an ordinary message", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	if cerr != nil {
		t.Fatalf("cerr = %+v, want nil", cerr)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}
	if llm.calls != 0 {
		t.Errorf("llm.calls = %d, want 0 -- no awaiting_approval plan exists, the classifier must never be invoked (§23.1: 'never invoked outside that state')", llm.calls)
	}
	if created.AnswerOnly != nil {
		t.Error("created.AnswerOnly should be nil when no awaiting_approval plan exists")
	}
}

// assertAwaitingApprovalDecline is the shared assertion every "must still
// decline" test above needs -- mirrors the exact 409/ErrPlanAwaitingApproval
// shape TestCreateTurnCore_OpenTurnDuringAwaitingApproval_BusyWins (this
// package) already asserts for the pre-existing gate, one level up: no
// turn created, the SAME sentinel/status/message every pre-existing caller
// already recognizes.
func assertAwaitingApprovalDecline(t *testing.T, wasCreated bool, cerr *CreateTurnError) {
	t.Helper()
	if wasCreated {
		t.Error("wasCreated = true, want false")
	}
	if cerr == nil {
		t.Fatal("cerr = nil, want a 409 CreateTurnError")
	}
	if cerr.Status != http.StatusConflict {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusConflict)
	}
	if cerr.Message != planAwaitingApprovalMessage {
		t.Errorf("cerr.Message = %q, want %q", cerr.Message, planAwaitingApprovalMessage)
	}
}
