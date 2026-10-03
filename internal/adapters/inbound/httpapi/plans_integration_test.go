//go:build integration

// Integration tests for the plan-mode UI's own additions to plans.go/
// session.go: restdtos.Plan.content (a per-VERSION, correctly-windowed
// scan, plans_integration_test.go's own reason to exist) and
// restdtos.Session.buildModelId/buildEffort (a plain passthrough of
// already-persisted columns, TestGetSession_ReflectsBuildModelAndEffort
// below).
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// dispatchTurn stamps turnID as Dispatched at the session's own CURRENT
// events-log high-water mark (EventStore.MaxEventIDForSession) -- the
// exact same two-step "read the watermark, then stamp it onto the turn"
// sequence tryPlanDispatch itself performs (internal/app/sessionactor/
// dispatch.go), reproduced here as a direct DB seed (this package's own
// established "surgical direct DB seed, bypassing the actor pipeline"
// precedent, planapprove_integration_test.go's seedAwaitingApprovalPlan).
// Returns the stamped dispatchedEventID for the caller's own assertions.
func dispatchTurn(ctx context.Context, t *testing.T, r testRig, sessionID, turnID pgtype.UUID) int64 {
	t.Helper()
	watermark, err := r.events.MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("MaxEventIDForSession: %v", err)
	}
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                turnID,
		Status:            sqlcgen.TurnStatusCompleted,
		DispatchedEventID: &watermark,
	}); err != nil {
		t.Fatalf("stamp dispatched_event_id: %v", err)
	}
	return watermark
}

// seedTokenEvent inserts one "token" event carrying text -- the exact
// payload shape internal/app/sessionactor's own tokenEventPayload (and,
// downstream, internal/domain/plan.ExtractContent via ToContentEvents)
// decodes. messageID must be unique per call (events are upserted by
// (session_id, message_id), §6.1) -- callers pass a fresh one per event.
func seedTokenEvent(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, messageID, text string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"type": "token", "text": text})
	if err != nil {
		t.Fatalf("marshal token payload: %v", err)
	}
	if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{
		SessionID: sessionID,
		Type:      "token",
		MessageID: messageID,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed token event: %v", err)
	}
}

// TestListPlans_ContentIsWindowedPerVersion_NeverLeaksALaterTurnsText is
// this Step's own central correctness proof: internal/domain/plan.
// ExtractContent's own upper bound exists specifically so that GET
// .../plans returns EACH plan version's OWN content, never a LATER turn's
// (the next plan revision's own turn, or -- the case that actually bit an
// unbounded-above reuse of the single-turn notifier algorithm -- the
// approval-dispatched IMPLEMENTATION turn that runs after an approved
// plan). Three turns run in sequence: plan v1's own producing turn, plan
// v2's own producing turn (a "request changes" revision), and a THIRD,
// non-plan turn (standing in for the implementation turn dispatched after
// approval) -- v1's content must never contaminate with v2's or the third
// turn's text, and v2's must never contaminate with the third turn's.
func TestListPlans_ContentIsWindowedPerVersion_NeverLeaksALaterTurnsText(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn1, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn1: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn1.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "turn1-msg", "v1's own plan text -- must never appear on any other version")
	plan1, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn1.ID, Version: 1, Status: sqlcgen.PlanStatusSuperseded})
	if err != nil {
		t.Fatalf("create plan1: %v", err)
	}

	turn2, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn2: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn2.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "turn2-msg", "v2's own plan text -- the revision, still awaiting approval")
	plan2, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn2.ID, Version: 2, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan2: %v", err)
	}

	// The third turn stands in for the approval-dispatched implementation
	// turn: it runs AFTER plan v2's own producing turn and carries no plan
	// row of its own. Before ExtractContent's own upper bound existed, an
	// unbounded-above scan for plan v2 would have picked up THIS text
	// instead (it is the newest token event in the session by construction).
	turn3, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: false})
	if err != nil {
		t.Fatalf("create turn3: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn3.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "turn3-msg", "the implementation turn's own build narration -- must NEVER appear as any plan's content")

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(resp.Plans) != 2 {
		t.Fatalf("len(Plans) = %d, want 2", len(resp.Plans))
	}

	byID := map[string]restdtos.Plan{}
	for _, p := range resp.Plans {
		byID[p.Id] = p
	}

	got1, ok := byID[plan1.ID.String()]
	if !ok {
		t.Fatalf("plan1 (id=%s) missing from response", plan1.ID.String())
	}
	if want := "v1's own plan text -- must never appear on any other version"; got1.Content != want {
		t.Errorf("plan1.Content = %q, want %q", got1.Content, want)
	}

	got2, ok := byID[plan2.ID.String()]
	if !ok {
		t.Fatalf("plan2 (id=%s) missing from response", plan2.ID.String())
	}
	if want := "v2's own plan text -- the revision, still awaiting approval"; got2.Content != want {
		t.Errorf("plan2.Content = %q, want %q -- if this instead shows turn3's build narration, ExtractContent's own upper bound has regressed", got2.Content, want)
	}
}

// TestListPlans_NoTokenEventsAtAll_FallsBackHonestly proves the httpapi
// wiring's own fallback path (not just plandomain.ExtractContent's pure
// behavior, already covered exhaustively by content_test.go): a plan whose
// producing turn genuinely streamed no token events returns the fixed,
// honest placeholder, never an empty string or a 500.
func TestListPlans_NoTokenEventsAtAll_FallsBackHonestly(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	// Deliberately no seedTokenEvent call.
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
	if want := "(plan content unavailable -- see the session's own event log)"; resp.Plans[0].Content != want {
		t.Errorf("Content = %q, want the honest fallback %q", resp.Plans[0].Content, want)
	}
}

// TestListPlans_UndispatchedPendingTurn_NeverBreaksTheBoundaryScan proves
// planContentMap's own "only dispatched turns carry a scan boundary"
// filter (plans.go): a session with a genuinely PENDING turn (never
// dispatched -- DispatchedEventID still nil, e.g. queued behind an
// in-flight turn) alongside an awaiting_approval plan must not panic or
// error -- the pending turn's own nil DispatchedEventID would otherwise
// be dereferenced by the boundary sort, and MUST be excluded from the
// ordered turn-boundary list before that sort ever runs.
func TestListPlans_UndispatchedPendingTurn_NeverBreaksTheBoundaryScan(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn1, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn1: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn1.ID)
	seedTokenEvent(ctx, t, rig, session.ID, "turn1-msg", "the plan's own real content")
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn1.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// A second turn, created but NEVER dispatched (no dispatchTurn call --
	// DispatchedEventID stays nil, exactly like a turn still queued behind
	// this session's own in-flight processing turn).
	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusPending, PlanMode: false}); err != nil {
		t.Fatalf("create pending turn: %v", err)
	}

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a pending, never-dispatched turn must never break this scan)", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	if resp.Plans[0].Id != plan.ID.String() {
		t.Fatalf("Plans[0].Id = %q, want %q", resp.Plans[0].Id, plan.ID.String())
	}
	if want := "the plan's own real content"; resp.Plans[0].Content != want {
		t.Errorf("Content = %q, want %q", resp.Plans[0].Content, want)
	}
}

// TestGetSession_ReflectsBuildModelAndEffort proves restdtos.Session.
// buildModelId/buildEffort (the plan-mode UI's own addition) are a real,
// read-back passthrough of sessions.build_model_id/build_effort -- both
// columns were write-only via CreateSessionRequest before this (never
// surfaced on any GET response).
func TestGetSession_ReflectsBuildModelAndEffort(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	buildModel := "anthropic/claude-sonnet-5"
	buildEffort := "high"
	session := createSessionForUser(ctx, t, rig, owner.ID, &buildModel, &buildEffort)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String(), nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.BuildModelId == nil || *got.BuildModelId != buildModel {
		t.Errorf("BuildModelId = %v, want %q", got.BuildModelId, buildModel)
	}
	if got.BuildEffort == nil || *got.BuildEffort != buildEffort {
		t.Errorf("BuildEffort = %v, want %q", got.BuildEffort, buildEffort)
	}
}

// TestGetSession_NilBuildModelAndEffort_WhenNeverSet proves the null case
// -- an ordinary (non-plan-mode) session never had CreateSessionRequest.
// buildModelId/buildEffort set, and GetSession must reflect that honestly
// as nil, never a fabricated default string.
func TestGetSession_NilBuildModelAndEffort_WhenNeverSet(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String(), nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.BuildModelId != nil {
		t.Errorf("BuildModelId = %v, want nil", got.BuildModelId)
	}
	if got.BuildEffort != nil {
		t.Errorf("BuildEffort = %v, want nil", got.BuildEffort)
	}
}

// TestListPlans_CutPlan_ReportsTheCut proves GET .../plans states a cut from
// FinalText's report, never from the text (technical plan §6.1): an
// awaiting plan whose final text is a cut frame reports cut {kept, total},
// its content is the cut text with its marker, and it has no structured
// steps even though the cut left a complete block at its start; a whole
// plan in the same session reports null.
func TestListPlans_CutPlan_ReportsTheCut(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	whole := seedDispatchedPlan(ctx, t, rig, session.ID, 1, func() {
		seedCutTokenFrame(ctx, t, rig, session.ID, "prt_v1", "prt_v1", "The first plan.\n\n"+cutPlanBlock, nil)
	})
	if _, err := rig.pool.Exec(ctx, `UPDATE plans SET status = 'superseded' WHERE id = $1`, whole.ID); err != nil {
		t.Fatalf("supersede v1: %v", err)
	}
	const total = 40960
	kept := len(cutPlanBlock) + 4
	cutText := cutPlanBlock + "More" + cutMarker(kept, total)
	cutPlan := seedDispatchedPlan(ctx, t, rig, session.ID, 2, func() {
		seedCutTokenFrame(ctx, t, rig, session.ID, "prt_v2", "prt_v2", "", nil)
		seedCutTokenFrame(ctx, t, rig, session.ID, "prt_v2#cut", "prt_v2", cutText, map[string]int{"kept": kept, "total": total})
	})

	var resp restdtos.ListPlansResponse
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	byID := map[string]restdtos.Plan{}
	for _, p := range resp.Plans {
		byID[p.Id] = p
	}
	got := byID[cutPlan.ID.String()]
	if got.Cut == nil || got.Cut.Kept != kept || got.Cut.Total != total {
		t.Errorf("cut plan's cut = %+v, want {%d %d}", got.Cut, kept, total)
	}
	if got.Content != cutText {
		t.Errorf("cut plan's content = %q, want the cut text with its marker", got.Content)
	}
	if got.Structured != nil {
		t.Errorf("cut plan's structured = %+v, want null: a cut plan has no structured steps", got.Structured)
	}
	if w := byID[whole.ID.String()]; w.Cut != nil || w.Structured == nil {
		t.Errorf("whole plan: cut = %+v, structured = %+v, want no cut and its steps", w.Cut, w.Structured)
	}

	// On the wire, a whole plan's cut is an explicit null.
	var raw struct {
		Plans []map[string]json.RawMessage `json:"plans"`
	}
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &raw, token); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, p := range raw.Plans {
		if c, ok := p["cut"]; !ok {
			t.Errorf("plan %s carries no cut key, want it present", p["id"])
		} else if string(p["id"]) == `"`+whole.ID.String()+`"` && string(c) != "null" {
			t.Errorf("whole plan's cut = %s, want null", c)
		}
	}
}
