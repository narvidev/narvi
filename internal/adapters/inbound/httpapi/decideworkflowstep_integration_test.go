//go:build integration

// Integration tests for §25.9's ("workflow HITL gate + circuit breaker",
// §25.9/§25.10/§25.11) own decide endpoint -- POST /api/workflow-runs/
// :runId/steps/:stepRunId/decide -- against a real Postgres instance,
// mirroring planapprove_integration_test.go's own house style exactly
// (createUserWithRole/createSessionForUser are that file's own helpers,
// reused here unchanged since both files share this package).
package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// workflowStepDecideResponseForTest mirrors restdtos.WorkflowStepDecideResponse's
// own wire shape with plain Go types -- the SAME "hand-copied test-local
// response struct" precedent planActionResponseForTest already establishes
// (planapprove_integration_test.go), so assertions never have to unwrap a
// named-pointer-type field.
type workflowStepDecideResponseForTest struct {
	StepRunID     string  `json:"stepRunId"`
	StepRunStatus string  `json:"stepRunStatus"`
	RunStatus     string  `json:"runStatus"`
	TurnID        *string `json:"turnId"`
}

// seedAwaitingDecisionRun seeds a custom, unbound workflow_definitions row
// with numSteps ordered steps (step 1 HITLAfter=true with pure default
// routing -- no edges), a workflow_runs row pinned to it, and step 1's own
// attempt already parked awaiting_decision carrying outcomeStatus --
// mirrors seedAwaitingApprovalPlan's own "surgical direct DB seed"
// precedent (planapprove_integration_test.go): these tests exist to prove
// DecideWorkflowStep's OWN behavior reacting to an existing awaiting_decision
// row, not OnTurnCompleted's own HITLAfter-detection logic (already covered
// by internal/app/workflowengine's own completion_integration_test.go).
func seedAwaitingDecisionRun(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, numSteps int, outcomeStatus string) (runID, stepRunID pgtype.UUID, stepIDs []pgtype.UUID) {
	t.Helper()

	var defID pgtype.UUID
	if err := r.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`,
		fmt.Sprintf("test-decide-%d", time.Now().UnixNano())).Scan(&defID); err != nil {
		t.Fatalf("insert test workflow_definitions row: %v", err)
	}

	stepIDs = make([]pgtype.UUID, numSteps)
	for i := 0; i < numSteps; i++ {
		hitlAfter := i == 0
		if err := r.pool.QueryRow(ctx,
			`INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, hitl_after) VALUES ($1, $2, 'agent', '{{prompt}}', $3) RETURNING id`,
			defID, i+1, hitlAfter,
		).Scan(&stepIDs[i]); err != nil {
			t.Fatalf("insert test step definition %d: %v", i, err)
		}
	}

	run, err := r.workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create test run: %v", err)
	}
	stepRun, err := r.workflows.CreateStepRun(ctx, run.ID, stepIDs[0])
	if err != nil {
		t.Fatalf("create test step run: %v", err)
	}
	if _, err := r.workflows.MarkAwaitingDecision(ctx, stepRun.ID, outcomeStatus); err != nil {
		t.Fatalf("mark test step run awaiting decision: %v", err)
	}

	return run.ID, stepRun.ID, stepIDs
}

func decidePath(runID, stepRunID pgtype.UUID) string {
	return "/api/workflow-runs/" + runID.String() + "/steps/" + stepRunID.String() + "/decide"
}

// --- 404s ---

func TestDecideWorkflowStep_UnknownRun_Returns404(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	_, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	var unknownRunID pgtype.UUID
	_ = unknownRunID.Scan("00000000-0000-0000-0000-000000000000")

	status := rig.doJSON(t, http.MethodPost, decidePath(unknownRunID, stepRunID), []byte(`{"verdict":"approve","text":null}`), nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestDecideWorkflowStep_UnknownStepRun_Returns404(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, _, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	var unknownStepRunID pgtype.UUID
	_ = unknownStepRunID.Scan("00000000-0000-0000-0000-000000000000")

	status := rig.doJSON(t, http.MethodPost, decidePath(runID, unknownStepRunID), []byte(`{"verdict":"approve","text":null}`), nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestDecideWorkflowStep_StepRunBelongsToDifferentRun_Returns404 proves the
// defensive cross-run mismatch check (mirrors DecidePlanOnTx's own
// identical "planRow.SessionID != sessionRow.ID" guard): a genuinely
// existing stepRunId, addressed via a DIFFERENT run's own URL, must never
// leak its real status.
func TestDecideWorkflowStep_StepRunBelongsToDifferentRun_Returns404(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session1 := createSessionForUser(ctx, t, rig, owner.ID, nil)
	session2 := createSessionForUser(ctx, t, rig, owner.ID, nil)
	_, stepRunID1, _ := seedAwaitingDecisionRun(ctx, t, rig, session1.ID, 1, "ok")
	runID2, _, _ := seedAwaitingDecisionRun(ctx, t, rig, session2.ID, 1, "ok")

	status := rig.doJSON(t, http.MethodPost, decidePath(runID2, stepRunID1), []byte(`{"verdict":"approve","text":null}`), nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// --- Request validation ---

func TestDecideWorkflowStep_MalformedBody_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"not-a-real-verdict","text":null}`), nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestDecideWorkflowStep_Revise_EmptyText_Returns400 proves the schema's own
// documented requirement ("required non-empty for verdict 'revise'"):
// neither an absent/null text nor whitespace-only text is accepted.
func TestDecideWorkflowStep_Revise_EmptyText_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"null text", `{"verdict":"revise","text":null}`},
		{"whitespace-only text", `{"verdict":"revise","text":"   "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh session per subtest: workflow_runs_one_running_per_session
			// would otherwise reject this subtest's own seeded run while the
			// PRIOR subtest's run (never advanced -- a 400 must never change
			// state) is still 'running' on the SAME session.
			session := createSessionForUser(ctx, t, rig, owner.ID, nil)
			runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")
			status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(tc.body), nil, token)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
			}
		})
	}
}

// --- Happy paths ---

// TestDecideWorkflowStep_Approve_SingleStep_CompletesRun proves the
// approve verdict's own "continue: consult NextStep with the step's
// outcome now that a human has approved" contract for the simplest case:
// a single-step definition's own last step has nowhere further to advance,
// so NextStep resolves NextComplete.
func TestDecideWorkflowStep_Approve_SingleStep_CompletesRun(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	var got workflowStepDecideResponseForTest
	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.StepRunStatus != "completed" {
		t.Errorf("stepRunStatus = %q, want completed", got.StepRunStatus)
	}
	if got.RunStatus != "completed" {
		t.Errorf("runStatus = %q, want completed (single-step definition, nowhere further to advance)", got.RunStatus)
	}
	if got.TurnID != nil {
		t.Errorf("turnId = %v, want nil (NextComplete dispatches nothing)", got.TurnID)
	}

	var dbRunStatus string
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1`, runID.String()).Scan(&dbRunStatus); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if dbRunStatus != "completed" {
		t.Errorf("db run status = %q, want completed", dbRunStatus)
	}

	var decision, decidedBy string
	if err := rig.pool.QueryRow(ctx, `SELECT decision, decided_by::text FROM workflow_step_runs WHERE id = $1`, stepRunID.String()).Scan(&decision, &decidedBy); err != nil {
		t.Fatalf("query step run: %v", err)
	}
	if decision != "approve" {
		t.Errorf("decision = %q, want approve", decision)
	}
	if decidedBy != owner.ID.String() {
		t.Errorf("decided_by = %q, want %q", decidedBy, owner.ID.String())
	}
}

// TestDecideWorkflowStep_Approve_MultiStep_AdvancesAndDispatchesNewTurn
// proves the same verdict's OTHER consequence: a two-step definition's own
// step 1 (HITLAfter, no explicit edge) approved with an 'ok' outcome
// advances to step 2 by Order -- and a REAL new turn is dispatched for it
// (§25.9 closing §25.6's own documented auto-dispatch gap), not just a
// bookkeeping row.
func TestDecideWorkflowStep_Approve_MultiStep_AdvancesAndDispatchesNewTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, stepIDs := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 2, "ok")

	var got workflowStepDecideResponseForTest
	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.StepRunStatus != "completed" {
		t.Errorf("stepRunStatus = %q, want completed", got.StepRunStatus)
	}
	if got.RunStatus != "running" {
		t.Errorf("runStatus = %q, want running (advanced, not finished)", got.RunStatus)
	}
	if got.TurnID == nil || *got.TurnID == "" {
		t.Fatal("turnId is nil/empty, want the newly dispatched turn's id")
	}

	liveStepRun, err := rig.workflows.GetLiveStepRunForRun(ctx, runID)
	if err != nil {
		t.Fatalf("get live step run: %v", err)
	}
	if liveStepRun.StepDefinitionID != stepIDs[1] {
		t.Errorf("live step definition id = %s, want step 2 %s", liveStepRun.StepDefinitionID, stepIDs[1])
	}
	if !liveStepRun.TurnID.Valid || liveStepRun.TurnID.String() != *got.TurnID {
		t.Errorf("live step run turn_id = %v, want %s", liveStepRun.TurnID, *got.TurnID)
	}

	newTurn, err := rig.turns.Get(ctx, liveStepRun.TurnID)
	if err != nil {
		t.Fatalf("get new turn: %v", err)
	}
	if newTurn.Status != sqlcgen.TurnStatusPending {
		t.Errorf("new turn status = %q, want pending", newTurn.Status)
	}
}

// TestDecideWorkflowStep_Reject_FailsRun proves the reject verdict ends the
// run (WorkflowRun.Status = failed, per WorkflowStepDecideResponse.runStatus's
// own documented example) and dispatches nothing.
func TestDecideWorkflowStep_Reject_FailsRun(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 2, "ok")

	var got workflowStepDecideResponseForTest
	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"reject","text":"not good enough"}`), &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.StepRunStatus != "failed" {
		t.Errorf("stepRunStatus = %q, want failed", got.StepRunStatus)
	}
	if got.RunStatus != "failed" {
		t.Errorf("runStatus = %q, want failed", got.RunStatus)
	}
	if got.TurnID != nil {
		t.Errorf("turnId = %v, want nil (reject dispatches nothing)", got.TurnID)
	}

	var dbRunStatus string
	var finishedAt *time.Time
	if err := rig.pool.QueryRow(ctx, `SELECT status, finished_at FROM workflow_runs WHERE id = $1`, runID.String()).Scan(&dbRunStatus, &finishedAt); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if dbRunStatus != "failed" {
		t.Errorf("db run status = %q, want failed", dbRunStatus)
	}
	if finishedAt == nil {
		t.Error("run finished_at is NULL, want set (reject is terminal)")
	}

	var decisionText *string
	if err := rig.pool.QueryRow(ctx, `SELECT decision_text FROM workflow_step_runs WHERE id = $1`, stepRunID.String()).Scan(&decisionText); err != nil {
		t.Fatalf("query step run: %v", err)
	}
	if decisionText == nil || *decisionText != "not good enough" {
		t.Errorf("decision_text = %v, want the reject reason", decisionText)
	}
}

// TestDecideWorkflowStep_Revise_ReExecutesSameStep_WithFeedbackAsPrompt
// proves the revise verdict's own mechanism (§25.9, mirroring plan mode's
// own "prompt = feedback" mechanism): a NEW attempt of the SAME step is
// created and dispatched, its own turn's prompt is the feedback text
// verbatim (rendered through the built-in "{{prompt}}" passthrough
// template), and the run never advances past step 1 at all.
func TestDecideWorkflowStep_Revise_ReExecutesSameStep_WithFeedbackAsPrompt(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, stepIDs := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 2, "ok")

	var got workflowStepDecideResponseForTest
	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"revise","text":"please add more detail"}`), &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.StepRunStatus != "completed" {
		t.Errorf("stepRunStatus = %q, want completed", got.StepRunStatus)
	}
	if got.RunStatus != "running" {
		t.Errorf("runStatus = %q, want running", got.RunStatus)
	}
	if got.TurnID == nil || *got.TurnID == "" {
		t.Fatal("turnId is nil/empty, want the newly dispatched revision turn's id")
	}

	liveStepRun, err := rig.workflows.GetLiveStepRunForRun(ctx, runID)
	if err != nil {
		t.Fatalf("get live step run: %v", err)
	}
	if liveStepRun.StepDefinitionID != stepIDs[0] {
		t.Errorf("live step definition id = %s, want the SAME step 1 %s (revise never advances)", liveStepRun.StepDefinitionID, stepIDs[0])
	}

	newTurn, err := rig.turns.Get(ctx, liveStepRun.TurnID)
	if err != nil {
		t.Fatalf("get new turn: %v", err)
	}
	if newTurn.Prompt == nil || *newTurn.Prompt != "please add more detail" {
		t.Errorf("new turn prompt = %v, want the feedback text verbatim", newTurn.Prompt)
	}

	attempts, err := rig.workflows.CountStepRunsForStepDefinition(ctx, runID, stepIDs[0])
	if err != nil {
		t.Fatalf("CountStepRunsForStepDefinition: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempt count for step 1 = %d, want 2 (the original + the revise re-execution)", attempts)
	}
}

// --- Idempotency / re-submission guard ---

// TestDecideWorkflowStep_AlreadyDecided_Returns409 proves the guarded
// UPDATE's own idempotency contract: deciding the SAME step-run twice
// returns 200 then 409, never a second state change.
func TestDecideWorkflowStep_AlreadyDecided_Returns409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	status1 := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), nil, token)
	if status1 != http.StatusOK {
		t.Fatalf("first decide: status = %d, want %d", status1, http.StatusOK)
	}

	status2 := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), nil, token)
	if status2 != http.StatusConflict {
		t.Errorf("second decide: status = %d, want %d", status2, http.StatusConflict)
	}
	status3 := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"reject","text":null}`), nil, token)
	if status3 != http.StatusConflict {
		t.Errorf("third decide (different verdict): status = %d, want %d", status3, http.StatusConflict)
	}
}

// TestDecideWorkflowStep_ConcurrentDoubleDecide_ExactlyOneWins mirrors
// TestApprovePlan_ConcurrentDoubleApprove_ExactlyOneWins's own errgroup-based
// concurrency pattern: firing two concurrent approve requests at the SAME
// awaiting_decision step-run must produce exactly one 200 and one 409.
func TestDecideWorkflowStep_ConcurrentDoubleDecide_ExactlyOneWins(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	path := decidePath(runID, stepRunID)
	body := []byte(`{"verdict":"approve","text":null}`)

	var eg errgroup.Group
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		eg.Go(func() error {
			statuses <- rig.doJSON(t, http.MethodPost, path, body, nil, token)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(statuses)

	var okCount, conflictCount int
	for s := range statuses {
		switch s {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Errorf("okCount=%d conflictCount=%d, want exactly one of each", okCount, conflictCount)
	}
}

// --- RBAC (§25.11: same matrix row as ActionApprovePlan) ---

func TestDecideWorkflowStep_NonOwnerNonParticipantMember_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, outsiderToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), nil, outsiderToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}

	var dbStatus string
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM workflow_step_runs WHERE id = $1`, stepRunID.String()).Scan(&dbStatus); err != nil {
		t.Fatalf("query step run: %v", err)
	}
	if dbStatus != "awaiting_decision" {
		t.Errorf("db status = %q, want awaiting_decision (a 403 must never change state)", dbStatus)
	}
}

func TestDecideWorkflowStep_Maintainer_NotOwnerOrParticipant_Allowed(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), nil, maintainerToken)
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d (maintainer may decide any workflow step)", status, http.StatusOK)
	}
}

func TestDecideWorkflowStep_Viewer_NotOwnerOrParticipant_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, viewerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleViewer)

	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 1, "ok")

	status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), nil, viewerToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestFreeze_WorkflowDecisionApproveDispatches pins that the autonomy
// freeze (technical plan §40.2) never stops a person: while autonomy is
// frozen, a person's approval of a step awaiting their decision advances
// the run and dispatches its next attempt's turn, exactly as when nothing
// is frozen, and holds nothing.
func TestFreeze_WorkflowDecisionApproveDispatches(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	settings := narvipg.NewPlatformSettingsStore(rig.pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	runID, stepRunID, stepIDs := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 2, "ok")

	var got workflowStepDecideResponseForTest
	if status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(`{"verdict":"approve","text":null}`), &got, token); status != http.StatusOK {
		t.Fatalf("status while frozen = %d, want %d", status, http.StatusOK)
	}
	if got.RunStatus != "running" || got.TurnID == nil || *got.TurnID == "" {
		t.Fatalf("response while frozen = run %q, turn %v; want the run advanced and the next attempt's turn dispatched", got.RunStatus, got.TurnID)
	}
	live, err := rig.workflows.GetLiveStepRunForRun(ctx, runID)
	if err != nil {
		t.Fatalf("get live step run: %v", err)
	}
	if live.StepDefinitionID != stepIDs[1] || !live.TurnID.Valid || live.TurnID.String() != *got.TurnID {
		t.Fatalf("live step run = step %v, turn %v; want step 2 with turn %s", live.StepDefinitionID, live.TurnID, *got.TurnID)
	}
	if _, err := rig.workflows.GetAdvanceHold(ctx, runID); err == nil {
		t.Fatal("the run holds an advance after a person's approval: a person's decision is never held")
	}
}
