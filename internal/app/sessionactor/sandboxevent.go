// This file (sandboxevent.go) implements handling a SandboxEvent command
// (command.go) -- the sandbox-WS-hub half of (§3.2 liveness, §6.1
// ack receipt / event persistence, §9.3 scenario #6 stale-gen rejection).
// internal/adapters/inbound/wshub (this same Step) is the ONLY caller: its
// read loop delivers one SandboxEvent per inbound wire frame, once that
// connection's own handshake-time gen has already been validated (§6.1's
// "403 on id/gen mismatch") -- this handler enforces the SEPARATE
// per-message half of §3.2's gen-fencing rule ("stale-gen inputs are
// rejected and logged"), persists every recognized event (append-only),
// always bumps liveness (last_seen_at = max of all signals), and fires
// the state transitions this Step's plan row (and §3.2's, "snapshots &
// restore") scope: "ready"/Connecting, "heartbeat"-nil-phase/Booting
// (both §3.2), "snapshot_ready"/Snapshotting (§3.2, design decision
// 3 -- see handleSnapshotReadyEvent below), and now Suspect-recovery
// (§3.2, "two-phase terminalization" -- see the section right below).
//
// # Suspect-state recovery-during-grace (§3.2, "two-phase terminalization")
//
// §3.2: "Any liveness signal during grace returns to previous state." A
// Suspect sandbox reconnecting through this handler is correctly ALLOWED
// (domain/sandbox.IsDeadSandboxStatus(Suspect) is false, so wshub's own
// handshake let the connection through) -- this is exactly the moment
// that rule fires. Right after this event's own raw persistence
// (appendRawEvent, below) and BEFORE computing whatever further
// transition cmd.Type itself drives, handleSandboxEvent checks: is
// row.Status genuinely Suspect, AND does it still carry a
// pre_suspect_status (set by transitionSandboxToSuspect, timerfired.go,
// in the SAME write that entered Suspect -- migrations/000023_sandbox_
// pre_suspect_status.up.sql)? If so, ANY recognized inbound event counts
// as the liveness signal the rule names -- deliberately NOT narrowed to
// specific event types, since the rule's own wording is unconditional --
// and sandbox.Transition(StateSuspect, gen, sandbox.RecoverTrigger(
// preSuspectStatus)) is attempted. On success: the recovered status is
// written, pre_suspect_status is cleared back to NULL, and
// TimerTerminalGrace is deleted, all in one statement
// (RecoverSandboxFromSuspect, queries/sandboxes.sql, plus deleteTimer) --
// that grace window no longer applies once the sandbox is provably alive
// again. row itself is reassigned to the freshly-read recovered row, so
// every line below (the sandboxTransitionTrigger check, the
// snapshot_ready/execution_complete switch) sees the NOW-RECOVERED
// status, never the stale "suspect" one.
//
// This is deliberately NOT an early return: the SAME event that just
// recovered the sandbox is allowed to ALSO drive a further transition or
// turn-completion in the same pass -- e.g. a genuinely late
// execution_complete arriving while Suspect recovers the sandbox AND
// completes whichever turn is Processing, in ONE commit (§9.3 scenario
// #4: "execution_complete arrives AFTER terminalization -> state
// reconciled"; §3.2's own "a genuinely late success... reconciles: turn
// marked completed, session status re-derived, automation run counters
// corrected"). The turn/session half of that sentence needs no new code
// of its own: turn.EvaluateTurnDeadline's own turn_deadline budget vastly
// exceeds the sandbox's Suspect-grace window, so the turn is still
// Processing when this fires, and completeProcessingTurn (pushpr.go)
// already drives Processing->Completed via the already-legal edge once
// this recovery makes the sandbox live again -- see internal/domain/turn/
// state.go's own top comment for why that package needed no change to
// make this true. The THIRD clause -- "automation run counters
// corrected" -- is honestly NOT addressed by this Step: automations are
// not a built feature anywhere in this codebase yet (§8.4's own work,
// not yet done) -- there is no automation_runs table, no automation
// domain package, nothing to correct. This is a genuine, currently-
// inapplicable gap in this Step's own delivery of §3.2's full sentence,
// not a silent omission: whichever future Step first builds automation
// run tracking will need its own equivalent late-success correction, the
// same way this Step's own turn/session correction was already free
// once Suspect-recovery existed.
//
// A Suspect row with no pre_suspect_status set is a defensive,
// practically-unreachable case (transitionSandboxToSuspect always sets it
// in the SAME write that enters Suspect) -- handled as a safe no-op,
// falling through to the pre-existing behavior (persisted, liveness
// bumped, left Suspect, no recovery transition attempted). Likewise, a
// pre_suspect_status naming an illegal recovery target (should never
// happen: it is only ever written from a state TriggerSuspect's own
// transition table already validated) is logged and left Suspect, never
// treated as a fatal error for this event. Do not call sandbox.Transition
// speculatively for any OTHER event type/status combination beyond the
// ones named here and in sandboxTransitionTrigger's own doc comment
// below.
//
// Formerly an honest, documented gap (left open by this Step's own
// original brief, which scoped only the recovery write itself): a sandbox
// that recovers DIRECTLY back to Ready (pre_suspect_status was Ready)
// does not get caught by the Booting->Ready guard below, because row is
// already reassigned to the freshly-recovered (already-Ready) row BEFORE
// that guard's own before/after comparison runs a few lines down -- so
// row.Status != target is always false for this path, no matter what.
// Audit finding F2 ("liveness/inactivity watchdogs not re-armed on every
// real ->Ready edge") covers this path too, on reflection: both timers
// were deleted by their own respective timeout handlers (timerfired.go)
// on the way INTO Suspect, and this recovery is exactly as genuine a
// ->Ready edge as Booting->Ready or either snapshot path. Closed now: the
// recovery branch below calls armReadyWatchdogs directly whenever
// recoveredTo is Ready, since it can never rely on the generic guard
// catching it. See that branch's own inline comment for why this is
// reachable at most once per real edge (no double-arm risk).

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

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// ackIDPeek peeks just the "ackId" field a critical wire event carries
// (contracts/gen/go/sandboxws's own per-type doc comments: present and
// non-empty ONLY on the 6 critical types, by construction). This package
// decodes it directly from cmd.Raw rather than importing wshub's own
// envelope type (adapters/inbound depending on app/sessionactor is the only
// legal direction, never the reverse) or hardcoding a critical-type-name
// list -- exactly mirroring how wsbridge's own SendCritical(msg any, ackID
// string) is type-agnostic (see ports.ClassifyAgentEvent's doc comment for
// why that existing OUTBOUND-direction helper does not apply to this
// INBOUND-direction, raw-JSON-bytes input).
type ackIDPeek struct {
	AckID string `json:"ackId"`
}

// peekAckID extracts raw's own "ackId" field, or "" if raw is not a JSON
// object, has no such field, or the field is absent -- cmd.Raw is always
// well-formed JSON by construction (wshub already successfully unmarshaled
// it once to peek Type/Gen/LastBootPhase before ever constructing a
// SandboxEvent), so a decode failure here is defensive, not expected.
func peekAckID(raw json.RawMessage) string {
	var peek ackIDPeek
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return peek.AckID
}

// sandboxTransitionTrigger reports which Trigger (if any) applies for the
// given (event type, LastBootPhase, current status) combination -- the two
// (and only two) mappings this Step implements:
//
//	(a) "ready" arriving while status is Connecting -> WSConnectedTrigger
//	    (Connecting -> Booting).
//	(b) "heartbeat" with a nil LastBootPhase arriving while status is
//	    Booting -> BootCompleteTrigger (Booting -> Ready), grounded
//	    directly in Heartbeat.LastBootPhase's own doc comment: "Null once
//	    boot has completed (no more boot phases to report)".
//
// ok is false for every other combination -- including "ready" outside
// Connecting, or "heartbeat"/nil-LastBootPhase outside Booting -- which is
// NOT an error (these two events can legitimately arrive outside their
// exact expected phase: reconnects, replays). Callers must fall through to
// a liveness-only bump in that case, never attempt a speculative
// sandbox.Transition call for a combination this function does not name.
func sandboxTransitionTrigger(eventType string, lastBootPhase *string, status sandbox.State) (sandbox.Trigger, bool) {
	switch {
	case eventType == "ready" && status == sandbox.StateConnecting:
		return sandbox.WSConnectedTrigger(), true
	case eventType == "heartbeat" && lastBootPhase == nil && status == sandbox.StateBooting:
		return sandbox.BootCompleteTrigger(), true
	default:
		return sandbox.Trigger{}, false
	}
}

// armReadyWatchdogs arms BOTH liveness_check and inactivity, using the
// exact same budget constants each timer's own handler already reuses for
// its own steady-state re-arm (handleLivenessCheckTimer/
// handleInactivityTimer, timerfired.go) -- the single shared definition of
// "what happens when a sandbox becomes Ready", called from every call site
// in this file that lands a real transition on sandbox.StateReady: the
// Booting->Ready guard below (handleSandboxEvent), the Suspect-recovery-
// directly-to-Ready branch a few lines above that guard (also
// handleSandboxEvent -- see this file's own top comment), handleSnapshotReadyEvent's
// own snapshot-success path, and revertSnapshotToReady's own compensating
// write (covering both of ITS callers, revertSnapshotBestEffort and
// handleSnapshotReadyEvent's own decode-failure branch).
//
// Audit finding F2: liveness_check is only ever re-armed by the Booting->
// Ready guard's own before/after transition check, or by its own handler's
// self-re-arm while already Ready -- and its handler deletes it, with no
// re-arm, the instant it fires while status != Ready (by design: it only
// watches the steady Ready state). Snapshotting is one of the statuses
// that "!= Ready" branch covers, and every turn spends up to
// SnapshotMintTimeout (~60s, a large fraction of the 90s
// SteadyHeartbeatBudget cadence) in Snapshotting -- so a liveness_check
// fire landing during that window got silently, permanently deleted with
// no re-arm anywhere, degrading detection of a genuinely dead sandbox down
// to the ~10-minute InactivityTimeout backstop for the rest of that
// generation. Every caller of THIS helper must only call it immediately
// after a transition that has ALREADY committed a genuine ->Ready edge
// (never unconditionally on every call/heartbeat while already Ready --
// that would keep pushing fires_at forward and the timer would never get
// a real chance to fire); each call site's own doc comment states why its
// own call satisfies that.
func (a *Actor) armReadyWatchdogs(ctx context.Context, tx pgx.Tx, now time.Time) error {
	if err := a.armTimer(ctx, tx, TimerLivenessCheck, now.Add(a.timeouts.SteadyHeartbeatBudget)); err != nil {
		return err
	}
	return a.armTimer(ctx, tx, TimerInactivity, now.Add(a.timeouts.InactivityMinCheckInterval))
}

// handleSandboxEvent processes one inbound sandbox-WS event delivered by
// wshub's read loop. Whatever a.transact's own closure decides, the
// resulting outcome is ALWAYS sent on cmd.Reply (via a non-blocking
// select-with-default -- the buffered channel wshub constructs makes this
// always succeed immediately) the INSTANT that transact returns -- before
// handleEnsureDispatched or either of this event's own best-effort push/PR
// side effects ever run (see the reply's own inline comment below for
// why: those side effects can each individually exceed
// platform.Timeouts.SandboxEventAckTimeout on real network latency, and
// must never be able to delay a critical event's own ack past its 5s
// window) -- and this function then returns transact's own error upward
// unchanged, so run()'s existing ErrStaleEpoch-is-fatal /
// other-errors-are-logged-not-fatal behavior (actor.go) keeps working
// exactly as it does today.
func (a *Actor) handleSandboxEvent(ctx context.Context, cmd SandboxEvent) error {
	var outcome SandboxEventOutcome
	// pushAfterCommit is non-nil only when THIS event just completed a
	// turn successfully (§9.3, "e2e happy path", pushpr.go) -- acted on
	// (a real SandboxCommander.SendCommand call) only AFTER this
	// function's own transact below has committed, never inside it (see
	// pushpr.go's own top comment for why).
	var pushAfterCommit *pushSignal
	// gitSyncReceived is set true by the "git_sync" case below (
	// "gitstate in-sandbox", §3.4 design section 6) -- acted on (a real
	// SandboxCommander.SendCommand call replying with GitSyncComplete)
	// only AFTER this function's own transact below has committed, never
	// inside it, exactly like pushAfterCommit above: a real network call
	// must never run while this transact's own row lock is held.
	var gitSyncReceived bool
	// eventInserted (audit fix, correctness) hoists appendRawEvent's own
	// Inserted flag out of the transact closure below, exactly like
	// pushAfterCommit/gitSyncReceived above -- needed because, unlike the
	// cmd.Type == "tool_call" case (which consumes the closure-local
	// `inserted` value directly, a few lines down, still inside the same
	// transact), the cmd.Type == "push_complete" side effect
	// (createPRBestEffort) runs in this function's own POST-commit block
	// below, where the closure-local variable is out of scope. See that
	// call site's own doc comment for why this flag must gate it.
	var eventInserted bool

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		row, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		// Per-message gen-fencing (§3.2: "stale-gen inputs are rejected and
		// logged"; §9.3 scenario #6: "Stale sandbox from old gen reconnects
		// -> rejected 403, logged, session unaffected" -- the connection-
		// level half of that scenario is wshub's own handshake check; THIS
		// is the per-message half commands.schema.json's own doc comment
		// requires in addition to it). A stale-gen message here is an
		// EXPECTED occurrence -- an old-gen sandbox reconnecting/replaying
		// after a respawn -- never a failure: skip persisting it entirely,
		// never touch last_seen_at, never ack. outcome stays the zero value
		// (Persisted: false, AckID: "").
		if cmd.Gen != int(row.Gen) {
			a.logger.Warn("sessionactor: ignoring stale-gen sandbox event",
				"event_type", cmd.Type, "event_gen", cmd.Gen, "sandbox_gen", row.Gen)
			return nil
		}

		// Persist ALWAYS, for every recognized event type -- this is the
		// append-only per-session event log §6.2's client hub will
		// replay from, not limited to the 6 critical types. inserted (an
		// audit-fix batch's own addition, finding M16) is reused a few
		// lines down, in the cmd.Type == "tool_call" case, to gate the new
		// mid-turn Linear progress notification against a wire-level
		// redelivery of an already-processed tool_call -- see
		// progressnotify.go's own doc comment. Also captured onto
		// eventInserted (this function's own outer-scoped copy, above) so
		// the cmd.Type == "push_complete" post-commit side effect below can
		// consume the SAME signal for the SAME reason, once this closure
		// has already returned.
		inserted, err := a.appendRawEvent(ctx, tx, cmd.Type, cmd.MessageID, cmd.Raw)
		if err != nil {
			return err
		}
		eventInserted = inserted

		// §3.2 ("two-phase terminalization"): Suspect-recovery-during-
		// grace -- see this file's own top comment for the full reasoning.
		// row is reassigned to the freshly-recovered row on success, so
		// every line below sees the NOW-RECOVERED status rather than the
		// stale "suspect" one.
		if sandbox.State(row.Status) == sandbox.StateSuspect && row.PreSuspectStatus != nil {
			// Captured before row is reassigned below (recErr == nil
			// branch) -- §5.3's watchdog_false_alarm_total (opsmetrics.go)
			// tags by this ORIGINAL pre_suspect_status, not whatever row
			// becomes after recovery.
			preSuspectStatus := string(*row.PreSuspectStatus)
			recoveredTo, recErr := sandbox.Transition(sandbox.StateSuspect, int(row.Gen),
				sandbox.RecoverTrigger(sandbox.State(*row.PreSuspectStatus)))
			if recErr != nil {
				// Defensive only -- see this file's own top comment for why
				// this should be unreachable in practice. Logged, not
				// fatal: the sandbox is simply left Suspect for this event.
				a.logger.Warn("sessionactor: suspect recovery rejected; leaving sandbox suspect",
					"pre_suspect_status", *row.PreSuspectStatus, "error", recErr)
			} else {
				recovered, err := a.stores.sandbox.WithTx(tx).RecoverFromSuspect(ctx, sqlcgen.RecoverSandboxFromSuspectParams{
					SessionID:  a.sessionID,
					Status:     sqlcgen.SandboxStatus(recoveredTo),
					LastSeenAt: pgtype.Timestamptz{Time: now, Valid: true},
				})
				if err != nil {
					return fmt.Errorf("sessionactor: recover sandbox from suspect: %w", err)
				}
				row = recovered
				if err := a.deleteTimer(ctx, tx, TimerTerminalGrace); err != nil {
					return err
				}
				// §5.3: "(and how many were false alarms -- target: ~0)".
				// This branch is reached only because a real, recognized
				// inbound event just arrived from a sandbox this Step's own
				// watchdog machinery had suspected dead -- proof, after the
				// fact, that the suspicion was wrong.
				a.recordWatchdogFalseAlarm(ctx, preSuspectStatus)
				// F2 fix: a recovery landing directly back on Ready is a
				// genuine ->Ready edge that the before/after guard a few
				// lines below can never catch on its own -- row is already
				// reassigned to this (already-Ready) recovered row above,
				// so that guard's own row.Status != target comparison is
				// always false for this path. Reachable at most once per
				// real edge: this whole branch is gated on row.Status
				// genuinely being Suspect with a real pre_suspect_status on
				// entry (checked above), and recErr == nil here means
				// RecoverFromSuspect's own write just committed that exact
				// edge moments ago -- not a re-check of an already-Ready
				// sandbox on some later, unrelated event.
				if recoveredTo == sandbox.StateReady {
					if err := a.armReadyWatchdogs(ctx, tx, now); err != nil {
						return err
					}
				}
			}
		}
		// If row.Status has no pre_suspect_status set (defensive-only, see
		// this file's own top comment): nothing above ran, row is
		// untouched, and this falls straight through to the liveness-only
		// bump below exactly like every other unrecognized-transition
		// event does.

		target := row.Status
		if trig, ok := sandboxTransitionTrigger(cmd.Type, cmd.LastBootPhase, sandbox.State(row.Status)); ok {
			to, err := sandbox.Transition(sandbox.State(row.Status), int(row.Gen), trig)
			if err != nil {
				return fmt.Errorf("sessionactor: sandbox transition via %s: %w", trig.Kind, err)
			}
			target = sqlcgen.SandboxStatus(to)
		}
		// If !ok: NOT an error (see sandboxTransitionTrigger's own doc) --
		// target stays row.Status unchanged, falling straight through to
		// the liveness-only bump below. This is also the path a sandbox
		// that just recovered above (or failed to, and is still Suspect)
		// takes when this SAME event names no further transition of its
		// own: still persisted, liveness bumped.

		if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
			SessionID:    a.sessionID,
			Status:       target,
			LastSeenAt:   pgtype.Timestamptz{Time: now, Valid: true},
			AgentVersion: cmd.AgentVersion,
			ImageDigest:  cmd.ImageDigest,
		}); err != nil {
			return fmt.Errorf("sessionactor: update sandbox status/liveness: %w", err)
		}

		// First-time Booting->Ready transition, this event: arm BOTH
		// liveness_check and inactivity exactly once, here, at the real
		// moment the sandbox becomes Ready -- closing the confirmed gap
		// where neither timer was ever armed for the first time by any
		// production code path (they only ever re-arm themselves once
		// already firing; see handleLivenessCheckTimer/
		// handleInactivityTimer, timerfired.go). The guard is
		// before-vs-after on THIS event's own transition, not a bare
		// "is target Ready" check: on every later heartbeat while already
		// Ready, row.Status and target are both already Ready, so this is
		// false and neither timer is touched -- re-arming liveness_check on
		// every 30s heartbeat would keep pushing its own fires_at forward
		// and it would never get a real chance to fire. The exact same
		// constants handleLivenessCheckTimer/handleInactivityTimer already
		// use for their own self-re-arm are used here, so this initial arm
		// looks identical in shape to every subsequent one -- matching
		// TimerConnectingDeadline's own exactly-once-at-spawn precedent
		// (dispatch.go's tryPlanSpawn). Once armed, handleConnectingDeadlineTimer's
		// own already-correct delete-on-non-connecting-phase logic
		// (timerfired.go) hands off cleanly the next time that stale timer
		// fires: liveness_check is already live and watching by then.
		//
		// Delegates to armReadyWatchdogs (below) -- the SAME shared
		// helper handleSnapshotReadyEvent's own success path,
		// revertSnapshotToReady's own compensating write, and the
		// Suspect-recovery-directly-to-Ready branch a few lines above (this
		// same function -- that branch cannot rely on THIS guard at all,
		// since it reassigns row before this comparison ever runs; see its
		// own inline comment) now also call, so every real transition INTO
		// Ready, by whichever path, re-arms identically (audit finding F2's
		// own fix: a snapshot_ready-driven Snapshotting->Ready never went
		// through this guard at all, since sandboxTransitionTrigger never
		// maps "snapshot_ready" and handleSnapshotReadyEvent/
		// revertSnapshotToReady each write row.Status directly, bypassing
		// target/row.Status entirely).
		if sandbox.State(row.Status) != sandbox.State(target) && sandbox.State(target) == sandbox.StateReady {
			if err := a.armReadyWatchdogs(ctx, tx, now); err != nil {
				return err
			}
		}

		// §3.3: "reported on every heartbeat" -- a heartbeat is a
		// sandbox-level liveness signal, not a turn-scoped one (it carries
		// no turn id), so this is a session-level write, independent of
		// whatever transition (if any) target computed above (§9.3,
		// "e2e happy path", design decision 6).
		if cmd.Type == "heartbeat" && cmd.ConversationID != nil {
			if _, err := a.stores.session.WithTx(tx).UpdateConversationID(ctx, sqlcgen.UpdateSessionConversationIDParams{
				ID:                     a.sessionID,
				OpencodeConversationID: cmd.ConversationID,
			}); err != nil {
				return fmt.Errorf("sessionactor: update session conversation id: %w", err)
			}
		}

		// §9.3 ("e2e happy path")/§3.2 ("snapshots & restore"): per-
		// type post-persist handling. A tagged switch, not an if/else-if
		// chain (staticcheck QF1003), since this is a genuine dispatch on
		// cmd.Type's own value, not a chain of unrelated conditions.
		switch cmd.Type {
		case "execution_complete":
			// A REAL execution_complete completes whichever turn is
			// currently Processing (§3.3) -- see completeProcessingTurn's
			// own doc comment (pushpr.go) for the full reasoning, including
			// why no synthetic execution_complete is ever appended on this
			// path. inserted (this function's own appendRawEvent result,
			// captured a few lines up) is threaded through so
			// completeProcessingTurn's own no-turn-Processing branch can
			// gate turn_false_failure_total on it -- a wire-level
			// redelivery of an already-processed execution_complete
			// (inserted == false) must never re-count the same false
			// failure a second time (confirmed audit finding, MEDIUM; see
			// that branch's own doc comment).
			sig, err := a.completeProcessingTurn(ctx, tx, row, cmd.Raw, inserted)
			if err != nil {
				return err
			}
			pushAfterCommit = sig
		case "snapshot_ready":
			// §3.2, design decision 3: a real snapshot_ready event
			// finalizes the Snapshotting->Ready transition
			// triggerSnapshotBestEffort (below) started, and persists the
			// sandbox's own confirmed snapshotId. Runs INSIDE this SAME
			// transact, right after appendRawEvent already persisted the
			// raw event above -- matching execution_complete's own "persist
			// first, then act" ordering exactly.
			if err := a.handleSnapshotReadyEvent(ctx, tx, row, cmd.Raw, now); err != nil {
				return err
			}
		case "git_sync":
			// §3.4 ("gitstate in-sandbox", §3.4 design section 6): a
			// git_sync event needs no DB-side mutation of its own at all --
			// the generic appendRawEvent persist+broadcast above already
			// covers "CP durably stores it and the browser UI can show it
			// as a live progress signal". This case's only job is flagging
			// that a GitSyncComplete reply is owed, sent AFTER this
			// transact commits (see gitSyncReceived's own doc comment above
			// and this function's post-commit block below) -- mirroring
			// cmd.Type == "push_complete"'s own identical "no in-transact
			// case needed, just an outside-transact reply" shape a few
			// lines below in this same function.
			gitSyncReceived = true
		case "boot_timing":
			// §33.3: relay of one already-measured sandbox_agent_*_
			// duration_seconds data point -- recorded here (never a DB
			// write, so no in-transact case beyond this call is needed,
			// mirroring "git_sync" immediately above) into the matching
			// opsMetrics histogram (opsmetrics.go), gated on inserted for
			// the exact same reconnect-resend-double-count reason the
			// "tool_call" case below gates maybeEnqueueLinearProgress on
			// it. See recordBootTiming's own doc comment (boottiming.go)
			// for the full best-effort/decode-failure contract.
			a.recordBootTiming(ctx, cmd.Raw, inserted)
		case "tool_call":
			// Audit finding M16 ("completeness", internal/adapters/outbound/
			// linearapi/doc.go): the FIRST tool_call event of a turn is
			// this batch's own chosen mid-turn milestone -- a hard,
			// discrete, already-flowing signal that unambiguously means
			// "the agent is now actively working" (contracts/sandbox-ws/
			// v1/events.schema.json's own ToolCall def). See
			// progressnotify.go's own doc comment for the full design,
			// including why this needs BOTH inserted (guards a wire-level
			// redelivery of an already-processed tool_call) and its own
			// internal turns.progress_notified_at marker (guards a LATER,
			// genuinely-distinct tool_call in the SAME turn).
			if err := a.maybeEnqueueLinearProgress(ctx, tx, inserted, pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
				return err
			}
		case "step_finish":
			// §25.15: sum this step's own cost.usd (when present) onto
			// whichever turn is currently processing for this session --
			// see stepcost.go's own top comment for the full design and
			// why this is independent of internal/adapters/outbound/
			// opencode's own turnState.spentUSD accumulator.
			if err := a.recordStepFinishCost(ctx, tx, cmd.Raw); err != nil {
				return err
			}
		}

		outcome.Persisted = true
		outcome.AckID = peekAckID(cmd.Raw)
		return nil
	})

	// Reply IMMEDIATELY once transact has committed (or deliberately
	// skipped persisting a stale-gen event) -- BEFORE any of the
	// best-effort post-commit side effects below ever run. This ordering
	// is deliberate and load-bearing: wshub's own readLoop
	// (internal/adapters/inbound/wshub/dispatch.go) is racing
	// platform.Timeouts.SandboxEventAckTimeout (5s) waiting on this exact
	// reply to write the ack back to the sandbox, and the side effects
	// below -- a full spawn/dispatch re-evaluation, a real git push, a
	// real GitHub API call -- can each individually take longer than
	// that budget on real network latency. Sending the reply here, before
	// any of them run, means a slow/failing side effect can never cause a
	// critical event's own ack to miss its window. The non-blocking
	// select-with-default is safe regardless: the buffered channel wshub
	// constructs makes this send always succeed immediately.
	select {
	case cmd.Reply <- outcome:
	default:
	}

	if err == nil {
		// §3.2 ("snapshots & restore"), design decision 1 -- CORRECTED
		// per independent review: §3.3's own governing rule is "On
		// terminal event: complete turn, trigger snapshot, re-derive
		// session status, dispatch next pending" -- i.e. the snapshot
		// trigger is scoped to a real turn-terminal wire event, not to
		// every sandbox-WS frame. The CALL is gated on cmd.Type ==
		// "execution_complete" here, mirroring exactly how
		// sendPushBestEffort/createPRBestEffort are gated a few lines
		// below (pushAfterCommit != nil / cmd.Type == "push_complete").
		// The brief's original text ("do not gate the CALL itself on the
		// event type... internally a no-op when not applicable") was
		// wrong: triggerSnapshotBestEffort's own internal eligibility
		// check is only "status == Ready", which heartbeat (sent every
		// steady_heartbeat_budget's own 30s interval, §6.1, even on a
		// completely idle sandbox) also satisfies -- gating on Ready-
		// status alone let every routine heartbeat on an already-Ready
		// session re-trigger a full snapshot cycle indefinitely, calling
		// the real SandboxProvider.TakeSnapshot roughly every 30s forever
		// with zero turn activity. Gating on cmd.Type == "execution_complete"
		// fires this exactly once per real terminal event (completed,
		// failed, or cancelled outcome alike -- §3.3 does not restrict
		// "trigger snapshot" to successful completions only, unlike the
		// push signal below which IS success-only) and is naturally
		// idempotent against a redelivered/duplicate execution_complete:
		// the first delivery already moved the sandbox off Ready, so
		// triggerSnapshotBestEffort's own internal check no-ops on the
		// redelivery.
		//
		// Fix (audit finding F4): this call runs BEFORE handleEnsureDispatched
		// below, matching §3.3's own literal order -- "complete turn ->
		// trigger snapshot -> re-derive status -> dispatch next" -- exactly.
		// The two must not run in the other order: dispatching the next
		// pending turn (handleEnsureDispatched) does not itself change the
		// SANDBOX's own status column (it stays Ready; only the TURN's own
		// row moves Pending->Processing and a real prompt is sent) --
		// which means triggerSnapshotBestEffort's own eligibility check
		// ("status == Ready") would still pass just as well AFTER a
		// dispatch-then-snapshot ordering as before it. So a dispatch-then-
		// snapshot order lets the snapshot fire while the NEXT turn is
		// already actively executing inside the sandbox -- capturing
		// mid-turn, in-flight state instead of the clean, idle state
		// between the two turns -- and that next turn's own later,
		// legitimate execution_complete snapshot attempt then finds the
		// sandbox still Snapshotting from this premature one and silently
		// no-ops, skipping a snapshot cycle entirely. Running this call
		// first instead means the sandbox is already Snapshotting (neither
		// Ready nor Suspect) by the time handleEnsureDispatched looks at
		// it, so planDispatch's own dispatch branch does not fire yet --
		// the next turn stays Pending until this snapshot cycle completes
		// and returns the sandbox to Ready, at which point a later
		// EnsureDispatched call picks it up.
		if cmd.Type == "execution_complete" {
			a.triggerSnapshotBestEffort(ctx)
		}

		// Design decision 3: unconditionally re-evaluate spawn/dispatch state
		// right after this event's own transact commits successfully (and,
		// per the fix above, after any execution_complete snapshot trigger
		// has already had its chance to run against the clean post-turn
		// Ready state) -- e.g. a "ready"/heartbeat-driven transition to
		// Booting/Ready is immediately followed by a fresh dispatch
		// evaluation, in case a pending turn is now dispatchable. Calls the
		// SAME handler function EnsureDispatched itself invokes, directly
		// (not via a.Send), since this already runs on the actor's own
		// single command-processing goroutine -- see command.go's own
		// EnsureDispatched doc comment. Deliberately does NOT alter this
		// function's own return value: a failure here is logged, never
		// treated as a failure of the sandbox event itself (which already
		// committed successfully).
		if dispatchErr := a.handleEnsureDispatched(ctx); dispatchErr != nil {
			a.logger.Warn("sessionactor: ensure-dispatched after sandbox event failed", "error", dispatchErr)
		}

		// §9.3 ("e2e happy path"): the two remaining best-effort side
		// effects this event may trigger, both deliberately run OUTSIDE
		// (i.e. after) the transact above committed, never inside it --
		// see pushpr.go's own top comment for why. Neither failure alters
		// this function's own return value: cmd.Type's own event already
		// committed successfully regardless of what either side effect
		// does next.
		if pushAfterCommit != nil {
			a.sendPushBestEffort(a.sessionID.String(), pushAfterCommit)
		}
		// Audit fix (correctness): push_complete is an at-least-once wire
		// event (internal/sandboxagent/wsbridge's own doc.go/buffer.go --
		// resent verbatim, identical MessageId and identical pushed.Sha,
		// until acked), so createPRBestEffort must not re-run for a wire
		// redelivery of a push_complete THIS session already processed --
		// gated on eventInserted here, exactly mirroring how the
		// "tool_call" case above (inside the transact) gates
		// maybeEnqueueLinearProgress on the same underlying signal.
		// recordPRArtifact's own URL-based dedup already made a
		// redelivery's CreatePR call a harmless no-op for the "pr"
		// artifact, but enqueuePreviewBestEffort (previewpr.go) is
		// deliberately NOT idempotency-guarded the same way -- each
		// GENUINE push carries a new sha, so a fresh preview row/outbox
		// pair per push is correct -- which meant a wire-level redelivery
		// of the SAME push_complete (same MessageId, same Sha) used to
		// enqueue a SECOND, spurious preview artifact and outbox pair
		// every time the sandbox connection re-sent it before its ack
		// landed. eventInserted (false on a redelivery: appendRawEvent's
		// own (session_id, messageID) upsert already saw this exact
		// message id, per its own doc comment above) is the correct
		// discriminator -- a URL-based dedup inside
		// enqueuePreviewBestEffort would be wrong instead, since a
		// LEGITIMATE second push reuses the same PR-derived friendly URL
		// but must still enqueue a fresh preview for its own new sha
		// (previewpr.go's own top comment; previewurl.go).
		if cmd.Type == "push_complete" && eventInserted {
			a.createPRBestEffort(ctx, cmd.Raw)
		}

		// §3.4 ("gitstate in-sandbox"): reply to a just-committed
		// git_sync event with GitSyncComplete -- same "outside the
		// transact, log-only on failure" shape as the two side effects
		// just above.
		if gitSyncReceived {
			a.sendGitSyncCompleteBestEffort(a.sessionID.String(), cmd.Gen)
		}
	}

	return err
}

// handleSnapshotReadyEvent implements design decision 3's own
// snapshot_ready handling (§3.2, "snapshots & restore"): transitions
// Snapshotting -> Ready via sandbox.SnapshotCompleteTrigger() and persists
// the sandbox's own reported snapshotId onto sandboxes.snapshot_id.
// Called from INSIDE handleSandboxEvent's own transact, the SAME transact
// that already ran appendRawEvent for this wire event moments ago (row is
// the sandbox row as read at the very top of that transact, before this
// event's own generic status/liveness bump ran -- i.e. its Status field
// still reflects whatever the sandbox's status genuinely was when this
// event arrived).
//
// A late/duplicate snapshot_ready arriving after the sandbox is no longer
// Snapshotting (e.g. a liveness watchdog already suspected it in the
// meantime, or this is a wire-level redelivery of an already-handled
// critical event) is logged and treated as a no-op, never a transact
// failure -- matching this codebase's general "a stale or duplicate
// signal is logged, not fatal" posture (this same function's own
// stale-gen handling above; completeProcessingTurn's identical "no turn
// currently Processing" no-op in pushpr.go). This does NOT touch
// sandbox.gen -- a snapshot cycle happens within the SAME gen; only
// RestoreFromSnapshot bumps gen (design decision 6, dispatch.go).
//
// Fix (message-id correlation): neither TriggerSnapshotStart nor
// TriggerSnapshotComplete is gen-fenced (by design -- gen doesn't change
// within a snapshot cycle), so the "is the sandbox's CURRENT status
// Snapshotting" check alone is satisfiable by a LATER, unrelated snapshot
// attempt at the same gen -- an independent review constructed and
// confirmed this exact race against a real Postgres instance: attempt
// #1's SendCommand appears to fail (but the frame actually got through) ->
// revertSnapshotBestEffort reverts to Ready -> attempt #2 starts -> attempt
// #1's real, delayed snapshot_ready arrives and would otherwise be wrongly
// accepted as completing attempt #2, stamping the STALE snapshotId. So an
// event is only accepted as completing the CURRENT attempt when its own
// CommandMessageId matches row.PendingSnapshotMessageID (set by
// triggerSnapshotBestEffort's own first transact, atomically with the
// Ready->Snapshotting transition) -- a mismatch, or that column being nil
// (no attempt outstanding), is treated exactly like the late/duplicate
// case just above: logged, no-op, never a transact failure.
//
// Fix (audit finding F2): the Snapshotting->Ready transition below used to
// leave BOTH liveness_check and inactivity un-re-armed -- neither
// handleLivenessCheckTimer nor handleInactivityTimer (timerfired.go) ever
// re-arms itself once it fires while status != Ready (by design: see
// handleLivenessCheckTimer's own doc comment), and this snapshot_ready path
// writes row.Status directly, never going through the generic before/after
// Booting->Ready guard a few lines up in handleSandboxEvent -- so a
// liveness_check fire landing during ANY Snapshotting window (every turn,
// §3.3's post-turn snapshot) silently and permanently disarmed the fast
// steady_heartbeat_budget watchdog for the rest of that sandbox generation.
// armReadyWatchdogs (below) is now called right after this function's own
// Snapshotting->Ready transition actually commits -- reachable only once
// per real edge, since the early-return above already guarantees row.Status
// genuinely WAS Snapshotting when this call started.
func (a *Actor) handleSnapshotReadyEvent(ctx context.Context, tx pgx.Tx, row sqlcgen.Sandbox, raw json.RawMessage, now time.Time) error {
	if sandbox.State(row.Status) != sandbox.StateSnapshotting {
		a.logger.Warn("sessionactor: snapshot_ready arrived while sandbox is not snapshotting; ignoring (late or duplicate delivery)",
			"status", row.Status)
		return nil
	}

	var evt sandboxws.SnapshotReady
	if err := json.Unmarshal(raw, &evt); err != nil {
		// Fix (was: log-only, leaving the sandbox permanently stuck
		// Snapshotting -- no watchdog covers that state, confirmed by both
		// the implementer and two independent reviewers): "we can't
		// understand this response" must be treated the same as "we don't
		// have a usable response," so revert exactly like
		// revertSnapshotBestEffort's own SendCommand-failure path does
		// (including clearing pending_snapshot_message_id) -- using the
		// SAME already-open tx this function was handed (not a second,
		// separate a.transact call: this function is already running
		// inside handleSandboxEvent's own transact, which already
		// persisted this raw event verbatim via appendRawEvent moments
		// ago; a decode failure here must never retroactively roll that
		// back, and reverting via a second, independent transaction here
		// would race the still-open outer one instead of composing with
		// it).
		a.logger.Warn("sessionactor: snapshot_ready failed schema decode; persisted verbatim, reverting sandbox to ready instead of leaving it stuck snapshotting",
			"error", err)
		return a.revertSnapshotToReady(ctx, tx, row, now)
	}

	pending := "<nil>"
	if row.PendingSnapshotMessageID != nil {
		pending = *row.PendingSnapshotMessageID
	}
	if row.PendingSnapshotMessageID == nil || evt.CommandMessageId == nil || *evt.CommandMessageId != *row.PendingSnapshotMessageID {
		a.logger.Warn("sessionactor: snapshot_ready commandMessageId does not match the sandbox's own currently-outstanding pending snapshot attempt; ignoring (stale/duplicate delivery from an earlier, already-completed-or-reverted attempt)",
			"pending_snapshot_message_id", pending)
		return nil
	}

	to, err := sandbox.Transition(sandbox.State(row.Status), int(row.Gen), sandbox.SnapshotCompleteTrigger())
	if err != nil {
		return fmt.Errorf("sessionactor: sandbox transition snapshotting->ready (snapshot_ready): %w", err)
	}
	if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: a.sessionID,
		Status:    sqlcgen.SandboxStatus(to),
	}); err != nil {
		return fmt.Errorf("sessionactor: update sandbox status to ready (snapshot complete): %w", err)
	}
	// §30.4(3)'s own fail-closed defense-in-depth (the primary fix is the
	// mint-time cache purge, cmd/sandbox-agent/main.go's own
	// HandleSnapshot): stamp this session's own effective egress mode onto
	// the snapshot row, resolved exactly ONCE, right here, at the moment
	// this snapshot is confirmed -- never re-derived by the restore-time
	// check that later reads it back (dispatch.go's own tryPlanSpawn).
	// Reuses the SAME session-repos-aggregated "any suppressed repo
	// suppresses the whole thing" formula the outbox's own identical
	// epoch-stamp need already established (outbox_shadow.go), rather
	// than hand-rolling a second copy of that aggregation here.
	//
	// But it takes the CONFIRMED variant, and the reason is the whole
	// polarity of this column. For the outbox, "shadow" is the safe
	// answer, so that resolver falls toward shadow whenever it cannot
	// read the session or parse its repos. Here "shadow" is the
	// PERMISSIVE answer: a true bit means "this snapshot never held more
	// than a read-only credential, it is safe to restore into a shadow
	// session", and dispatch.go only refuses a restore when the bit is
	// FALSE. Stamping the plain resolver's output would therefore mark
	// an unreadable session's snapshot as trusted -- turning a failure to
	// read into a grant, which is exactly backwards.
	//
	// So the bit is true only when shadow was positively established.
	// Anything else -- including "shadow, probably, but I could not read
	// the repos to be sure" -- leaves it false, and false means the
	// restore into a shadow session is refused.
	shadow, confirmed := a.stores.outbox.WithTx(tx).ResolveEffectiveModeConfirmed(ctx, a.sessionID)
	suppressedInShadow := shadow && confirmed
	if shadow && !confirmed {
		a.logger.Warn("sessionactor: snapshot: this session's egress mode could not be positively established; stamping the snapshot NOT shadow-clean, so a later restore into a shadow session is refused",
			"session_id", a.sessionID.String(), "snapshot_id", evt.SnapshotId)
	}

	// Also clears pending_snapshot_message_id back to nil in this SAME
	// statement -- see UpdateSandboxSnapshotID's own generated doc
	// comment (queries/sandboxes.sql).
	if _, err := a.stores.sandbox.WithTx(tx).UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
		SessionID:                  a.sessionID,
		SnapshotID:                 &evt.SnapshotId,
		SnapshotSuppressedInShadow: suppressedInShadow,
	}); err != nil {
		return fmt.Errorf("sessionactor: record snapshot id: %w", err)
	}
	// F2 fix: this Snapshotting->Ready transition just committed above -- a
	// genuine edge, since the early-return at the top of this function
	// already confirmed row.Status WAS Snapshotting -- so both watchdogs
	// must be re-armed here exactly as the Booting->Ready guard in
	// handleSandboxEvent does for its own edge.
	return a.armReadyWatchdogs(ctx, tx, now)
}

// revertSnapshotToReady performs the Snapshotting->Ready compensating
// transition and clears pending_snapshot_message_id back to nil, using
// the CALLER's own tx (never opening its own transact) -- shared by
// revertSnapshotBestEffort (below, which opens its own fresh transact and
// passes it here) and handleSnapshotReadyEvent's decode-failure path
// (above, which passes the SAME tx it was itself handed, already open
// inside handleSandboxEvent's own transact). row is read (and, in the
// decode-failure caller's case, already known to be Snapshotting) by the
// caller; this function re-confirms that itself so it is safe to call
// from either context without a caller-side duplicate check.
func (a *Actor) revertSnapshotToReady(ctx context.Context, tx pgx.Tx, row sqlcgen.Sandbox, now time.Time) error {
	if sandbox.State(row.Status) != sandbox.StateSnapshotting {
		// Already moved on via some other path (e.g. a liveness watchdog
		// already suspected it in the meantime) -- nothing to revert.
		a.logger.Warn("sessionactor: revert snapshot to ready: sandbox no longer snapshotting; ignoring",
			"status", row.Status)
		return nil
	}
	to, err := sandbox.Transition(sandbox.State(row.Status), int(row.Gen), sandbox.SnapshotCompleteTrigger())
	if err != nil {
		return fmt.Errorf("sessionactor: sandbox transition snapshotting->ready (revert): %w", err)
	}
	if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: a.sessionID,
		Status:    sqlcgen.SandboxStatus(to),
	}); err != nil {
		return fmt.Errorf("sessionactor: update sandbox status to ready (revert): %w", err)
	}
	if _, err := a.stores.sandbox.WithTx(tx).UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID:                a.sessionID,
		PendingSnapshotMessageID: nil,
	}); err != nil {
		return fmt.Errorf("sessionactor: clear pending snapshot message id (revert): %w", err)
	}
	// F2 fix: this compensating write just landed Snapshotting->Ready too
	// (reachable only on a genuine edge -- the early-return at the top of
	// this function already confirmed row.Status WAS Snapshotting) -- both
	// of THIS function's own two callers (handleSnapshotReadyEvent's
	// decode-failure branch and revertSnapshotBestEffort, covering its
	// SendCommand-failure path) get the re-arm for free from this one call.
	return a.armReadyWatchdogs(ctx, tx, now)
}

// snapshotPlan is what triggerSnapshotBestEffort's own first transact
// hands to itself when (and only when) it actually transitioned the
// sandbox Ready->Snapshotting -- the real SandboxCommander.SendCommand
// call happens OUTSIDE that transact, using exactly this plan, mirroring
// dispatch.go's own spawnPlan/dispatchPlan "commit state, THEN make the
// real call" shape.
type snapshotPlan struct {
	gen              int
	commandMessageID string
}

// triggerSnapshotBestEffort implements design decision 1's own post-turn
// snapshot trigger ("snapshots & restore", docs/IMPLEMENTATION_
// PLAN.md row 22's own "post-turn snapshot" bullet, and §3.3's own "On
// terminal event: complete turn, trigger snapshot..."). Called from
// handleSandboxEvent's own post-commit block ONLY when cmd.Type ==
// "execution_complete" (a real turn-terminal wire event) -- corrected per
// independent review from this file's own earlier, unconditional-call
// design, which let every routine heartbeat on an already-Ready session
// re-trigger a full snapshot cycle indefinitely (see the call site's own
// comment in handleSandboxEvent for the full reasoning). Its own logic,
// still worth keeping defensive/idempotent in its own right (a redelivered
// execution_complete, or any other future caller, must never double-fire
// a snapshot):
//
//  1. Mint the Snapshot command's own MessageId FIRST, before any
//     transact -- needed both to persist it (step 2) and to build the
//     real command (step 3).
//  2. Read the current sandbox row, in a NEW transact. If status != Ready,
//     no-op: a sandbox that's Suspect/Snapshotting/Booting/etc. is not
//     eligible (exactly one live sandbox per session, and it must be
//     idle-and-ready to snapshot). Otherwise transition Ready ->
//     Snapshotting via sandbox.SnapshotStartTrigger() AND persist the
//     minted MessageId onto sandboxes.pending_snapshot_message_id, in this
//     SAME transact (message-id correlation fix, below), commit.
//  3. OUTSIDE that transact: send a real sandboxws.Snapshot command via
//     ports.SandboxCommander.SendCommand (the SAME port used for
//     Prompt commands -- reused, not a second sandbox-command channel),
//     carrying the SAME MessageId just persisted.
//  4. If SendCommand fails (including ports.ErrNoLiveSandboxConnection):
//     SendCommand is a context-bounded conn.Write, a classic ambiguous-
//     write case -- the frame can already have been flushed to the OS/TCP
//     layer before the local call ever times out or errors, so "SendCommand
//     failed" does NOT reliably mean "the sandbox will never complete
//     this." revertSnapshotBestEffort (below) runs a SECOND, small
//     transact reverting Snapshotting back to Ready via
//     sandbox.SnapshotCompleteTrigger() AND clearing
//     pending_snapshot_message_id back to nil (a compensating write; no
//     gen-fencing concern since neither trigger is gen-fenced) -- so that
//     IF the frame actually did get through and a real snapshot_ready for
//     THIS attempt eventually arrives late (e.g. after a second attempt
//     has since started), handleSnapshotReadyEvent's own commandMessageId
//     match against the CURRENT pending id (now either nil or a later
//     attempt's own different id) correctly discards it as stale instead
//     of wrongly completing whatever attempt happens to be outstanding
//     when it finally arrives -- this closes a real race an independent
//     review confirmed against a real Postgres instance (see
//     migrations/000022_sandbox_snapshot_id.up.sql's own
//     pending_snapshot_message_id doc comment for the full scenario).
//     Logged at Warn, never treated as fatal to the turn-completion flow
//     that triggered it -- matches sendPushBestEffort/createPRBestEffort's
//     own "BestEffort" naming and never-alters-caller-return-value
//     discipline exactly (this function returns nothing at all).
//  5. If SendCommand succeeds: nothing more to do here -- wait for the
//     snapshot_ready event (handleSnapshotReadyEvent, above). HONEST GAP
//     (documented, not fixed by this Step -- see cmd/sandbox-agent/
//     main.go's own HandleSnapshot doc comment for the matching statement
//     of the same fact): if the command is genuinely lost on the wire
//     despite SendCommand reporting success (rather than merely delayed),
//     or the sandbox process itself dies mid-snapshot before ever
//     reporting snapshot_ready, the sandbox is left stuck Snapshotting --
//     no watchdog covers that state today, and building one (a dedicated
//     snapshot-timeout timer, or a broader NACK mechanism) is explicitly
//     out of this Step's own scope.
func (a *Actor) triggerSnapshotBestEffort(ctx context.Context) {
	messageID := uuid.NewString()

	var plan *snapshotPlan
	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No sandbox row at all -- defensive only: the ONLY
				// current caller (handleSandboxEvent) already read this
				// same row successfully moments ago in this very
				// invocation, so this branch is unreachable in practice
				// today, but a future caller invoking this unconditionally
				// with no guaranteed-existing row must not panic or error
				// loudly here.
				return nil
			}
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}
		if sandbox.State(row.Status) != sandbox.StateReady {
			return nil
		}

		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session for snapshot eligibility: %w", err)
		}

		// §27.8's own genuinely-unresolved point, resolved here (§27.5
		// brief, point D): "Capabilities() is a flat, provider-level
		// report; a provider whose snapshot support differs by runtime
		// (Modal gVisor vs VM runtime) cannot express that today." There
		// is no real Modal deployment this codebase can verify snapshot
		// parity against (doc.go: every wire shape in the Modal adapter
		// is this codebase's own invention, tested against a fake
		// httptest.Server) — so rather than silently ASSUME a Docker-
		// enabled (VM-runtime) sandbox snapshots identically to a gVisor
		// one, this control plane simply never attempts one: a Docker-
		// required Environment's sandbox degrades to resume-only recovery
		// (§3.2) until a real §9.3-class restore-with-docker scenario
		// (test/resilience) proves parity against a provider's actual
		// behavior — exactly the escape hatch §27.8 itself names ("decided at
		// implementation time... not guessed here"). Concretely: this
		// sandbox's own snapshot_id column simply never gets populated,
		// so dispatch.go's EvaluateSpawnDecision Restore branch (which
		// requires SnapshotImageID != "") can never fire for it either —
		// a lost Docker-required sandbox recovers via a fresh respawn
		// only, never a snapshot restore. This is a REAL, NAMED cost
		// (§27.5's own "the costs are real and named" convention,
		// extended here): a Docker-enabled sandbox loses whatever
		// in-progress state a snapshot would otherwise have preserved
		// across a respawn.
		_, dockerRequired, _, err := a.environmentSubstrate(ctx, tx, sessionRow.EnvironmentID)
		if err != nil {
			return fmt.Errorf("sessionactor: resolve docker_required for snapshot eligibility: %w", err)
		}
		if dockerRequired {
			a.logger.Info("sessionactor: skipping snapshot trigger for a Docker-required session (§27.8 unresolved VM-runtime snapshot-parity point; resume-only recovery until proven safe)",
				"session_id", a.sessionID.String())
			return nil
		}

		to, err := sandbox.Transition(sandbox.State(row.Status), int(row.Gen), sandbox.SnapshotStartTrigger())
		if err != nil {
			return fmt.Errorf("sessionactor: sandbox transition ready->snapshotting: %w", err)
		}
		if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
			SessionID: a.sessionID,
			Status:    sqlcgen.SandboxStatus(to),
		}); err != nil {
			return fmt.Errorf("sessionactor: update sandbox status to snapshotting: %w", err)
		}
		// Message-id correlation fix: persist the freshly-minted command
		// MessageId in this SAME transact (no extra round trip) --
		// handleSnapshotReadyEvent only accepts a later snapshot_ready as
		// completing THIS attempt if its own commandMessageId matches this
		// column's value.
		if _, err := a.stores.sandbox.WithTx(tx).UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
			SessionID:                a.sessionID,
			PendingSnapshotMessageID: &messageID,
		}); err != nil {
			return fmt.Errorf("sessionactor: record pending snapshot message id: %w", err)
		}
		plan = &snapshotPlan{gen: int(row.Gen), commandMessageID: messageID}
		return nil
	})
	if err != nil {
		a.logger.Warn("sessionactor: trigger snapshot: transact failed", "error", err)
		return
	}
	if plan == nil {
		// Not Ready (or no sandbox row at all) -- no snapshot warranted
		// this round; the common, expected case for most sandbox events.
		return
	}

	if a.commander == nil {
		// Defensive: mirrors tryPlanDispatch/tryPlanSpawn's own identical
		// nil-port guards exactly -- some tests, and any future caller
		// genuinely without one, must not panic here. The sandbox is
		// already committed Snapshotting at this point, so it must be
		// reverted back to Ready -- there is no live channel to ever
		// deliver the snapshot command it's waiting for.
		a.logger.Warn("sessionactor: sandbox is ready to snapshot but no SandboxCommander is configured; reverting to ready")
		a.revertSnapshotBestEffort(ctx)
		return
	}

	cmd := sandboxws.Snapshot{
		Type:      "snapshot",
		MessageId: plan.commandMessageID,
		SessionId: a.sessionID.String(),
		Gen:       plan.gen,
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		a.logger.Error("sessionactor: marshal snapshot command failed", "error", err)
		a.revertSnapshotBestEffort(ctx)
		return
	}

	if err := a.commander.SendCommand(a.sessionID.String(), payload); err != nil {
		// Covers ports.ErrNoLiveSandboxConnection and every other send
		// failure identically -- see this function's own doc comment,
		// point 4.
		a.logger.Warn("sessionactor: send snapshot command failed; reverting sandbox to ready", "error", err)
		a.revertSnapshotBestEffort(ctx)
	}
}

// revertSnapshotBestEffort is triggerSnapshotBestEffort's own compensating
// write for when the snapshot command never reached the sandbox: opens
// its OWN fresh transact (unlike handleSnapshotReadyEvent's own decode-
// failure revert, which reuses an already-open tx it was handed -- see
// revertSnapshotToReady's own doc comment for why the two calling contexts
// need different transaction-acquisition strategies), reads the current
// row, then delegates the actual revert-to-Ready-and-clear-pending-id
// logic to revertSnapshotToReady so both callers share exactly one
// implementation of it. A failure of THIS revert is logged at Warn too,
// never escalated -- there is nothing further this best-effort path can
// do about it (matches sendPushBestEffort/createPRBestEffort's own
// terminal, log-only failure handling).
func (a *Actor) revertSnapshotBestEffort(ctx context.Context) {
	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}
		return a.revertSnapshotToReady(ctx, tx, row, time.Now())
	})
	if err != nil {
		a.logger.Warn("sessionactor: revert snapshot to ready failed", "error", err)
	}
}

// sendGitSyncCompleteBestEffort implements handleSandboxEvent's own
// git_sync reply ("gitstate in-sandbox", §3.4 design section 6):
// a real sandboxws.GitSyncComplete command, a pure acknowledgment carrying
// no fields beyond the envelope (commands.schema.json's own
// GitSyncComplete def) -- mirrors sendPushBestEffort/triggerSnapshotBestEffort's
// own exact "mint a fresh MessageId, marshal, SendCommand, log a Warn on
// failure, never escalate" shape. git_sync itself carries no ackId (a
// best-effort event, not one of the 6 critical types), and the sandbox's
// own reconciliation sequence (internal/sandboxagent/gitclone.SyncAll)
// does not gate on or wait for this reply before proceeding to its next
// phase -- see cmd/sandbox-agent/main.go's own HandleGitSyncComplete doc
// comment for the sandbox-side half of this design. Never alters
// handleSandboxEvent's own return value: the git_sync event that triggered
// this already committed successfully regardless of whether this reply
// can be delivered.
func (a *Actor) sendGitSyncCompleteBestEffort(sessionID string, gen int) {
	if a.commander == nil {
		a.logger.Warn("sessionactor: git_sync received but no SandboxCommander is configured; cannot ack")
		return
	}

	messageID := uuid.NewString()
	msg := sandboxws.GitSyncComplete{
		Type:      "git_sync_complete",
		MessageId: messageID,
		SessionId: sessionID,
		Gen:       gen,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		a.logger.Error("sessionactor: marshal git_sync_complete command failed", "error", err)
		return
	}

	if err := a.commander.SendCommand(sessionID, payload); err != nil {
		a.logger.Warn("sessionactor: send git_sync_complete command failed", "error", err)
	}
}
