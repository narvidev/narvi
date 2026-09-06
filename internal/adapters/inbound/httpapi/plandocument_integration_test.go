//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// This file proves §31.3's own durability fix: DecidePlanOnTx now
// snapshots an approved plan's own prose into plan_documents, in the SAME
// transaction as the guarded UPDATE that flips the plan to 'approved' --
// see migrations/000112_plan_documents.up.sql's own comment for the full
// "why" (an approved plan's prose used to live ONLY in the events log,
// which cascades away with its session exactly like plans' own
// session_id does, and is reconstructed at read time only via a bounded,
// best-effort scan).

// TestApprovePlan_SnapshotsApprovedContentIntoPlanDocuments is this
// Step's own coverage measurement: every plan approved through the REST
// endpoint gets exactly one plan_documents row, carrying the SAME prose
// (the producing turn's own final streamed assistant text) the
// approval-request UI itself would have shown.
func TestApprovePlan_SnapshotsApprovedContentIntoPlanDocuments(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create producing turn: %v", err)
	}
	dispatchTurn(ctx, t, rig, session.ID, turn.ID)
	const wantContent = "1. Do the first thing\n2. Do the second thing"
	seedTokenEvent(ctx, t, rig, session.ID, "plan-doc-msg", wantContent)

	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create awaiting_approval plan: %v", err)
	}

	var got planActionResponseForTest
	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetByPlanID(%s): %v -- every approved plan must have exactly one plan_documents row", plan.ID.String(), err)
	}
	if doc.Content == nil || *doc.Content != wantContent {
		t.Errorf("plan_documents.content = %v, want %q", doc.Content, wantContent)
	}
}

// TestApprovePlan_SnapshotConflict_AbortsWholeApproval proves the
// same-transaction property migrations/000112_plan_documents.up.sql and
// decideplan.go's own snapshotApprovedPlanContent exist for: a
// plan_documents INSERT that fails must abort the ENTIRE approval,
// undoing the guarded UPDATE that already flipped the row to 'approved'
// within the SAME transaction -- never a plan left reading 'approved'
// with no (or a stale) snapshot to show for it.
//
// Forced failure: a plan_documents row is pre-seeded for this EXACT plan
// id before approval ever runs, so DecidePlanOnTx's own real snapshot
// INSERT hits plan_id's UNIQUE constraint
// (migrations/000112_plan_documents.up.sql) and fails -- a genuine
// Postgres error, never a fake/mocked one. If the snapshot write were
// ever moved outside the transition's own transaction (or its error
// swallowed rather than propagated), this test would observe the plan
// sitting at 'approved' below instead, with no way to undo it -- exactly
// the defect this table exists to close.
func TestApprovePlan_SnapshotConflict_AbortsWholeApproval(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	if _, err := rig.planDocuments.Create(ctx, plan.ID, "a pre-existing row already occupying this plan's own unique slot"); err != nil {
		t.Fatalf("pre-seed conflicting plan_documents row: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token)
	if status == http.StatusOK {
		t.Fatalf("status = %d, want a non-200 failure (the snapshot INSERT must have hit plan_id's UNIQUE constraint)", status)
	}

	reloaded, err := rig.plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("re-fetch plan: %v", err)
	}
	if reloaded.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("plan.status = %q, want %q -- the guarded UPDATE must roll back together with the failed snapshot INSERT (same transaction), never commit on its own", reloaded.Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if reloaded.DecidedAt.Valid || reloaded.DecidedBy.Valid {
		t.Errorf("plan decided_at.Valid=%v decided_by.Valid=%v, want both false -- a rolled-back decision must leave no partial trace", reloaded.DecidedAt.Valid, reloaded.DecidedBy.Valid)
	}
}
