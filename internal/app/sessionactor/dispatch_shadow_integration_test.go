//go:build integration

package sessionactor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// This file proves §30.4(3)'s own fail-closed defense-in-depth: a
// restore into a session whose own effective egress mode is currently
// shadow is refused (downgraded to a fresh spawn) unless the snapshot
// row itself was stamped shadow at the moment it was taken. See
// tryPlanSpawn's own new check (dispatch.go) and handleSnapshotReadyEvent's
// own new stamp (sandboxevent.go).

// seedStoppedSandboxWithSnapshotAndShadowBit mirrors
// seedStoppedSandboxWithSnapshot (dispatch_integration_test.go) exactly,
// except it also stamps snapshot_suppressed_in_shadow explicitly -- a
// separate helper, rather than adding a parameter to the shared one, so
// every pre-existing call to seedStoppedSandboxWithSnapshot keeps its own
// unchanged, implicit "absent" (false) behavior.
func seedStoppedSandboxWithSnapshotAndShadowBit(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, snapshotID string, suppressedInShadow bool) *narvipg.SandboxStore {
	t.Helper()
	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusStopped,
	}); err != nil {
		t.Fatalf("move sandbox to stopped: %v", err)
	}
	if _, err := sandboxStore.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID: sessionID, SnapshotID: &snapshotID, SnapshotSuppressedInShadow: suppressedInShadow,
	}); err != nil {
		t.Fatalf("seed snapshot_id: %v", err)
	}
	return sandboxStore
}

// TestHandleEnsureDispatched_ShadowSession_RefusesRestoreOfUnstampedSnapshot
// proves §30.4(3)'s own polarity verbatim: "an absent bit ... is treated
// as live and restore into a shadow session is refused". The session
// names a real repo with NO repo_settings row at all, so its own
// effective egress mode resolves shadow (egressmode.Resolve's own
// fail-closed default) -- and the Stopped sandbox's own snapshot was
// never stamped shadow (seedStoppedSandboxWithSnapshot's own implicit
// "false" default, matching every pre-existing snapshot this Step did
// not touch). The restore must be refused: a fresh CreateSandbox call,
// never RestoreFromSnapshot.
func TestHandleEnsureDispatched_ShadowSession_RefusesRestoreOfUnstampedSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/acme/repo.git", "main")

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	sandboxStore := seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-shadow-refused-1")

	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "fresh-spawn-1"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool {
		return provider.callCount() == 1
	})
	if got := provider.restoreCallCount(); got != 0 {
		t.Errorf("provider.RestoreFromSnapshot called %d times, want 0 (a shadow session must never restore a snapshot whose own shadow bit is absent)", got)
	}

	row, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if row.SnapshotID == nil || *row.SnapshotID != "snap-shadow-refused-1" {
		t.Errorf("sandbox snapshot_id = %v, want unchanged %q (a fresh spawn does not clear it)", row.SnapshotID, "snap-shadow-refused-1")
	}

	// Technical plan §35.5b: the downgrade is visible -- one persisted
	// warning at the new gen naming the snapshot and the shadow mode -- and
	// written once per snapshot: the sandbox's next death downgrades the
	// same snapshot again, silently.
	assertOneSnapshotWarning := func() {
		t.Helper()
		warnings := storedWarnings(ctx, t, pool, sessionID)
		if len(warnings) != 1 || warnings[0].messageID != snapshotRefusalMessageIDPrefix+"snap-shadow-refused-1" || warnings[0].gen != 2 ||
			!strings.Contains(warnings[0].message, "snap-shadow-refused-1") || !strings.Contains(warnings[0].message, "shadow mode") {
			t.Fatalf("warnings %+v, want one at gen 2 naming snap-shadow-refused-1 and shadow mode", warnings)
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
	if provider.restoreCallCount() != 0 {
		t.Fatalf("RestoreFromSnapshot called %d times on the second death, want 0", provider.restoreCallCount())
	}
	assertOneSnapshotWarning()
}

// TestHandleEnsureDispatched_ShadowSession_RestoresSnapshotStampedShadow
// proves the OTHER half of the same polarity: a snapshot explicitly
// stamped shadow (taken while this session's own predecessor held no
// more than read-only) IS eligible for restore into a shadow session.
func TestHandleEnsureDispatched_ShadowSession_RestoresSnapshotStampedShadow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/acme/repo2.git", "main")

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	seedStoppedSandboxWithSnapshotAndShadowBit(ctx, t, pool, sessionID, "snap-shadow-safe-1", true)

	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-shadow-safe-1"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool {
		return provider.restoreCallCount() == 1
	})
	if got := provider.callCount(); got != 0 {
		t.Errorf("provider.CreateSandbox called %d times, want 0 (a snapshot stamped shadow must restore normally into a shadow session)", got)
	}
	if call := provider.lastRestoreCall(); call.snapshotID != "snap-shadow-safe-1" {
		t.Errorf("RestoreFromSnapshot snapshotID = %q, want %q", call.snapshotID, "snap-shadow-safe-1")
	}
}

// TestHandleEnsureDispatched_LiveSession_RestoresUnstampedSnapshot proves
// the check is scoped to a CURRENTLY-shadow session only: a session whose
// own repo is explicitly promoted to live_egress_enabled=true restores an
// unstamped snapshot normally -- this Step's own fix targets exactly the
// live-credential-into-shadow-session leak, not restores in general.
func TestHandleEnsureDispatched_LiveSession_RestoresUnstampedSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/acme/repo3.git", "main")

	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/repo3", true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-live-session-1")

	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-live-session-1"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool {
		return provider.restoreCallCount() == 1
	})
	if got := provider.callCount(); got != 0 {
		t.Errorf("provider.CreateSandbox called %d times, want 0 (a live session restores an unstamped snapshot normally)", got)
	}
}
