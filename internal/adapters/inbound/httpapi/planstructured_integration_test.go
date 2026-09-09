//go:build integration

// Integration tests for §12.2 item 3's own structured-plan-document schema
// (internal/domain/plan.ExtractStructured, plans.go's own planWireMap,
// decideplan.go's own snapshotApprovedPlanContent, and create.go/turn.go's
// own prompt-instruction injection). Unit-level coverage of the extraction
// and validation rules themselves lives in internal/domain/plan/
// structured_test.go; this file proves the wiring: a real content string,
// recovered from a real event log exactly like plans_integration_test.go's
// own tests, actually reaches restdtos.Plan.structured and
// plan_documents.structured_steps end to end.
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

const wellFormedPlanStepsContent = "Here is my plan.\n\n1. Add a table.\n\n" +
	"```plan-steps\n" +
	`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["migrations/000200.up.sql"]}],"scopeEstimate":"1 file"}` +
	"\n```\n"

// wellFormedTwoStepPlanStepsContent is wellFormedPlanStepsContent's own
// two-step twin, used only by the happy-path test below -- this Step's own
// review found that EVERY structured-plan wire fixture in this file (and in
// internal/domain/plan/structured_test.go's own multiple-steps case, which
// only proves the DOMAIN layer preserves order) carried exactly one step, so
// nothing at the WIRE level (planWireMap/planStructuredWireMap's own JSON
// marshaling of the []Step slice) ever proved step ORDER survives the trip
// through restdtos.PlanStep and back. Two distinctly-named, ORDER-SENSITIVE
// steps close that gap: a mutation that reordered (e.g. sorted by title) or
// mismatched fields across steps would flip which title/fileRef lands at
// index 0 vs 1 below.
const wellFormedTwoStepPlanStepsContent = "Here is my plan.\n\n1. Add a table.\n2. Wire it up.\n\n" +
	"```plan-steps\n" +
	`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["migrations/000200.up.sql"]},{"title":"Wire it up","description":"Consume the new table.","fileRefs":["internal/app/wireup.go","internal/app/wireup_test.go"]}],"scopeEstimate":"2 files"}` +
	"\n```\n"

// TestListPlans_StructuredField_PresentWhenContentCarriesAValidBlock proves
// the happy path end to end: a producing turn's own event-log text that
// contains a well-formed ```plan-steps block surfaces on the wire as a
// non-nil Plan.structured, AND Plan.content still carries the full,
// unmodified prose -- structured is additive, never a replacement. Also
// pins step ORDER at the wire level (see wellFormedTwoStepPlanStepsContent's
// own doc comment): step 0 and step 1 must land in the SAME order they were
// authored in, all the way through planWireMap/planStructuredWireMap's own
// JSON marshaling.
func TestListPlans_StructuredField_PresentWhenContentCarriesAValidBlock(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "structured-msg", wellFormedTwoStepPlanStepsContent)
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	got := resp.Plans[0]
	if got.Id != plan.ID.String() {
		t.Fatalf("Plans[0].Id = %q, want %q", got.Id, plan.ID.String())
	}
	if got.Content != wellFormedTwoStepPlanStepsContent {
		t.Errorf("Content = %q, want the full unmodified prose %q -- structured must never replace it", got.Content, wellFormedTwoStepPlanStepsContent)
	}
	if got.Structured == nil {
		t.Fatalf("Structured = nil, want a non-nil structured document")
	}
	if len(got.Structured.Steps) != 2 {
		t.Fatalf("len(Structured.Steps) = %d, want 2", len(got.Structured.Steps))
	}
	if want := "Add table"; got.Structured.Steps[0].Title != want {
		t.Errorf("Steps[0].Title = %q, want %q -- step order must survive the wire round-trip", got.Structured.Steps[0].Title, want)
	}
	if want := "New migration."; got.Structured.Steps[0].Description != want {
		t.Errorf("Steps[0].Description = %q, want %q", got.Structured.Steps[0].Description, want)
	}
	if want := []string{"migrations/000200.up.sql"}; len(got.Structured.Steps[0].FileRefs) != 1 || got.Structured.Steps[0].FileRefs[0] != want[0] {
		t.Errorf("Steps[0].FileRefs = %v, want %v", got.Structured.Steps[0].FileRefs, want)
	}
	if want := "Wire it up"; got.Structured.Steps[1].Title != want {
		t.Errorf("Steps[1].Title = %q, want %q -- step order must survive the wire round-trip", got.Structured.Steps[1].Title, want)
	}
	if want := "Consume the new table."; got.Structured.Steps[1].Description != want {
		t.Errorf("Steps[1].Description = %q, want %q", got.Structured.Steps[1].Description, want)
	}
	if want := []string{"internal/app/wireup.go", "internal/app/wireup_test.go"}; len(got.Structured.Steps[1].FileRefs) != 2 || got.Structured.Steps[1].FileRefs[0] != want[0] || got.Structured.Steps[1].FileRefs[1] != want[1] {
		t.Errorf("Steps[1].FileRefs = %v, want %v", got.Structured.Steps[1].FileRefs, want)
	}
	if want := "2 files"; got.Structured.ScopeEstimate != want {
		t.Errorf("Structured.ScopeEstimate = %q, want %q", got.Structured.ScopeEstimate, want)
	}
}

// TestListPlans_StructuredField_NilWhenContentHasNoBlock proves the most
// common real case: an ordinary plan-mode turn's own prose (no
// ```plan-steps block at all, e.g. every plan that predates this schema)
// yields Structured == nil, never a synthesized or partially-guessed
// value, while Content is unaffected.
func TestListPlans_StructuredField_NilWhenContentHasNoBlock(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	const prose = "1. Add a table.\n2. Wire it up.\n"
	seedTokenEvent(ctx, t, rig, session.ID, "prose-msg", prose)
	if _, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	if resp.Plans[0].Content != prose {
		t.Errorf("Content = %q, want %q", resp.Plans[0].Content, prose)
	}
	if resp.Plans[0].Structured != nil {
		t.Errorf("Structured = %+v, want nil for prose with no ```plan-steps block", resp.Plans[0].Structured)
	}
}

// TestListPlans_StructuredField_NilWhenBlockHasZeroSteps is this Step's own
// single most important correctness property, proven at the wire level
// (not merely in structured_test.go's own pure unit test): a
// ```plan-steps block with an explicit, well-formed, but EMPTY steps
// array must render IDENTICALLY to no block at all -- Structured == nil,
// never a distinguishable "real zero" the client could mistake for a
// confidently-computed empty plan.
func TestListPlans_StructuredField_NilWhenBlockHasZeroSteps(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	content := "```plan-steps\n" + `{"steps":[],"scopeEstimate":"0 files"}` + "\n```\n"
	seedTokenEvent(ctx, t, rig, session.ID, "zero-steps-msg", content)
	if _, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	if resp.Plans[0].Structured != nil {
		t.Errorf("Structured = %+v, want nil -- a zero-step block must never render as a distinguishable real zero", resp.Plans[0].Structured)
	}
	if resp.Plans[0].Content != content {
		t.Errorf("Content = %q, want the full unmodified prose %q", resp.Plans[0].Content, content)
	}
}

// TestApprovePlan_SnapshotsStructuredStepsIntoPlanDocuments extends
// plandocument_integration_test.go's own coverage measurement: an approved
// plan whose content carries a valid block gets its structured form
// durably persisted into plan_documents.structured_steps, in the SAME
// snapshot as content -- migrations/000126_plan_documents_structured.up.sql.
func TestApprovePlan_SnapshotsStructuredStepsIntoPlanDocuments(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "approve-structured-msg", wellFormedPlanStepsContent)
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create awaiting_approval plan: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v", plan.ID.String(), err)
	}
	if doc.StructuredSteps == nil {
		t.Fatalf("plan_documents.structured_steps = nil, want a non-nil JSON payload")
	}
	var got plandomain.Structured
	if err := json.Unmarshal(doc.StructuredSteps, &got); err != nil {
		t.Fatalf("unmarshal structured_steps: %v", err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Title != "Add table" {
		t.Errorf("structured_steps = %+v, want one step titled %q", got, "Add table")
	}
	if got.ScopeEstimate != "1 file" {
		t.Errorf("structured_steps.ScopeEstimate = %q, want %q", got.ScopeEstimate, "1 file")
	}
}

// TestApprovePlan_SnapshotsStructuredStepsIntoPlanDocuments_UsesTheApprovedPlansOwnTurn
// closes a real fixture gap this Step's own review found: every OTHER test
// in this file has exactly one session, one plan, one turn -- so
// planRow.TurnID (decideplan.go's own snapshotApprovedPlanContent) is
// trivially the only candidate turnContentBounds could ever resolve to, and
// a mutation that snapshots some OTHER turn instead (e.g. always the
// session's first-dispatched one) would leave every one of those tests
// green. This seeds TWO plan versions, each with its own producing turn and
// its own DISTINCT structured content, approves the SECOND, and asserts the
// snapshot carries v2's own steps -- never v1's.
func TestApprovePlan_SnapshotsStructuredStepsIntoPlanDocuments_UsesTheApprovedPlansOwnTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn1, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn1: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn1.ID)
	content1 := "v1's own plan.\n\n```plan-steps\n" +
		`{"steps":[{"title":"V1 STEP -- must never appear on plan v2's own snapshot","description":"D1","fileRefs":["v1.go"]}],"scopeEstimate":"v1 scope"}` +
		"\n```\n"
	seedTokenEvent(ctx, t, rig, session.ID, "turn1-structured-msg", content1)
	plan1, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn1.ID, Version: 1, Status: sqlcgen.PlanStatusSuperseded})
	if err != nil {
		t.Fatalf("create plan1: %v", err)
	}

	turn2, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn2: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn2.ID)
	content2 := "v2's own revised plan.\n\n```plan-steps\n" +
		`{"steps":[{"title":"V2 STEP -- the one that must be snapshotted","description":"D2","fileRefs":["v2.go"]}],"scopeEstimate":"v2 scope"}` +
		"\n```\n"
	seedTokenEvent(ctx, t, rig, session.ID, "turn2-structured-msg", content2)
	plan2, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn2.ID, Version: 2, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan2: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan2.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	// plan1 was never approved -- it must have no snapshot of its own at all,
	// which also rules out a mutation that snapshots EVERY plan in the
	// session rather than only the one actually being approved.
	if _, err := rig.planDocuments.GetByPlanID(ctx, plan1.ID); err == nil {
		t.Errorf("plan1 (never approved) has a plan_documents row -- want none")
	}

	doc, err := rig.planDocuments.GetByPlanID(ctx, plan2.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v", plan2.ID.String(), err)
	}
	if doc.StructuredSteps == nil {
		t.Fatalf("plan_documents.structured_steps = nil, want a non-nil JSON payload")
	}
	var got plandomain.Structured
	if err := json.Unmarshal(doc.StructuredSteps, &got); err != nil {
		t.Fatalf("unmarshal structured_steps: %v", err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Title != "V2 STEP -- the one that must be snapshotted" {
		t.Errorf("structured_steps = %+v, want v2's own single step, never v1's", got)
	}
	if got.ScopeEstimate != "v2 scope" {
		t.Errorf("structured_steps.ScopeEstimate = %q, want %q (v1's own scope leaking here means the wrong turn was snapshotted)", got.ScopeEstimate, "v2 scope")
	}
}

// TestApprovePlan_StructuredStepsNullWhenContentUnstructured is the
// previous test's own negative twin: an approved plan whose content never
// carried a valid block gets structured_steps == NULL, never an empty
// object or empty array standing in for "no structure" -- the same
// null-is-the-only-representation discipline the wire field itself
// enforces.
func TestApprovePlan_StructuredStepsNullWhenContentUnstructured(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "approve-unstructured-msg", "1. Add a table.\n2. Wire it up.\n")
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create awaiting_approval plan: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v", plan.ID.String(), err)
	}
	if doc.StructuredSteps != nil {
		t.Errorf("plan_documents.structured_steps = %s, want NULL for unstructured content", doc.StructuredSteps)
	}
}

// TestApprovePlan_NulInStructuredField_StillApproves is the end-to-end
// proof for the one failure that made a plan UNAPPROVABLE rather than
// merely rendered badly.
//
// U+0000 is valid JSON and a valid Go string, but Postgres jsonb refuses it
// with 22P05. structured_steps is written inside the SAME transaction that
// approves the plan, so a single NUL escape anywhere in a model-emitted
// title, description, fileRef or scope estimate rolled that transaction
// back -- and because the content is re-derived deterministically from the
// immutable event log on every attempt, retrying could never help: the plan
// stayed awaiting_approval forever, on the web button, the Slack button and
// the Linear reply alike, with no plan_documents row and no implementation
// turn. The content is model-authored text about a repository the model
// reads, so the byte is attacker-influenceable.
//
// ExtractStructured now folds a NUL-carrying block to nil, which is the same
// thing it does with every other deviation: the plan renders as the prose it
// always was, and it approves.
func TestApprovePlan_NulInStructuredField_StillApproves(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)

	// A fully compliant block whose title carries the (valid JSON) escape
	// for U+0000. Note the content itself holds no NUL byte -- only the
	// six-character escape text -- which is why it reached the TEXT content
	// column harmlessly before the jsonb column existed.
	content := "Here is my plan.\n\n```plan-steps\n" +
		`{"steps":[{"title":"Add` + `\u0000` + `table","description":"New migration.","fileRefs":["a.sql"]}],"scopeEstimate":"1 file"}` +
		"\n```\n"
	if strings.ContainsRune(content, 0) {
		t.Fatalf("fixture bug: the content must carry the ESCAPE, not a literal NUL byte")
	}
	seedTokenEvent(ctx, t, rig, session.ID, "approve-nul-msg", content)
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create awaiting_approval plan: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("approve status = %d, want %d -- a NUL in a structured field must never make a plan unapprovable", status, http.StatusOK)
	}

	got, err := rig.plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", plan.ID.String(), err)
	}
	if got.Status != sqlcgen.PlanStatusApproved {
		t.Errorf("plan status = %q, want %q", got.Status, sqlcgen.PlanStatusApproved)
	}

	doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v", plan.ID.String(), err)
	}
	if doc.StructuredSteps != nil {
		t.Errorf("plan_documents.structured_steps = %s, want NULL -- the block was rejected, so there is no structure to snapshot", doc.StructuredSteps)
	}
}

// TestCreateSession_PlanMode_FirstTurnCarriesStructureInstruction proves
// create.go's own CreateSessionOnTx injection site end to end (the OTHER
// of the two real places a plan_mode=true turn's prompt is assembled,
// turn.go's own CreateTurnCore being the one turncore_integration_test.go/
// workflowengine_characterization_integration_test.go already cover): a
// brand-new session created with planMode=true and a prompt gets that
// prompt's own stored turn prefixed with
// plandomain.RenderStructureInstruction's own fixed text, exactly like
// every other plan_mode=true turn.
func TestCreateSession_PlanMode_FirstTurnCarriesStructureInstruction(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": "plan mode session",
		"prompt": "draft a plan for adding dark mode",
		"repos": [{"name": "narvi", "url": "https://github.com/narvidev/narvi", "branch": null}],
		"modelId": null, "effort": null,
		"planMode": true
	}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	turns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1", len(turns))
	}
	wantPrompt := plandomain.MaybeInjectStructureInstruction(true, "draft a plan for adding dark mode")
	if turns[0].Prompt == nil || *turns[0].Prompt != wantPrompt {
		t.Errorf("turns[0].Prompt = %v, want %q", turns[0].Prompt, wantPrompt)
	}
	if !turns[0].PlanMode {
		t.Error("turns[0].PlanMode = false, want true")
	}
}
