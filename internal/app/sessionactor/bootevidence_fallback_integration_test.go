//go:build integration

// §3.2's boot-evidence fallback (bootevidence.go) against a real Postgres:
// a generation that shows no boot evidence, Booting with its heartbeats
// still arriving for longer than platform.Timeouts.BootEvidenceFallback,
// has its null boot phase accepted as boot completion. Only a
// sandbox-agent built before boot_timing existed, booting a repo with no
// service and no Docker, sends that wire -- and every snapshot descended
// from a sandbox it booted keeps sending it, restore after restore.
package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

const bootEvidenceFallbackMetric = "sandbox_boot_evidence_fallback_total"

// newFallbackTestRegistry is newDispatchTestRegistry with the fallback bound
// shortened to bound, every other timeout at its default.
func newFallbackTestRegistry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, bound time.Duration, provider ports.SandboxProvider, commander ports.SandboxCommander) *Registry {
	t.Helper()
	timeouts := platform.DefaultTimeouts()
	timeouts.BootEvidenceFallback = bound
	r, err := NewRegistry(ctx, pool, timeouts, nil, commander, provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	return r
}

// genString renders a nullable gen column for a failure message.
func genString(gen *int32) string {
	if gen == nil {
		return "NULL"
	}
	return fmt.Sprint(*gen)
}

// promptCount is how many prompt commands f has been asked to send.
func promptCount(f *fakeSendCommander) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.payloads {
		var env struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(p, &env) == nil && env.Type == "prompt" {
			n++
		}
	}
	return n
}

// TestBootEvidenceFallback_RestoredPreBootTimingAgent is round 1's
// reproduction. A session last snapshotted by an agent built before
// boot_timing is restored for a pending turn; that agent sends "ready",
// then only null-phase heartbeats. Before the bound the sandbox stays
// Booting and nothing is dispatched. The first heartbeat past it moves the
// sandbox Ready -- once, through the fallback, not through evidence -- and
// the turn is dispatched; later heartbeats change nothing.
func TestBootEvidenceFallback_RestoredPreBootTimingAgent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	turn := createPendingTurn(ctx, t, turns, sessionID, "continue the work")
	sandboxes := seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-lineage-from-july")

	// bound is the shortened fallback; slack absorbs the gap between this
	// test's clock and the database's, and one heartbeat's processing.
	const (
		bound    = 1500 * time.Millisecond
		slack    = 250 * time.Millisecond
		interval = 100 * time.Millisecond
	)
	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-provider-object"}}
	commander := &fakeSendCommander{}
	r := newFallbackTestRegistry(ctx, t, pool, bound, provider, commander)
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	fallbacksBefore := readCounterSum(ctx, t, otelReader, bootEvidenceFallbackMetric)

	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool {
		row, err := sandboxes.Get(ctx, sessionID)
		return err == nil && row.Gen == 2 && row.Status == sqlcgen.SandboxStatusConnecting
	})
	if got := provider.restoreCallCount(); got != 1 {
		t.Fatalf("RestoreFromSnapshot calls = %d, want 1", got)
	}
	if got := provider.lastRestoreCall().snapshotID; got != "snap-lineage-from-july" {
		t.Fatalf("restored snapshot = %q, want the lineage's own", got)
	}

	sendSandboxEventForTest(ctx, t, a, readyEvent("r-g2", 2))
	bootingAt := time.Now()
	if got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got != sqlcgen.SandboxStatusBooting {
		t.Fatalf("status after ready = %s, want %s", got, sqlcgen.SandboxStatusBooting)
	}

	readyAfter := time.Duration(-1)
	heartbeats := 0
	for {
		sent := time.Since(bootingAt)
		sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat(fmt.Sprintf("h-g2-%d", heartbeats), 2))
		heartbeats++
		switch got := sandboxStatusNow(ctx, t, sandboxes, sessionID).Status; got {
		case sqlcgen.SandboxStatusBooting:
			if readyAfter >= 0 {
				t.Fatalf("heartbeat %d: status back to %s after ready", heartbeats, got)
			}
			if sent > bound+slack {
				t.Fatalf("heartbeat %d, sent %s into Booting: still %s past the %s bound", heartbeats, sent, got, bound)
			}
			if n := promptCount(commander); n != 0 {
				t.Fatalf("heartbeat %d: %d prompts sent while booting, want 0", heartbeats, n)
			}
		case sqlcgen.SandboxStatusReady:
			if readyAfter < 0 {
				readyAfter = sent
				if sent < bound-slack {
					t.Fatalf("heartbeat %d, sent %s into Booting: %s before the %s bound", heartbeats, sent, got, bound)
				}
			}
		default:
			t.Fatalf("heartbeat %d: status = %s, want booting, then ready", heartbeats, got)
		}
		if readyAfter >= 0 && sent >= readyAfter+5*interval {
			break
		}
		time.Sleep(interval)
	}

	row := sandboxStatusNow(ctx, t, sandboxes, sessionID)
	if row.BootEvidenceGen != nil {
		t.Errorf("boot_evidence_gen = %d, want NULL: this agent showed none, the fallback moved it", *row.BootEvidenceGen)
	}
	if got := readCounterSum(ctx, t, otelReader, bootEvidenceFallbackMetric) - fallbacksBefore; got != 1 {
		t.Errorf("%s grew by %d over %d heartbeats, want exactly 1", bootEvidenceFallbackMetric, got, heartbeats)
	}
	if got := promptCount(commander); got != 1 {
		t.Errorf("prompts sent = %d, want exactly 1", got)
	}
	gotTurn, err := turns.Get(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusProcessing {
		t.Errorf("turn status = %s, want %s", gotTurn.Status, sqlcgen.TurnStatusProcessing)
	}
	var watchdogs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name IN ('liveness_check', 'inactivity')`, sessionID).Scan(&watchdogs); err != nil {
		t.Fatalf("count ready watchdogs: %v", err)
	}
	if watchdogs != 2 {
		t.Errorf("liveness_check + inactivity timers = %d, want 2: armed once, on the ->Ready edge", watchdogs)
	}
}

// TestBootEvidenceFallback_BootingStartScopedToGen: the start the bound is
// measured from is the gen's own. The Connecting -> Booting edge records
// it; a sandbox found Booting with none (Booting when migration 000148 ran)
// gets one at its first null-phase heartbeat; a later heartbeat keeps the
// first; and a respawn's own edge replaces it for the new gen.
func TestBootEvidenceFallback_BootingStartScopedToGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	_, a, sandboxes := newBootEvidenceActor(ctx, t, pool, sessionID, sqlcgen.SandboxStatusBooting)

	start := func(t *testing.T) (gen *int32, at time.Time) {
		t.Helper()
		var ts *time.Time
		if err := pool.QueryRow(ctx, `SELECT booting_since_gen, booting_since FROM sandboxes WHERE session_id = $1`, sessionID).Scan(&gen, &ts); err != nil {
			t.Fatalf("read booting start: %v", err)
		}
		if ts != nil {
			at = *ts
		}
		return gen, at
	}

	if gen, _ := start(t); gen != nil {
		t.Fatalf("booting_since_gen = %d before any event, want NULL (seeded straight into Booting)", *gen)
	}
	sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("g1-h1", 1))
	gen1, at1 := start(t)
	if gen1 == nil || *gen1 != 1 {
		t.Fatalf("booting_since_gen after the first null-phase heartbeat = %s, want 1", genString(gen1))
	}
	sendSandboxEventForTest(ctx, t, a, nullPhaseHeartbeat("g1-h2", 1))
	if _, again := start(t); !again.Equal(at1) {
		t.Fatalf("booting_since moved from %s to %s on a later heartbeat, want the first kept", at1, again)
	}

	if _, err := sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusConnecting}); err != nil {
		t.Fatalf("move to connecting: %v", err)
	}
	sendSandboxEventForTest(ctx, t, a, readyEvent("g2-r", 2))
	gen2, at2 := start(t)
	if gen2 == nil || *gen2 != 2 {
		t.Fatalf("booting_since_gen after gen 2's ready = %s, want 2", genString(gen2))
	}
	if !at2.After(at1) {
		t.Fatalf("gen 2's booting_since %s is not after gen 1's %s", at2, at1)
	}
}
