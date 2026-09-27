//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

// TestApprovePlan_MultiFrameStreamedPlan_SnapshotsFullContentAndStructuredSteps
// proves the approval snapshot -- plan_documents, "the immutable record of
// what was approved", which the plan read path prefers over the event log
// -- freezes a plan's FINAL text when that plan streamed as more than one
// frame of one text part. The frames go through the real session actor,
// exactly as the sandbox socket delivers them, not seeded as rows: before
// per-frame storage only the first frame was stored, so the snapshot froze
// the placeholder (empty first frame) or a prefix, and structured_steps was
// NULL because a prefix never contains the closing ```plan-steps block the
// plan asks for last.
func TestApprovePlan_MultiFrameStreamedPlan_SnapshotsFullContentAndStructuredSteps(t *testing.T) {
	const partID = "prt_plan"
	tests := []struct {
		name  string
		first string
	}{
		{name: "empty first frame", first: ""},
		{name: "prefix first frame", first: "Here is my plan.\n\n1. Add a table."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			owner, token := rig.createAuthenticatedUser(ctx, t)
			session := createSessionForUser(ctx, t, rig, owner.ID, nil)

			// The producing turn is Processing while its frames arrive, as a
			// dispatched turn is (the session actor stores a frame only then),
			// and completes afterwards.
			turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, PlanMode: true})
			if err != nil {
				t.Fatalf("create producing turn: %v", err)
			}
			watermark, err := rig.events.MaxEventIDForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("MaxEventIDForSession: %v", err)
			}
			if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
				ID: turn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedEventID: &watermark,
			}); err != nil {
				t.Fatalf("stamp dispatched_event_id: %v", err)
			}
			if _, err := rig.sandboxes.Create(ctx, session.ID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			actor, err := rig.registry.GetOrSpawn(ctx, session.ID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			for i, text := range []string{tt.first, wellFormedPlanStepsContent} {
				raw, err := json.Marshal(sandboxws.Token{Type: "token", MessageId: partID, SessionId: session.ID.String(), Gen: 1, Text: text})
				if err != nil {
					t.Fatalf("marshal frame %d: %v", i, err)
				}
				reply := make(chan sessionactor.SandboxEventOutcome, 1)
				if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "token", Gen: 1, MessageID: partID, Raw: raw, Reply: reply}); err != nil {
					t.Fatalf("Send frame %d: %v", i, err)
				}
				select {
				case outcome := <-reply:
					if !outcome.Persisted {
						t.Fatalf("frame %d not persisted", i)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for frame %d's outcome", i)
				}
			}

			if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turn.ID, Status: sqlcgen.TurnStatusCompleted}); err != nil {
				t.Fatalf("complete producing turn: %v", err)
			}

			plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
			if err != nil {
				t.Fatalf("create awaiting_approval plan: %v", err)
			}
			if status := rig.doJSON(t, http.MethodPost,
				"/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", []byte{}, nil, token); status != http.StatusOK {
				t.Fatalf("approve status = %d, want %d", status, http.StatusOK)
			}

			doc, err := rig.planDocuments.GetByPlanID(ctx, plan.ID)
			if err != nil {
				t.Fatalf("GetByPlanID(%s): %v", plan.ID.String(), err)
			}
			if doc.Content == nil {
				t.Errorf("plan_documents.content = NULL, want the plan's full final text %q", wellFormedPlanStepsContent)
			} else if *doc.Content != wellFormedPlanStepsContent {
				t.Errorf("plan_documents.content = %q, want the plan's full final text %q", *doc.Content, wellFormedPlanStepsContent)
			}
			if doc.StructuredSteps == nil {
				t.Fatalf("plan_documents.structured_steps = NULL, want the steps of the final frame's ```plan-steps block")
			}
			var got plandomain.Structured
			if err := json.Unmarshal(doc.StructuredSteps, &got); err != nil {
				t.Fatalf("unmarshal structured_steps: %v", err)
			}
			if len(got.Steps) != 1 || got.Steps[0].Title != "Add table" || got.ScopeEstimate != "1 file" {
				t.Errorf("structured_steps = %+v, want one step titled %q, scope %q", got, "Add table", "1 file")
			}
		})
	}
}
