//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"testing"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestHandleSnapshotReadyEvent_AnyProvenanceKeepsTheSnapshot is the
// event-path half of TestDecodeSnapshotReady_AnyProvenanceKeepsTheSnapshot
// (technical plan §35.5b): whatever a snapshot_ready's provenance holds --
// what the minting agent reports, a member of the wrong type, or no object
// at all -- the snapshot is recorded and the sandbox returns to Ready, as
// it did before the generated SnapshotReady named the key. A decode failure
// there reverts the snapshot instead.
func TestHandleSnapshotReadyEvent_AnyProvenanceKeepsTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sandboxStore := narvipg.NewSandboxStore(pool)
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	for _, provenance := range []string{
		`{"agentProtocol":"1.25.0","runtimeVersion":"1.14.19"}`,
		`{"agentProtocol":42}`,
		`{"runtimeVersion":["1.14.19"]}`,
		`"1.25.0"`,
	} {
		t.Run(provenance, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusSnapshotting}); err != nil {
				t.Fatalf("move sandbox to snapshotting: %v", err)
			}
			pendingID := "cmd-provenance"
			if _, err := sandboxStore.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
				SessionID: sessionID, PendingSnapshotMessageID: &pendingID,
			}); err != nil {
				t.Fatalf("seed pending snapshot message id: %v", err)
			}
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			raw := json.RawMessage(`{"type":"snapshot_ready","messageId":"sr-p","sessionId":"s","gen":1,"ackId":"snapshot_ready:sr-p",` +
				`"snapshotId":"snap-provenance","commandMessageId":"cmd-provenance","provenance":` + provenance + `}`)
			if outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: raw}); !outcome.Persisted {
				t.Fatal("snapshot_ready: Persisted = false, want true")
			}
			row, err := sandboxStore.Get(ctx, sessionID)
			if err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			if row.Status != sqlcgen.SandboxStatusReady || row.SnapshotID == nil || *row.SnapshotID != "snap-provenance" {
				t.Errorf("after a snapshot_ready with provenance %s: status %s, snapshot_id %v; want ready with snap-provenance recorded",
					provenance, row.Status, row.SnapshotID)
			}
		})
	}
}
