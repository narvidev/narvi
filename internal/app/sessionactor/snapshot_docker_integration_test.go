//go:build integration

// This file proves §27.5's own resolution of §27.8's genuinely
// unresolved point D ("Capabilities() is flat... a provider whose
// snapshot support differs by runtime (Modal gVisor vs VM runtime)
// cannot express that today"): triggerSnapshotBestEffort
// (sandboxevent.go) never even ATTEMPTS a snapshot for a Docker-required
// session, regardless of how eligible the sandbox otherwise looks
// (Ready, no in-flight snapshot) -- see this repository's own new
// resilience scenario (test/resilience) for the companion §9.3-class
// restore-with-docker proof this decision is named alongside.
package sessionactor

import (
	"context"
	"strings"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestTriggerSnapshotBestEffort_DockerRequiredSession_NeverSnapshots is a
// MUTATION-TESTABLE guard: a sandbox that is otherwise fully eligible to
// snapshot (Ready, no pending attempt) but whose session's own
// Environment has docker_required=true must be left completely
// untouched -- no SendCommand call, no Ready->Snapshotting transition,
// no snapshot_id ever populated (which would otherwise let
// dispatch.go's own Restore branch attempt a snapshot restore this
// codebase cannot verify is safe under Modal's VM runtime, per §27.8).
func TestTriggerSnapshotBestEffort_DockerRequiredSession_NeverSnapshots(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	environmentID := createTestEnvironmentWithDocker(ctx, t, pool)
	sessionID := createTestSessionWithEnvironment(ctx, t, pool, environmentID)

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

	if got := commander.callCount(); got != 0 {
		t.Errorf("SendCommand called %d times, want 0 -- a Docker-required session must never be snapshotted (§27.8 unresolved VM-runtime parity)", got)
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want unchanged %s -- no Ready->Snapshotting transition for a Docker-required session", row.Status, sqlcgen.SandboxStatusReady)
	}
}

// TestTriggerSnapshotBestEffort_DockerFalseSession_StillSnapshots is the
// refusal test's own positive control, proving the gate is a real
// discriminator (not something that happens to always refuse): the
// IDENTICAL setup, minus docker_required, snapshots exactly as it always
// has.
func TestTriggerSnapshotBestEffort_DockerFalseSession_StillSnapshots(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	env, err := narvipg.NewEnvironmentStore(pool).Create(ctx, sqlcgen.CreateEnvironmentParams{DockerRequired: false})
	if err != nil {
		t.Fatalf("create test environment: %v", err)
	}
	sessionID := createTestSessionWithEnvironment(ctx, t, pool, env.ID)

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
		t.Fatalf("SendCommand called %d times, want 1 -- a Docker-false Environment must snapshot exactly as before this Step", got)
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.Status != sqlcgen.SandboxStatusSnapshotting {
		t.Errorf("sandbox status = %s, want %s", row.Status, sqlcgen.SandboxStatusSnapshotting)
	}
}

// --- §9.3-class resilience scenario #17: restore-with-docker (§27.8) ---
//
// §27.8's own closing bullet: "Snapshotting a running dockerd: daemon/
// image-store state inside snapshots is untested territory; §27.5
// must add a §9.3-class scenario for restore-with-docker before
// claiming it works." This codebase does not, and does not claim to,
// support restoring a Docker-enabled sandbox from a snapshot at all
// (point D's own resolution, dispatch.go's tryPlanSpawn) -- so THIS
// scenario proves the honest, safe alternative actually holds under a
// REAL dispatch cycle: a Docker-required session whose sandbox needs
// recovery NEVER attempts RestoreFromSnapshot, even in the edge case
// where a stale snapshot_id somehow already exists on its own row
// (simulated directly here, since this codebase's own normal flow can
// never produce one -- triggerSnapshotBestEffort's own gate, proven
// above, prevents it structurally) -- it always takes a fresh
// CreateSandbox instead, exactly the §10-P2 "never block a spawn"
// guarantee, minus the one specific operation (a cross-runtime restore)
// §27.8 names as unverified.

// TestResilienceScenario17_RestoreWithDocker_NeverRestoresStaleSnapshot
// is a MUTATION-TESTABLE guard proving dispatch.go's own restore-
// downgrade logic (tryPlanSpawn, immediately after
// refuseIfSubstrateUnsupported) through a REAL EnsureDispatched cycle:
// a Docker-required session's sandbox is Stopped with a real,
// non-empty snapshot_id planted directly (the edge case this defense-
// in-depth guard exists for) -- EvaluateSpawnDecision would ordinarily
// resolve this to SpawnActionRestore, but the real spawn that actually
// happens is a fresh CreateSandbox, never RestoreFromSnapshot.
func TestResilienceScenario17_RestoreWithDocker_NeverRestoresStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	environmentID := createTestEnvironmentWithDocker(ctx, t, pool)
	sessionID := createTestSessionWithEnvironment(ctx, t, pool, environmentID)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// Simulate the edge case this guard defends against: a real,
	// non-empty snapshot_id already on this Docker-required session's
	// own sandbox row. This codebase's own normal flow can never
	// produce one (triggerSnapshotBestEffort's own gate above), so this
	// plants it directly, standing in for any OTHER way a stale value
	// could theoretically end up here (a manual fixup, data predating a
	// future Environment-mutation feature, ...).
	staleSnapshotID := "stale-snapshot-from-before-docker-required"
	if _, err := sandboxStore.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID: sessionID, SnapshotID: &staleSnapshotID,
	}); err != nil {
		t.Fatalf("plant stale snapshot_id: %v", err)
	}
	// Stopped + a non-empty SnapshotImageID is exactly
	// EvaluateSpawnDecision's own Restore-eligible combination
	// (internal/domain/sandbox/spawndecision.go) -- ProviderObjectID
	// stays empty so Resume (which would otherwise take priority) never
	// applies either.
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusStopped,
	}); err != nil {
		t.Fatalf("move sandbox to stopped: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "recover me")

	logs := captureDefaultLoggerJSONSync(t)
	before := restoreCounts(ctx, t)
	provider := &fakeSpawnProvider{
		nextRef:         ports.SandboxRef{ProviderID: "fresh-spawn-not-a-restore"},
		dockerSupported: true, // supported in general -- the refusal is about THIS restore specifically, not provider capability
	}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	if got := provider.restoreCallCount(); got != 0 {
		t.Errorf("RestoreFromSnapshot called %d times, want 0 -- a Docker-required session must never restore from a snapshot (§27.8)", got)
	}

	spec := provider.lastSpec()
	if !spec.Docker {
		t.Error("the fresh spawn's own CreateSpec.Docker = false, want true (still a real Docker-required spawn, just via CreateSandbox not RestoreFromSnapshot)")
	}

	// Technical plan §35.5b: the downgrade is visible -- one persisted
	// warning at the new gen naming the snapshot and the Docker
	// requirement -- the snapshot is kept, and the warning is written once
	// per snapshot: the sandbox's next death downgrades it again, silently.
	assertOneSnapshotWarning := func() {
		t.Helper()
		warnings := storedWarnings(ctx, t, pool, sessionID)
		if len(warnings) != 1 || warnings[0].messageID != snapshotRefusalMessageIDPrefix+staleSnapshotID || warnings[0].gen != 2 ||
			!strings.Contains(warnings[0].message, staleSnapshotID) || !strings.Contains(warnings[0].message, "requires Docker") {
			t.Fatalf("warnings %+v, want one at gen 2 naming %s and the Docker requirement", warnings, staleSnapshotID)
		}
		row, err := sandboxStore.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if row.SnapshotID == nil || *row.SnapshotID != staleSnapshotID {
			t.Fatalf("snapshot_id = %v, want %s kept", row.SnapshotID, staleSnapshotID)
		}
	}
	waitForConnecting(ctx, t, pool, sessionID)
	assertOneSnapshotWarning()
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 2 })
	waitForConnecting(ctx, t, pool, sessionID)
	if got := provider.restoreCallCount(); got != 0 {
		t.Fatalf("RestoreFromSnapshot called %d times on the second death, want 0", got)
	}
	assertOneSnapshotWarning()
	// A downgraded restore is a fresh spawn: it is never decided on the
	// snapshot's provenance, so neither death is logged or counted as one.
	assertNoRestoreDecision(ctx, t, logs, before)
}

// TestResilienceScenario17_RestoreWithDocker_DockerFalseStillRestores is
// the scenario's own positive control: the IDENTICAL Stopped+snapshot_id
// fixture, minus docker_required, restores exactly as it always has --
// proving the downgrade is a real, narrow discriminator, not something
// that happens to suppress every restore.
func TestResilienceScenario17_RestoreWithDocker_DockerFalseStillRestores(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	env, err := narvipg.NewEnvironmentStore(pool).Create(ctx, sqlcgen.CreateEnvironmentParams{DockerRequired: false})
	if err != nil {
		t.Fatalf("create test environment: %v", err)
	}
	sessionID := createTestSessionWithEnvironment(ctx, t, pool, env.ID)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	snapshotID := "real-snapshot-1"
	if _, err := sandboxStore.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID: sessionID, SnapshotID: &snapshotID,
	}); err != nil {
		t.Fatalf("plant snapshot_id: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusStopped,
	}); err != nil {
		t.Fatalf("move sandbox to stopped: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "recover me")

	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-1"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount() == 1 })

	if got := provider.callCount(); got != 0 {
		t.Errorf("CreateSandbox called %d times, want 0 -- a Docker-false Environment with a real snapshot must restore, not fresh-spawn, exactly as before this Step", got)
	}
}
