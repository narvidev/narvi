//go:build integration

// This file holds the seeds the cut-plan tests share (technical plan §6.1:
// a `token` frame the sandbox-agent cut on its way to the control plane
// carries `cut`, and no surface approves a plan whose text is one). The
// frames are injected straight into `events`, as the session actor stores
// them -- a part's first frame under its bare part id, each later one
// under a key of its own -- since nothing produces a cut yet.
package httpapi_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// cutPlanBlock is a complete plan-steps block: a plan whose text holds it
// whole has structured steps; a cut plan has none, even when the cut left
// the block intact at its start.
const cutPlanBlock = "```plan-steps\n" +
	`{"steps":[{"title":"Add the migration","description":"One column.","fileRefs":["migrations/000999.up.sql"]}],"scopeEstimate":"1 file"}` +
	"\n```\n"

// cutMarker is the line the cutter ends a cut string with.
func cutMarker(kept, total int) string {
	return "\n[text cut at " + strconv.Itoa(kept) + " of " + strconv.Itoa(total) + " bytes on its way from the sandbox]"
}

// seedCutTokenFrame stores one `token` frame of part under storageKey,
// carrying cut as its raw `cut` property when cut is not nil.
func seedCutTokenFrame(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, storageKey, part, text string, cut any) {
	t.Helper()
	payload := map[string]any{"type": "token", "messageId": part, "sessionId": sessionID.String(), "gen": 1, "text": text}
	if cut != nil {
		payload["cut"] = cut
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal token frame: %v", err)
	}
	if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "token", MessageID: storageKey, Payload: raw}); err != nil {
		t.Fatalf("store token frame: %v", err)
	}
}

// seedDispatchedPlan seeds a completed plan-mode turn, dispatched at the
// session's current watermark so its frames fall in its window, stores its
// text part's frames through store, and an awaiting_approval plan atop it.
func seedDispatchedPlan(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, version int32, store func()) sqlcgen.Plan {
	t.Helper()
	turn, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	dispatchTurn(ctx, t, r, sessionID, turn.ID)
	store()
	plan, err := r.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sessionID, TurnID: turn.ID, Version: version, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}
	return plan
}

// planStatusOf reads a plan's status straight from its row.
func planStatusOf(ctx context.Context, t *testing.T, r testRig, planID pgtype.UUID) sqlcgen.PlanStatus {
	t.Helper()
	var status sqlcgen.PlanStatus
	if err := r.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, planID).Scan(&status); err != nil {
		t.Fatalf("read plan status: %v", err)
	}
	return status
}
