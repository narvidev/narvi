// This file (runway.go) is the session actor's half of the pre-dispatch
// runway gate (technical plan §35.3): before a turn is dispatched into a
// live sandbox with less than min(RotationRunwayFloor, lifetime/6) of its
// lifetime left, the sandbox is rotated, and the turn stays pending for the
// replacement. That is no guarantee: the rotation is declined, and the turn
// dispatched below the threshold as before, when no deadline is known, for
// a re-send of a turn in flight, to a suspect sandbox, and for a session
// that cannot snapshot (EvaluateRunway's rows 8 and 10 to 12), so the queue
// never stalls on the gate. The decision is the pure
// sandbox.EvaluateRunway, over facts this file reads in one statement on
// the database's clock (GetSandboxRunway); this file only acts on it.
//
// # The rotation is a sequence of dispatch evaluations
//
// No state, trigger or edge is added. A rotation is data on the sandbox row
// -- the gen rotating, since when, and the snapshot command it waits on --
// and each evaluation of a pending turn reads it:
//
//   - ready, below the threshold: ready -> snapshotting over
//     snapshot_start, the rotation and its snapshot command recorded, the
//     dispatch timer armed at RotationSnapshotWait; after the commit the
//     snapshot command is sent (sendRotationSnapshot). A send that fails
//     reverts the sandbox to ready, and the next evaluation retires it.
//   - the snapshot reported (handleSnapshotReadyEvent, whose own
//     evaluation follows), or the wait run out: the gen is retired, live ->
//     suspect -> stopped (retireLiveGen), its pending snapshot command
//     forgotten; after the commit `shutdown` is sent to it and its provider
//     object stopped (finishRotationRetirement), and the evaluation runs
//     again at once.
//   - stopped: tryPlanSpawn's ordinary decision restores the last snapshot
//     recorded -- the rotation's, or the one before it when the rotation's
//     was never reported -- or spawns fresh, through every gate a restore
//     passes (§27.8, §30.4(3), the snapshot's provenance, §35.5b). The
//     rotation never calls planRestore itself. A restore of a snapshot taken
//     before the rotation started writes the fallback warning
//     (recordRotationFallback).
//
// A sandbox past its deadline whose status still reads live, and whose
// agent is silent, is suspected through the liveness path before anything
// is sent to it -- terminal grace, recovered by any sign of life, failed and
// replaced when the grace runs out -- since silence can be frames that
// could not be recorded; one whose agent is still heard from has outlived
// the claim's estimate, and is rotated through its own snapshot once its
// snapshot in progress and its delivery are done. A rotating gen's
// retirement waits for an open delivery too. A snapshot that is not a
// rotation's is bounded by SnapshotReportWait, then read by its agent's
// silence: suspected through the liveness path when the agent is silent,
// given up -- the sandbox back to ready -- when the agent was heard from
// after the wait and past its connection's replay, and held otherwise
// until one or the other (EvaluateRunway's rows 7 to 7d). An agent that
// reconnects sends ready, then replays its unacked report, so a ready is
// no sign that no report is coming: the give-up waits for the second
// heartbeat after it (handleSandboxEvent counts them), and a replayed
// report is recorded.
//
// # What the gate composes with
//
//   - The session spend cap (§40.1): planDispatch ends a capped session's
//     pending turns before the gate, so a capped session never rotates.
//   - The autonomy freeze (§40.2): a rotation is not an automatic action
//     (the dispatch of a turn already enqueued is not one; domain/autonomy
//     names sandbox rotation among the loops the freeze never gates), so
//     the gate never consults it.
//   - A review turn's checkout (§21.1): the gate runs before
//     applyReviewCheckout, so no checkout is sent to a gen about to rotate,
//     and the new gen is asked again, since a checkout is requested per gen.
//
// RotationRunwayFloor and RotationSnapshotWait are read here and in
// platform.Timeouts.Validate only (TestRotationFieldsAreReadOnlyInPlatform).

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/platform"
)

// runwayNote is one decision of the gate, with what it was measured on,
// logged and counted once the evaluation that made it has committed
// (recordRunwayGate), never inside it.
type runwayNote struct {
	gen      int32
	facts    sandbox.RunwayFacts
	decision sandbox.RunwayDecision
	// pendingSnapshot is the messageId of the snapshot command the
	// sandbox waited on, "" for none.
	pendingSnapshot string
}

// rotationSnapshotPlan is the snapshot command of a rotation the gate
// recorded, which handleEnsureDispatched sends after the commit.
type rotationSnapshotPlan struct {
	gen       int32
	messageID string
}

// rotationRetirement is a gen the gate retired: handleEnsureDispatched
// sends it `shutdown` and stops its provider object after the commit, then
// evaluates again.
type rotationRetirement struct {
	retired retiredGen
	reason  sandbox.RunwayReason
}

// runwayGateResult is what applyRunwayGate tells its caller to do.
type runwayGateResult struct {
	// note is the decision, recorded once the evaluation commits.
	note *runwayNote
	// action is the decision's action: pass lets the evaluation go on.
	action sandbox.RunwayAction
	// snapshot is set for a rotation, retire for a retirement.
	snapshot *rotationSnapshotPlan
	retire   *rotationRetirement
}

// runwayConfig is the gate's configuration from platform.Timeouts.
func (a *Actor) runwayConfig() sandbox.RunwayConfig {
	return sandbox.RunwayConfig{
		Floor:           a.timeouts.RotationRunwayFloor,
		SnapshotWait:    a.timeouts.RotationSnapshotWait,
		ReportWait:      a.timeouts.SnapshotReportWait,
		HeartbeatBudget: a.timeouts.SteadyHeartbeatBudget,
		Grace:           a.timeouts.TerminalGracePeriod,
	}
}

// shortestLifetime is the lifetime the threshold is measured against when
// the gen's row records none -- its deadline came from an agent's report
// alone, or it has none: the shortest any kind is given, which is the
// conservative direction for the threshold's lifetime/6 cap.
func (a *Actor) shortestLifetime() time.Duration {
	var shortest time.Duration
	for _, kind := range sandbox.AllLifetimeKinds() {
		if l := a.timeouts.SandboxLifetimeFor(kind); shortest == 0 || l < shortest {
			shortest = l
		}
	}
	return shortest
}

// runwayFacts builds the gate's facts from row, the runway read. A
// snapshotting row whose start is unknown is taken to start now: the caller
// has just stamped it so.
func (a *Actor) runwayFacts(row sqlcgen.GetSandboxRunwayRow, inFlight bool) sandbox.RunwayFacts {
	facts := sandbox.RunwayFacts{
		Status:                  sandbox.State(row.Status),
		DeadlineKnown:           row.DeadlineKnown,
		Runway:                  time.Duration(row.RunwayNanos),
		Lifetime:                platform.SecondsToDuration(row.LifetimeSeconds),
		TurnInFlight:            inFlight,
		CanSnapshot:             true, // provisional: canRotate reads it only when a row below the declines decides
		Rotating:                row.Rotating,
		RotationSnapshotPending: row.RotationSnapshotPending,
		SinceRotationStart:      time.Duration(row.SinceRotationNanos),
		RotationSnapshotTaken:   row.RotationSnapshotTaken,
		SinceLastSeen:           time.Duration(row.SinceLastSeenNanos),
		SuspectedMidSnapshot:    row.SuspectedMidSnapshot,
		ReplayPending:           row.ReplayPending,
	}
	if row.LifetimeSeconds <= 0 {
		facts.Lifetime = a.shortestLifetime()
	}
	if row.SnapshotStartKnown {
		facts.SinceSnapshotStart = time.Duration(row.SinceSnapshotStartNanos)
	}
	if row.PrDeliveryStartedAt.Valid && row.ObservedAt.Valid {
		started, observed := row.PrDeliveryStartedAt.Time, row.ObservedAt.Time
		if session.PRDeliveryOpen(started, observed, a.timeouts.MCPStatusDeliveryWindow) {
			facts.DeliveryOpen = true
			facts.DeliveryRemaining = started.Add(a.timeouts.MCPStatusDeliveryWindow).Sub(observed)
		}
	}
	return facts
}

// canRotate reports whether the session's sandbox can be rotated: the
// provider takes snapshots, a commander can send the snapshot command, and
// the session needs no container daemon (§27.8: such a sandbox is never
// snapshotted, triggerSnapshotBestEffort). The environment read is made
// only here, when a row below the declines would decide.
func (a *Actor) canRotate(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) (bool, error) {
	if a.provider == nil || a.commander == nil || !a.provider.Capabilities().Snapshots {
		return false, nil
	}
	_, dockerRequired, _, err := a.environmentSubstrate(ctx, tx, sessionRow.EnvironmentID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: runway gate: resolve whether the session needs a container daemon: %w", err)
	}
	return !dockerRequired, nil
}

// applyRunwayGate decides, inside the dispatch evaluation's transaction,
// what the evaluation does with sandboxRow -- its sandbox, ready, suspect
// or snapshotting -- before a turn is sent to it, and acts on the
// decision (this file's top comment). inFlight is true when the turn to
// send is already processing, re-sent to a newer gen: nothing is rotated
// then (row 10), and only a silence past the deadline and a rotation
// already under way act.
//
//   - pass: nothing; the evaluation goes on.
//   - hold: the dispatch timer armed at the decision's next look.
//   - rotate: ready -> snapshotting, the rotation recorded, the dispatch
//     timer armed at the snapshot wait; the snapshot command is sent after
//     the commit.
//   - retire: a rotating gen retired to stopped, its pending snapshot
//     command forgotten, so a late report of it is ignored, and the
//     dispatch timer armed due at once, which the caller's next evaluation
//     takes.
//   - revert: snapshotting -> ready, the pending snapshot command
//     forgotten, the ready watchdogs armed, and the dispatch timer armed due
//     at once.
//   - suspect: ready or snapshotting -> suspect through
//     transitionSandboxToSuspect, the liveness path every silence takes:
//     terminal_grace armed, a pending snapshot command kept, so a sign of
//     life during the grace recovers the sandbox -- an agent that
//     reconnects sends ready, then replays its unacked report, which is
//     recorded, since the give-up waits for the connection's replay -- and
//     a grace that runs out fails it, and that timer's own evaluation
//     restores or spawns the replacement. Nothing is dispatched meanwhile
//     (EvaluateRunway's rows 2a and 7d).
func (a *Actor) applyRunwayGate(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, inFlight bool) (runwayGateResult, error) {
	sandboxes := a.stores.sandbox.WithTx(tx)
	row, err := sandboxes.Runway(ctx, a.sessionID)
	if err != nil {
		return runwayGateResult{}, fmt.Errorf("sessionactor: runway gate: read the sandbox's runway: %w", err)
	}
	if sandbox.State(row.Status) == sandbox.StateSnapshotting && !row.SnapshotStartKnown {
		// A snapshot with no start: in progress when migration 000171 ran,
		// or started by the previous binary. Its wait runs from now.
		if _, err := sandboxes.StampSnapshotStart(ctx, a.sessionID, row.Gen); err != nil {
			return runwayGateResult{}, fmt.Errorf("sessionactor: runway gate: record the start of a snapshot in progress: %w", err)
		}
	}

	cfg := a.runwayConfig()
	facts := a.runwayFacts(row, inFlight)
	decision := sandbox.EvaluateRunway(facts, cfg)
	if decision.Reason == sandbox.RunwayReasonBelowThreshold || decision.Reason == sandbox.RunwayReasonDeliveryOpen {
		can, err := a.canRotate(ctx, tx, sessionRow)
		if err != nil {
			return runwayGateResult{}, err
		}
		if !can {
			facts.CanSnapshot = false
			decision = sandbox.EvaluateRunway(facts, cfg)
		}
	}
	result := runwayGateResult{
		note:   &runwayNote{gen: row.Gen, facts: facts, decision: decision, pendingSnapshot: stringOrEmpty(sandboxRow.PendingSnapshotMessageID)},
		action: decision.Action,
	}

	switch decision.Action {
	case sandbox.RunwayPass:
		return result, nil

	case sandbox.RunwayHold:
		if err := a.stores.timer.WithTx(tx).ArmDispatchAfter(ctx, a.sessionID, decision.NextLook); err != nil {
			return runwayGateResult{}, fmt.Errorf("sessionactor: runway gate: arm the dispatch timer: %w", err)
		}
		return result, nil

	case sandbox.RunwayRotate:
		plan, err := a.startRotation(ctx, tx, sandboxRow, decision)
		if err != nil {
			return runwayGateResult{}, err
		}
		result.snapshot = plan
		return result, nil

	case sandbox.RunwayRetire:
		retired, err := a.retireLiveGen(ctx, tx, sandboxRow)
		if err != nil {
			return runwayGateResult{}, err
		}
		if sandbox.State(sandboxRow.Status) == sandbox.StateSnapshotting || sandboxRow.PendingSnapshotMessageID != nil {
			// The snapshot in progress is forgotten with the gen, so its
			// report, should it come late, matches nothing
			// (handleSnapshotReadyEvent): a failed attempt never becomes a
			// restore point.
			if _, err := sandboxes.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
				SessionID: a.sessionID, PendingSnapshotMessageID: nil,
			}); err != nil {
				return runwayGateResult{}, fmt.Errorf("sessionactor: runway gate: forget the retired gen's pending snapshot: %w", err)
			}
		}
		if err := a.armDispatchNow(ctx, tx); err != nil {
			return runwayGateResult{}, err
		}
		result.retire = &rotationRetirement{retired: *retired, reason: decision.Reason}
		return result, nil

	case sandbox.RunwayRevert:
		if err := a.revertSnapshotToReady(ctx, tx, sandboxRow, time.Now()); err != nil {
			return runwayGateResult{}, err
		}
		if err := a.armDispatchNow(ctx, tx); err != nil {
			return runwayGateResult{}, err
		}
		return result, nil

	case sandbox.RunwaySuspect:
		if err := a.transitionSandboxToSuspect(ctx, tx, sandboxRow, time.Now(), watchdogRunwayGate, facts.SinceLastSeen); err != nil {
			return runwayGateResult{}, err
		}
		return result, nil

	default:
		return runwayGateResult{}, fmt.Errorf("sessionactor: runway gate: unhandled decision %+v", decision)
	}
}

// startRotation starts a rotation of sandboxRow's live gen, ready, in tx:
// ready -> snapshotting through the machine's own snapshot_start edge and
// the recorder, the rotation and its snapshot command's messageId recorded
// on the row (StartSandboxRotation), and the dispatch timer armed at the
// snapshot wait, which ends the wait whether or not the snapshot reports.
func (a *Actor) startRotation(ctx context.Context, tx pgx.Tx, sandboxRow sqlcgen.Sandbox, decision sandbox.RunwayDecision) (*rotationSnapshotPlan, error) {
	to, err := sandbox.Transition(sandbox.State(sandboxRow.Status), int(sandboxRow.Gen), sandbox.SnapshotStartTrigger())
	if err != nil {
		return nil, fmt.Errorf("sessionactor: runway gate: sandbox transition ready->snapshotting (rotation): %w", err)
	}
	if _, err := a.sandboxWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: a.sessionID,
		Status:    sqlcgen.SandboxStatus(to),
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: runway gate: update sandbox status to snapshotting (rotation): %w", err)
	}
	messageID := uuid.NewString()
	started, err := a.stores.sandbox.WithTx(tx).StartRotation(ctx, a.sessionID, sandboxRow.Gen, messageID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: runway gate: record the rotation: %w", err)
	}
	if !started {
		// Unreachable while the evaluation holds the session's lock: the
		// row was read at this gen in this transaction.
		return nil, fmt.Errorf("sessionactor: runway gate: the sandbox is no longer at gen %d", sandboxRow.Gen)
	}
	if err := a.stores.timer.WithTx(tx).ArmDispatchAfter(ctx, a.sessionID, decision.NextLook); err != nil {
		return nil, fmt.Errorf("sessionactor: runway gate: arm the dispatch timer: %w", err)
	}
	return &rotationSnapshotPlan{gen: sandboxRow.Gen, messageID: messageID}, nil
}

// sendRotationSnapshot sends the snapshot command of a rotation a committed
// evaluation started, outside any transaction, built as
// triggerSnapshotBestEffort builds the post-turn one. It reports whether
// the command was written. When it was not -- no live connection, or the
// write failed -- the sandbox is reverted to ready with its pending command
// forgotten (revertSnapshotBestEffort), so a frame that did get through
// matches nothing, and the caller's next evaluation retires the gen
// (EvaluateRunway's row 6), counted there as rotation_snapshot_failed.
func (a *Actor) sendRotationSnapshot(ctx context.Context, plan rotationSnapshotPlan) bool {
	payload, err := json.Marshal(sandboxws.Snapshot{
		Type:      "snapshot",
		MessageId: plan.messageID,
		SessionId: a.sessionID.String(),
		Gen:       int(plan.gen),
	})
	if err == nil {
		err = a.commander.SendCommand(a.sessionID.String(), payload)
	}
	if err != nil {
		a.logger.Warn("sessionactor: runway gate: the rotation's snapshot command could not be sent; the sandbox goes back to ready and its gen is retired, falling back to its previous snapshot",
			"gen", plan.gen, "message_id", plan.messageID, "reason", "send_failed",
			"no_live_connection", errors.Is(err, ports.ErrNoLiveSandboxConnection), "error", err)
		a.revertSnapshotBestEffort(ctx)
		return false
	}
	a.logger.Info("sessionactor: runway gate: asked the sandbox for the rotation's snapshot; the turn waits for its replacement",
		"gen", plan.gen, "message_id", plan.messageID)
	return true
}

// finishRotationRetirement runs after the commit that retired a gen for
// its runway: the gen's agent is sent `shutdown`, fenced with its gen,
// best effort -- it ends its connection and stops its work -- and its
// provider object is stopped (stopSandboxOfRetiredGen); whatever neither
// reaches, the reconciler reaps. From the commit on, the row is stopped,
// so the gen's handshake and its credential and snapshot calls answer 410,
// and the restore that follows replaces its token.
func (a *Actor) finishRotationRetirement(ctx context.Context, r rotationRetirement) {
	a.sendShutdown(r.retired.gen)
	a.stopSandboxOfRetiredGen(ctx, r.retired)
}

// sendShutdown sends the sandbox `shutdown` command (sandboxws.Shutdown)
// fenced with gen, best effort.
func (a *Actor) sendShutdown(gen int) {
	if a.commander == nil {
		a.logger.Warn("sessionactor: shutdown would be sent to a retired gen but no SandboxCommander is configured", "gen", gen)
		return
	}
	payload, err := json.Marshal(sandboxws.Shutdown{
		Type:      "shutdown",
		MessageId: uuid.NewString(),
		SessionId: a.sessionID.String(),
		Gen:       gen,
	})
	if err != nil {
		a.logger.Error("sessionactor: marshal shutdown command failed", "error", err)
		return
	}
	if err := a.commander.SendCommand(a.sessionID.String(), payload); err != nil {
		if errors.Is(err, ports.ErrNoLiveSandboxConnection) {
			a.logger.Info("sessionactor: shutdown not sent to a retired gen: no live sandbox connection", "gen", gen)
			return
		}
		a.logger.Warn("sessionactor: send shutdown to a retired gen failed", "gen", gen, "error", err)
		return
	}
	a.logger.Info("sessionactor: shutdown sent to a retired gen", "gen", gen)
}

// The outcome attribute of sandbox_rotation_restore_total (opsmetrics.go):
// what the claim that replaced a rotated gen did.
const (
	rotationOutcomeRotated  = "rotated"
	rotationOutcomeFellBack = "fell_back"
	rotationOutcomeFresh    = "fresh"
)

// rotationOutcome is what the claim that replaced a rotated gen did,
// logged and counted once it committed (logRotationRestore).
type rotationOutcome struct {
	outcome    string
	retiredGen int32
	snapshotID string
}

// rotating reports whether row records a rotation of its live gen.
func rotating(row sqlcgen.Sandbox) bool {
	return row.RotationGen != nil && *row.RotationGen == row.Gen
}

// rotationSnapshotTaken is GetSandboxRunway's rotation_snapshot_taken on a
// row already read: the row's snapshot was recorded, with its provenance,
// at or after its rotation started -- the rotation's own snapshot. Both
// instants are the database's now() at their writes.
func rotationSnapshotTaken(row sqlcgen.Sandbox) bool {
	return rotating(row) && row.SnapshotID != nil && row.SnapshotProvenanceID != nil &&
		*row.SnapshotProvenanceID == *row.SnapshotID && row.SnapshotMintedAt.Valid && row.RotationStartedAt.Valid &&
		!row.SnapshotMintedAt.Time.Before(row.RotationStartedAt.Time)
}

// rotationFallbackMessageIDPrefix keys the warning recordRotationFallback
// writes: sandbox_rotation_fallback:{retired gen}.
const rotationFallbackMessageIDPrefix = "sandbox_rotation_fallback:"

// rotationFallbackWarning is the warning of a rotation restored from the
// snapshot before the one it could not take.
func rotationFallbackWarning(snapshotID string, mintedAt pgtype.Timestamptz) string {
	taken := ""
	if mintedAt.Valid {
		taken = " (taken " + mintedAt.Time.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return "This session's sandbox reached the end of its lifetime, and the snapshot that would have carried its latest state could not be taken, " +
		"so it was restored from its previous snapshot, " + snapshotID + taken + ". " +
		"Anything done in the sandbox after that snapshot, including the agent's conversation since then, is not in the new sandbox. " +
		"Commits already pushed stay on their branch: a later push from this sandbox that lacks them is refused, never forced."
}

// recordRotationFallback writes, in the restore claim's transaction, the
// persisted session warning that a rotation fell back to the previous
// snapshot (technical plan §35.3's amendment): at gen, the new one, under
// the deterministic messageId sandbox_rotation_fallback:{retiredGen},
// which the event store dedupes on, naming the snapshot restored and when
// it was taken. It is stored, so fetch_history and a reload show it, and
// broadcast after the commit; it is not posted to any channel. Only the
// claim that actually restores such a snapshot writes it: a restore
// downgraded or refused writes its own warning instead, and a fresh spawn
// writes none -- that is a fresh lineage (§35.5).
func (a *Actor) recordRotationFallback(ctx context.Context, tx pgx.Tx, gen int, retiredGen int32, snapshotID string, mintedAt pgtype.Timestamptz) error {
	msg := sandboxws.Warning{
		Type:      "warning",
		MessageId: fmt.Sprintf("%s%d", rotationFallbackMessageIDPrefix, retiredGen),
		SessionId: a.sessionID.String(),
		Gen:       gen,
		Message:   rotationFallbackWarning(snapshotID, mintedAt),
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("sessionactor: marshal the rotation fallback warning: %w", err)
	}
	if _, err := a.appendRawEvent(ctx, tx, "warning", msg.MessageId, raw); err != nil {
		return fmt.Errorf("sessionactor: append the rotation fallback warning: %w", err)
	}
	return nil
}

// runwayRuledOutResume reports whether a resume of row's provider object is
// ruled out because the gate took its gen out, or would: a rotation of it,
// the gate's every retirement (EvaluateRunway:
// TestEvaluateRunway_Properties pins that no other row retires), or its
// deadline passed. The gate's suspicion of a silent agent ends, when the
// grace runs out, in failed, which is never resumed; a gen past its
// deadline that another watchdog ended stale is ruled out here too. A
// resume claims the same provider object and carries its deadline forward
// (technical plan §35.2), so it would bring back the sandbox the gate
// replaces, and the gate would take it out again. Read only for a provider
// that resumes, on the database's clock.
func (a *Actor) runwayRuledOutResume(ctx context.Context, tx pgx.Tx, row sqlcgen.Sandbox) (bool, error) {
	if rotating(row) {
		return true, nil
	}
	runway, err := a.stores.sandbox.WithTx(tx).Runway(ctx, a.sessionID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: read the sandbox's runway before a resume: %w", err)
	}
	return runway.DeadlineKnown && runway.RunwayNanos <= 0, nil
}

// logRotationRestore logs and counts what the claim that replaced a
// rotated gen did, once it committed (executePlans): restored the
// rotation's own snapshot, fell back to the one before it, or spawned
// fresh.
func (a *Actor) logRotationRestore(ctx context.Context, plan *spawnPlan) {
	r := plan.rotation
	if r == nil {
		return
	}
	fields := []any{"retired_gen", r.retiredGen, "gen", plan.gen, "outcome", r.outcome, "snapshot_id", r.snapshotID}
	switch r.outcome {
	case rotationOutcomeFellBack:
		a.logger.Warn("sessionactor: rotation restored the sandbox from its previous snapshot: the rotation's own snapshot was not taken", fields...)
	case rotationOutcomeFresh:
		a.logger.Warn("sessionactor: rotation spawned the sandbox fresh: no snapshot to restore", fields...)
	default:
		a.logger.Info("sessionactor: rotation restored the sandbox from its own snapshot", fields...)
	}
	if a.opsMetrics.rotationRestore == nil {
		return
	}
	a.opsMetrics.rotationRestore.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", r.outcome)))
}

// recordRunwayGate logs and counts one gate decision, once the evaluation
// that made it committed. A pass with runway to spare is neither: it is
// every ordinary dispatch.
func (a *Actor) recordRunwayGate(ctx context.Context, note *runwayNote) {
	if note == nil || note.decision.Reason == sandbox.RunwayReasonOK {
		return
	}
	d, f := note.decision, note.facts
	fields := []any{
		"gen", note.gen, "status", string(f.Status), "action", string(d.Action), "reason", string(d.Reason),
		"threshold", d.Threshold.String(), "deadline_known", f.DeadlineKnown, "turn_in_flight", f.TurnInFlight,
	}
	if f.DeadlineKnown {
		fields = append(fields, "runway_seconds", int64(f.Runway.Seconds()))
	}
	if d.NextLook > 0 {
		fields = append(fields, "next_look", d.NextLook.String())
	}
	if f.Status == sandbox.StateSnapshotting {
		fields = append(fields, "pending_snapshot_message_id", note.pendingSnapshot,
			"snapshotting_seconds", int64(f.SinceSnapshotStart.Seconds()), "silent_seconds", int64(f.SinceLastSeen.Seconds()),
			"replay_pending", f.ReplayPending)
	} else if d.Reason == sandbox.RunwayReasonDeadlinePassed {
		fields = append(fields, "silent_seconds", int64(f.SinceLastSeen.Seconds()))
	}
	switch {
	case d.Action == sandbox.RunwayRetire || d.Action == sandbox.RunwayRevert || d.Action == sandbox.RunwaySuspect:
		a.logger.Warn(runwayGateLogMessage(d), fields...)
	case d.Reason == sandbox.RunwayReasonDeadlineUnknown:
		a.logger.Debug(runwayGateLogMessage(d), fields...)
	default:
		a.logger.Info(runwayGateLogMessage(d), fields...)
	}
	if a.opsMetrics.runwayGate == nil {
		return
	}
	a.opsMetrics.runwayGate.Add(ctx, 1, metric.WithAttributes(
		attribute.String("action", string(d.Action)),
		attribute.String("reason", string(d.Reason)),
	))
}

// runwayGateLogMessage is the line a gate decision is logged with.
func runwayGateLogMessage(d sandbox.RunwayDecision) string {
	switch d.Action {
	case sandbox.RunwayRotate:
		return "sessionactor: runway gate: the sandbox is under its runway threshold; rotating it before the turn is dispatched"
	case sandbox.RunwayRetire:
		return "sessionactor: runway gate: the sandbox gen is retired before anything more is sent to it"
	case sandbox.RunwayRevert:
		return "sessionactor: runway gate: a snapshot never reported is given up; the sandbox goes back to ready and takes the turn"
	case sandbox.RunwaySuspect:
		return "sessionactor: runway gate: the sandbox's agent fell silent, past its deadline or mid-snapshot; the sandbox is suspected, through its terminal grace"
	case sandbox.RunwayHold:
		if d.Reason == sandbox.RunwayReasonSnapshotInProgress {
			return snapshotHeldLogMessage
		}
		return "sessionactor: runway gate: the turn is held"
	default:
		return "sessionactor: runway gate: dispatched below the rotation threshold; the rotation was declined"
	}
}

// snapshotHeldLogMessage is the line of a turn held behind a snapshot in
// progress, by the gate and by the spawn decision's own skip alike
// (logHeldBehindSnapshot).
const snapshotHeldLogMessage = "sessionactor: turn held: its sandbox is snapshotting"
