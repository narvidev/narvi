//go:build integration

// Resilience test #2 (§9.3, "turn recovery"): "Kill the sandbox mid-turn -> suspect -> grace ->
// respawn+resume with same conversation id." This is the resilience
// scenario this whole Step's own re-enqueue/conversation-id work exists to
// make real -- see dispatch.go's own planReenqueueOrRespawn/
// tryPlanReenqueue for the mechanism this test proves end to end.
//
// A genuinely self-contained integration test using this codebase's own
// already-established testcontainers-Postgres pattern (matching every
// prior Step's own *_integration_test.go convention exactly), mirroring
// timerfired_integration_test.go's own TestTerminalGraceTimerFired_
// RedeliversEnsureDispatched_FreshSpawnAttempt (the terminal_grace ->
// respawn half) combined with sandboxevent_integration_test.go's own
// event-driving pattern (the Connecting -> Booting -> Ready half) into one
// full round trip against a SINGLE actor/pool -- unlike resilience_killpod_
// integration_test.go's own two-pool pod-kill harness, this scenario is
// about the SANDBOX dying, not the control-plane pod, so one long-lived
// actor genuinely living through the whole sequence (dispatch -> suspect ->
// grace -> respawn -> reconnect -> reenqueue) is the faithful shape, not a
// pod-handover race.
package sessionactor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

func TestResilience_KillSandboxMidTurn_SuspectGraceRespawnReenqueueSameConversation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	// §3.3: "the turn records the OpenCode conversation id at turn
	// start... so follow-up prompts on a fresh sandbox resume the same
	// conversation" -- seeded as already-recorded from a PRIOR turn/
	// heartbeat, exactly the precondition this scenario's own "resume with
	// same conversation id" half needs.
	sessionStore := narvipg.NewSessionStore(pool)
	existingConversationID := "conv-scenario-2-resilience"
	if _, err := sessionStore.UpdateConversationID(ctx, sqlcgen.UpdateSessionConversationIDParams{
		ID: sessionID, OpencodeConversationID: &existingConversationID,
	}); err != nil {
		t.Fatalf("seed existing conversation id: %v", err)
	}

	// --- Dispatch a turn to a real (test) sandbox at gen 1. ---
	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready (gen 1): %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	const prompt = "resume this exact conversation once the sandbox comes back"
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusProcessing,
		Prompt:    stringPtrForTest(prompt),
	})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	gen1 := int32(1)
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen1,
	}); err != nil {
		t.Fatalf("stamp dispatched_sandbox_gen=1: %v", err)
	}

	// --- Force that sandbox through Suspect (mirrors suspectrecovery_
	// integration_test.go's own seedSuspectSandboxWithPreSuspectStatus
	// precondition-seed style: a surgical direct seed of exactly the
	// (status, pre_suspect_status) pair transitionSandboxToSuspect leaves
	// behind, rather than driving a real watchdog timeout). ---
	preSuspect := sqlcgen.SandboxStatusReady
	if _, err := sandboxStore.UpdateStatusToSuspect(ctx, sqlcgen.UpdateSandboxStatusToSuspectParams{
		SessionID: sessionID, PreSuspectStatus: &preSuspect,
	}); err != nil {
		t.Fatalf("move sandbox to suspect: %v", err)
	}

	timerStore := narvipg.NewTimerStore(pool)
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID, Name: TimerTerminalGrace,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true}, // already overdue
	}); err != nil {
		t.Fatalf("arm overdue terminal_grace timer: %v", err)
	}

	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "resilience-scenario-2-gen2-object"}}
	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, provider, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// --- Let TimerTerminalGrace elapse without recovery: Suspect ->
	// Failed (already-correct, pre-existing machinery), immediately
	// followed by handleTerminalGraceTimer's own already-present
	// handleEnsureDispatched call -- now, per this Step's own fix,
	// genuinely finding the in-flight (still Processing!) turn's own
	// stale dispatched_sandbox_gen and triggering a real respawn. ---
	if err := a.Send(ctx, TimerFired{Name: TimerTerminalGrace}); err != nil {
		t.Fatalf("Send(TimerFired{terminal_grace}): %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		return provider.callCount() == 1
	})

	// --- Confirm a NEW sandbox (gen 2) gets spawned. ---
	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusConnecting
	})
	gotAfterRespawn, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox after respawn: %v", err)
	}
	if gotAfterRespawn.Gen != 2 {
		t.Fatalf("sandbox gen = %d, want 2 (a genuinely fresh respawn)", gotAfterRespawn.Gen)
	}

	// The turn was NEVER transitioned to Failed at any point in this
	// whole sequence -- it stayed Processing throughout, exactly as §3.2's
	// "a turn's own execution never really 'restarted' -- only its
	// underlying sandbox did" reasoning requires.
	gotTurnAfterRespawn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn after respawn: %v", err)
	}
	if gotTurnAfterRespawn.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %s, want still %s (never failed by the sandbox's own death)",
			gotTurnAfterRespawn.Status, sqlcgen.TurnStatusProcessing)
	}
	if commander.callCount() != 0 {
		t.Fatalf("commander.callCount() = %d, want 0 (no re-enqueue yet -- the new sandbox hasn't connected)", commander.callCount())
	}

	// --- The new gen-2 sandbox connects and boots: "ready" (Connecting ->
	// Booting), its boot_timing (gen 2's own boot evidence, §3.2), then a
	// nil-lastBootPhase "heartbeat" (Booting -> Ready). Each event's own
	// post-commit block re-evaluates EnsureDispatched (sandboxevent.go's
	// own established design). ---
	readyRaw := json.RawMessage(`{"type":"ready","messageId":"scenario2-ready","sessionId":"s","gen":2}`)
	readyOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "ready", Gen: 2, MessageID: "scenario2-ready", Raw: readyRaw,
	})
	if !readyOutcome.Persisted {
		t.Fatal("ready event: Persisted = false, want true")
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusBooting
	})

	btRaw := json.RawMessage(`{"type":"boot_timing","messageId":"scenario2-bt","sessionId":"s","gen":2,"metric":"boot_duration","seconds":42,"failed":false}`)
	if btOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "boot_timing", Gen: 2, MessageID: "scenario2-bt", Raw: btRaw,
	}); !btOutcome.Persisted {
		t.Fatal("boot_timing event: Persisted = false, want true")
	}

	hbRaw := json.RawMessage(`{"type":"heartbeat","messageId":"scenario2-hb","sessionId":"s","gen":2,"conversationId":null,"lastBootPhase":null}`)
	hbOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "heartbeat", Gen: 2, MessageID: "scenario2-hb", Raw: hbRaw, LastBootPhase: nil,
	})
	if !hbOutcome.Persisted {
		t.Fatal("heartbeat event: Persisted = false, want true")
	}

	// --- Confirm the SAME turn (never transitioned to Failed, stayed
	// Processing throughout) gets re-dispatched to gen 2, with a Prompt
	// payload carrying the correct sessions.opencode_conversation_id, and
	// confirm turns.dispatched_sandbox_gen updates to 2. ---
	waitUntil(t, 5*time.Second, func() bool {
		return commander.callCount() == 1
	})

	gotSandbox, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want %s", gotSandbox.Status, sqlcgen.SandboxStatusReady)
	}

	gotTurn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusProcessing {
		t.Errorf("turn status = %s, want still %s (re-enqueue never transitions the turn)", gotTurn.Status, sqlcgen.TurnStatusProcessing)
	}
	if gotTurn.DispatchedSandboxGen == nil || *gotTurn.DispatchedSandboxGen != 2 {
		t.Errorf("dispatched_sandbox_gen = %v, want 2", gotTurn.DispatchedSandboxGen)
	}

	var prompt2 sandboxws.Prompt
	if err := json.Unmarshal(commander.lastPayload(), &prompt2); err != nil {
		t.Fatalf("unmarshal dispatched payload as sandboxws.Prompt: %v", err)
	}
	if prompt2.SessionId != sessionID.String() {
		t.Errorf("Prompt.SessionId = %q, want %q", prompt2.SessionId, sessionID.String())
	}
	if prompt2.Gen != 2 {
		t.Errorf("Prompt.Gen = %d, want 2 (the CURRENT, respawned sandbox)", prompt2.Gen)
	}
	if prompt2.ConversationId == nil || *prompt2.ConversationId != existingConversationID {
		t.Errorf("Prompt.ConversationId = %v, want %q (§9.3 scenario #2: 'respawn+resume with same conversation id')",
			prompt2.ConversationId, existingConversationID)
	}
	if prompt2.Text != prompt {
		t.Errorf("Prompt.Text = %q, want %q", prompt2.Text, prompt)
	}

	var turnDeadlineCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerTurnDeadline,
	).Scan(&turnDeadlineCount); err != nil {
		t.Fatalf("count turn_deadline timers: %v", err)
	}
	if turnDeadlineCount != 1 {
		t.Errorf("turn_deadline timer count = %d, want 1 (a fresh, full budget re-armed for the new sandbox)", turnDeadlineCount)
	}

	var terminalGraceCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerTerminalGrace,
	).Scan(&terminalGraceCount); err != nil {
		t.Fatalf("count terminal_grace timers: %v", err)
	}
	if terminalGraceCount != 0 {
		t.Errorf("terminal_grace timer count = %d, want 0 (deleted once handled by handleTerminalGraceTimer)", terminalGraceCount)
	}
}
