//go:build integration

// §3.2's boot-evidence rule (bootevidence.go) against a real Postgres: a
// heartbeat with a null lastBootPhase moves a Booting sandbox to Ready
// only once the same generation has shown that its boot ran. The frames
// here are the ones a sandbox-agent built before the rule sends -- null
// from connect until a service reports a phase -- which is what the rule
// exists for; the fixed agent's own frames are driven end to end in
// internal/adapters/inbound/wshub/bootready_integration_test.go.
package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// newBootEvidenceActor seeds a sandbox for sessionID at gen 1 in status,
// and returns a registry-owned actor for it plus the sandbox store.
func newBootEvidenceActor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, status sqlcgen.SandboxStatus) (*Registry, *Actor, *narvipg.SandboxStore) {
	t.Helper()
	sandboxes := narvipg.NewSandboxStore(pool)
	if _, err := sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: status}); err != nil {
		t.Fatalf("move sandbox to %s: %v", status, err)
	}
	r, a := newBootEvidenceRegistry(ctx, t, pool, sessionID)
	return r, a, sandboxes
}

func newBootEvidenceRegistry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) (*Registry, *Actor) {
	t.Helper()
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return r, a
}

// Wire frames, as a sandbox-agent sends them, for gen.
func nullPhaseHeartbeat(id string, gen int) SandboxEvent {
	return SandboxEvent{Type: "heartbeat", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"heartbeat","messageId":%q,"sessionId":"s","gen":%d,"conversationId":null,"lastBootPhase":null}`, id, gen))}
}

func phaseHeartbeat(id string, gen int, phase string) SandboxEvent {
	p := phase
	return SandboxEvent{Type: "heartbeat", Gen: gen, MessageID: id, LastBootPhase: &p,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"heartbeat","messageId":%q,"sessionId":"s","gen":%d,"conversationId":null,"lastBootPhase":%q}`, id, gen, phase))}
}

func bootProgressEvent(id string, gen int, phase string) SandboxEvent {
	return SandboxEvent{Type: "boot_progress", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"boot_progress","messageId":%q,"sessionId":"s","gen":%d,"phase":%q}`, id, gen, phase))}
}

func bootTimingEvent(id string, gen int, metric, failedJSON string) SandboxEvent {
	return SandboxEvent{Type: "boot_timing", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"boot_timing","messageId":%q,"sessionId":"s","gen":%d,"metric":%q,"seconds":42,"failed":%s}`, id, gen, metric, failedJSON))}
}

func gitSyncEvent(id string, gen int) SandboxEvent {
	return SandboxEvent{Type: "git_sync", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"git_sync","messageId":%q,"sessionId":"s","gen":%d,"status":"synced","branch":"main","repo":"app"}`, id, gen))}
}

func readyEvent(id string, gen int) SandboxEvent {
	return SandboxEvent{Type: "ready", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":"s","gen":%d}`, id, gen))}
}

func sandboxStatusNow(ctx context.Context, t *testing.T, sandboxes *narvipg.SandboxStore, sessionID pgtype.UUID) sqlcgen.Sandbox {
	t.Helper()
	row, err := sandboxes.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row
}

// TestBootEvidence_NullPhaseMarksReadyOnlyAfterEvidence: a Booting
// sandbox receives some event of its boot, then a null-phase heartbeat.
// Only an event showing that the boot ran lets that heartbeat mark it
// Ready; with none, or one that does not, it stays Booting -- the
// pre-fix agent's clone-and-hooks window, which this used to cut short.
func TestBootEvidence_NullPhaseMarksReadyOnlyAfterEvidence(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	tests := []struct {
		name      string
		before    []SandboxEvent
		wantReady bool
	}{
		{name: "no event before it: a pre-fix agent still cloning", before: nil, wantReady: false},
		{name: "several null-phase heartbeats before it", before: []SandboxEvent{nullPhaseHeartbeat("h-a", 1), nullPhaseHeartbeat("h-b", 1)}, wantReady: false},
		{name: "a git_sync: the boot is still running", before: []SandboxEvent{gitSyncEvent("gs", 1)}, wantReady: false},
		{name: "a hook_rerun_duration: the boot is still running", before: []SandboxEvent{bootTimingEvent("bt", 1, "hook_rerun_duration", "false")}, wantReady: false},
		{name: "a failed boot_duration", before: []SandboxEvent{bootTimingEvent("bt", 1, "boot_duration", "true")}, wantReady: false},
		{name: "a successful boot_duration: a pre-fix agent with no service, after its boot sequence", before: []SandboxEvent{bootTimingEvent("bt", 1, "boot_duration", "false")}, wantReady: true},
		{name: "a boot_progress: a service reported its phase", before: []SandboxEvent{bootProgressEvent("bp", 1, "web:starting")}, wantReady: true},
		{name: "a heartbeat carrying a phase: a fixed agent mid-boot", before: []SandboxEvent{phaseHeartbeat("h-p", 1, "starting")}, wantReady: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			_, a, sandboxes := newBootEvidenceActor(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)

			for _, ev := range tc.before {
				if outcome := sendSandboxEventForTest(ctx, t, a, ev); !outcome.Persisted {
					t.Fatalf("%s %s: Persisted = false, want true", ev.Type, ev.MessageID)
				}
				if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusBooting {
					t.Fatalf("status after %s %s = %s, want %s: no event but a null-phase heartbeat may move it",
						ev.Type, ev.MessageID, got, sqlcgen.SandboxStatusBooting)
				}
			}

			if outcome := sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("h-final", 1)); !outcome.Persisted {
				t.Fatal("null-phase heartbeat: Persisted = false, want true")
			}
			row := sandboxStatusNow(ctx, t, sandboxes, sessionID)
			want := sqlcgen.SandboxStatusBooting
			if tc.wantReady {
				want = sqlcgen.SandboxStatusReady
			}
			if row.Status != want {
				t.Errorf("status after the null-phase heartbeat = %s, want %s", row.Status, want)
			}
			if gotEvidence := row.BootEvidenceGen != nil && *row.BootEvidenceGen == 1; gotEvidence != tc.wantReady {
				t.Errorf("boot_evidence_gen = %v, want evidence recorded for gen 1: %v", row.BootEvidenceGen, tc.wantReady)
			}
			if !row.LastSeenAt.Valid {
				t.Error("last_seen_at not set: a heartbeat that does not transition is still a liveness signal")
			}
		})
	}
}

// TestBootEvidence_ScopedToGeneration: evidence shown by gen 1 says
// nothing about the boot of the sandbox a respawn puts in its place. Gen
// 2's null-phase heartbeat waits for gen 2's own evidence.
func TestBootEvidence_ScopedToGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	_, a, sandboxes := newBootEvidenceActor(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)

	sendSandboxEventForTest(ctx, t, a, bootTimingEvent("g1-bt", 1, "boot_duration", "false"))
	sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("g1-h", 1))
	if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusReady {
		t.Fatalf("gen 1: status = %s, want %s", got, sqlcgen.SandboxStatusReady)
	}

	// Respawn: the same statement tryPlanSpawn uses bumps gen, then the
	// provider acks (Spawning -> Connecting).
	respawned, err := sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID})
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if respawned.Gen != 2 {
		t.Fatalf("gen after respawn = %d, want 2", respawned.Gen)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusConnecting}); err != nil {
		t.Fatalf("move to connecting: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, readyEvent("g2-r", 2))
	sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("g2-h1", 2))
	row := sandboxStatusNow(ctx, t, sandboxes, sessionID)
	if row.Status != sqlcgen.SandboxStatusBooting {
		t.Fatalf("gen 2 before its own evidence: status = %s, want %s (gen 1's evidence must not count)", row.Status, sqlcgen.SandboxStatusBooting)
	}
	if row.BootEvidenceGen == nil || *row.BootEvidenceGen != 1 {
		t.Fatalf("boot_evidence_gen = %v, want gen 1's, untouched", row.BootEvidenceGen)
	}

	sendSandboxEventForTest(ctx, t, a, bootProgressEvent("g2-bp", 2, "web:ready"))
	sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("g2-h2", 2))
	if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusReady {
		t.Fatalf("gen 2 after its own evidence: status = %s, want %s", got, sqlcgen.SandboxStatusReady)
	}
}

// TestBootEvidence_StaleGenEventIsFencedOut is the other half of scoping
// evidence to a generation: after a respawn, an evidence event from the
// previous gen -- still in flight on the old connection, or replayed by
// the old agent -- is fenced out per message before it can be recorded,
// so it cannot become the new gen's evidence. The per-message gen fence is
// the only thing that stops it: evidence is recorded against the row's
// live gen, not the event's.
func TestBootEvidence_StaleGenEventIsFencedOut(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for _, tc := range []struct {
		name  string
		stale SandboxEvent
	}{
		{name: "a boot_progress", stale: bootProgressEvent("g1-bp", 1, "web:ready")},
		{name: "a heartbeat carrying a phase", stale: phaseHeartbeat("g1-hp", 1, "starting")},
		{name: "a successful boot_duration", stale: bootTimingEvent("g1-bt", 1, "boot_duration", "false")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			_, a, sandboxes := newBootEvidenceActor(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)

			if _, err := sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID}); err != nil {
				t.Fatalf("respawn: %v", err)
			}
			if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusConnecting}); err != nil {
				t.Fatalf("move to connecting: %v", err)
			}
			sendSandboxEventForTest(ctx, t, a, readyEvent("g2-r", 2))

			if outcome := sendSandboxEventForTest(ctx, t, a, tc.stale); outcome.Persisted {
				t.Fatalf("stale gen-1 %s: Persisted = true, want false (fenced out)", tc.stale.Type)
			}
			sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("g2-h", 2))

			row := sandboxStatusNow(ctx, t, sandboxes, sessionID)
			if row.BootEvidenceGen != nil {
				t.Errorf("boot_evidence_gen = %d, want NULL: gen 1's event is not gen 2's evidence", *row.BootEvidenceGen)
			}
			if row.Status != sqlcgen.SandboxStatusBooting {
				t.Errorf("gen 2 status after its first null-phase heartbeat = %s, want %s", row.Status, sqlcgen.SandboxStatusBooting)
			}
		})
	}
}

// TestBootEvidence_SurvivesControlPlaneRestart: evidence lives on the
// sandbox row, not in the actor. A sandbox whose evidence arrived before a
// control-plane restart still goes Ready on its first null-phase heartbeat
// to the new process -- and one whose evidence event was stored before
// this rule existed (boot_evidence_gen still NULL: a sandbox Booting when
// migration 000147 ran) gets it back from the agent's replay of that same
// event on reconnect, although the replay stores no new row.
func TestBootEvidence_SurvivesControlPlaneRestart(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	t.Run("evidence recorded before the restart", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		r, a, sandboxes := newBootEvidenceActor(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)

		sendSandboxEventForTest(ctx, t, a, bootTimingEvent("bt", 1, "boot_duration", "false"))
		if err := r.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Shutdown: %v", err)
		}

		_, a2 := newBootEvidenceRegistry(ctx, t, pool, sessionID)
		sendSandboxEventForTest(ctx, t, a2, readyEvent("r2", 1)) // the agent reconnects
		if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusBooting {
			t.Fatalf("status after the reconnect's ready = %s, want %s", got, sqlcgen.SandboxStatusBooting)
		}
		sendSandboxEventForTest(ctx, t, a2, nullPhaseHeartbeat("h", 1))
		if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusReady {
			t.Fatalf("status after the restart's first null-phase heartbeat = %s, want %s", got, sqlcgen.SandboxStatusReady)
		}
	})

	t.Run("evidence event stored before the rule, replayed on reconnect", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		_, a, sandboxes := newBootEvidenceActor(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)

		bt := bootTimingEvent("bt-replayed", 1, "boot_duration", "false")
		sendSandboxEventForTest(ctx, t, a, bt)
		// As if it had been stored by a control plane without the column.
		if _, err := pool.Exec(ctx, `UPDATE sandboxes SET boot_evidence_gen = NULL WHERE session_id = $1`, sessionID); err != nil {
			t.Fatalf("clear boot_evidence_gen: %v", err)
		}
		sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("h1", 1))
		if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusBooting {
			t.Fatalf("status with no recorded evidence = %s, want %s", got, sqlcgen.SandboxStatusBooting)
		}

		if outcome := sendSandboxEventForTest(ctx, t, a, bt); !outcome.Persisted {
			t.Fatal("replayed boot_timing: Persisted = false, want true")
		}
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'boot_timing'`, sessionID).Scan(&rows); err != nil {
			t.Fatalf("count boot_timing rows: %v", err)
		}
		if rows != 1 {
			t.Fatalf("boot_timing rows = %d, want 1: the replay is a duplicate", rows)
		}
		sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("h2", 1))
		if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusReady {
			t.Fatalf("status after the replayed evidence = %s, want %s", got, sqlcgen.SandboxStatusReady)
		}
	})
}

// TestBootEvidence_SuspectRecoveryIntoBooting: a Booting sandbox the
// connecting watchdog suspected returns to Booting on its next event
// (§3.2, "any liveness signal during grace returns to previous state").
// When that event is a null-phase heartbeat, the same pass may carry it on
// to Ready only if the generation has already shown boot evidence.
func TestBootEvidence_SuspectRecoveryIntoBooting(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for _, tc := range []struct {
		name       string
		evidence   bool
		wantStatus sqlcgen.SandboxStatus
	}{
		{name: "no evidence yet: recovers to Booting and stays there", evidence: false, wantStatus: sqlcgen.SandboxStatusBooting},
		{name: "evidence shown before it was suspected: recovers and goes Ready", evidence: true, wantStatus: sqlcgen.SandboxStatusReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			sandboxes := seedSuspectSandboxWithPreSuspectStatus(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)
			if tc.evidence {
				if err := sandboxes.MarkBootEvidence(ctx, sessionID, 1); err != nil {
					t.Fatalf("MarkBootEvidence: %v", err)
				}
			}
			_, a := newBootEvidenceRegistry(ctx, t, pool, sessionID)

			sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("h-recover", 1))
			if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != tc.wantStatus {
				t.Errorf("status = %s, want %s", got, tc.wantStatus)
			}
		})
	}
}
