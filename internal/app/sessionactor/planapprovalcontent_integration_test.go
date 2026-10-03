//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves §8.1's ("plan mode, cross-channel", §8.1/§13.3) own
// best-effort plan-content extraction (planapprovalcontent.go's
// planContentText) against a REAL Postgres instance, specifically the case
// planContentEventFetchLimit's own doc comment describes: a session that
// has already accumulated more events than that fixed fetch limit BEFORE
// the plan-mode turn under test ever runs. planContentText must still find
// this turn's own final token event -- the most RECENT activity in the
// session -- rather than silently falling back to
// planContentFallbackText because the fetch window looked at the WRONG
// end of a long event log.

// TestPlanContentText_LongEventHistory_StillFindsCurrentTurnsTokenText
// proves planContentText finds THIS plan-mode turn's own final streamed
// token text even when the session already has more prior events than
// planContentEventFetchLimit -- the real regression this Step's own fix
// (ListRecentForSession, newest-first) closes: an oldest-first fetch
// bounded to the same limit would never even reach this turn's own event,
// silently rendering the Slack/Linear plan-approval notification with
// planContentFallbackText instead of the real plan content.
func TestPlanContentText_LongEventHistory_StillFindsCurrentTurnsTokenText(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}

	eventStore := narvipg.NewEventStore(pool)

	// More filler events than planContentEventFetchLimit, all BEFORE the
	// plan-mode turn's own real token event below -- an oldest-first fetch
	// bounded to planContentEventFetchLimit would exhaust its whole budget
	// on these and never see the real one.
	const fillerCount = planContentEventFetchLimit + 500
	for i := 0; i < fillerCount; i++ {
		if _, err := eventStore.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionID,
			Type:      "message",
			MessageID: fmt.Sprintf("filler-%d", i),
			Payload:   json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("seed filler event %d: %v", i, err)
		}
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurnWithPlanMode(ctx, t, turnStore, sessionID, true, nil)

	const wantPlanText = "1. Do the thing\n2. Do the other thing"
	tokenPayload, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "token", Text: wantPlanText})
	if err != nil {
		t.Fatalf("marshal token payload: %v", err)
	}
	if _, err := eventStore.Create(ctx, sqlcgen.CreateEventParams{
		SessionID: sessionID,
		Type:      "token",
		MessageID: "plan-turn-final-token",
		Payload:   tokenPayload,
	}); err != nil {
		t.Fatalf("seed plan turn's own token event: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	if row.Kind != "slack_plan_approval" {
		t.Fatalf("Kind = %q, want %q", row.Kind, "slack_plan_approval")
	}

	var payload slackapi.PlanApprovalPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload as slackapi.PlanApprovalPayload: %v", err)
	}
	if payload.Text != wantPlanText {
		t.Errorf("Text = %q, want %q (the plan-mode turn's own real token text, not the fallback)", payload.Text, wantPlanText)
	}
}

// TestPlanContentText_ToolRowsAfterTheTextPastTheTail_StillFindsIt: a
// plan-mode turn streams its plan, then works through 600 tool steps of
// its own before it completes -- about four stored rows a step, since a
// turn's tool calls, results and step ends are stored (technical plan
// §6.1). The plan's text then lies more than 2000 rows below the log's end,
// past the newest-2000 tail planContentText read it from before, which
// sent the Slack approval notice the placeholder. It reads the turn's own
// window now, and the notice carries the plan.
func TestPlanContentText_ToolRowsAfterTheTextPastTheTail_StillFindsIt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	turn := createProcessingTurnWithPlanMode(ctx, t, turnStore, sessionID, true, nil)
	watermark, err := narvipg.NewEventStore(pool).MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the watermark: %v", err)
	}
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedEventID: &watermark}); err != nil {
		t.Fatalf("stamp dispatched_event_id: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	const wantPlanText = "1. Add the column\n2. Backfill it"
	sendTokenFrame(ctx, t, a, sessionID, "prt_plan", "")
	sendTokenFrame(ctx, t, a, sessionID, "prt_plan", wantPlanText)
	// The turn's own tool steps after its plan, stored as the actor stores
	// them (toolevent.go), in one statement for speed.
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT $1, kind.type,
		       CASE kind.type WHEN 'step_start' THEN 'msg_' || s ELSE 'msg_' || s || '#' || kind.type || ':c' || s END,
		       jsonb_build_object('type', kind.type, 'messageId', 'msg_' || s, 'output', jsonb_build_object('output', repeat('o', 200)))
		FROM generate_series(1, 600) s
		CROSS JOIN LATERAL (VALUES (1, 'step_start'), (2, 'tool_call'), (3, 'tool_result'), (4, 'step_finish')) AS kind(ord, type)
		ORDER BY s, kind.ord`, sessionID); err != nil {
		t.Fatalf("store the turn's tool steps: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	var payload slackapi.PlanApprovalPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload as slackapi.PlanApprovalPayload: %v", err)
	}
	if payload.Text != wantPlanText {
		t.Errorf("notice text = %q, want the plan %q, read from the turn's own window", payload.Text, wantPlanText)
	}
}
