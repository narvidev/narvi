//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/contracts"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves technical plan §35.5b on the real actor and real
// Postgres: a snapshot carries what the sandbox-agent that minted it
// reported about itself, recorded at mint and kept across every restore;
// and a restore is decided on it -- compatible restores, unknown restores
// and is counted as unknown, and incompatible is refused visibly: a fresh
// spawn instead, the snapshot cleared, one persisted session warning, a
// WARN line and sandbox_snapshot_restore_total.

// provenanceRow is the part of a sandbox row the provenance is.
type provenanceRow struct {
	gen                           int32
	snapshotID, provenanceID      *string
	agentProtocol, runtimeVersion *string
	mintedAt                      pgtype.Timestamptz
	suppressedInShadow            bool
	status                        sqlcgen.SandboxStatus
}

func readProvenanceRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) provenanceRow {
	t.Helper()
	row, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return provenanceRow{
		gen: row.Gen, snapshotID: row.SnapshotID, provenanceID: row.SnapshotProvenanceID,
		agentProtocol: row.SnapshotAgentProtocol, runtimeVersion: row.SnapshotRuntimeVersion,
		mintedAt: row.SnapshotMintedAt, suppressedInShadow: row.SnapshotSuppressedInShadow, status: row.Status,
	}
}

func (r provenanceRow) String() string {
	str := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	return fmt.Sprintf("gen %d %s, snapshot %s, provenance id %s, protocol %s, runtime %s, minted %v",
		r.gen, r.status, str(r.snapshotID), str(r.provenanceID), str(r.agentProtocol), str(r.runtimeVersion), r.mintedAt.Valid)
}

// snapshottingSandbox makes sessionID's sandbox, at gen, Snapshotting with
// an outstanding attempt cmdID -- creating it at gen 1 when it has none.
func snapshottingSandbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, cmdID string) {
	t.Helper()
	store := narvipg.NewSandboxStore(pool)
	if _, err := store.Get(ctx, sessionID); err != nil {
		if _, err := store.Create(ctx, sessionID); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}
	if _, err := store.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusSnapshotting}); err != nil {
		t.Fatalf("move sandbox to snapshotting: %v", err)
	}
	if _, err := store.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID: sessionID, PendingSnapshotMessageID: &cmdID,
	}); err != nil {
		t.Fatalf("seed pending snapshot message id: %v", err)
	}
}

// snapshotReady is the agent's snapshot_ready of gen answering cmdID with
// snapshotID; provenance is the raw value of its provenance key, "" for
// none.
func snapshotReady(gen int, cmdID, snapshotID, provenance string) SandboxEvent {
	key := ""
	if provenance != "" {
		key = `,"provenance":` + provenance
	}
	id := "sr-" + snapshotID
	return SandboxEvent{Type: "snapshot_ready", Gen: gen, MessageID: id, Raw: json.RawMessage(fmt.Sprintf(
		`{"type":"snapshot_ready","messageId":%q,"sessionId":"s","gen":%d,"ackId":"snapshot_ready:%s","snapshotId":%q,"commandMessageId":%q%s}`,
		id, gen, id, snapshotID, cmdID, key))}
}

// mintSnapshot drives one snapshot of sessionID's sandbox at gen through
// the actor, as the agent's snapshot_ready.
func mintSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool, a *Actor, sessionID pgtype.UUID, gen int, snapshotID, provenance string) {
	t.Helper()
	cmdID := "cmd-" + snapshotID
	snapshottingSandbox(ctx, t, pool, sessionID, cmdID)
	if outcome := sendSandboxEvent(ctx, t, a, snapshotReady(gen, cmdID, snapshotID, provenance)); !outcome.Persisted {
		t.Fatalf("snapshot_ready of %s: Persisted = false, want true", snapshotID)
	}
}

// seedStoppedSandboxWithProvenance is seedStoppedSandboxWithSnapshot
// (dispatch_integration_test.go) with what the minting agent reported.
func seedStoppedSandboxWithProvenance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, snapshotID string, protocol, runtime *string) {
	t.Helper()
	store := narvipg.NewSandboxStore(pool)
	if _, err := store.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := store.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusStopped}); err != nil {
		t.Fatalf("move sandbox to stopped: %v", err)
	}
	if _, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID: sessionID, SnapshotID: &snapshotID, AgentProtocol: protocol, RuntimeVersion: runtime,
	}); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
}

// snapshotRestoreCount is sandbox_snapshot_restore_total{provenance,
// outcome}.
func snapshotRestoreCount(ctx context.Context, t *testing.T, provenance sandbox.ProvenanceState, outcome string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "sandbox_snapshot_restore_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("sandbox_snapshot_restore_total data = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				p, _ := dp.Attributes.Value(attribute.Key("provenance"))
				o, _ := dp.Attributes.Value(attribute.Key("outcome"))
				if p.AsString() == string(provenance) && o.AsString() == outcome {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// restoreCounts reads every label pair of sandbox_snapshot_restore_total.
func restoreCounts(ctx context.Context, t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, p := range []sandbox.ProvenanceState{sandbox.ProvenanceCompatible, sandbox.ProvenanceIncompatible, sandbox.ProvenanceUnknown} {
		for _, o := range []string{snapshotRestoreOutcomeRestored, snapshotRestoreOutcomeRefused} {
			out[string(p)+"/"+o] = snapshotRestoreCount(ctx, t, p, o)
		}
	}
	return out
}

// countDelta is after minus before, for every label pair that moved.
func countDelta(before, after map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			out[k] = d
		}
	}
	return out
}

// storedWarning is one warning event as the event store lists it.
type storedWarning struct {
	messageID string
	gen       int
	message   string
}

// storedWarnings lists sessionID's warnings through the event store's own
// read, the one fetch_history and the REST history serve.
func storedWarnings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []storedWarning {
	t.Helper()
	events, err := narvipg.NewEventStore(pool).ListForSession(ctx, sessionID, 0, 1000)
	if err != nil {
		t.Fatalf("list the session's events: %v", err)
	}
	var out []storedWarning
	for _, e := range events {
		if e.Type != "warning" {
			continue
		}
		var w struct {
			MessageID string `json:"messageId"`
			Gen       int    `json:"gen"`
			Message   string `json:"message"`
		}
		if err := json.Unmarshal(e.Payload, &w); err != nil {
			t.Fatalf("decode a stored warning: %v", err)
		}
		out = append(out, storedWarning{messageID: w.MessageID, gen: w.Gen, message: w.Message})
	}
	return out
}

// TestSnapshotProvenance_RecordedAtMintFromTheAgentsOwnReport: the
// snapshot_ready that records a snapshot records, in the same statement,
// what the minting agent reported about itself, keyed by the snapshot's
// id, with the database's instant of the mint.
func TestSnapshotProvenance_RecordedAtMintFromTheAgentsOwnReport(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	tests := []struct {
		name              string
		provenance        string
		protocol, runtime *string
	}{
		{"both reported", `{"agentProtocol":"1.25.0","runtimeVersion":"1.14.19"}`, strPtr("1.25.0"), strPtr("1.14.19")},
		{"no runtime version discovered", `{"agentProtocol":"1.25.0","runtimeVersion":null}`, strPtr("1.25.0"), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			var before time.Time
			if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			mintSnapshot(ctx, t, pool, a, sessionID, 1, "snap-mint", tc.provenance)

			row := readProvenanceRow(ctx, t, pool, sessionID)
			if row.status != sqlcgen.SandboxStatusReady || row.snapshotID == nil || *row.snapshotID != "snap-mint" ||
				row.provenanceID == nil || *row.provenanceID != "snap-mint" ||
				stringOrEmpty(row.agentProtocol) != stringOrEmpty(tc.protocol) || (row.agentProtocol == nil) != (tc.protocol == nil) ||
				stringOrEmpty(row.runtimeVersion) != stringOrEmpty(tc.runtime) || (row.runtimeVersion == nil) != (tc.runtime == nil) {
				t.Fatalf("after the mint: %v; want ready, snap-mint keyed, protocol %v, runtime %v", row, stringOrEmpty(tc.protocol), stringOrEmpty(tc.runtime))
			}
			if !row.mintedAt.Valid || row.mintedAt.Time.Before(before) {
				t.Fatalf("snapshot_minted_at = %v, want the database's instant of the mint, at or after %v", row.mintedAt, before)
			}
		})
	}
}

// TestSnapshotProvenance_NeverTheControlPlanesOwnVersions: what is
// recorded is the agent's own report, whatever this control plane's
// protocol and runtime configuration are -- a protocol this control plane
// has never heard of is recorded as it is, and an agent that reports
// nothing records nothing, never contracts.Version or the runtime version
// this control plane would configure.
func TestSnapshotProvenance_NeverTheControlPlanesOwnVersions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const configuredRuntime = "9.9.9-control-plane"
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, configuredRuntime, nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	tests := []struct {
		name              string
		provenance        string
		protocol, runtime *string
	}{
		{"a protocol newer than this control plane's", `{"agentProtocol":"1.99.0","runtimeVersion":"0.0.1-agent"}`, strPtr("1.99.0"), strPtr("0.0.1-agent")},
		{"an agent that reports nothing", "", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			mintSnapshot(ctx, t, pool, a, sessionID, 1, "snap-own", tc.provenance)

			row := readProvenanceRow(ctx, t, pool, sessionID)
			if row.provenanceID == nil || *row.provenanceID != "snap-own" {
				t.Fatalf("after the mint: %v; want the provenance keyed to snap-own", row)
			}
			for name, got := range map[string]*string{"protocol": row.agentProtocol, "runtime": row.runtimeVersion} {
				if got != nil && (*got == contracts.Version || *got == configuredRuntime) {
					t.Errorf("recorded %s %q is the control plane's own version", name, *got)
				}
			}
			if stringOrEmpty(row.agentProtocol) != stringOrEmpty(tc.protocol) || (row.agentProtocol == nil) != (tc.protocol == nil) ||
				stringOrEmpty(row.runtimeVersion) != stringOrEmpty(tc.runtime) || (row.runtimeVersion == nil) != (tc.runtime == nil) {
				t.Fatalf("after the mint: %v; want exactly what the agent reported: protocol %v, runtime %v", row, tc.protocol, tc.runtime)
			}
		})
	}
}

// TestSnapshotProvenance_MalformedProvenanceNeverCostsTheSnapshot: a
// provenance of the wrong shape records the snapshot, returns the sandbox
// to Ready, and records nothing for the members it cannot read, which
// reads "provenance unknown" -- never a failed decode that reverts the
// snapshot.
func TestSnapshotProvenance_MalformedProvenanceNeverCostsTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	for _, provenance := range []string{`{"agentProtocol":42}`, `{"agentProtocol":"1.25.0","runtimeVersion":["1.14.19"]}`, `"1.25.0"`} {
		t.Run(provenance, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			mintSnapshot(ctx, t, pool, a, sessionID, 1, "snap-malformed", provenance)

			row := readProvenanceRow(ctx, t, pool, sessionID)
			if row.status != sqlcgen.SandboxStatusReady || row.snapshotID == nil || *row.snapshotID != "snap-malformed" ||
				row.provenanceID == nil || *row.provenanceID != "snap-malformed" || row.runtimeVersion != nil {
				t.Fatalf("after a snapshot_ready with provenance %s: %v; want ready, snap-malformed recorded and keyed, no runtime", provenance, row)
			}
			if provenance == `{"agentProtocol":42}` || provenance == `"1.25.0"` {
				if row.agentProtocol != nil {
					t.Fatalf("protocol recorded %q from %s, want none", *row.agentProtocol, provenance)
				}
			}
		})
	}
}

// TestSnapshotProvenance_KeptAcrossEveryRestore: a restore leaves the
// provenance of the snapshot it restored on the row, and the restored gen
// -- running the snapshot's own agent -- reports the same at its next
// mint, two restores deep.
func TestSnapshotProvenance_KeptAcrossEveryRestore(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-provenance"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	const reported = `{"agentProtocol":"1.25.0","runtimeVersion":"1.14.19"}`
	assertKept := func(gen int32, snapshotID string) {
		t.Helper()
		row := readProvenanceRow(ctx, t, pool, sessionID)
		if row.gen != gen || row.snapshotID == nil || *row.snapshotID != snapshotID || row.provenanceID == nil || *row.provenanceID != snapshotID ||
			stringOrEmpty(row.agentProtocol) != "1.25.0" || stringOrEmpty(row.runtimeVersion) != "1.14.19" || !row.mintedAt.Valid {
			t.Fatalf("row: %v; want gen %d still describing %s as 1.25.0 / 1.14.19", row, gen, snapshotID)
		}
	}
	stopAndRestore := func(restores int, snapshotID string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
		sendEnsureDispatched(ctx, t, a)
		waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount() == restores })
		waitForConnecting(ctx, t, pool, sessionID)
		if call := provider.lastRestoreCall(); string(call.snapshotID) != snapshotID {
			t.Fatalf("restore %d restored %q, want %q", restores, call.snapshotID, snapshotID)
		}
	}

	mintSnapshot(ctx, t, pool, a, sessionID, 1, "snap-s1", reported)
	assertKept(1, "snap-s1")

	// A follow-up arrives for a sandbox that has stopped: it is restored.
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "kept across restores")
	stopAndRestore(1, "snap-s1")
	assertKept(2, "snap-s1")

	// The restored gen runs the snapshot's agent, which reports the same.
	mintSnapshot(ctx, t, pool, a, sessionID, 2, "snap-s2", reported)
	assertKept(2, "snap-s2")

	stopAndRestore(2, "snap-s2")
	assertKept(3, "snap-s2")
	if provider.callCount() != 0 {
		t.Fatalf("fresh spawns %d, want none: every restore of a compatible snapshot restores", provider.callCount())
	}
}

// TestSnapshotRestore_IncompatibleIsRefusedVisibly: a snapshot whose
// minting agent speaks a protocol below the floor, or of another MAJOR, is
// never restored. The sandbox is spawned fresh instead, the snapshot is
// cleared with its provenance, one persisted warning at the new gen names
// the snapshot and the versions on either side -- listed by the event store
// after the commit -- a WARN line says so, and
// sandbox_snapshot_restore_total{incompatible, refused} counts it.
func TestSnapshotRestore_IncompatibleIsRefusedVisibly(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		reason   sandbox.ProvenanceReason
		named    []string
	}{
		{"BelowTheFloor", "1.24.0", sandbox.ReasonBelowFloor, []string{"protocol 1.24.0", "from protocol " + sandbox.MinRestorableAgentProtocol + " on"}},
		{"OtherMajor", "2.0.0", sandbox.ReasonMajor, []string{"protocol 2.0.0", "at protocol " + contracts.Version + ", cannot restore"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "refused restore")
			const snapshotID = "snap-incompatible"
			seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, snapshotID, strPtr(tc.protocol), strPtr("1.14.19"))

			logs := captureDefaultLoggerJSONSync(t)
			before := restoreCounts(ctx, t)
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "fresh-instead"}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })
			waitForConnecting(ctx, t, pool, sessionID)

			if provider.restoreCallCount() != 0 {
				t.Fatalf("RestoreFromSnapshot called %d times, want 0", provider.restoreCallCount())
			}
			row := readProvenanceRow(ctx, t, pool, sessionID)
			if row.gen != 2 || row.snapshotID != nil || row.provenanceID != nil || row.agentProtocol != nil || row.runtimeVersion != nil || row.mintedAt.Valid {
				t.Fatalf("after the refusal: %v; want gen 2 with the snapshot and its provenance cleared", row)
			}
			warnings := storedWarnings(ctx, t, pool, sessionID)
			if len(warnings) != 1 || warnings[0].messageID != snapshotRefusalMessageIDPrefix+snapshotID || warnings[0].gen != 2 {
				t.Fatalf("warnings %+v, want exactly one, %s%s, at gen 2", warnings, snapshotRefusalMessageIDPrefix, snapshotID)
			}
			for _, want := range append([]string{snapshotID, "(taken ", "started fresh"}, tc.named...) {
				if !strings.Contains(warnings[0].message, want) {
					t.Errorf("warning %q does not name %q", warnings[0].message, want)
				}
			}
			line := waitForLogEntry(t, logs, 5*time.Second, snapshotRestoreRefusedLogMessage)
			if line["level"] != "WARN" || line["snapshot_id"] != snapshotID || line["snapshot_agent_protocol"] != tc.protocol ||
				line["floor_agent_protocol"] != sandbox.MinRestorableAgentProtocol || line["control_plane_protocol"] != contracts.Version ||
				line["reason"] != string(tc.reason) || line["gen"] != float64(2) || line["session_id"] != sessionID.String() {
				t.Errorf("logged %v, want a WARN naming the snapshot, %s, the floor, the control plane's protocol, %s and gen 2", line, tc.protocol, tc.reason)
			}
			if got := countDelta(before, restoreCounts(ctx, t)); len(got) != 1 || got["incompatible/refused"] != 1 {
				t.Errorf("sandbox_snapshot_restore_total moved by %v, want incompatible/refused by 1 alone", got)
			}
		})
	}
}

// TestSnapshotRestore_RefusedSnapshotIsNotOfferedAgain: once refused, the
// snapshot is gone, so a later death of the fresh gen spawns fresh again
// rather than deciding on the same snapshot -- and writes no second
// warning.
func TestSnapshotRestore_RefusedSnapshotIsNotOfferedAgain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "not offered again")
	seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-once", strPtr("1.24.0"), nil)
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "fresh"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })
	waitForConnecting(ctx, t, pool, sessionID)
	before := restoreCounts(ctx, t)

	// The fresh gen dies before it takes a snapshot of its own.
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 2 })
	waitForConnecting(ctx, t, pool, sessionID)

	if provider.restoreCallCount() != 0 {
		t.Fatalf("RestoreFromSnapshot called %d times, want 0", provider.restoreCallCount())
	}
	if row := readProvenanceRow(ctx, t, pool, sessionID); row.gen != 3 || row.snapshotID != nil {
		t.Fatalf("after the second death: %v; want gen 3, no snapshot", row)
	}
	if w := storedWarnings(ctx, t, pool, sessionID); len(w) != 1 {
		t.Fatalf("warnings %+v, want the one written at the refusal", w)
	}
	if got := countDelta(before, restoreCounts(ctx, t)); len(got) != 0 {
		t.Fatalf("sandbox_snapshot_restore_total moved by %v on a spawn with no snapshot, want nothing", got)
	}
}

// TestSnapshotRestore_UnknownProvenance_RestoredAndCountedAsUnknown: a
// snapshot whose provenance was never recorded -- every snapshot that
// predates the record, or one the previous control-plane binary recorded
// over another's provenance -- or whose agent reported none, or one that
// does not parse, is restored, with no warning, logged at INFO and counted
// on its own label, never as compatible. The middle case is the one a
// snapshot recorded by the previous binary is: its id no longer matches
// the provenance left by the snapshot before it, a protocol below the
// floor, which is never read for it.
func TestSnapshotRestore_UnknownProvenance_RestoredAndCountedAsUnknown(t *testing.T) {
	tests := []struct {
		name   string
		seed   func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID)
		reason sandbox.ProvenanceReason
	}{
		{"recorded before the record existed", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
			seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-unknown", nil, nil)
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_provenance_id = NULL, snapshot_minted_at = NULL WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
		}, sandbox.ReasonNotRecorded},
		{"recorded by the previous binary over a refusable snapshot's provenance", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
			seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-older", strPtr("1.24.0"), nil)
			// The previous binary's UpdateSandboxSnapshotID, as its sqlc
			// output sent it: snapshot_id alone moves.
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = $2, snapshot_suppressed_in_shadow = $3, pending_snapshot_message_id = NULL, updated_at = now() WHERE session_id = $1`,
				sessionID, "snap-unknown", false); err != nil {
				t.Fatal(err)
			}
		}, sandbox.ReasonNotRecorded},
		{"an agent that reported nothing", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
			seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-unknown", nil, strPtr("1.14.19"))
		}, sandbox.ReasonNotReported},
		{"a protocol that does not parse", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
			seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-unknown", strPtr("v0.1"), nil)
		}, sandbox.ReasonUnreadable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "unknown restore")
			tc.seed(ctx, t, pool, sessionID)

			logs := captureDefaultLoggerJSONSync(t)
			before := restoreCounts(ctx, t)
			provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-unknown"}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount() == 1 })
			waitForConnecting(ctx, t, pool, sessionID)

			if provider.callCount() != 0 || provider.lastRestoreCall().snapshotID != "snap-unknown" {
				t.Fatalf("fresh spawns %d, restored %q; want snap-unknown restored", provider.callCount(), provider.lastRestoreCall().snapshotID)
			}
			if row := readProvenanceRow(ctx, t, pool, sessionID); row.snapshotID == nil || *row.snapshotID != "snap-unknown" {
				t.Fatalf("after the restore: %v; want the snapshot kept", row)
			}
			if w := storedWarnings(ctx, t, pool, sessionID); len(w) != 0 {
				t.Fatalf("warnings %+v, want none for a restore", w)
			}
			line := waitForLogEntry(t, logs, 5*time.Second, snapshotRestoreUnknownLogMessage)
			if line["level"] != "INFO" || line["provenance"] != string(sandbox.ProvenanceUnknown) || line["reason"] != string(tc.reason) || line["snapshot_id"] != "snap-unknown" {
				t.Errorf("logged %v, want an INFO line naming snap-unknown as unknown, %s", line, tc.reason)
			}
			if n := countLogLines(t, logs, snapshotRestoreRefusedLogMessage); n != 0 {
				t.Errorf("%d refusal lines, want none", n)
			}
			if got := countDelta(before, restoreCounts(ctx, t)); len(got) != 1 || got["unknown/restored"] != 1 {
				t.Errorf("sandbox_snapshot_restore_total moved by %v, want unknown/restored by 1 alone", got)
			}
		})
	}
}

// TestSnapshotRestore_Compatible_Restored: a snapshot whose agent speaks
// this control plane's protocol is restored, counted as compatible, with
// no warning and no line of its own.
func TestSnapshotRestore_Compatible_Restored(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "compatible restore")
	seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-compatible", strPtr(contracts.Version), strPtr("1.14.19"))

	logs := captureDefaultLoggerJSONSync(t)
	before := restoreCounts(ctx, t)
	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-compatible"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount() == 1 })
	waitForConnecting(ctx, t, pool, sessionID)

	if provider.callCount() != 0 {
		t.Fatalf("fresh spawns %d, want none", provider.callCount())
	}
	if w := storedWarnings(ctx, t, pool, sessionID); len(w) != 0 {
		t.Fatalf("warnings %+v, want none", w)
	}
	if got := countDelta(before, restoreCounts(ctx, t)); len(got) != 1 || got["compatible/restored"] != 1 {
		t.Errorf("sandbox_snapshot_restore_total moved by %v, want compatible/restored by 1 alone", got)
	}
	if n := countLogLines(t, logs, snapshotRestoreRefusedLogMessage) + countLogLines(t, logs, snapshotRestoreUnknownLogMessage); n != 0 {
		t.Errorf("%d refusal or unknown lines for a compatible restore, want none", n)
	}
}

// TestSnapshotRestore_ARolledBackClaimRecordsNothing: the refusal is
// written in the claim's own transaction, and logged and counted only once
// it commits. A claim that rolls back -- here at the public base URL's
// scheme, which the fresh spawn's session config refuses after the
// upsert -- leaves the snapshot, no warning, no WARN line and no count.
func TestSnapshotRestore_ARolledBackClaimRecordsNothing(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "rolled back")
	seedStoppedSandboxWithProvenance(ctx, t, pool, sessionID, "snap-rollback", strPtr("1.24.0"), nil)

	logs := captureDefaultLoggerJSONSync(t)
	before := restoreCounts(ctx, t)
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "never"}}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, provider, "ftp://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitForLogEntry(t, logs, 5*time.Second, "sessionactor: command handling failed")

	if provider.callCount() != 0 || provider.restoreCallCount() != 0 {
		t.Fatalf("provider called (%d spawns, %d restores), want neither", provider.callCount(), provider.restoreCallCount())
	}
	if row := readProvenanceRow(ctx, t, pool, sessionID); row.gen != 1 || row.snapshotID == nil || *row.snapshotID != "snap-rollback" {
		t.Fatalf("after the rolled-back claim: %v; want gen 1 with its snapshot", row)
	}
	if w := storedWarnings(ctx, t, pool, sessionID); len(w) != 0 {
		t.Fatalf("warnings %+v after a rolled-back claim, want none", w)
	}
	if n := countLogLines(t, logs, snapshotRestoreRefusedLogMessage); n != 0 {
		t.Errorf("a rolled-back refusal was logged %d times", n)
	}
	if got := countDelta(before, restoreCounts(ctx, t)); len(got) != 0 {
		t.Errorf("sandbox_snapshot_restore_total moved by %v for a rolled-back claim, want nothing", got)
	}
}
