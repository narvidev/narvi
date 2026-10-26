//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// This file proves technical plan §35.5's instrumentation on the real
// actor and real Postgres: a snapshot records, at mint, the conversation
// the session held, keyed by its id; every restore compares it with the
// conversation the restored gen's prompts will name, and counts the
// outcome on sandbox_restore_conversation_total once the claim has
// committed -- a WARN line for differs, an INFO line for unknown. Nothing
// else: such a restore writes no warning, keeps the session's conversation
// and sends the request as it is, with no recap.

// compatibleProvenance is a snapshot_ready provenance this control plane
// restores without a line of its own.
var compatibleProvenance = fmt.Sprintf(`{"agentProtocol":%q}`, contracts.Version)

// previousBinaryUpdateSnapshot is UpdateSandboxSnapshotID as the control
// plane before migration 000177 sends it, without its RETURNING list: the
// snapshot and its provenance move, the conversation record does not.
const previousBinaryUpdateSnapshot = `UPDATE sandboxes
SET snapshot_id = $1,
    snapshot_suppressed_in_shadow = $2,
    pending_snapshot_message_id = NULL,
    snapshot_started_at = NULL,
    snapshot_start_message_id = NULL,
    snapshot_provenance_id = $1,
    snapshot_agent_protocol = $3,
    snapshot_runtime_version = $4,
    snapshot_skipped_repo_hooks_id = CASE WHEN skip_repo_hooks_told_gen = gen AND skip_repo_hooks_gen = gen THEN $1 END,
    snapshot_minted_at = now(),
    updated_at = now()
WHERE session_id = $5`

// setConversation sets sessionID's recorded conversation, "" for none.
func setConversation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, conversation string) {
	t.Helper()
	var value *string
	if conversation != "" {
		value = &conversation
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET opencode_conversation_id = $2 WHERE id = $1`, sessionID, value); err != nil {
		t.Fatalf("set the session's conversation to %q: %v", conversation, err)
	}
}

// restoreConversationOutcomes is every outcome sandbox_restore_conversation_total carries.
var restoreConversationOutcomes = []sandbox.RestoreConversation{
	sandbox.RestoreConversationHeld, sandbox.RestoreConversationDiffers,
	sandbox.RestoreConversationNone, sandbox.RestoreConversationUnknown,
}

// restoreConversationCounts reads sandbox_restore_conversation_total by
// outcome.
func restoreConversationCounts(ctx context.Context, t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, o := range restoreConversationOutcomes {
		out[string(o)] = readCounterSumByAttr(ctx, t, otelReader, "sandbox_restore_conversation_total", "outcome", string(o))
	}
	return out
}

// restoreConversationRig is one session whose gen 1 minted snapshotID
// through the actor, under the conversation atMint, then stopped with a
// pending turn, ready for an evaluation to restore it.
type restoreConversationRig struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	provider  *fakeSpawnProvider
	actor     *Actor
	logs      *syncLogBuffer
}

const restoreConversationSnapshot = "snap-conversation"

func newRestoreConversationRig(ctx context.Context, t *testing.T, atMint string, commander ports.SandboxCommander) *restoreConversationRig {
	t.Helper()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	setConversation(ctx, t, pool, sessionID, atMint)
	logs := captureDefaultLoggerJSONSync(t)
	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-conversation"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	mintSnapshot(ctx, t, pool, a, sessionID, 1, restoreConversationSnapshot, compatibleProvenance)
	return &restoreConversationRig{pool: pool, sessionID: sessionID, provider: provider, actor: a, logs: logs}
}

// restore stops gen 1 with a pending turn carrying request and lets one
// evaluation restore it, returning once gen 2 is connecting.
func (rig *restoreConversationRig) restore(ctx context.Context, t *testing.T, request string) sqlcgen.Turn {
	t.Helper()
	turn := createPendingTurn(ctx, t, narvipg.NewTurnStore(rig.pool), rig.sessionID, request)
	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, rig.sessionID); err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, rig.actor)
	waitUntil(t, 5*time.Second, func() bool { return rig.provider.restoreCallCount() == 1 })
	waitForConnecting(ctx, t, rig.pool, rig.sessionID)
	if rig.provider.callCount() != 0 {
		t.Fatalf("fresh spawns %d, want none: the snapshot is restored", rig.provider.callCount())
	}
	return turn
}

// TestRestoreConversation_Counted: each restore counts, once, how the
// conversation the snapshot was minted under -- recorded through the
// actor's own snapshot_ready -- compares with the one the session's
// prompts name when the restore is claimed; differs is logged at WARN
// naming the snapshot and both conversations, unknown at INFO, held and
// none not at all.
func TestRestoreConversation_Counted(t *testing.T) {
	tests := []struct {
		name               string
		atMint, atRestore  string
		afterMint          func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID)
		want               sandbox.RestoreConversation
		wantLine, wantLvl  string
		snapshotConvLogged string
	}{
		{name: "Held", atMint: "conv-a", atRestore: "conv-a", want: sandbox.RestoreConversationHeld},
		{name: "Differs", atMint: "conv-a", atRestore: "conv-b", want: sandbox.RestoreConversationDiffers,
			wantLine: restoreConversationDiffersLogMessage, wantLvl: "WARN", snapshotConvLogged: "conv-a"},
		{name: "Differs_MintedUnderNone", atRestore: "conv-b", want: sandbox.RestoreConversationDiffers,
			wantLine: restoreConversationDiffersLogMessage, wantLvl: "WARN"},
		{name: "None", want: sandbox.RestoreConversationNone},
		{name: "Unknown", atMint: "conv-a", atRestore: "conv-a", want: sandbox.RestoreConversationUnknown,
			afterMint: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
				// Recorded before the record existed: the snapshot is
				// there, nothing describes its conversation.
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_conversation_id = NULL, snapshot_conversation_recorded_id = NULL WHERE session_id = $1`, sessionID); err != nil {
					t.Fatal(err)
				}
			},
			wantLine: restoreConversationUnknownLogMessage, wantLvl: "INFO"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newRestoreConversationRig(ctx, t, tc.atMint, nil)
			if tc.afterMint != nil {
				tc.afterMint(ctx, t, rig.pool, rig.sessionID)
			}
			setConversation(ctx, t, rig.pool, rig.sessionID, tc.atRestore)
			before := restoreConversationCounts(ctx, t)
			rig.restore(ctx, t, "carry on")

			if got := countDelta(before, restoreConversationCounts(ctx, t)); len(got) != 1 || got[string(tc.want)] != 1 {
				t.Errorf("sandbox_restore_conversation_total moved by %v, want %s by 1 alone", got, tc.want)
			}
			for _, msg := range []string{restoreConversationDiffersLogMessage, restoreConversationUnknownLogMessage} {
				want := 0
				if msg == tc.wantLine {
					want = 1
				}
				if n := countLogLines(t, rig.logs, msg); n != want {
					t.Errorf("%d lines %q, want %d", n, msg, want)
				}
			}
			if tc.wantLine == "" {
				return
			}
			line := waitForLogEntry(t, rig.logs, 5*time.Second, tc.wantLine)
			if line["level"] != tc.wantLvl || line["outcome"] != string(tc.want) || line["snapshot_id"] != restoreConversationSnapshot ||
				line["gen"] != float64(2) || line["session_id"] != rig.sessionID.String() ||
				line["snapshot_conversation_id"] != tc.snapshotConvLogged || line["session_conversation_id"] != tc.atRestore {
				t.Errorf("logged %v, want %s naming %s, gen 2, %s, snapshot conversation %q and session conversation %q",
					line, tc.wantLvl, restoreConversationSnapshot, tc.want, tc.snapshotConvLogged, tc.atRestore)
			}
		})
	}
}

// TestRestoreConversation_PreviousBinarysSnapshotIsUnknown: a snapshot the
// control plane before migration 000177 records moves snapshot_id alone,
// leaving the record on the snapshot before it -- here minted under the
// very conversation the session still names. The record is keyed by the
// snapshot's id, so the new snapshot's restore is unknown, never held.
func TestRestoreConversation_PreviousBinarysSnapshotIsUnknown(t *testing.T) {
	ctx := context.Background()
	rig := newRestoreConversationRig(ctx, t, "conv-a", nil)
	const newer = "snap-previous-binary"
	if _, err := rig.pool.Exec(ctx, previousBinaryUpdateSnapshot, newer, false, contracts.Version, nil, rig.sessionID); err != nil {
		t.Fatalf("the previous binary's mint: %v", err)
	}
	row, err := narvipg.NewSandboxStore(rig.pool).Get(ctx, rig.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stringOrEmpty(row.SnapshotID) != newer || stringOrEmpty(row.SnapshotConversationID) != "conv-a" ||
		stringOrEmpty(row.SnapshotConversationRecordedID) != restoreConversationSnapshot {
		t.Fatalf("seeded snapshot %v, conversation %v recorded for %v; want %s, with conv-a still recorded for %s",
			stringOrEmpty(row.SnapshotID), stringOrEmpty(row.SnapshotConversationID), stringOrEmpty(row.SnapshotConversationRecordedID), newer, restoreConversationSnapshot)
	}
	before := restoreConversationCounts(ctx, t)
	rig.restore(ctx, t, "carry on")

	if call := rig.provider.lastRestoreCall(); string(call.snapshotID) != newer {
		t.Fatalf("restored %q, want %q", call.snapshotID, newer)
	}
	if got := countDelta(before, restoreConversationCounts(ctx, t)); len(got) != 1 || got[string(sandbox.RestoreConversationUnknown)] != 1 {
		t.Errorf("sandbox_restore_conversation_total moved by %v, want unknown by 1 alone", got)
	}
	line := waitForLogEntry(t, rig.logs, 5*time.Second, restoreConversationUnknownLogMessage)
	if line["snapshot_id"] != newer || line["snapshot_conversation_id"] != "" {
		t.Errorf("logged %v, want %s with no snapshot conversation read for it", line, newer)
	}
}

// TestRestoreConversation_NeverMarksNorWarns: a restore whose snapshot was
// minted under another conversation is counted and logged, and nothing
// else. It restores; it writes no warning and enqueues nothing; the session
// keeps its conversation; and the restored gen's first prompt names that
// conversation and carries the request exactly, with no recap.
func TestRestoreConversation_NeverMarksNorWarns(t *testing.T) {
	ctx := context.Background()
	commander := &fakeSendCommander{}
	rig := newRestoreConversationRig(ctx, t, "conv-then", commander)
	setConversation(ctx, t, rig.pool, rig.sessionID, "conv-now")
	before := restoreConversationCounts(ctx, t)
	const request = "carry on from where the restored sandbox left off"
	turn := rig.restore(ctx, t, request)

	// Gen 2 connects, boots and reports itself ready, with no conversation
	// of its own yet.
	a := rig.actor
	if !sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "ready", Gen: 2, MessageID: "rc-ready",
		Raw: json.RawMessage(`{"type":"ready","messageId":"rc-ready","sessionId":"s","gen":2}`)}).Persisted {
		t.Fatal("ready: Persisted = false, want true")
	}
	waitUntil(t, 5*time.Second, func() bool {
		row, err := narvipg.NewSandboxStore(rig.pool).Get(ctx, rig.sessionID)
		return err == nil && row.Status == sqlcgen.SandboxStatusBooting
	})
	if !sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "boot_timing", Gen: 2, MessageID: "rc-bt",
		Raw: json.RawMessage(`{"type":"boot_timing","messageId":"rc-bt","sessionId":"s","gen":2,"metric":"boot_duration","seconds":3,"failed":false}`)}).Persisted {
		t.Fatal("boot_timing: Persisted = false, want true")
	}
	if !sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "heartbeat", Gen: 2, MessageID: "rc-hb",
		Raw: json.RawMessage(`{"type":"heartbeat","messageId":"rc-hb","sessionId":"s","gen":2,"conversationId":null,"lastBootPhase":null}`)}).Persisted {
		t.Fatal("heartbeat: Persisted = false, want true")
	}
	waitUntil(t, 5*time.Second, func() bool { return commander.callCount() == 1 })

	var prompt sandboxws.Prompt
	if err := json.Unmarshal(commander.lastPayload(), &prompt); err != nil {
		t.Fatalf("decode the dispatched prompt: %v", err)
	}
	if prompt.Type != "prompt" || prompt.Gen != 2 || prompt.Text != request || prompt.ConversationId == nil || *prompt.ConversationId != "conv-now" {
		t.Fatalf("dispatched %s gen %d, text %q, conversation %v; want a prompt to gen 2 naming conv-now with the request exactly", prompt.Type, prompt.Gen, prompt.Text, prompt.ConversationId)
	}
	got, err := narvipg.NewTurnStore(rig.pool).Get(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 2 {
		t.Fatalf("turn %s on gen %v, want processing on gen 2", got.Status, got.DispatchedSandboxGen)
	}

	session, err := narvipg.NewSessionStore(rig.pool).Get(ctx, rig.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stringOrEmpty(session.OpencodeConversationID) != "conv-now" {
		t.Errorf("the session's conversation = %q, want conv-now, kept", stringOrEmpty(session.OpencodeConversationID))
	}
	if w := storedWarnings(ctx, t, rig.pool, rig.sessionID); len(w) != 0 {
		t.Errorf("warnings %+v, want none: the restore is counted, never announced", w)
	}
	var outbox int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, rig.sessionID).Scan(&outbox); err != nil {
		t.Fatalf("count the session's outbox rows: %v", err)
	}
	if outbox != 0 {
		t.Errorf("%d outbox rows, want none", outbox)
	}
	if got := countDelta(before, restoreConversationCounts(ctx, t)); len(got) != 1 || got[string(sandbox.RestoreConversationDiffers)] != 1 {
		t.Errorf("sandbox_restore_conversation_total moved by %v, want differs by 1 alone", got)
	}
	if n := countLogLines(t, rig.logs, restoreConversationDiffersLogMessage); n != 1 {
		t.Errorf("%d differs lines, want 1", n)
	}
}

// TestRestoreConversation_RotationRestoresAreCounted: the claim that
// replaces a gen the runway gate rotated (technical plan §35.3) is a
// restore like any other, and is counted like one. A restore of the
// rotation's own snapshot, minted under the conversation the session still
// names, is held. The fallback to the snapshot before it, recorded under
// another conversation, is differs: what is read is the record of the
// snapshot actually restored.
func TestRestoreConversation_RotationRestoresAreCounted(t *testing.T) {
	t.Run("Rotated", func(t *testing.T) {
		ctx := context.Background()
		before := restoreConversationCounts(ctx, t)
		rotatedBefore := rotationRestoreCount(ctx, t, rotationOutcomeRotated)
		rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute,
			beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
				setConversation(ctx, t, pool, sessionID, "conv-rotated")
			}})
		settle(ctx, t, rig.actor)
		cmd := onlySnapshotCommand(t, rig.commander)
		deliver(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, runwayRotationSnap, ""))

		if call := rig.waitRestored(t, 1); call.snapshotID != runwayRotationSnap || call.spec.Gen != 2 {
			t.Fatalf("RestoreFromSnapshot(%q) for gen %d, want the rotation's own %q for gen 2", call.snapshotID, call.spec.Gen, runwayRotationSnap)
		}
		if got := rotationRestoreCount(ctx, t, rotationOutcomeRotated) - rotatedBefore; got != 1 {
			t.Fatalf("sandbox_rotation_restore_total{rotated} moved by %d, want 1", got)
		}
		if got := countDelta(before, restoreConversationCounts(ctx, t)); len(got) != 1 || got[string(sandbox.RestoreConversationHeld)] != 1 {
			t.Errorf("sandbox_restore_conversation_total moved by %v, want held by 1 alone", got)
		}
	})
	t.Run("FellBack", func(t *testing.T) {
		ctx := context.Background()
		before := restoreConversationCounts(ctx, t)
		fellBackBefore := rotationRestoreCount(ctx, t, rotationOutcomeFellBack)
		rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute,
			commander: &fakeSendCommander{failTypes: map[string]error{"snapshot": ports.ErrNoLiveSandboxConnection}},
			beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
				setConversation(ctx, t, pool, sessionID, "conv-now")
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_conversation_id = 'conv-then',
					snapshot_conversation_recorded_id = snapshot_id WHERE session_id = $1`, sessionID); err != nil {
					t.Fatalf("record the previous snapshot's conversation: %v", err)
				}
			}})
		settle(ctx, t, rig.actor)

		if call := rig.waitRestored(t, 1); call.snapshotID != runwayPreviousSnap || call.spec.Gen != 2 {
			t.Fatalf("RestoreFromSnapshot(%q) for gen %d, want the previous snapshot %q for gen 2", call.snapshotID, call.spec.Gen, runwayPreviousSnap)
		}
		if got := rotationRestoreCount(ctx, t, rotationOutcomeFellBack) - fellBackBefore; got != 1 {
			t.Fatalf("sandbox_rotation_restore_total{fell_back} moved by %d, want 1", got)
		}
		if got := countDelta(before, restoreConversationCounts(ctx, t)); len(got) != 1 || got[string(sandbox.RestoreConversationDiffers)] != 1 {
			t.Errorf("sandbox_restore_conversation_total moved by %v, want differs by 1 alone", got)
		}
	})
}

// TestRestoreConversation_RefusedRestoresAreNotCounted: a restore turned
// into a fresh spawn restores nothing, so it is not counted. That holds
// whether it was refused on the snapshot's provenance (§35.5b) or
// downgraded for a session that runs a container daemon (§27.8) or a
// shadow session (§30.4(3)), and even though the snapshot it did not
// restore records the very conversation the session names.
func TestRestoreConversation_RefusedRestoresAreNotCounted(t *testing.T) {
	tests := []struct {
		name     string
		session  func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
		protocol string
		docker   bool
	}{
		{"ProvenanceRefused", createTestSession, "1.24.0", false},
		{"ContainerDaemonDowngrade", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			return createTestSessionWithEnvironment(ctx, t, pool, createTestEnvironmentWithDocker(ctx, t, pool))
		}, contracts.Version, true},
		{"ShadowDowngrade", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			return createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/example/restore-conversation-shadow.git", "main")
		}, contracts.Version, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := tc.session(ctx, t, pool)
			setConversation(ctx, t, pool, sessionID, "conv-a")
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "refused restore")
			const snapshotID = "snap-not-restored"
			seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, snapshotID, strPtr(tc.protocol), nil)
			row, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if stringOrEmpty(row.SnapshotConversationID) != "conv-a" || stringOrEmpty(row.SnapshotConversationRecordedID) != snapshotID {
				t.Fatalf("seeded conversation %q recorded for %q, want conv-a recorded for %s", stringOrEmpty(row.SnapshotConversationID),
					stringOrEmpty(row.SnapshotConversationRecordedID), snapshotID)
			}

			logs := captureDefaultLoggerJSONSync(t)
			before := restoreConversationCounts(ctx, t)
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "fresh-instead"}, dockerSupported: tc.docker}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })
			waitForConnecting(ctx, t, pool, sessionID)

			if n := provider.restoreCallCount(); n != 0 {
				t.Fatalf("RestoreFromSnapshot called %d times, want 0: the restore was turned into a fresh spawn", n)
			}
			if got := countDelta(before, restoreConversationCounts(ctx, t)); len(got) != 0 {
				t.Errorf("sandbox_restore_conversation_total moved by %v for a fresh spawn, want nothing", got)
			}
			for _, msg := range []string{restoreConversationDiffersLogMessage, restoreConversationUnknownLogMessage} {
				if n := countLogLines(t, logs, msg); n != 0 {
					t.Errorf("%d lines %q for a fresh spawn, want none", n, msg)
				}
			}
		})
	}
}
