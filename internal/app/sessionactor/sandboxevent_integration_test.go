//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// TestHandleSandboxEvent_FullRoundTrip drives handleSandboxEvent directly
// through Actor.Send against a real Postgres instance (bypassing
// internal/adapters/inbound/wshub entirely -- that package's own
// integration tests cover the full WS-to-ack round trip; this one isolates
// handleSandboxEvent's own transactional behavior), proving: a
// non-critical, non-transitioning event still persists and bumps
// last_seen_at with no ack; a critical event's outcome carries its ackId
// verbatim; "ready" while Connecting transitions to Booting; "heartbeat"
// with a nil lastBootPhase while Booting transitions to Ready; "ready"
// while already Ready is a silent no-op (persisted, liveness bumped,
// status unchanged, no error); and a stale (too-low) gen is rejected
// outright -- not persisted, last_seen_at untouched, no ack.
func TestHandleSandboxEvent_FullRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)

	created, err := sandboxStore.Create(ctx, sessionID)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.Gen != 1 {
		t.Fatalf("sandbox gen = %d, want 1", created.Gen)
	}

	moveTo := func(status sqlcgen.SandboxStatus) {
		t.Helper()
		if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
			SessionID: sessionID,
			Status:    status,
		}); err != nil {
			t.Fatalf("move sandbox to %s: %v", status, err)
		}
	}

	countEvents := func(eventType string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM events WHERE session_id = $1 AND type = $2`,
			sessionID, eventType,
		).Scan(&n); err != nil {
			t.Fatalf("count %s events: %v", eventType, err)
		}
		return n
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

	send := func(t *testing.T, cmd SandboxEvent) SandboxEventOutcome {
		t.Helper()
		reply := make(chan SandboxEventOutcome, 1)
		cmd.Reply = reply
		if err := a.Send(ctx, cmd); err != nil {
			t.Fatalf("Send: %v", err)
		}
		select {
		case outcome := <-reply:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for SandboxEventOutcome")
			return SandboxEventOutcome{}
		}
	}

	// --- (a) a non-critical, non-transitioning event still persists and
	// bumps last_seen_at, produces no ack. ---
	beforeToken, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}

	tokenRaw := json.RawMessage(`{"type":"token","messageId":"tok-1","sessionId":"s","gen":1}`)
	outcome := send(t, SandboxEvent{Type: "token", Gen: 1, MessageID: "tok-1", Raw: tokenRaw})
	if !outcome.Persisted {
		t.Error("token event: Persisted = false, want true")
	}
	if outcome.AckID != "" {
		t.Errorf("token event: AckID = %q, want empty (non-critical type)", outcome.AckID)
	}
	if got := countEvents("token"); got != 1 {
		t.Errorf("token event count = %d, want 1", got)
	}

	afterToken, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !afterToken.LastSeenAt.Valid {
		t.Fatal("last_seen_at not set after token event")
	}
	if beforeToken.LastSeenAt.Valid && !afterToken.LastSeenAt.Time.After(beforeToken.LastSeenAt.Time) {
		t.Errorf("last_seen_at did not advance: before=%v after=%v", beforeToken.LastSeenAt.Time, afterToken.LastSeenAt.Time)
	}
	if afterToken.Status != sqlcgen.SandboxStatusPending {
		t.Errorf("token event changed status to %s, want unchanged (%s)", afterToken.Status, sqlcgen.SandboxStatusPending)
	}

	// --- (b) a critical event persists AND its outcome carries the ackId
	// verbatim. ---
	critRaw := json.RawMessage(`{"type":"execution_complete","messageId":"m1","sessionId":"s","gen":1,"ackId":"execution_complete:m1","outcome":"completed"}`)
	outcome = send(t, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: "m1", Raw: critRaw})
	if !outcome.Persisted {
		t.Error("execution_complete: Persisted = false, want true")
	}
	if outcome.AckID != "execution_complete:m1" {
		t.Errorf("execution_complete: AckID = %q, want %q", outcome.AckID, "execution_complete:m1")
	}
	if got := countEvents("execution_complete"); got != 1 {
		t.Errorf("execution_complete event count = %d, want 1", got)
	}

	// --- (c) "ready" while Connecting transitions to Booting. ---
	moveTo(sqlcgen.SandboxStatusConnecting)
	readyRaw := json.RawMessage(`{"type":"ready","messageId":"r1","sessionId":"s","gen":1}`)
	outcome = send(t, SandboxEvent{Type: "ready", Gen: 1, MessageID: "r1", Raw: readyRaw})
	if !outcome.Persisted {
		t.Error("ready: Persisted = false, want true")
	}
	got, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Status != sqlcgen.SandboxStatusBooting {
		t.Errorf("status after ready-while-connecting = %s, want %s", got.Status, sqlcgen.SandboxStatusBooting)
	}

	// --- (d) "heartbeat" with nil lastBootPhase while Booting transitions
	// to Ready. ---
	hbRaw := json.RawMessage(`{"type":"heartbeat","messageId":"h1","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`)
	outcome = send(t, SandboxEvent{Type: "heartbeat", Gen: 1, MessageID: "h1", Raw: hbRaw, LastBootPhase: nil})
	if !outcome.Persisted {
		t.Error("heartbeat: Persisted = false, want true")
	}
	got, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("status after heartbeat-while-booting = %s, want %s", got.Status, sqlcgen.SandboxStatusReady)
	}

	// --- (e) "ready" while ALREADY Ready is a silent no-op: event still
	// persisted, last_seen_at still bumped, status stays Ready, no error. ---
	beforeReady, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // ensure a distinguishable later timestamp
	readyAgainRaw := json.RawMessage(`{"type":"ready","messageId":"r2","sessionId":"s","gen":1}`)
	outcome = send(t, SandboxEvent{Type: "ready", Gen: 1, MessageID: "r2", Raw: readyAgainRaw})
	if !outcome.Persisted {
		t.Error("second ready: Persisted = false, want true")
	}
	afterReady, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if afterReady.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("status after redundant ready = %s, want unchanged %s", afterReady.Status, sqlcgen.SandboxStatusReady)
	}
	if !afterReady.LastSeenAt.Time.After(beforeReady.LastSeenAt.Time) {
		t.Errorf("last_seen_at did not advance on redundant ready: before=%v after=%v", beforeReady.LastSeenAt.Time, afterReady.LastSeenAt.Time)
	}
	if got := countEvents("ready"); got != 2 {
		t.Errorf("ready event count = %d, want 2 (both ready frames persisted)", got)
	}

	// --- (f) a stale (too-low) gen is NOT persisted, does NOT bump
	// last_seen_at, and produces no ack. ---
	beforeStale, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	staleRaw := json.RawMessage(`{"type":"heartbeat","messageId":"h-stale","sessionId":"s","gen":0}`)
	outcome = send(t, SandboxEvent{Type: "heartbeat", Gen: 0, MessageID: "h-stale", Raw: staleRaw})
	if outcome.Persisted {
		t.Error("stale-gen event: Persisted = true, want false")
	}
	if outcome.AckID != "" {
		t.Errorf("stale-gen event: AckID = %q, want empty", outcome.AckID)
	}
	afterStale, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !afterStale.LastSeenAt.Time.Equal(beforeStale.LastSeenAt.Time) {
		t.Errorf("last_seen_at moved on a stale-gen event: before=%v after=%v", beforeStale.LastSeenAt.Time, afterStale.LastSeenAt.Time)
	}
	if got := countEvents("heartbeat"); got != 1 {
		t.Errorf("heartbeat event count = %d, want 1 (the stale-gen one must not have been persisted)", got)
	}
}

// --- §3.2 ("snapshots & restore"): triggerSnapshotBestEffort's own full
// decision tree (design decision 1) and handleSandboxEvent's new
// snapshot_ready branch (design decision 3).

// sendSandboxEvent drives cmd through Actor.Send and returns the resulting
// SandboxEventOutcome -- factored out of TestHandleSandboxEvent_FullRoundTrip's
// own local `send` closure (above) since the snapshot_ready tests below need
// the identical shape more than once.
func sendSandboxEvent(ctx context.Context, t *testing.T, a *Actor, cmd SandboxEvent) SandboxEventOutcome {
	t.Helper()
	reply := make(chan SandboxEventOutcome, 1)
	cmd.Reply = reply
	if err := a.Send(ctx, cmd); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case outcome := <-reply:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SandboxEventOutcome")
		return SandboxEventOutcome{}
	}
}

// TestTriggerSnapshotBestEffort_ReadyToSnapshotting_SendsCommand proves the
// "eligible" half of triggerSnapshotBestEffort's own decision tree: a Ready
// sandbox transitions to Snapshotting and a real, schema-valid
// sandboxws.Snapshot command is sent via SandboxCommander.SendCommand,
// carrying the sandbox's own session id and gen. Called directly (bypassing
// Actor.Send/the mailbox) -- mirrors TestExecuteSpawn_
// StaleEpochOnRecord_PropagatesErrStaleEpoch's own precedent of driving an
// unexported Actor method directly from this white-box (package
// sessionactor) test file.
func TestTriggerSnapshotBestEffort_ReadyToSnapshotting_SendsCommand(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	a.triggerSnapshotBestEffort(ctx)

	if got := commander.callCount(); got != 1 {
		t.Fatalf("SendCommand called %d times, want 1", got)
	}

	var cmd sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &cmd); err != nil {
		t.Fatalf("unmarshal SendCommand payload as sandboxws.Snapshot: %v", err)
	}
	if cmd.Type != "snapshot" {
		t.Errorf("Snapshot.Type = %q, want %q", cmd.Type, "snapshot")
	}
	if cmd.SessionId != sessionID.String() {
		t.Errorf("Snapshot.SessionId = %q, want %q", cmd.SessionId, sessionID.String())
	}
	if cmd.Gen != 1 {
		t.Errorf("Snapshot.Gen = %d, want 1", cmd.Gen)
	}
	if cmd.MessageId == "" {
		t.Error("Snapshot.MessageId is empty, want a freshly minted uuid")
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusSnapshotting {
		t.Errorf("sandbox status = %s, want %s", row.Status, sqlcgen.SandboxStatusSnapshotting)
	}
}

// TestTriggerSnapshotBestEffort_NotReady_NoOp proves the "ineligible" half:
// a sandbox that is not Ready (Booting here -- mid-boot, not yet idle) is
// left completely untouched: no transition, no SendCommand call at all.
func TestTriggerSnapshotBestEffort_NotReady_NoOp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusBooting,
	}); err != nil {
		t.Fatalf("move sandbox to booting: %v", err)
	}

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	a.triggerSnapshotBestEffort(ctx)

	if got := commander.callCount(); got != 0 {
		t.Errorf("SendCommand called %d times, want 0 (a Booting sandbox is not eligible to snapshot)", got)
	}
	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusBooting {
		t.Errorf("sandbox status = %s, want unchanged %s", row.Status, sqlcgen.SandboxStatusBooting)
	}
}

// TestTriggerSnapshotBestEffort_SendCommandFails_RevertsToReady proves the
// compensating-write half: when SendCommand fails (the snapshot command
// never reached the sandbox), the sandbox -- already committed Snapshotting
// by this point -- is reverted back to Ready by a second, small transact,
// logged, not fatal.
func TestTriggerSnapshotBestEffort_SendCommandFails_RevertsToReady(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	commander := &fakeSendCommander{nextErr: ports.ErrNoLiveSandboxConnection}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	a.triggerSnapshotBestEffort(ctx)

	if got := commander.callCount(); got != 1 {
		t.Errorf("SendCommand called %d times, want 1", got)
	}
	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want %s (reverted after the failed send, never left stuck snapshotting)", row.Status, sqlcgen.SandboxStatusReady)
	}
}

// TestHandleSnapshotReadyEvent_Normal_TransitionsToReadyAndPersistsID proves
// the normal case: a real snapshot_ready event, whose own commandMessageId
// matches the sandbox row's currently-outstanding pending_snapshot_message_id
// (message-id correlation fix), arriving while the sandbox IS Snapshotting
// transitions it to Ready and persists the reported snapshotId onto
// sandboxes.snapshot_id (clearing pending_snapshot_message_id back to nil in
// the same statement), with the ack outcome carrying the event's own ackId
// verbatim.
func TestHandleSnapshotReadyEvent_Normal_TransitionsToReadyAndPersistsID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusSnapshotting,
	}); err != nil {
		t.Fatalf("move sandbox to snapshotting: %v", err)
	}
	pendingID := "cmd-msg-1"
	if _, err := sandboxStore.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID: sessionID, PendingSnapshotMessageID: &pendingID,
	}); err != nil {
		t.Fatalf("seed pending snapshot message id: %v", err)
	}

	// No commander configured. Note that handleSandboxEvent's own
	// post-commit triggerSnapshotBestEffort call is gated on cmd.Type ==
	// "execution_complete" (design decision 1, as corrected by review) --
	// this event is "snapshot_ready", not "execution_complete", so that
	// call is never even attempted here; the sandbox simply lands on
	// Ready via handleSnapshotReadyEvent itself and stays there.
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw := json.RawMessage(`{"type":"snapshot_ready","messageId":"sr-1","sessionId":"s","gen":1,"ackId":"snapshot_ready:sr-1","snapshotId":"snap-confirmed-1","commandMessageId":"cmd-msg-1"}`)
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: raw})

	if !outcome.Persisted {
		t.Error("snapshot_ready: Persisted = false, want true")
	}
	if outcome.AckID != "snapshot_ready:sr-1" {
		t.Errorf("snapshot_ready: AckID = %q, want %q", outcome.AckID, "snapshot_ready:sr-1")
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want %s", row.Status, sqlcgen.SandboxStatusReady)
	}
	if row.SnapshotID == nil || *row.SnapshotID != "snap-confirmed-1" {
		t.Errorf("sandbox snapshot_id = %v, want %q", row.SnapshotID, "snap-confirmed-1")
	}
	if row.PendingSnapshotMessageID != nil {
		t.Errorf("sandbox pending_snapshot_message_id = %q, want nil (cleared on accept)", *row.PendingSnapshotMessageID)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'snapshot_ready'`,
		sessionID,
	).Scan(&n); err != nil {
		t.Fatalf("count snapshot_ready events: %v", err)
	}
	if n != 1 {
		t.Errorf("snapshot_ready event count = %d, want 1", n)
	}
}

// TestPostTurnSnapshotCycle_RearmsLivenessAndInactivityWatchdogsOnReturnToReady
// is the core regression test for audit finding F2 ("A routine post-turn
// snapshot permanently disarms the fast liveness_check watchdog for a
// sandbox generation"). It drives a REAL, full post-turn cycle end to end,
// through Actor.Send exactly as production traffic would: a Ready sandbox
// receives a genuine "execution_complete" SandboxEvent (turn-terminal, §3.3)
// -> handleSandboxEvent's own post-commit block calls the real
// triggerSnapshotBestEffort, which transitions Ready->Snapshotting and sends
// a real sandboxws.Snapshot command via the fake commander -> a real
// "snapshot_ready" SandboxEvent, carrying that exact command's own
// MessageId back as commandMessageId (message-id correlation, §22 design
// decision 3), completes Snapshotting->Ready via handleSnapshotReadyEvent.
//
// Before this fix: liveness_check/inactivity were never armed by ANY of
// this (they are only armed by the Booting->Ready guard in
// handleSandboxEvent, which sandboxTransitionTrigger never maps
// "snapshot_ready" onto, and handleSnapshotReadyEvent wrote row.Status
// directly, bypassing that guard entirely) -- so if either timer happened
// to already be armed from an earlier Booting->Ready transition and then
// fired while the sandbox was Snapshotting, handleLivenessCheckTimer's own
// "status != Ready -> delete, no re-arm" branch would delete it here with
// nothing to ever bring it back, even once the sandbox returned to Ready.
// This test seeds BOTH timers already armed (mirroring a sandbox that
// really did go through a real Booting->Ready transition earlier in its
// life) and asserts that after this full cycle they are not merely still
// present, but re-armed with a FRESH fires_at strictly after the cycle
// began -- proving armReadyWatchdogs' own call from handleSnapshotReadyEvent
// genuinely fired, not that some stale earlier arm merely survived by
// accident.
func TestPostTurnSnapshotCycle_RearmsLivenessAndInactivityWatchdogsOnReturnToReady(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	timerStore := narvipg.NewTimerStore(pool)
	// Seed both watchdogs already armed, as a real earlier Booting->Ready
	// transition would have left them -- with a STALE fires_at, so a fresh
	// arm from this cycle is unambiguously distinguishable from "it was
	// simply never touched".
	staleFiresAt := time.Now().Add(-1 * time.Hour)
	for _, name := range []string{TimerLivenessCheck, TimerInactivity} {
		if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
			SessionID: sessionID, Name: name,
			FiresAt: pgtype.Timestamptz{Time: staleFiresAt, Valid: true},
		}); err != nil {
			t.Fatalf("seed stale %s timer: %v", name, err)
		}
	}

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	cycleStart := time.Now()

	// --- Turn completes: a real execution_complete drives the post-commit
	// triggerSnapshotBestEffort call, Ready -> Snapshotting. ---
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})
	if !outcome.Persisted {
		t.Fatal("execution_complete: Persisted = false, want true")
	}

	waitUntil(t, 5*time.Second, func() bool {
		return commander.callCount() == 1
	})

	var snapshotCmd sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &snapshotCmd); err != nil {
		t.Fatalf("unmarshal Snapshot command: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusSnapshotting
	})

	// --- The sandbox's own real snapshot_ready reply, correlated to the
	// Snapshot command's own MessageId, completes Snapshotting -> Ready. ---
	snapshotReadyRaw := json.RawMessage(`{"type":"snapshot_ready","messageId":"sr-cycle-1","sessionId":"` +
		sessionID.String() + `","gen":1,"ackId":"snapshot_ready:sr-cycle-1","snapshotId":"snap-cycle-1","commandMessageId":"` +
		snapshotCmd.MessageId + `"}`)
	outcome = sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: snapshotReadyRaw})
	if !outcome.Persisted {
		t.Fatal("snapshot_ready: Persisted = false, want true")
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("sandbox status = %s, want %s (snapshot_ready must complete the cycle back to ready)", row.Status, sqlcgen.SandboxStatusReady)
	}

	// --- The core F2 assertion: both watchdogs are armed with a FRESH
	// fires_at, not deleted and not left at their stale seeded value. ---
	for _, name := range []string{TimerLivenessCheck, TimerInactivity} {
		timerRow, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s timer was deleted with no re-arm after the post-turn snapshot cycle returned to ready -- this is exactly audit finding F2", name)
		}
		if err != nil {
			t.Fatalf("get %s timer: %v", name, err)
		}
		if !timerRow.FiresAt.Time.After(cycleStart) {
			t.Errorf("%s fires_at = %v, want strictly after this cycle's own start %v (stale seed value was never actually re-armed)",
				name, timerRow.FiresAt.Time, cycleStart)
		}
	}
}

// TestHandleSandboxEvent_HeartbeatOnReadySandbox_DoesNotTriggerSnapshot
// reproduces, and proves fixed, the defect an independent review of this
// Step found: a routine "heartbeat" event arriving on an already-Ready,
// otherwise-idle sandbox must NOT re-trigger a snapshot cycle.
// triggerSnapshotBestEffort is only warranted on a genuine turn-terminal
// event (§3.3: "On terminal event: complete turn, trigger snapshot...");
// handleSandboxEvent's own post-commit block now gates that call on
// cmd.Type == "execution_complete" (design decision 1, as corrected by
// review) rather than on the sandbox's own Ready status alone. Two
// heartbeats are sent in a row -- mirroring the review's own two-heartbeat
// repro exactly -- to prove the fix holds on a first AND a subsequent
// heartbeat, not just the first.
func TestHandleSandboxEvent_HeartbeatOnReadySandbox_DoesNotTriggerSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	heartbeat := func(messageID string) SandboxEvent {
		return SandboxEvent{
			Type: "heartbeat", Gen: 1,
			Raw: json.RawMessage(`{"type":"heartbeat","messageId":"` + messageID + `","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`),
		}
	}

	outcome := sendSandboxEvent(ctx, t, a, heartbeat("hb-1"))
	if !outcome.Persisted {
		t.Error("first heartbeat: Persisted = false, want true")
	}
	if got := commander.callCount(); got != 0 {
		t.Fatalf("after first heartbeat: SendCommand called %d times, want 0 (a routine idle heartbeat must never trigger a snapshot cycle)", got)
	}
	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("status after first heartbeat = %s, want unchanged %s", row.Status, sqlcgen.SandboxStatusReady)
	}

	outcome = sendSandboxEvent(ctx, t, a, heartbeat("hb-2"))
	if !outcome.Persisted {
		t.Error("second heartbeat: Persisted = false, want true")
	}
	if got := commander.callCount(); got != 0 {
		t.Errorf("after second heartbeat: SendCommand called %d times, want 0 (the behavior must not start firing on a later heartbeat either)", got)
	}
	row, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("status after second heartbeat = %s, want unchanged %s", row.Status, sqlcgen.SandboxStatusReady)
	}
}

// TestHandleSandboxEvent_ExecutionComplete_TriggersSnapshotOnReadySandbox
// proves the positive half of the same gating fix: a real
// execution_complete event (a genuine turn-terminal event, §3.3) DOES
// still trigger a snapshot cycle when the sandbox is Ready -- Ready
// transitions to Snapshotting and a real sandboxws.Snapshot command is
// sent -- exactly as design decision 1 always intended, just now
// correctly gated on the event actually being turn-terminal instead of on
// every sandbox-WS frame.
func TestHandleSandboxEvent_ExecutionComplete_TriggersSnapshotOnReadySandbox(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})
	if !outcome.Persisted {
		t.Error("execution_complete: Persisted = false, want true")
	}

	// This session names no repos (createTestSession's own default), so
	// completeProcessingTurn's own pushSignal is nil and sendPushBestEffort
	// no-ops -- the only SendCommand call this test expects is the
	// snapshot one, so waiting for callCount() == 1 unambiguously observes
	// triggerSnapshotBestEffort's own asynchronous (post-Send) effect.
	waitUntil(t, 5*time.Second, func() bool {
		return commander.callCount() == 1
	})

	var cmd sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &cmd); err != nil {
		t.Fatalf("unmarshal SendCommand payload as sandboxws.Snapshot: %v", err)
	}
	if cmd.Type != "snapshot" {
		t.Errorf("SendCommand payload Type = %q, want %q", cmd.Type, "snapshot")
	}
	if cmd.SessionId != sessionID.String() {
		t.Errorf("Snapshot.SessionId = %q, want %q", cmd.SessionId, sessionID.String())
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusSnapshotting {
		t.Errorf("sandbox status = %s, want %s", row.Status, sqlcgen.SandboxStatusSnapshotting)
	}
}

// TestHandleSandboxEvent_ExecutionComplete_SnapshotRunsBeforeNextTurnDispatch
// is the regression test for audit finding F4 ("post-turn snapshot fires
// after dispatch-next, inverting section 3.3"): a real execution_complete
// event, with a pending NEXT turn already queued, must trigger
// triggerSnapshotBestEffort (Ready -> Snapshotting) BEFORE
// handleEnsureDispatched ever gets a chance to dispatch that next turn --
// so the next turn is left Pending (not dispatched, no prompt command
// sent) until the snapshot cycle later completes and returns the sandbox
// to Ready. Under the OLD (pre-fix) ordering, handleEnsureDispatched ran
// FIRST while the sandbox was still Ready, dispatched the next turn
// immediately (moving it to Processing and sending it a real prompt
// command), and only THEN did triggerSnapshotBestEffort run -- snapshotting
// a sandbox that had just been handed brand-new work, and leaving the new
// turn's own later, legitimate snapshot attempt to find the sandbox still
// Snapshotting and silently no-op.
func TestHandleSandboxEvent_ExecutionComplete_SnapshotRunsBeforeNextTurnDispatch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	completingTurn := createProcessingTurn(ctx, t, turnStore, sessionID)
	nextTurn := createPendingTurn(ctx, t, turnStore, sessionID, "the next queued prompt")

	commander := &fakeSendCommander{}
	provider := &fakeSpawnProvider{}
	r := newDispatchTestRegistry(t, ctx, pool, provider, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})
	if !outcome.Persisted {
		t.Fatal("execution_complete: Persisted = false, want true")
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusSnapshotting
	})

	// Force strict synchronization with the actor's own single mailbox
	// goroutine: the transition just observed above is committed by a
	// post-commit block that runs entirely synchronously (same goroutine,
	// no intervening yield) inside the FIRST event's own handleSandboxEvent
	// call. Sending a SECOND SandboxEvent and waiting for ITS OWN reply
	// guarantees the first event's entire handler -- including both
	// triggerSnapshotBestEffort AND handleEnsureDispatched -- has already
	// run to completion, since the actor's mailbox processes exactly one
	// command at a time, start to finish, before it ever starts the next.
	// A plain heartbeat is inert here: it matches no transition case for a
	// Snapshotting sandbox and (cmd.Type != "execution_complete") never
	// re-triggers the snapshot call.
	settle := sendSandboxEvent(ctx, t, a, SandboxEvent{
		Type: "heartbeat", Gen: 1,
		Raw: json.RawMessage(`{"type":"heartbeat","messageId":"settle-1","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`),
	})
	if !settle.Persisted {
		t.Fatal("settle heartbeat: Persisted = false, want true")
	}

	// The core F4 assertion: the next turn was NOT dispatched -- it is
	// still Pending, never touched by handleEnsureDispatched, because by
	// the time that call ran the sandbox was already Snapshotting (neither
	// Ready nor Suspect), so planDispatch's own dispatch branch never fired
	// for it.
	nextTurnRow, err := turnStore.Get(ctx, nextTurn.ID)
	if err != nil {
		t.Fatalf("get next turn: %v", err)
	}
	if nextTurnRow.Status != sqlcgen.TurnStatusPending {
		t.Errorf("next turn status = %s, want %s (F4: the snapshot must be given a chance to run BEFORE the next turn is dispatched)",
			nextTurnRow.Status, sqlcgen.TurnStatusPending)
	}

	// The completed turn really did complete (sanity: proves this is a
	// genuine execution_complete, not some other no-op path).
	completingTurnRow, err := turnStore.Get(ctx, completingTurn.ID)
	if err != nil {
		t.Fatalf("get completing turn: %v", err)
	}
	if completingTurnRow.Status != sqlcgen.TurnStatusCompleted {
		t.Errorf("completing turn status = %s, want %s", completingTurnRow.Status, sqlcgen.TurnStatusCompleted)
	}

	// The ONLY SendCommand call so far is the Snapshot one -- never a
	// prompt/dispatch payload for the next turn.
	if got := commander.callCount(); got != 1 {
		t.Fatalf("SendCommand called %d times, want 1 (the Snapshot command only -- the next turn must not have been dispatched yet)", got)
	}
	var snap sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &snap); err != nil {
		t.Fatalf("unmarshal SendCommand payload as sandboxws.Snapshot: %v", err)
	}
	if snap.Type != "snapshot" {
		t.Errorf("SendCommand payload Type = %q, want %q (the ordering fix means this must be the Snapshot call, not a turn-dispatch prompt)", snap.Type, "snapshot")
	}
}

// TestHandleSnapshotReadyEvent_LateOrDuplicate_NoOp proves the late/duplicate
// case: a snapshot_ready event arriving while the sandbox is NO LONGER
// Snapshotting (here: already Ready, e.g. because a liveness watchdog
// already resolved it some other way in the meantime) is logged and treated
// as a no-op -- the event is still persisted/acked (never a transact
// failure), but neither the status nor the previously-recorded snapshot_id
// is touched by this event's own reported (and here, ignored) snapshotId.
func TestHandleSnapshotReadyEvent_LateOrDuplicate_NoOp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	oldSnapshotID := "snap-old-already-recorded"
	if _, err := sandboxStore.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID: sessionID, SnapshotID: &oldSnapshotID,
	}); err != nil {
		t.Fatalf("seed old snapshot_id: %v", err)
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

	raw := json.RawMessage(`{"type":"snapshot_ready","messageId":"sr-2","sessionId":"s","gen":1,"ackId":"snapshot_ready:sr-2","snapshotId":"snap-new-must-be-ignored"}`)
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: raw})

	if !outcome.Persisted {
		t.Error("snapshot_ready: Persisted = false, want true (still persisted verbatim even though no transition applies)")
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want unchanged %s", row.Status, sqlcgen.SandboxStatusReady)
	}
	if row.SnapshotID == nil || *row.SnapshotID != oldSnapshotID {
		t.Errorf("sandbox snapshot_id = %v, want unchanged %q (a late/duplicate snapshot_ready must not overwrite it)", row.SnapshotID, oldSnapshotID)
	}
}

// TestSnapshotMessageIDCorrelation_StaleAttemptDiscardedNotAcceptedByLaterAttempt
// reproduces, and proves fixed, the exact race an independent review
// constructed and confirmed against a real Postgres instance (the most
// severe finding of this Step's own 3-lens adversarial review):
//
//  1. Attempt #1's SendCommand reports a failure -- the classic ambiguous-
//     write case (SendCommand is a context-bounded conn.Write; the frame
//     can already have been flushed to the OS/TCP layer despite the local
//     call erroring) -- triggering the compensating revert back to Ready.
//  2. Attempt #2 starts at the SAME gen (neither snapshot trigger is
//     gen-fenced by design) and succeeds.
//  3. Attempt #1's real, delayed snapshot_ready now arrives. Before the
//     message-id-correlation fix, the ONLY correctness check
//     handleSnapshotReadyEvent had was "is the sandbox's CURRENT status
//     Snapshotting" -- true here, satisfied by attempt #2 -- so this stale
//     event would have been wrongly accepted as completing attempt #2,
//     stamping the STALE snapshotId with no surfaced error.
//  4. This test proves the fix: the stale event (its own commandMessageId
//     matching attempt #1's, not attempt #2's currently-outstanding
//     pending id) is discarded as stale -- attempt #2 remains outstanding,
//     untouched -- and attempt #2's own real snapshot_ready subsequently
//     IS correctly accepted.
func TestSnapshotMessageIDCorrelation_StaleAttemptDiscardedNotAcceptedByLaterAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	// Attempt #1: SendCommand reports failure.
	commander := &fakeSendCommander{nextErr: ports.ErrNoLiveSandboxConnection}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	a.triggerSnapshotBestEffort(ctx)
	if got := commander.callCount(); got != 1 {
		t.Fatalf("attempt #1: SendCommand called %d times, want 1", got)
	}
	var attempt1Cmd sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &attempt1Cmd); err != nil {
		t.Fatalf("unmarshal attempt #1 Snapshot command: %v", err)
	}
	attempt1MessageID := attempt1Cmd.MessageId
	if attempt1MessageID == "" {
		t.Fatal("attempt #1 MessageId is empty")
	}

	// Confirm the compensating revert already ran: back to Ready, pending
	// id cleared.
	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("after attempt #1's failed send, status = %s, want %s", row.Status, sqlcgen.SandboxStatusReady)
	}
	if row.PendingSnapshotMessageID != nil {
		t.Fatalf("after attempt #1's revert, pending_snapshot_message_id = %v, want nil", *row.PendingSnapshotMessageID)
	}

	// Attempt #2: a later, genuinely successful attempt at the SAME gen.
	commander.nextErr = nil
	a.triggerSnapshotBestEffort(ctx)
	if got := commander.callCount(); got != 2 {
		t.Fatalf("attempt #2: SendCommand called %d times total, want 2", got)
	}
	var attempt2Cmd sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &attempt2Cmd); err != nil {
		t.Fatalf("unmarshal attempt #2 Snapshot command: %v", err)
	}
	attempt2MessageID := attempt2Cmd.MessageId
	if attempt2MessageID == "" || attempt2MessageID == attempt1MessageID {
		t.Fatalf("attempt #2 MessageId = %q, want a fresh id distinct from attempt #1's %q", attempt2MessageID, attempt1MessageID)
	}

	row, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusSnapshotting {
		t.Fatalf("after attempt #2's send, status = %s, want %s", row.Status, sqlcgen.SandboxStatusSnapshotting)
	}
	if row.PendingSnapshotMessageID == nil || *row.PendingSnapshotMessageID != attempt2MessageID {
		t.Fatalf("after attempt #2's send, pending_snapshot_message_id = %v, want %q", row.PendingSnapshotMessageID, attempt2MessageID)
	}

	// Attempt #1's real, delayed snapshot_ready now arrives late, carrying
	// attempt #1's own commandMessageId -- NOT attempt #2's, which is the
	// one currently outstanding.
	staleRaw := json.RawMessage(`{"type":"snapshot_ready","messageId":"stale-evt","sessionId":"s","gen":1,"ackId":"snapshot_ready:stale-evt","snapshotId":"snap-STALE-must-be-discarded","commandMessageId":"` + attempt1MessageID + `"}`)
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: staleRaw})
	if !outcome.Persisted {
		t.Error("attempt #1's late snapshot_ready: Persisted = false, want true (still persisted verbatim)")
	}

	row, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusSnapshotting {
		t.Errorf("after the STALE event, status = %s, want unchanged %s (attempt #2 must still be outstanding)", row.Status, sqlcgen.SandboxStatusSnapshotting)
	}
	if row.SnapshotID != nil {
		t.Errorf("after the STALE event, snapshot_id = %q, want nil (the stale attempt's snapshotId must never be recorded)", *row.SnapshotID)
	}
	if row.PendingSnapshotMessageID == nil || *row.PendingSnapshotMessageID != attempt2MessageID {
		t.Errorf("after the STALE event, pending_snapshot_message_id = %v, want unchanged %q (attempt #2's own pending id must survive the stale delivery)", row.PendingSnapshotMessageID, attempt2MessageID)
	}

	// Attempt #2's own real snapshot_ready then arrives and IS correctly
	// accepted.
	realRaw := json.RawMessage(`{"type":"snapshot_ready","messageId":"real-evt","sessionId":"s","gen":1,"ackId":"snapshot_ready:real-evt","snapshotId":"snap-real-attempt-2","commandMessageId":"` + attempt2MessageID + `"}`)
	outcome = sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: realRaw})
	if !outcome.Persisted {
		t.Error("attempt #2's real snapshot_ready: Persisted = false, want true")
	}

	row, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("after attempt #2's real snapshot_ready, status = %s, want %s", row.Status, sqlcgen.SandboxStatusReady)
	}
	if row.SnapshotID == nil || *row.SnapshotID != "snap-real-attempt-2" {
		t.Errorf("after attempt #2's real snapshot_ready, snapshot_id = %v, want %q", row.SnapshotID, "snap-real-attempt-2")
	}
	if row.PendingSnapshotMessageID != nil {
		t.Errorf("after attempt #2's real snapshot_ready, pending_snapshot_message_id = %v, want nil (cleared on accept)", *row.PendingSnapshotMessageID)
	}
}

// TestHandleSnapshotReadyEvent_DecodeFailure_RevertsToReadyInsteadOfWedging
// proves Finding 2's own fix: a snapshot_ready that fails schema decode
// (genuinely malformed per sandboxws.SnapshotReady's own generated
// UnmarshalJSON -- e.g. missing a required field -- which wshub's own
// permissive read-loop peek does NOT filter out before constructing a
// SandboxEvent) must revert the sandbox Snapshotting->Ready, exactly like
// the SendCommand-failure path already does, instead of leaving it
// permanently stuck Snapshotting (no watchdog covers that state).
func TestHandleSnapshotReadyEvent_DecodeFailure_RevertsToReadyInsteadOfWedging(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusSnapshotting,
	}); err != nil {
		t.Fatalf("move sandbox to snapshotting: %v", err)
	}
	pendingID := "pending-msg-1"
	if _, err := sandboxStore.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID: sessionID, PendingSnapshotMessageID: &pendingID,
	}); err != nil {
		t.Fatalf("seed pending snapshot message id: %v", err)
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

	// Syntactically valid JSON, but missing "ackId" and "snapshotId" --
	// both required by sandboxws.SnapshotReady's own generated
	// UnmarshalJSON, so json.Unmarshal into that type fails even though
	// wshub's own permissive envelope peek (type/gen/lastBootPhase only)
	// already let it through as a SandboxEvent.
	malformedRaw := json.RawMessage(`{"type":"snapshot_ready","messageId":"m-bad","sessionId":"s","gen":1}`)
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: malformedRaw})

	if !outcome.Persisted {
		t.Error("malformed snapshot_ready: Persisted = false, want true (the raw event and the compensating revert commit together)")
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want %s (a decode failure must revert, not leave the sandbox stuck snapshotting)", row.Status, sqlcgen.SandboxStatusReady)
	}
	if row.PendingSnapshotMessageID != nil {
		t.Errorf("pending_snapshot_message_id = %q, want nil (cleared by the revert)", *row.PendingSnapshotMessageID)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'snapshot_ready'`,
		sessionID,
	).Scan(&n); err != nil {
		t.Fatalf("count snapshot_ready events: %v", err)
	}
	if n != 1 {
		t.Errorf("snapshot_ready event count = %d, want 1 (persisted verbatim despite the decode failure)", n)
	}
}

// TestHandleSnapshotReadyEvent_DecodeFailureRevert_RearmsLivenessAndInactivityWatchdogs
// covers audit finding F2's second landing-on-ready path: revertSnapshotToReady's
// own compensating write (here reached via handleSnapshotReadyEvent's
// decode-failure branch -- the same malformed-payload repro
// TestHandleSnapshotReadyEvent_DecodeFailure_RevertsToReadyInsteadOfWedging
// above already proves reverts Snapshotting->Ready) must ALSO re-arm both
// watchdogs, exactly like the success path does -- since
// revertSnapshotToReady is the single shared helper behind BOTH of its own
// callers (this decode-failure branch, and revertSnapshotBestEffort's own
// SendCommand-failure path), one passing test here covers both by
// construction.
func TestHandleSnapshotReadyEvent_DecodeFailureRevert_RearmsLivenessAndInactivityWatchdogs(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusSnapshotting,
	}); err != nil {
		t.Fatalf("move sandbox to snapshotting: %v", err)
	}
	pendingID := "pending-msg-revert-rearm"
	if _, err := sandboxStore.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID: sessionID, PendingSnapshotMessageID: &pendingID,
	}); err != nil {
		t.Fatalf("seed pending snapshot message id: %v", err)
	}

	timerStore := narvipg.NewTimerStore(pool)
	staleFiresAt := time.Now().Add(-1 * time.Hour)
	for _, name := range []string{TimerLivenessCheck, TimerInactivity} {
		if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
			SessionID: sessionID, Name: name,
			FiresAt: pgtype.Timestamptz{Time: staleFiresAt, Valid: true},
		}); err != nil {
			t.Fatalf("seed stale %s timer: %v", name, err)
		}
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

	revertStart := time.Now()

	// Same genuinely-malformed-per-schema payload as the sibling test above
	// (missing "ackId"/"snapshotId") -- drives the exact same decode-failure
	// -> revertSnapshotToReady path.
	malformedRaw := json.RawMessage(`{"type":"snapshot_ready","messageId":"m-bad-rearm","sessionId":"s","gen":1}`)
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: malformedRaw})
	if !outcome.Persisted {
		t.Fatal("malformed snapshot_ready: Persisted = false, want true")
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("sandbox status = %s, want %s (decode failure must revert to ready)", row.Status, sqlcgen.SandboxStatusReady)
	}

	for _, name := range []string{TimerLivenessCheck, TimerInactivity} {
		timerRow, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s timer was deleted with no re-arm after the decode-failure revert landed back on ready -- audit finding F2", name)
		}
		if err != nil {
			t.Fatalf("get %s timer: %v", name, err)
		}
		if !timerRow.FiresAt.Time.After(revertStart) {
			t.Errorf("%s fires_at = %v, want strictly after this revert's own start %v (stale seed value was never actually re-armed)",
				name, timerRow.FiresAt.Time, revertStart)
		}
	}
}

// TestHandleSandboxEvent_ArmsLivenessAndInactivityOnceOnBootingToReady
// proves the fix for the confirmed HIGH-severity audit finding
// (sessionactor-ports & docs-completeness-vs-plan lenses):
// liveness_check and inactivity are armed for the first time exactly at
// the real Booting->Ready transition -- driven through a real
// handleSandboxEvent via Actor.Send, exactly matching how
// sandboxTransitionTrigger documents "heartbeat" with a nil
// LastBootPhase as the Booting->Ready trigger -- and are NOT re-armed
// (their fires_at pushed forward) by a later heartbeat while already
// Ready, proving the once-only guard rather than a
// re-arm-every-heartbeat regression. Reads session_timers directly via
// raw SQL/TimerStore, never trusting handleSandboxEvent's own return
// value alone.
func TestHandleSandboxEvent_ArmsLivenessAndInactivityOnceOnBootingToReady(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	timerStore := narvipg.NewTimerStore(pool)

	created, err := sandboxStore.Create(ctx, sessionID)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID,
		Status:    sqlcgen.SandboxStatusBooting,
	}); err != nil {
		t.Fatalf("move sandbox to booting: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	send := func(t *testing.T, cmd SandboxEvent) SandboxEventOutcome {
		t.Helper()
		reply := make(chan SandboxEventOutcome, 1)
		cmd.Reply = reply
		if err := a.Send(ctx, cmd); err != nil {
			t.Fatalf("Send: %v", err)
		}
		select {
		case outcome := <-reply:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for SandboxEventOutcome")
			return SandboxEventOutcome{}
		}
	}

	getFiresAt := func(t *testing.T, name string) (time.Time, bool) {
		t.Helper()
		row, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false
		}
		if err != nil {
			t.Fatalf("get %s timer: %v", name, err)
		}
		return row.FiresAt.Time, true
	}

	beforeHeartbeat := time.Now()

	// First heartbeat, nil lastBootPhase, while Booting -> transitions to
	// Ready (sandboxTransitionTrigger's own documented (b) mapping).
	hbRaw := json.RawMessage(`{"type":"heartbeat","messageId":"h1","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`)
	outcome := send(t, SandboxEvent{Type: "heartbeat", Gen: int(created.Gen), MessageID: "h1", Raw: hbRaw, LastBootPhase: nil})
	if !outcome.Persisted {
		t.Fatal("first heartbeat: Persisted = false, want true")
	}

	gotSandbox, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("status after heartbeat-while-booting = %s, want %s", gotSandbox.Status, sqlcgen.SandboxStatusReady)
	}

	livenessFiresAt, ok := getFiresAt(t, TimerLivenessCheck)
	if !ok {
		t.Fatal("liveness_check timer was not armed by the Booting->Ready transition -- the confirmed gap this batch fixes")
	}
	inactivityFiresAt, ok := getFiresAt(t, TimerInactivity)
	if !ok {
		t.Fatal("inactivity timer was not armed by the Booting->Ready transition -- the confirmed gap this batch fixes")
	}

	const tolerance = 5 * time.Second
	if want := beforeHeartbeat.Add(timeouts.SteadyHeartbeatBudget); absDuration(livenessFiresAt.Sub(want)) > tolerance {
		t.Errorf("liveness_check fires_at = %v, want ~%v (now+SteadyHeartbeatBudget, +/-%v)", livenessFiresAt, want, tolerance)
	}
	if want := beforeHeartbeat.Add(timeouts.InactivityMinCheckInterval); absDuration(inactivityFiresAt.Sub(want)) > tolerance {
		t.Errorf("inactivity fires_at = %v, want ~%v (now+InactivityMinCheckInterval, +/-%v)", inactivityFiresAt, want, tolerance)
	}

	// Second heartbeat, still Ready (nil lastBootPhase again -- a real
	// sandbox reports it on every heartbeat regardless of phase):
	// sandboxTransitionTrigger's own (b) mapping requires status ==
	// Booting, which no longer holds, so this is NOT a transition -- just
	// a liveness-only bump. Neither timer's fires_at must move: proving
	// the once-only guard, not a re-arm-every-heartbeat regression that
	// would starve liveness_check of ever actually firing.
	hb2Raw := json.RawMessage(`{"type":"heartbeat","messageId":"h2","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`)
	outcome = send(t, SandboxEvent{Type: "heartbeat", Gen: int(created.Gen), MessageID: "h2", Raw: hb2Raw, LastBootPhase: nil})
	if !outcome.Persisted {
		t.Fatal("second heartbeat: Persisted = false, want true")
	}

	gotSandbox, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("status after second heartbeat = %s, want unchanged %s", gotSandbox.Status, sqlcgen.SandboxStatusReady)
	}

	livenessFiresAt2, ok := getFiresAt(t, TimerLivenessCheck)
	if !ok {
		t.Fatal("liveness_check timer disappeared after second heartbeat")
	}
	inactivityFiresAt2, ok := getFiresAt(t, TimerInactivity)
	if !ok {
		t.Fatal("inactivity timer disappeared after second heartbeat")
	}
	if !livenessFiresAt2.Equal(livenessFiresAt) {
		t.Errorf("liveness_check fires_at changed on a second, already-Ready heartbeat: before=%v after=%v (must stay untouched)", livenessFiresAt, livenessFiresAt2)
	}
	if !inactivityFiresAt2.Equal(inactivityFiresAt) {
		t.Errorf("inactivity fires_at changed on a second, already-Ready heartbeat: before=%v after=%v (must stay untouched)", inactivityFiresAt, inactivityFiresAt2)
	}
}

// blockingCommander is a test-only ports.SandboxCommander whose
// SendCommand call closes started (the FIRST time it is ever called, only)
// and then blocks until release is closed -- a controllable synchronization
// point, not a sleep/timing guess, for proving a happens-before ordering
// between two goroutines-in-effect (here: the actor's own single
// mailbox-processing goroutine, at two different points in its own
// sequential execution of handleSandboxEvent).
type blockingCommander struct {
	started chan struct{}
	release chan struct{}
}

var _ ports.SandboxCommander = (*blockingCommander)(nil)

func newBlockingCommander() *blockingCommander {
	return &blockingCommander{started: make(chan struct{}), release: make(chan struct{})}
}

func (c *blockingCommander) SendCommand(string, json.RawMessage) error {
	close(c.started)
	<-c.release
	return nil
}

// TestHandleSandboxEvent_AckReplySentBeforeSlowPostCommitSideEffects proves
// Finding 1's own fix: cmd.Reply already carries an outcome BEFORE the
// slow best-effort post-commit side effect a real execution_complete
// triggers (sendPushBestEffort's own SandboxCommander.SendCommand call)
// is ever invoked -- not merely before it returns. The proof is a real
// happens-before relationship established via channels, not a sleep or
// timing guess (matching internal/adapters/outbound/opencode's own
// TestFinalize_LateSubtaskEmitIsDroppedNotRacedPastExecutionComplete
// precedent for this kind of controllable-blocking-point test):
// blockingCommander.SendCommand only closes `started` the instant it is
// entered, then blocks indefinitely on `release`. Since handleSandboxEvent
// runs entirely on the actor's own single command-processing goroutine,
// `started` being closed proves SendCommand has genuinely been reached --
// and, under the OLD (wrong) ordering this batch fixes, cmd.Reply would
// not have been sent yet at that exact instant (it ran AFTER
// sendPushBestEffort returned); under the fixed ordering, the reply was
// already placed on the buffered reply channel strictly before
// handleEnsureDispatched/sendPushBestEffort ever ran, so a non-blocking
// receive on reply must already succeed the moment `started` fires.
func TestHandleSandboxEvent_AckReplySentBeforeSlowPostCommitSideEffects(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{},
		"repo1", "https://github.com/acme/repo1.git", "feature-x")
	// §30.7/§30.9: sendPushBestEffort now honors this turn's own frozen
	// push/PR egress decision -- without arming this repo live, its
	// fail-closed default is shadow (egressmode's own doc comment) and
	// SendCommand (the exact side effect this test blocks on) would never
	// be invoked at all. This test proves the ack-before-side-effect
	// ORDERING, unrelated to §30's own shadow/live distinction.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/repo1", true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	commander := newBlockingCommander()
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	reply := make(chan SandboxEventOutcome, 1)
	cmd := SandboxEvent{
		Type:  "execution_complete",
		Gen:   1,
		Raw:   executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
		Reply: reply,
	}
	if err := a.Send(ctx, cmd); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Wait for SendCommand to have genuinely been entered -- proof that
	// handleSandboxEvent's own transact already committed (a "completed"
	// execution_complete only ever reaches sendPushBestEffort's
	// commander.SendCommand call AFTER its transact commits, pushpr.go),
	// and that handleEnsureDispatched has already run too (it runs first,
	// per the fix's own ordering).
	select {
	case <-commander.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the blocking SandboxCommander.SendCommand to be invoked")
	}

	// At this EXACT instant, SendCommand is still blocked on release (never
	// closed yet below) -- so if cmd.Reply already has a value ready RIGHT
	// NOW, it was necessarily placed there strictly before SendCommand was
	// ever called, since both run sequentially on the same goroutine.
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Error("outcome.Persisted = false, want true")
		}
		if outcome.AckID == "" {
			t.Error("outcome.AckID is empty, want a real ack for this critical event")
		}
	default:
		t.Fatal("cmd.Reply had no value ready while the slow post-commit side effect (SandboxCommander.SendCommand) was still blocked -- the ack is being delayed by a side effect, the exact regression this batch fixes")
	}

	// Release the blocked SendCommand so the actor can finish handling
	// this command and shut down cleanly.
	close(commander.release)
}

// TestHandleSandboxEvent_ReadyPersistsBootFingerprint is §12.2 item 1's
// own runtime-fingerprint gap, end to end through handleSandboxEvent:
// a "ready" event's real AgentVersion/ImageDigest lands on the sandboxes
// row: a LATER, unrelated event (heartbeat, carrying neither field) must
// never clobber them back to NULL (UpdateSandboxStatus's own
// COALESCE-guarded columns); and a fresh respawn (UpsertSandboxForSpawn)
// must reset them to NULL -- a connecting/booting new gen must never
// display its predecessor's now-stale fingerprint.
func TestHandleSandboxEvent_ReadyPersistsBootFingerprint(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
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

	send := func(t *testing.T, cmd SandboxEvent) SandboxEventOutcome {
		t.Helper()
		reply := make(chan SandboxEventOutcome, 1)
		cmd.Reply = reply
		if err := a.Send(ctx, cmd); err != nil {
			t.Fatalf("Send: %v", err)
		}
		select {
		case outcome := <-reply:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for SandboxEventOutcome")
			return SandboxEventOutcome{}
		}
	}

	agentVersion, imageDigest := "v1.4.2", "sha256:9f31c00"
	readyRaw := json.RawMessage(`{"type":"ready","messageId":"r1","sessionId":"s","gen":1,"agentVersion":"v1.4.2","imageDigest":"sha256:9f31c00"}`)
	outcome := send(t, SandboxEvent{Type: "ready", Gen: 1, MessageID: "r1", Raw: readyRaw, AgentVersion: &agentVersion, ImageDigest: &imageDigest})
	if !outcome.Persisted {
		t.Fatal("ready: Persisted = false, want true")
	}

	got, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.AgentVersion == nil || *got.AgentVersion != agentVersion {
		t.Errorf("AgentVersion = %v, want %q", got.AgentVersion, agentVersion)
	}
	if got.ImageDigest == nil || *got.ImageDigest != imageDigest {
		t.Errorf("ImageDigest = %v, want %q", got.ImageDigest, imageDigest)
	}

	// A later, unrelated event (heartbeat, carrying neither field) must
	// never clobber the fingerprint back to NULL.
	hbRaw := json.RawMessage(`{"type":"heartbeat","messageId":"h1","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`)
	if outcome := send(t, SandboxEvent{Type: "heartbeat", Gen: 1, MessageID: "h1", Raw: hbRaw}); !outcome.Persisted {
		t.Fatal("heartbeat: Persisted = false, want true")
	}
	got, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox after heartbeat: %v", err)
	}
	if got.AgentVersion == nil || *got.AgentVersion != agentVersion {
		t.Errorf("AgentVersion after unrelated heartbeat = %v, want unchanged %q (a non-ready event must never clobber the fingerprint)", got.AgentVersion, agentVersion)
	}
	if got.ImageDigest == nil || *got.ImageDigest != imageDigest {
		t.Errorf("ImageDigest after unrelated heartbeat = %v, want unchanged %q", got.ImageDigest, imageDigest)
	}

	// A fresh respawn (the NEXT gen) must reset the fingerprint to NULL --
	// UpsertSandboxForSpawn's own doc comment: a connecting/booting new
	// gen must never display its predecessor's now-stale value.
	respawned, err := sandboxStore.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: nil})
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if respawned.Gen != 2 {
		t.Fatalf("respawned gen = %d, want 2", respawned.Gen)
	}
	if respawned.AgentVersion != nil {
		t.Errorf("AgentVersion after respawn = %v, want nil (reset -- this new gen has not reported 'ready' yet)", respawned.AgentVersion)
	}
	if respawned.ImageDigest != nil {
		t.Errorf("ImageDigest after respawn = %v, want nil", respawned.ImageDigest)
	}
}

// absDuration returns d's absolute value.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
