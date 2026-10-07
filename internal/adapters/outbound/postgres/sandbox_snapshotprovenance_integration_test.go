//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// snapshotColumns is the snapshot part of a sandbox row.
type snapshotColumns struct {
	snapshotID, provenanceID, protocol, runtime *string
	mintedAt                                    pgtype.Timestamptz
	shadow                                      bool
}

func snapshotColumnsOf(row sqlcgen.Sandbox) snapshotColumns {
	return snapshotColumns{
		snapshotID: row.SnapshotID, provenanceID: row.SnapshotProvenanceID, protocol: row.SnapshotAgentProtocol,
		runtime: row.SnapshotRuntimeVersion, mintedAt: row.SnapshotMintedAt, shadow: row.SnapshotSuppressedInShadow,
	}
}

func equalStringPtr(a *string, b string, isNil bool) bool {
	if isNil {
		return a == nil
	}
	return a != nil && *a == b
}

// TestUpdateSandboxSnapshotID_RecordsTheProvenanceKeyedToTheSnapshot pins
// technical plan §35.5b's record at mint: the statement that records a
// snapshot records what its minting agent reported, the transaction's
// now() as the mint, and the snapshot's id as the key -- each mint
// overwriting the last, an agent that reported nothing included, so no
// snapshot ever reads another's provenance; and a spawn, restore or
// resume claim leaves it as it is.
func TestUpdateSandboxSnapshotID_RecordsTheProvenanceKeyedToTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := store.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	mint := func(snapshotID string, protocol, runtime *string) (sqlcgen.Sandbox, time.Time) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		row, err := store.WithTx(tx).UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
			SessionID: sessionID, SnapshotID: &snapshotID, AgentProtocol: protocol, RuntimeVersion: runtime,
		})
		if err != nil {
			t.Fatalf("UpdateSnapshotID(%s): %v", snapshotID, err)
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return row, now
	}
	protocol, runtime := "1.25.0", "1.14.19"

	row, now := mint("snap-1", &protocol, &runtime)
	c := snapshotColumnsOf(row)
	if !equalStringPtr(c.snapshotID, "snap-1", false) || !equalStringPtr(c.provenanceID, "snap-1", false) ||
		!equalStringPtr(c.protocol, protocol, false) || !equalStringPtr(c.runtime, runtime, false) || !c.mintedAt.Valid || !c.mintedAt.Time.Equal(now) {
		t.Fatalf("after the first mint: %+v; want snap-1 keyed, %s, %s, minted at the transaction's now() %v", c, protocol, runtime, now)
	}

	// A claim -- spawn, restore or resume -- keeps the snapshot's
	// provenance: the restored gen runs the snapshot's agent.
	tokenHash := "token-hash-provenance"
	claimed, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: ptrInt32(7200)})
	if err != nil {
		t.Fatalf("UpsertForSpawn: %v", err)
	}
	if got := snapshotColumnsOf(claimed); got.String() != c.String() {
		t.Fatalf("after a claim: %+v; want %+v, kept", got, c)
	}

	// The next mint, by an agent that reported nothing, records nothing
	// for its own snapshot rather than keeping snap-1's values.
	row, now = mint("snap-2", nil, nil)
	c = snapshotColumnsOf(row)
	if !equalStringPtr(c.snapshotID, "snap-2", false) || !equalStringPtr(c.provenanceID, "snap-2", false) ||
		c.protocol != nil || c.runtime != nil || !c.mintedAt.Time.Equal(now) {
		t.Fatalf("after a mint that reported nothing: %+v; want snap-2 keyed with no protocol and no runtime, minted at %v", c, now)
	}
}

func (c snapshotColumns) String() string {
	str := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	return str(c.snapshotID) + "|" + str(c.provenanceID) + "|" + str(c.protocol) + "|" + str(c.runtime) + "|" +
		c.mintedAt.Time.Format(time.RFC3339Nano) + "|" + map[bool]string{true: "shadow", false: "live"}[c.shadow]
}

// TestClearSandboxSnapshot_GuardedOnTheSnapshot pins the clear technical
// plan §35.5b's refusal and §21.1's retirement share: it clears only the
// snapshot the caller decided on -- a snapshot recorded since is kept --
// and takes the snapshot's shadow bit and provenance with it.
func TestClearSandboxSnapshot_GuardedOnTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := store.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	snapshotID, protocol := "snap-current", "1.24.0"
	if _, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID: sessionID, SnapshotID: &snapshotID, SnapshotSuppressedInShadow: true, AgentProtocol: &protocol,
	}); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
	get := func() sqlcgen.Sandbox {
		t.Helper()
		row, err := store.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	seeded := snapshotColumnsOf(get())

	if n, err := store.ClearSnapshot(ctx, sessionID, "snap-decided-on-earlier"); err != nil || n != 0 {
		t.Fatalf("ClearSnapshot of another snapshot = (%d, %v), want (0, nil)", n, err)
	}
	if got := snapshotColumnsOf(get()); got.String() != seeded.String() {
		t.Fatalf("after clearing another snapshot: %+v; want %+v, kept", got, seeded)
	}

	if n, err := store.ClearSnapshot(ctx, sessionID, snapshotID); err != nil || n != 1 {
		t.Fatalf("ClearSnapshot of the snapshot = (%d, %v), want (1, nil)", n, err)
	}
	if got := snapshotColumnsOf(get()); got.snapshotID != nil || got.provenanceID != nil || got.protocol != nil || got.runtime != nil || got.mintedAt.Valid || got.shadow {
		t.Fatalf("after the clear: %+v; want the snapshot, its provenance and its shadow bit gone", got)
	}
	if n, err := store.ClearSnapshot(ctx, sessionID, snapshotID); err != nil || n != 0 {
		t.Fatalf("ClearSnapshot again = (%d, %v), want (0, nil)", n, err)
	}
}
