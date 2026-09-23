//go:build integration

// This file proves plans.go's own "snapshot first, live recompute as
// fallback" read-path rule end to end: GET .../plans now makes the durable
// plan_documents snapshot (migrations/000112_plan_documents.up.sql,
// written once at approval time by decideplan.go's own
// snapshotApprovedPlanContent) its FIRST choice, falling back to the
// bounded live event-log recompute (planContentEventFetchLimit-newest
// events) only where no USABLE snapshot exists for a given plan.
//
// Three eras, one uniform rule ("prefer the snapshot only where a usable
// snapshot exists") -- each proven here by a test that FAILS if its own
// branch is removed or inverted:
//
//   - no plan_documents row at all (an approved plan that predates the
//     snapshot table, or otherwise never got one) -- falls back to EXACTLY
//     today's pre-snapshot behavior, TestListPlans_ApprovedPlanWithNoSnapshotRow_FallsBackToLiveRecompute;
//   - a snapshot row whose content has aged out of the live event-log
//     window -- the durable snapshot answers where a live recompute could
//     only return the placeholder, TestListPlans_ApprovedPlanEventsAgedOut_ReturnsDurableSnapshotNotPlaceholder;
//   - a snapshot row whose content is NULL (a future retention policy's own
//     null-out) -- treated as "no usable snapshot", falls back exactly like
//     the no-row case, TestListPlans_SnapshotRowWithNullContent_FallsBackToLiveRecompute;
//   - structured comes from the snapshot's own persisted structured_steps
//     when non-NULL (never re-derived from content, even when content
//     itself carries a DIFFERENT valid block), and is derived via
//     plandomain.ExtractStructured(snapshotContent) only when
//     structured_steps itself is NULL --
//     TestListPlans_SnapshotStructuredSteps_PreferredOverReDerivationFromContent
//     and TestListPlans_SnapshotStructuredStepsNull_DerivesFromSnapshotContent.
package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

// planReadPathEventFetchLimit mirrors plans.go's own unexported
// planContentEventFetchLimit (this package's test binary is package
// httpapi_test, an external test package, so it cannot reach an unexported
// constant directly) -- kept as its own named copy, not a magic number, so
// the "why 2000, and why one more than that" reasoning in the aging test
// below is traceable back to the SAME bound plans.go itself enforces.
const planReadPathEventFetchLimit = 2000

// seedFillerEvents bulk-inserts count "noise"-typed events for sessionID,
// via one raw INSERT ... SELECT FROM generate_series -- a single round
// trip, since the aging-out test below needs to push a real token event
// out of the live recompute's own newest-planReadPathEventFetchLimit
// window, and a naive one-call-per-event loop would be needlessly slow
// (and irrelevant to what this test actually proves) for a count in the
// low thousands. type is deliberately "noise", never "token": these rows
// exist purely to occupy id-space newer than the real token event, and
// must never themselves be mistaken for plan content by
// sessionactor.ToContentEvents (which only ever reads a "token" event's
// own payload).
func seedFillerEvents(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, count int) {
	t.Helper()
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO events (session_id, type, payload, message_id)
		SELECT $1, 'noise', '{}'::jsonb, 'filler-' || gs::text
		FROM generate_series(1, $2::int) AS gs
	`, sessionID, count); err != nil {
		t.Fatalf("bulk-insert filler events: %v", err)
	}
}

// TestListPlans_ApprovedPlanEventsAgedOut_ReturnsDurableSnapshotNotPlaceholder
// is mutation-verification (a): an approved plan's own token event, once it
// ages out of the live recompute's bounded window, must render from the
// durable plan_documents snapshot -- NOT plandomain.ContentFallbackText.
// Before this Step, GET .../plans recomputed content live on every
// request, unconditionally, so this exact scenario returned the
// placeholder even though the approved prose sat durably in plan_documents
// the whole time (written by DecidePlanOnTx's own snapshotApprovedPlanContent,
// but read by nothing in production). If the snapshot-first branch in
// resolvePlanRenderedContent (plans.go) were removed or inverted, this
// test observes the placeholder instead and fails.
func TestListPlans_ApprovedPlanEventsAgedOut_ReturnsDurableSnapshotNotPlaceholder(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	const wantContent = "the durable approved prose -- must survive even once its own token event ages out of the live recompute window"
	seedTokenEvent(ctx, t, rig, session.ID, "aging-msg", wantContent)

	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create awaiting_approval plan: %v", err)
	}

	// Approve now, while the token event is still well within the live
	// recompute's own window -- this is what makes DecidePlanOnTx's own
	// snapshot write correct BEFORE the event log is flooded below.
	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status != http.StatusOK {
		t.Fatalf("approve status = %d, want %d", status, http.StatusOK)
	}
	doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v -- approval must have snapshotted this plan", plan.ID.String(), err)
	}
	if doc.Content == nil || *doc.Content != wantContent {
		t.Fatalf("sanity check failed: plan_documents.content = %v, want %q", doc.Content, wantContent)
	}

	// Flood the session's own event log with more filler than the live
	// recompute's own window -- the ORIGINAL token event (the lowest id in
	// the session) now sits well outside ListRecentForSession's own
	// newest-planReadPathEventFetchLimit slice, so a live recompute alone
	// can no longer find it.
	seedFillerEvents(ctx, t, rig, session.ID, planReadPathEventFetchLimit+100)

	var resp restdtos.ListPlansResponse
	status = rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	if resp.Plans[0].Content != wantContent {
		t.Errorf("Content = %q, want the durable snapshot's own prose %q -- if this is instead the placeholder, the read path fell back to a live recompute that can no longer see the aged-out token event", resp.Plans[0].Content, wantContent)
	}
}

// TestListPlans_ApprovedPlanWithNoSnapshotRow_FallsBackToLiveRecompute is
// mutation-verification (b): this Step's own settled design question. A
// plan that reads 'approved' but has NO plan_documents row at all (exactly
// what every plan approved before migration 000112 created the table looks
// like today, reproduced directly here by seeding an already-'approved'
// plans row through the store rather than the real approve endpoint, so no
// snapshot is ever written) must render EXACTLY as it always did: the live
// event-log recompute, placeholder included where the window has nothing
// -- never an error, and never a snapshot lookup that panics or fabricates
// content for a plan_id with no row.
func TestListPlans_ApprovedPlanWithNoSnapshotRow_FallsBackToLiveRecompute(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	const wantContent = "legacy approved plan's own prose -- predates plan_documents, must still render from the live event log"
	seedTokenEvent(ctx, t, rig, session.ID, "legacy-msg", wantContent)

	// Created directly as 'approved' -- bypassing ApprovePlan/DecidePlanOnTx
	// entirely, so snapshotApprovedPlanContent never runs and no
	// plan_documents row is ever written for this plan, exactly like a real
	// pre-migration-000112 approved plan looks today.
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusApproved})
	if err != nil {
		t.Fatalf("create pre-snapshotted approved plan: %v", err)
	}
	if _, err := rig.planDocuments.GetByPlanID(ctx, plan.ID); err == nil {
		t.Fatalf("sanity check failed: plan %s has a plan_documents row, want none", plan.ID.String())
	}

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	if resp.Plans[0].Content != wantContent {
		t.Errorf("Content = %q, want the live-recomputed prose %q -- a plan with no snapshot row must fall back exactly like pre-snapshot behavior", resp.Plans[0].Content, wantContent)
	}
}

// TestListPlans_SnapshotRowWithNullContent_FallsBackToLiveRecompute is
// mutation-verification (c): a plan_documents row whose content has been
// retention-nulled (migrations/000112_plan_documents.up.sql's own reason
// that column is nullable at all -- "a future retention policy can null
// out CONTENT ALONE") is not a usable snapshot, and must fall back to the
// live recompute exactly like having no row at all -- never a nil-pointer
// dereference of snapshot.Content, and never an empty string standing in
// for "no content".
func TestListPlans_SnapshotRowWithNullContent_FallsBackToLiveRecompute(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	const wantContent = "the live prose that must be returned since the snapshot's own content was retention-nulled"
	seedTokenEvent(ctx, t, rig, session.ID, "null-content-msg", wantContent)

	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// A plan_documents row with NULL content -- never producible through
	// PlanDocumentStore.Create (its content parameter is a plain,
	// always-non-nil string), so seeded directly, standing in for what a
	// future retention policy's own single-column UPDATE would leave
	// behind.
	if _, err := rig.pool.Exec(ctx, `INSERT INTO plan_documents (plan_id, content, structured_steps) VALUES ($1, NULL, NULL)`, plan.ID); err != nil {
		t.Fatalf("seed retention-nulled plan_documents row: %v", err)
	}

	var resp restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a NULL-content snapshot row must never panic or 500)", status)
	}
	if len(resp.Plans) != 1 {
		t.Fatalf("len(Plans) = %d, want 1", len(resp.Plans))
	}
	if resp.Plans[0].Content != wantContent {
		t.Errorf("Content = %q, want the live-recomputed prose %q -- a NULL-content snapshot row is not usable and must fall back exactly like having no row", resp.Plans[0].Content, wantContent)
	}
}

// TestListPlans_SnapshotStructuredSteps_PreferredOverReDerivationFromContent
// is mutation-verification (d), positive half: when a usable snapshot's
// own structured_steps is non-NULL, it is used VERBATIM -- never
// re-derived from the snapshot's own content, even when that content
// happens to carry its OWN, independently valid ```plan-steps block. The
// persisted structured_steps is the immutable record of what was actually
// approved (single-source-of-truth); a content string that could ALSO
// parse to a different structured document is deliberately constructed
// here so that "prefer the persisted column" and "always re-derive from
// content" produce two distinguishably different results -- only the
// former is correct.
func TestListPlans_SnapshotStructuredSteps_PreferredOverReDerivationFromContent(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// content carries its OWN valid ```plan-steps block -- if re-derivation
	// ever ran instead of trusting structured_steps, THIS title would
	// surface on the wire.
	content := "Here is my plan.\n\n```plan-steps\n" +
		`{"steps":[{"title":"IGNORED-BLOCK-IN-CONTENT-MUST-NEVER-SURFACE","description":"d","fileRefs":["ignored.go"]}],"scopeEstimate":"ignored scope"}` +
		"\n```\n"
	persisted := &plandomain.Structured{
		Steps:         []plandomain.Step{{Title: "PERSISTED-STEP-FROM-STRUCTURED-STEPS-COLUMN", Description: "d2", FileRefs: []string{"persisted.go"}}},
		ScopeEstimate: "persisted scope",
	}
	if _, err := rig.planDocuments.Create(ctx, plan.ID, content, persisted); err != nil {
		t.Fatalf("seed plan_documents row with structured_steps: %v", err)
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
	if got.Content != content {
		t.Errorf("Content = %q, want the full unmodified snapshot prose %q", got.Content, content)
	}
	if got.Structured == nil {
		t.Fatalf("Structured = nil, want the persisted structured_steps document")
	}
	if len(got.Structured.Steps) != 1 || got.Structured.Steps[0].Title != "PERSISTED-STEP-FROM-STRUCTURED-STEPS-COLUMN" {
		t.Errorf("Structured.Steps = %+v, want exactly the persisted step -- never the block embedded in content", got.Structured.Steps)
	}
}

// TestListPlans_SnapshotStructuredStepsNull_DerivesFromSnapshotContent is
// mutation-verification (d), negative half: when a usable snapshot's own
// structured_steps IS NULL (a plan_documents row written before migration
// 000126 added that column), structured must still be derived --
// plandomain.ExtractStructured run over the SNAPSHOT's own content, not
// simply nil because the persisted column happened to be empty.
func TestListPlans_SnapshotStructuredStepsNull_DerivesFromSnapshotContent(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	content := "Here is my plan.\n\n```plan-steps\n" +
		`{"steps":[{"title":"DERIVED-FROM-SNAPSHOT-CONTENT-STEP","description":"d","fileRefs":["derived.go"]}],"scopeEstimate":"derived scope"}` +
		"\n```\n"
	// structured (nil) -- PlanDocumentStore.Create writes SQL NULL into
	// structured_steps for a nil value, reproducing a pre-000126 row.
	if _, err := rig.planDocuments.Create(ctx, plan.ID, content, nil); err != nil {
		t.Fatalf("seed plan_documents row with NULL structured_steps: %v", err)
	}
	doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v", plan.ID.String(), err)
	}
	if doc.StructuredSteps != nil {
		t.Fatalf("sanity check failed: structured_steps = %s, want NULL", doc.StructuredSteps)
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
	if got.Structured == nil {
		t.Fatalf("Structured = nil, want a document derived from the snapshot's own content -- structured_steps being NULL must fall back to re-derivation, not to nil")
	}
	if len(got.Structured.Steps) != 1 || got.Structured.Steps[0].Title != "DERIVED-FROM-SNAPSHOT-CONTENT-STEP" {
		t.Errorf("Structured.Steps = %+v, want exactly the block embedded in the snapshot's own content", got.Structured.Steps)
	}
}
