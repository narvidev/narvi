//go:build integration

// Integration tests for the Request-changes modal's answer when the
// revision it submits is not created (technical plan §43.21(b)): while
// any turn of the session is pending, dispatched or processing,
// CreateTurnCore refuses the revision with RejectIfOpen's 409, and the
// modal must stay open with the feedback still in it and say a turn is
// running; any other failure of the create shows the generic error. Both
// answer with Slack's response_action "errors", keyed on the modal's own
// input block. Nothing is queued, and once the session is idle the same
// submission creates the revision. Runs against the real Postgres and the
// real handler, reusing interactive_integration_test.go's rig.
package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/turnguard"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// The two texts the modal shows when the revision is not created. The
// generic one is the text the handler already shows when the authorization
// check itself fails (authz_backend_error_integration_test.go pins it on
// that path).
const (
	wantRequestChangesBusyText  = "A turn is still running on this session, so this change wasn't submitted or queued. Submit it again once that turn ends."
	wantRequestChangesErrorText = "Something went wrong submitting this. Please try again."
)

// seedApprovedImplementation seeds a session whose plan v1 was approved:
// the completed plan-mode turn that wrote it, the approved plan row, and
// the implementation turn the approval queued, left at implStatus.
func seedApprovedImplementation(ctx context.Context, t *testing.T, rig *interactiveTestRig, implStatus sqlcgen.TurnStatus) (sqlcgen.Session, sqlcgen.Plan, sqlcgen.Turn) {
	t.Helper()
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	planTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed plan-mode turn: %v", err)
	}
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: planTurn.ID, Version: 1, Status: sqlcgen.PlanStatusApproved})
	if err != nil {
		t.Fatalf("seed approved plan: %v", err)
	}
	impl, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: implStatus, PlanMode: false})
	if err != nil {
		t.Fatalf("seed implementation turn: %v", err)
	}
	return session, plan, impl
}

// requestChangesModal is what the handler's views.open call rendered: the
// modal's private_metadata and the ids of its one input block.
type requestChangesModal struct {
	privateMetadata, blockID, actionID string
}

// openRequestChangesModal clicks Request changes on plan through the real
// block_actions path and reads the modal back from the views.open request
// the fake Slack server recorded, so a submission is built from, and an
// inline error is looked up under, the input block the modal actually
// renders rather than a constant the test assumes.
func openRequestChangesModal(t *testing.T, rig *interactiveTestRig, session sqlcgen.Session, plan sqlcgen.Plan) requestChangesModal {
	t.Helper()
	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	rec := httptest.NewRecorder()
	rig.handler(rec, signedInteractivityRequest(t, blockActionsPayloadJSON(slackapi.ActionRequestChangesPlan, value, "C1", "1700000000.000301", "trigger-request-changes")))
	if rec.Code != http.StatusOK {
		t.Fatalf("request_changes_plan click: status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	opened := awaitRequestWithPath(t, rig.requests, "/views.open")
	view, _ := opened.body["view"].(map[string]any)
	blocks, _ := view["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("views.open rendered %d blocks, want the one feedback input: %v", len(blocks), view)
	}
	block, _ := blocks[0].(map[string]any)
	element, _ := block["element"].(map[string]any)
	modal := requestChangesModal{}
	modal.privateMetadata, _ = view["private_metadata"].(string)
	modal.blockID, _ = block["block_id"].(string)
	modal.actionID, _ = element["action_id"].(string)
	if block["type"] != "input" || modal.privateMetadata == "" || modal.blockID == "" || modal.actionID == "" {
		t.Fatalf("views.open rendered no usable input block: %v", view)
	}
	return modal
}

// submitRequestChanges submits feedback through modal as Slack would, from
// the pre-linked maintainer every rig request is attributed to.
func submitRequestChanges(t *testing.T, rig *interactiveTestRig, modal requestChangesModal, feedback string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]string{"id": interactivityDefaultUserID},
		"view": map[string]any{
			"callback_id":      slackapi.RequestChangesCallbackID,
			"private_metadata": modal.privateMetadata,
			"state": map[string]any{
				"values": map[string]any{
					modal.blockID: map[string]any{
						modal.actionID: map[string]any{"type": "plain_text_input", "value": feedback},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal view_submission payload: %v", err)
	}
	rec := httptest.NewRecorder()
	rig.handler(rec, signedInteractivityRequest(t, string(raw)))
	return rec
}

// modalErrors decodes a response_action "errors" answer and returns its
// errors map, failing the test on any other answer -- a bare 200 closes
// the modal as accepted. The answer must be declared JSON: Slack reads a
// view_submission's response_action from an application/json body, and a
// body written without the header would reach it as text/plain.
func modalErrors(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want %q (body=%s)", got, "application/json", rec.Body.String())
	}
	var resp struct {
		ResponseAction string            `json:"response_action"`
		Errors         map[string]string `json:"errors"`
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty 200: Slack closes the modal as accepted and the feedback is lost, want a response_action \"errors\" answer")
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rec.Body.String())
	}
	if resp.ResponseAction != "errors" {
		t.Fatalf("response_action = %q, want \"errors\" (body=%s)", resp.ResponseAction, rec.Body.String())
	}
	return resp.Errors
}

// turnCreateAudits counts the turn.create audit rows written for session.
func turnCreateAudits(ctx context.Context, t *testing.T, rig *interactiveTestRig, session sqlcgen.Session) int {
	t.Helper()
	var n int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'turn.create' AND detail_json->>'session_id' = $1`, session.ID.String()).Scan(&n); err != nil {
		t.Fatalf("count turn.create audit rows: %v", err)
	}
	return n
}

// TestInteractivityHandler_ViewSubmission_RefusedCreateKeepsTheModalOpen:
// a Request-changes submission on a session whose approved implementation
// is pending, dispatched or processing is refused by CreateTurnCore
// (RejectIfOpen), and the modal gets a response_action "errors" answer
// under its feedback block saying a turn is running -- no turn inserted,
// none audited, the implementation and the approved plan untouched. Once
// the implementation has completed, the same submission creates the
// revision and closes the modal, as before. A create that fails on the
// server (a closed pool behind its transaction, authorization having
// passed on the real stores) shows the generic error instead of closing
// the modal.
func TestInteractivityHandler_ViewSubmission_RefusedCreateKeepsTheModalOpen(t *testing.T) {
	const feedback = "keep the env fallback, and split the migration in two"

	tests := []struct {
		name        string
		implStatus  sqlcgen.TurnStatus
		breakCreate bool
		wantText    string
	}{
		{name: "implementation pending", implStatus: sqlcgen.TurnStatusPending, wantText: wantRequestChangesBusyText},
		{name: "implementation dispatched", implStatus: sqlcgen.TurnStatusDispatched, wantText: wantRequestChangesBusyText},
		{name: "implementation processing", implStatus: sqlcgen.TurnStatusProcessing, wantText: wantRequestChangesBusyText},
		{name: "create fails on the server", implStatus: sqlcgen.TurnStatusCompleted, breakCreate: true, wantText: wantRequestChangesErrorText},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)

			var mutate func(*slack.InteractiveDeps)
			if tc.breakCreate {
				// A SEPARATE pool on the same database, closed at once:
				// CreateTurnCore's own transaction (deps.Pool) cannot
				// begin, while authorization and every other read run on
				// the real stores -- the same technique
				// authz_backend_error_integration_test.go uses for the
				// authorization check.
				brokenPool, err := narvipg.NewPool(ctx, pool.Config().ConnString())
				if err != nil {
					t.Fatalf("new broken pool: %v", err)
				}
				brokenPool.Close()
				mutate = func(d *slack.InteractiveDeps) { d.Pool = brokenPool }
			}
			rig := newInteractiveTestRigWithDeps(t, pool, platform.DefaultTimeouts(), mutate)

			session, plan, impl := seedApprovedImplementation(ctx, t, rig, tc.implStatus)
			modal := openRequestChangesModal(t, rig, session, plan)

			got := modalErrors(t, submitRequestChanges(t, rig, modal, feedback))
			if want := map[string]string{modal.blockID: tc.wantText}; !maps.Equal(got, want) {
				t.Fatalf("errors = %v, want %v (one message, under the modal's feedback block %q)", got, want, modal.blockID)
			}

			turns, err := rig.turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}
			if len(turns) != 2 {
				t.Fatalf("len(turns) = %d after the refused submission, want the 2 seeded turns (nothing queued)", len(turns))
			}
			if n := turnCreateAudits(ctx, t, rig, session); n != 0 {
				t.Errorf("turn.create audit rows = %d after the refused submission, want 0", n)
			}
			implAfter, err := rig.turns.Get(ctx, impl.ID)
			if err != nil {
				t.Fatalf("get implementation turn: %v", err)
			}
			if implAfter.Status != tc.implStatus {
				t.Errorf("implementation status = %q after the refused submission, want %q (nothing cancelled)", implAfter.Status, tc.implStatus)
			}
			planAfter, err := rig.plans.Get(ctx, plan.ID)
			if err != nil {
				t.Fatalf("get plan: %v", err)
			}
			if planAfter.Status != sqlcgen.PlanStatusApproved {
				t.Errorf("plan status = %q after the refused submission, want %q", planAfter.Status, sqlcgen.PlanStatusApproved)
			}

			if tc.breakCreate {
				return
			}

			// The implementation ends; the same submission now creates the
			// revision as before and closes the modal with an empty 200.
			if _, err := rig.pool.Exec(ctx, `UPDATE turns SET status = 'completed' WHERE id = $1`, impl.ID); err != nil {
				t.Fatalf("complete the implementation turn: %v", err)
			}
			rec := submitRequestChanges(t, rig, modal, feedback)
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Fatalf("idle submission: status = %d body = %q, want an empty 200 (closes the modal)", rec.Code, rec.Body.String())
			}
			turns, err = rig.turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}
			var revision *sqlcgen.Turn
			for i := range turns {
				if turns[i].ID != plan.TurnID && turns[i].ID != impl.ID {
					revision = &turns[i]
				}
			}
			if len(turns) != 3 || revision == nil {
				t.Fatalf("len(turns) = %d after the idle submission, want the 2 seeded turns and the revision", len(turns))
			}
			wantPrompt := plandomain.MaybeInjectStructureInstruction(true, feedback)
			if !revision.PlanMode || revision.Prompt == nil || *revision.Prompt != wantPrompt {
				t.Errorf("revision: plan_mode = %v prompt = %q, want a plan-mode turn carrying the feedback %q", revision.PlanMode, promptOrNilText(revision.Prompt), wantPrompt)
			}
			if n := turnCreateAudits(ctx, t, rig, session); n != 1 {
				t.Errorf("turn.create audit rows = %d after the idle submission, want 1", n)
			}
		})
	}
}

// TestRequestChangesRefusalText_KeysOnTheSentinel: the modal's text for a
// failed create follows httpapi.ErrTurnAlreadyOpen alone. The open-turn
// refusal is taken from a real CreateTurnCore call on a busy session; a
// copy of it reworded still reads as busy, while a failure carrying its
// exact text and status but not its sentinel, the real 409 CreateTurnCore
// returns for an ordinary prompt against a plan awaiting approval, and a
// server error all get the generic error. A check on the Message or on
// the Status fails one of these.
func TestRequestChangesRefusalText_KeysOnTheSentinel(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newInteractiveTestRig(t, pool)

	busySession, _, _ := seedApprovedImplementation(ctx, t, rig, sqlcgen.TurnStatusProcessing)
	_, _, openTurn := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), busySession.ID, "a revision", nil, true, false, rig.defaultActorUserID, httpapi.RejectIfOpen)
	if openTurn == nil || !errors.Is(openTurn, httpapi.ErrTurnAlreadyOpen) || openTurn.Status != http.StatusConflict {
		t.Fatalf("CreateTurnCore on a busy session = %#v, want RejectIfOpen's 409 carrying httpapi.ErrTurnAlreadyOpen", openTurn)
	}
	reworded := *openTurn
	reworded.Message = "a different wording of the same refusal"

	awaitingSession, _ := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
	_, _, awaiting := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), awaitingSession.ID, "an ordinary prompt", nil, false, false, rig.defaultActorUserID, httpapi.RejectIfOpen)
	if awaiting == nil || !errors.Is(awaiting, httpapi.ErrPlanAwaitingApproval) || awaiting.Status != http.StatusConflict {
		t.Fatalf("CreateTurnCore against a plan awaiting approval = %#v, want its 409 carrying httpapi.ErrPlanAwaitingApproval", awaiting)
	}

	tests := []struct {
		name string
		cerr *httpapi.CreateTurnError
		want string
	}{
		{name: "the open-turn refusal", cerr: openTurn, want: wantRequestChangesBusyText},
		{name: "the open-turn refusal reworded", cerr: &reworded, want: wantRequestChangesBusyText},
		{name: "its text and status without its sentinel", cerr: &httpapi.CreateTurnError{Status: openTurn.Status, Message: openTurn.Message}, want: wantRequestChangesErrorText},
		{name: "its text on a server error", cerr: &httpapi.CreateTurnError{Status: http.StatusInternalServerError, Message: openTurn.Message}, want: wantRequestChangesErrorText},
		{name: "the plan-awaiting-approval 409", cerr: awaiting, want: wantRequestChangesErrorText},
		{name: "a server error", cerr: &httpapi.CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}, want: wantRequestChangesErrorText},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := slack.RequestChangesRefusalTextForTest(tc.cerr); got != tc.want {
				t.Errorf("text for %#v = %q, want %q", tc.cerr, got, tc.want)
			}
		})
	}
}
