package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// This file is the control plane's half of technical plan §35.5b: a
// snapshot carries the runtime it was minted under, and a restore is
// decided on it.
//
//   - At mint, handleSnapshotReadyEvent records what the minting
//     sandbox-agent reported about itself on its snapshot_ready
//     (reportedSnapshotProvenance) in the statement that records the
//     snapshot, keyed by its id (UpdateSandboxSnapshotID). Never a version
//     the control plane holds.
//   - Before every restore, tryPlanSpawn decides on it
//     (sandbox.EvaluateSnapshotRestore): compatible restores; unknown
//     restores too, counted as unknown; incompatible is refused visibly --
//     the sandbox spawns fresh, the snapshot is cleared, and one persisted
//     session warning names it (recordSnapshotRefusal), once the claim has
//     committed a WARN line, and sandbox_snapshot_restore_total counts it.
//   - Every other path that discards a snapshot's context -- §27.8's and
//     §30.4(3)'s restore downgrades, and the review checkout's retirement
//     of a gen whose snapshot it clears (reviewcheckout.go) -- writes its
//     warning through the same helper, once per snapshot.

// reportedSnapshotProvenance returns the provenance a snapshot_ready
// reports -- the protocol the minting agent's binary was compiled with and
// the agent runtime version it discovered -- each nil when it reports none.
// It reads the one key itself, leniently, never through the generated
// sandboxws types, which decode the frame without it (decodeSnapshotReady,
// framekey.go): a provenance that is not an object, or a member that is not
// a string, reports nothing for it and never costs the snapshot.
func reportedSnapshotProvenance(raw json.RawMessage) (agentProtocol, runtimeVersion *string) {
	var frame struct {
		Provenance json.RawMessage `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || len(frame.Provenance) == 0 {
		return nil, nil
	}
	var provenance struct {
		AgentProtocol  json.RawMessage `json:"agentProtocol"`
		RuntimeVersion json.RawMessage `json:"runtimeVersion"`
	}
	if err := json.Unmarshal(frame.Provenance, &provenance); err != nil {
		return nil, nil
	}
	return reportedString(provenance.AgentProtocol), reportedString(provenance.RuntimeVersion)
}

// reportedString is raw when it is a non-empty JSON string, and nil for
// anything else -- absent, null, empty, or another type.
func reportedString(raw json.RawMessage) *string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return nil
	}
	return &s
}

// snapshotProvenance is what row records of the agent that minted its
// snapshot: recorded only while snapshot_provenance_id names that snapshot,
// so a snapshot the previous control-plane binary recorded, or one taken
// before the record existed, reads as not recorded.
func snapshotProvenance(row sqlcgen.Sandbox) sandbox.SnapshotProvenance {
	if row.SnapshotID == nil || row.SnapshotProvenanceID == nil || *row.SnapshotProvenanceID != *row.SnapshotID {
		return sandbox.SnapshotProvenance{}
	}
	return sandbox.SnapshotProvenance{
		Recorded:       true,
		AgentProtocol:  stringOrEmpty(row.SnapshotAgentProtocol),
		RuntimeVersion: stringOrEmpty(row.SnapshotRuntimeVersion),
	}
}

// snapshotMintedAt is when row's snapshot was recorded, invalid when row
// records nothing for that snapshot.
func snapshotMintedAt(row sqlcgen.Sandbox) pgtype.Timestamptz {
	if !snapshotProvenance(row).Recorded {
		return pgtype.Timestamptz{}
	}
	return row.SnapshotMintedAt
}

// restoreFloor is what a snapshot's provenance is measured against: the
// domain's floors and this binary's own protocol.
func restoreFloor() sandbox.RestoreFloor {
	return sandbox.RestoreFloor{
		AgentProtocol:        sandbox.MinRestorableAgentProtocol,
		RuntimeVersion:       sandbox.MinRestorableRuntimeVersion,
		ControlPlaneProtocol: contracts.Version,
	}
}

// snapshotRestoreDecision is the restore decision tryPlanSpawn made on a
// snapshot, carried on its spawnPlan so it is logged and counted only once
// the claim has committed (logSnapshotRestore).
type snapshotRestoreDecision struct {
	snapshotID string
	provenance sandbox.SnapshotProvenance
	floor      sandbox.RestoreFloor
	verdict    sandbox.RestoreVerdict
}

// snapshotRefusal is a restore tryPlanSpawn turned into a fresh spawn: the
// snapshot, the warning that says so, and whether the snapshot is cleared
// -- only for a refusal on its provenance, which never stops applying.
type snapshotRefusal struct {
	snapshotID string
	warning    string
	clear      bool
}

// evaluateSnapshotRestore decides the restore of row's snapshot, snapshotID
// (sandbox.EvaluateSnapshotRestore), and returns the decision, and the
// refusal when the snapshot is not restored.
func evaluateSnapshotRestore(row sqlcgen.Sandbox, snapshotID string) (*snapshotRestoreDecision, *snapshotRefusal) {
	decision := &snapshotRestoreDecision{
		snapshotID: snapshotID,
		provenance: snapshotProvenance(row),
		floor:      restoreFloor(),
	}
	decision.verdict = sandbox.EvaluateSnapshotRestore(decision.provenance, decision.floor)
	if decision.verdict.Restore {
		return decision, nil
	}
	return decision, &snapshotRefusal{
		snapshotID: snapshotID,
		warning:    snapshotProvenanceRefusalWarning(snapshotID, snapshotMintedAt(row), decision.provenance, decision.verdict, decision.floor),
		clear:      true,
	}
}

// The opening and close every warning of a fresh spawn in place of a
// restore shares.
const (
	snapshotRefusalLost = "Work that was not pushed before the snapshot, and the agent's own conversation, are not in the new sandbox."
	snapshotRefusalHint = " If this repeats, rebuild the sandbox image so new snapshots carry a current agent."
)

// freshInsteadOfSnapshot opens a warning of a fresh spawn in place of the
// restore of snapshotID, naming when it was taken when that is recorded.
func freshInsteadOfSnapshot(snapshotID string, mintedAt pgtype.Timestamptz) string {
	taken := ""
	if mintedAt.Valid {
		taken = " (taken " + mintedAt.Time.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return "This session's sandbox was started fresh instead of from its last snapshot, " + snapshotID + taken + ": "
}

// snapshotProvenanceRefusalWarning is the session warning of a restore
// refused on the snapshot's provenance.
func snapshotProvenanceRefusalWarning(snapshotID string, mintedAt pgtype.Timestamptz, p sandbox.SnapshotProvenance, verdict sandbox.RestoreVerdict, floor sandbox.RestoreFloor) string {
	var why string
	switch verdict.Reason {
	case sandbox.ReasonMajor:
		why = fmt.Sprintf("that snapshot holds a sandbox agent built for protocol %s, which this control plane, at protocol %s, cannot restore.", p.AgentProtocol, floor.ControlPlaneProtocol)
	case sandbox.ReasonBelowFloor:
		why = fmt.Sprintf("that snapshot holds a sandbox agent built for protocol %s, and this control plane restores snapshots from protocol %s on.", p.AgentProtocol, floor.AgentProtocol)
	case sandbox.ReasonRuntimeBelowFloor:
		why = fmt.Sprintf("that snapshot holds agent runtime %s, older than the %s this control plane restores.", p.RuntimeVersion, floor.RuntimeVersion)
	default:
		why = "nothing readable was recorded of the sandbox agent that snapshot holds, and this control plane does not restore such a snapshot."
	}
	return freshInsteadOfSnapshot(snapshotID, mintedAt) + why + " " + snapshotRefusalLost + snapshotRefusalHint
}

// dockerRestoreRefusal is §27.8's downgrade made visible: a Docker-required
// session never restores a snapshot. The snapshot is kept, as the
// downgrade always kept it.
func dockerRestoreRefusal(row sqlcgen.Sandbox, snapshotID string) *snapshotRefusal {
	return &snapshotRefusal{
		snapshotID: snapshotID,
		warning: freshInsteadOfSnapshot(snapshotID, snapshotMintedAt(row)) +
			"this session's environment requires Docker, and a snapshot is never restored into a sandbox that runs Docker. " + snapshotRefusalLost,
	}
}

// shadowRestoreRefusal is §30.4(3)'s downgrade made visible: a session in
// shadow mode never restores a snapshot not taken in shadow mode. The
// snapshot is kept: the session may leave shadow mode.
func shadowRestoreRefusal(row sqlcgen.Sandbox, snapshotID string) *snapshotRefusal {
	return &snapshotRefusal{
		snapshotID: snapshotID,
		warning: freshInsteadOfSnapshot(snapshotID, snapshotMintedAt(row)) +
			"this session runs in shadow mode, and that snapshot was not taken in shadow mode, so it may hold a credential a shadow session must not have. " + snapshotRefusalLost,
	}
}

// snapshotRefusalMessageIDPrefix keys the warning recordSnapshotRefusal
// writes: snapshot_restore_refused:{snapshot id}.
const snapshotRefusalMessageIDPrefix = "snapshot_restore_refused:"

// recordSnapshotRefusal writes, in tx, the persisted session warning that
// a snapshot's context was not brought back -- its restore refused or
// downgraded to a fresh spawn, or the snapshot discarded with its gen --
// at gen. Like recordSessionWarning, but under the deterministic messageId
// snapshot_restore_refused:{snapshotID}, which the event store dedupes on:
// one warning per snapshot, whichever path discards it and however often.
// It is stored, so fetch_history and a reload show it, and broadcast after
// the commit; it is not posted to any channel.
func (a *Actor) recordSnapshotRefusal(ctx context.Context, tx pgx.Tx, gen int, snapshotID, message string) error {
	msg := sandboxws.Warning{
		Type:      "warning",
		MessageId: snapshotRefusalMessageIDPrefix + snapshotID,
		SessionId: a.sessionID.String(),
		Gen:       gen,
		Message:   message,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("sessionactor: marshal the snapshot refusal warning: %w", err)
	}
	if _, err := a.appendRawEvent(ctx, tx, "warning", msg.MessageId, raw); err != nil {
		return fmt.Errorf("sessionactor: append the snapshot refusal warning: %w", err)
	}
	return nil
}

// applySnapshotRefusal settles, in the claim's transaction, a restore
// tryPlanSpawn turned into plan, a fresh spawn: the warning, written at the
// new gen, and, for a refusal on the snapshot's provenance, the snapshot
// cleared -- guarded on its id, so a snapshot recorded since is kept.
func (a *Actor) applySnapshotRefusal(ctx context.Context, tx pgx.Tx, plan *spawnPlan, refusal *snapshotRefusal) error {
	if err := a.recordSnapshotRefusal(ctx, tx, plan.gen, refusal.snapshotID, refusal.warning); err != nil {
		return err
	}
	if !refusal.clear {
		return nil
	}
	if _, err := a.stores.sandbox.WithTx(tx).ClearSnapshot(ctx, a.sessionID, refusal.snapshotID); err != nil {
		return fmt.Errorf("sessionactor: clear the refused snapshot: %w", err)
	}
	return nil
}

// The provenance and outcome attributes of sandbox_snapshot_restore_total
// (opsmetrics.go).
const (
	snapshotRestoreOutcomeRestored = "restored"
	snapshotRestoreOutcomeRefused  = "refused"
)

// snapshotRestoreRefusedLogMessage and snapshotRestoreUnknownLogMessage are
// logSnapshotRestore's messages.
const (
	snapshotRestoreRefusedLogMessage = "sessionactor: refusing a snapshot restore: the snapshot's runtime is one this control plane does not restore; spawning fresh"
	snapshotRestoreUnknownLogMessage = "sessionactor: restoring a snapshot whose provenance is unknown"
)

// logSnapshotRestore logs and counts the restore decision plan's claim
// made, after that claim committed (executePlans), never inside it: a
// refusal at WARN, an unknown provenance at INFO, and every decision on
// sandbox_snapshot_restore_total. A plan that decided none -- a spawn that
// was never a restore, a resume -- records nothing.
func (a *Actor) logSnapshotRestore(ctx context.Context, plan *spawnPlan) {
	d := plan.restoreDecision
	if d == nil {
		return
	}
	outcome := snapshotRestoreOutcomeRestored
	if !d.verdict.Restore {
		outcome = snapshotRestoreOutcomeRefused
	}
	fields := []any{
		"session_id", a.sessionID.String(), "gen", plan.gen, "snapshot_id", d.snapshotID,
		"snapshot_provenance_recorded", d.provenance.Recorded,
		"snapshot_agent_protocol", d.provenance.AgentProtocol, "snapshot_runtime_version", d.provenance.RuntimeVersion,
		"floor_agent_protocol", d.floor.AgentProtocol, "floor_runtime_version", d.floor.RuntimeVersion,
		"control_plane_protocol", d.floor.ControlPlaneProtocol,
		"provenance", string(d.verdict.State), "reason", string(d.verdict.Reason),
	}
	switch {
	case !d.verdict.Restore:
		a.logger.Warn(snapshotRestoreRefusedLogMessage, fields...)
	case d.verdict.State == sandbox.ProvenanceUnknown:
		a.logger.Info(snapshotRestoreUnknownLogMessage, fields...)
	}
	if a.opsMetrics.snapshotRestore == nil {
		return
	}
	a.opsMetrics.snapshotRestore.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provenance", string(d.verdict.State)),
		attribute.String("outcome", outcome),
	))
}
