//go:build integration

// This file proves §25.15's own control-plane wiring end to end: a real
// step_finish sandbox-ws event, driven through the real handleSandboxEvent
// entry point (exactly like every other test in this package -- see
// boottiming_integration_test.go's own top comment), genuinely adds its
// own cost.usd onto the session's currently-processing turn's own
// cost_usd running total -- and a forced §6.1 reconnect replay (the SAME
// raw bytes delivered twice) leaves that total holding the increment
// exactly once, never twice, mirroring boot_timing's own identical
// redelivery-safety proof one wire event type over.
package sessionactor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// stepFinishRaw marshals a real, schema-valid sandboxws.StepFinish wire
// payload -- mirrors bootTimingRaw's own shape exactly
// (boottiming_integration_test.go), including its own "return the
// freshly-minted messageId separately" convention, needed for the
// redelivery test below to resend the IDENTICAL raw bytes a second time.
// usd is a pointer so the nil case (§6.1: cost.usd is optional/nullable)
// can be exercised directly, mirroring StepFinishCost.Usd's own
// *float64-shaped generated type.
//
// NOTE the fresh message id per call: that is what the SUMMATION tests
// need (two step_finish events must be two distinct events), and it is
// NOT what production sends -- a real step_finish shares its message id
// with the step_start that preceded it. Relying on this helper alone once
// hid a bug that made every production cost write a no-op, so the
// production shape has its own test above
// (TestHandleSandboxEvent_StepStartThenStepFinish_SharedMessageID_
// StillRecordsCost). Do not treat a green run of the tests below as
// evidence that the wire path works.
func stepFinishRaw(t *testing.T, sessionID string, gen int, usd *float64) (json.RawMessage, string) {
	t.Helper()
	messageID := uuid.NewString()
	evt := sandboxws.StepFinish{
		Type:      "step_finish",
		MessageId: messageID,
		SessionId: sessionID,
		Gen:       gen,
		StepId:    "step-" + messageID,
		Cost: sandboxws.StepFinishCost{
			Tokens: sandboxws.StepFinishCostTokens{Input: 10, Output: 20},
			Usd:    sandboxws.StepFinishCostUsd(usd),
		},
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal step_finish: %v", err)
	}
	return raw, messageID
}

// costUSDf64 is a small helper so call sites can write costUSDf64(1.23)
// inline rather than declaring a local variable just to take its address.
func costUSDf64(v float64) *float64 { return &v }

// TestHandleSandboxEvent_StepFinish_AddsCostToProcessingTurn proves the
// happy path: a step_finish carrying a real cost.usd adds exactly that
// amount onto the session's own currently-processing turn.
func TestHandleSandboxEvent_StepFinish_AddsCostToProcessingTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	turn := createProcessingTurn(ctx, t, turnStore, sessionID)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw, messageID := stepFinishRaw(t, sessionID.String(), 1, costUSDf64(1.23))
	outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type:      "step_finish",
		Gen:       1,
		MessageID: messageID,
		Raw:       raw,
	})
	if !outcome.Persisted {
		t.Error("outcome.Persisted = false, want true")
	}

	got, err := turnStore.Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	v, ok := appreviewtriage.NumericToFloat64(got.CostUsd)
	if !ok {
		t.Fatal("turn cost_usd is NULL after a step_finish with a real cost.usd, want 1.23")
	}
	if v != 1.23 {
		t.Errorf("turn cost_usd = %v, want 1.23", v)
	}
}

// TestHandleSandboxEvent_StepStartThenStepFinish_SharedMessageID_StillRecordsCost
// is the shape production ACTUALLY sends, and the one every other test in
// this file was missing.
//
// OpenCode emits step-start and step-finish as two parts of the SAME
// assistant message, and both wire events carry that enclosing message's
// id (translate.go -- the token event is the only part-derived event that
// uses its own part id). The step_start therefore claims the
// (session_id, message_id) row in the events table, and the step_finish
// upserts onto it and comes back inserted=false.
//
// An earlier version of the cost write gated on exactly that flag, so no
// production step_finish ever reached it and turns.cost_usd was NULL for
// every turn that had ever run. Every test here passed anyway, because the
// fixture minted a fresh message id per step_finish and never sent the
// step_start that always precedes it. This test sends both, in order, with
// the shared id -- if the cost write ever goes back to keying on anything
// but the step id, this is what fails.
func TestHandleSandboxEvent_StepStartThenStepFinish_SharedMessageID_StillRecordsCost(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	turn := createProcessingTurn(ctx, t, turnStore, sessionID)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw, messageID := stepFinishRaw(t, sessionID.String(), 1, costUSDf64(1.23))

	// The step_start that precedes it in every real turn, carrying the
	// SAME message id -- this is what claims the events row first.
	startRaw, err := json.Marshal(map[string]any{
		"type": "step_start", "messageId": messageID,
		"sessionId": sessionID.String(), "gen": 1, "stepId": "start-" + messageID,
	})
	if err != nil {
		t.Fatalf("marshal step_start: %v", err)
	}
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_start", Gen: 1, MessageID: messageID, Raw: startRaw})
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: 1, MessageID: messageID, Raw: raw})

	got, err := turnStore.Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	v, ok := appreviewtriage.NumericToFloat64(got.CostUsd)
	if !ok {
		t.Fatal("turn cost_usd is NULL after step_start + step_finish sharing one messageId -- the production shape; want 1.23")
	}
	if v != 1.23 {
		t.Errorf("turn cost_usd = %v, want 1.23", v)
	}
}

// TestHandleSandboxEvent_StepFinish_SumsAcrossMultipleEvents proves
// accumulation, not replacement: TWO distinct step_finish events (main
// lane and, in production, potentially a §7.1 sub-task lane -- this test
// only needs two DIFFERENT messageIds to prove summation, not the fan-out
// itself) must both land, summed.
func TestHandleSandboxEvent_StepFinish_SumsAcrossMultipleEvents(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	turn := createProcessingTurn(ctx, t, turnStore, sessionID)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw1, msgID1 := stepFinishRaw(t, sessionID.String(), 1, costUSDf64(0.10))
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: 1, MessageID: msgID1, Raw: raw1})

	raw2, msgID2 := stepFinishRaw(t, sessionID.String(), 1, costUSDf64(0.25))
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: 1, MessageID: msgID2, Raw: raw2})

	got, err := turnStore.Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	v, ok := appreviewtriage.NumericToFloat64(got.CostUsd)
	if !ok {
		t.Fatal("turn cost_usd is NULL after two step_finish events, want 0.35")
	}
	if v != 0.35 {
		t.Errorf("turn cost_usd = %v, want 0.35 (0.10 + 0.25, summed not replaced)", v)
	}
}

// TestHandleSandboxEvent_RedeliveredStepFinish_DoesNotDoubleCountCost
// proves the SAME raw bytes (identical messageId), sent through
// handleSandboxEvent TWICE -- reproducing a real §6.1 buffered-resend
// replay of a not-yet-acked event before reconnect (step_finish is not
// one of the 6 critical acked types, but CreateEvent's own upsert-on-
// messageId dedup applies to every event type, not only the critical
// ones) -- leaves the turn's own cost_usd holding that ONE step_finish's
// cost exactly once, never twice.
func TestHandleSandboxEvent_RedeliveredStepFinish_DoesNotDoubleCountCost(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	turn := createProcessingTurn(ctx, t, turnStore, sessionID)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw, messageID := stepFinishRaw(t, sessionID.String(), 1, costUSDf64(2.00))
	first := sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: 1, MessageID: messageID, Raw: raw})
	if !first.Persisted {
		t.Fatal("first delivery: outcome.Persisted = false, want true")
	}
	second := sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: 1, MessageID: messageID, Raw: raw})
	if !second.Persisted {
		t.Fatal("redelivery: outcome.Persisted = false, want true (still acked, just deduped)")
	}

	got, err := turnStore.Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	v, ok := appreviewtriage.NumericToFloat64(got.CostUsd)
	if !ok {
		t.Fatal("turn cost_usd is NULL after a step_finish delivered twice, want 2.00")
	}
	if v != 2.00 {
		t.Errorf("turn cost_usd = %v, want 2.00 (the SAME step_finish counted once, not twice)", v)
	}
}

// TestHandleSandboxEvent_StepFinish_NilUsd_LeavesCostNull proves §25.15's
// own "no cost yet must never render as free" requirement holds even at
// the wire-decode level: a step_finish whose cost.usd is absent/null
// (§6.1: OPTIONAL) must leave the turn's own cost_usd column genuinely
// NULL, never a fabricated 0.
func TestHandleSandboxEvent_StepFinish_NilUsd_LeavesCostNull(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	turn := createProcessingTurn(ctx, t, turnStore, sessionID)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw, messageID := stepFinishRaw(t, sessionID.String(), 1, nil)
	outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: 1, MessageID: messageID, Raw: raw})
	if !outcome.Persisted {
		t.Error("outcome.Persisted = false, want true")
	}

	got, err := turnStore.Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if v, ok := appreviewtriage.NumericToFloat64(got.CostUsd); ok {
		t.Errorf("turn cost_usd = %v (valid), want NULL (cost.usd was absent on the wire)", v)
	}
}

// TestStepCost_StepFinishStored_CountedOnce: a step_finish in the shape
// production sends -- under its message's id, behind that message's
// step_start -- is stored once, under the key its stepId derives, and its
// cost is counted once; a resend of the whole message, as a reconnect
// replays it, adds neither a row nor money. The page sums its cost from
// stored step_finish rows (web/src/session/costRollup.ts), so before the
// row was stored the page read none, while turns.cost_usd, keyed on the
// stepId, was right all along.
func TestStepCost_StepFinishStored_CountedOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turn := createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	message := oneMessageForTest(t, sessionID.String(), "msg_1")
	for range 2 { // sent, then replayed
		for _, cmd := range message {
			sendSandboxEventForTest(ctx, t, a, cmd)
		}
	}

	var rows int
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT count(*), max(payload::text) FROM events WHERE session_id = $1 AND type = 'step_finish'`, sessionID).Scan(&rows, &stored); err != nil {
		t.Fatalf("read the stored step_finish: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d step_finish rows, want exactly 1", rows)
	}
	var finish sandboxws.StepFinish
	if err := json.Unmarshal(stored, &finish); err != nil || finish.Cost.Usd == nil || *finish.Cost.Usd != 0.25 || finish.Cost.Tokens.Input != 100 {
		t.Fatalf("stored step_finish = %s (%v), want the step's cost, $0.25 and 100 tokens in", stored, err)
	}

	got, err := narvipg.NewTurnStore(pool).Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if v, ok := appreviewtriage.NumericToFloat64(got.CostUsd); !ok || v != 0.25 {
		t.Fatalf("turn cost_usd = %v (valid %v), want 0.25, counted once", v, ok)
	}
	var steps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turn_step_costs WHERE turn_id = $1`, turn.ID).Scan(&steps); err != nil {
		t.Fatalf("count turn_step_costs: %v", err)
	}
	if steps != 1 {
		t.Fatalf("%d turn_step_costs rows, want 1", steps)
	}
}
