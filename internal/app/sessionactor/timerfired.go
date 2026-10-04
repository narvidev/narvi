// This file (timerfired.go) implements handling a TimerFired command
// for each of the 5 named persistent timers (§2), using ONLY the
// already-built domain packages (internal/domain/{sandbox,turn,session})
// for every actual decision -- this file's own job is orchestration
// (reading current state, calling the right decision function, writing
// the result back transactionally) and never reimplementing a decision
// those packages already make. §24 ("review: automatic re-review on
// new commits", §24) adds a 6th named timer, review_retrigger_debounce --
// its own fire handler, handleReviewRetriggerDebounceTimer, is dispatched
// from the SAME switch below but implemented in reviewretrigger.go, not
// this file, since it is armed from OUTSIDE the actor entirely (see that
// file's own top comment) rather than by any of this file's own handlers,
// unlike the 5 timers this file's rest describes. Technical plan §3.3's
// stop adds a 7th, stop, armed from outside the actor the same way (by
// POST /api/sessions/{sessionID}/stop) and implemented in stop.go. The
// durable dispatch trigger (§2, §3.3) adds an 8th, dispatch, armed in the
// transaction that creates a turn (postgres.TurnStore.CreateAndArmDispatch)
// and handled by handleEnsureDispatched (dispatch.go), whose planDispatch
// deletes it. Technical plan §24.9's owed human review requests add a
// 9th, owed_review_request, armed by the dispatch that ends a person's
// queued review attempt context_moved and handled by
// handleOwedReviewRequestTimer (owedreviewrequest.go).
//
// All 5 named timers' RE-ARM/handling logic is fully wired here -- none
// needed a SandboxProvider or AgentRuntime (neither port exists yet). The
// initial arm (the very first time each timer is ever set) is
// each timer's OWN concern, not this file's: connecting_deadline and
// turn_deadline are armed for the first time at spawn/dispatch time
// (dispatch.go), and liveness_check/inactivity are armed for the first
// time at the real Booting->Ready transition (sandboxevent.go's
// handleSandboxEvent) -- see that function's own doc comment for why
// arming them there, exactly once, rather than here, is correct.
// terminal_grace is armed for the first time by transitionSandboxToSuspect
// below, itself called from three different watchdog timeouts (inactivity,
// connecting_deadline, liveness_check) and from a permanent spawn failure
// (dispatch.go's recordSpawnFailure) -- never from this file directly
// either.
//
//   - inactivity: domain/sandbox.EvaluateInactivityTimeout. One
//     deliberate, documented simplification: ConnectedClientCount is
//     always 0. The client WS hub itself now exists and does track
//     connected participants (internal/adapters/inbound/wshub's *Hub,
//     §6.2), but this package has no port through which to ask it for
//     a live count -- see handleInactivityTimer. This does not stop the
//     timer from being fully wired; it just means the "clients connected
//     -> extend + warn" branch stays unreachable until that wiring lands.
//   - connecting_deadline / liveness_check: domain/sandbox.
//     EvaluateConnectingTimeout / EvaluateHeartbeatHealth, respectively.
//   - terminal_grace: unconditionally treated as a genuine timeout (see
//     handleTerminalGraceTimer for why the Stopped/Stale peer outcomes
//     are not reachable yet, and why that is a documented absence of
//     their triggering conditions, not a shortcut taken here).
//   - turn_deadline: domain/turn.EvaluateTurnDeadline +
//     RequiresSyntheticExecutionComplete + DeriveFailureReason.
//
// Every handler ends by either re-arming (UpsertSessionTimer to a fresh,
// meaningful fires_at) or deleting the timer that fired. This is a hard
// contract, not a style preference: the timer pump's claim step (§2)
// pushes fires_at forward as a "delivery in progress" marker before this
// actor ever sees the command, precisely so a crash mid-handling
// self-heals once the claim window elapses. A handler that did neither --
// left the claimed row untouched -- would have that same claim window
// silently expire and redeliver the identical TimerFired command forever,
// even long after the condition it was watching stopped being relevant.
// The one deliberate exception is a kind this binary does not know, left
// untouched while its row is young enough that a newer replica's pump may
// still want it at the claim cadence -- and bounded by the row's age past
// that (handleUnknownTimer).

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

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// handle dispatches a Command to its handler. Command's isCommand method
// is unexported (command.go), so the default case below is unreachable
// dead-code protection, not a real possibility this package needs to
// handle gracefully.
func (a *Actor) handle(ctx context.Context, cmd Command) error {
	switch c := cmd.(type) {
	case TimerFired:
		return a.handleTimerFired(ctx, c)
	case SandboxEvent:
		return a.handleSandboxEvent(ctx, c)
	case EnsureDispatched:
		return a.handleEnsureDispatched(ctx)
	default:
		return fmt.Errorf("sessionactor: unhandled command type %T", cmd)
	}
}

func (a *Actor) handleTimerFired(ctx context.Context, cmd TimerFired) error {
	if handler, ok := a.timerHandler(cmd.Name); ok {
		return handler(ctx)
	}
	// TEXT column, not an enum (§2) -- an unrecognized name is handled
	// defensively (deny-list-not-allow-list, same convention as
	// domain/sandbox.IsDeadSandboxStatus), never a fatal error.
	return a.handleUnknownTimer(ctx, cmd.Name)
}

// timerHandler returns the handler of a timer kind this binary knows, and
// false for any other name, which handleTimerFired sends to
// handleUnknownTimer instead. It lists the kinds ClassifyTimer classifies:
// timerwork_test.go fails when a declared Timer* kind has no handler here,
// so a kind this binary declares never reaches the unknown-kind path,
// whatever its row's age.
func (a *Actor) timerHandler(name string) (func(context.Context) error, bool) {
	switch name {
	case TimerInactivity:
		return a.handleInactivityTimer, true
	case TimerConnectingDeadline:
		return a.handleConnectingDeadlineTimer, true
	case TimerLivenessCheck:
		return a.handleLivenessCheckTimer, true
	case TimerTerminalGrace:
		return a.handleTerminalGraceTimer, true
	case TimerTurnDeadline:
		return a.handleTurnDeadlineTimer, true
	case TimerReviewRetriggerDebounce:
		return a.handleReviewRetriggerDebounceTimer, true
	case TimerStop:
		return a.handleStopTimer, true
	case TimerDispatch:
		// The evaluation deletes the row itself, first, in planDispatch's
		// own transaction, as it does for every other caller.
		return a.handleEnsureDispatched, true
	case TimerOwedReviewRequest:
		return a.handleOwedReviewRequestTimer, true
	default:
		return nil, false
	}
}

// handleUnknownTimer handles a timer whose kind this binary does not know
// (technical plan §2). The kind is either a newer binary's -- a newer
// replica armed it during a rolling deploy, and its own pump will claim it
// -- or one nothing will handle again, left behind by a rollback or a
// retired kind. This binary cannot tell which, so DecideUnknownTimer goes
// by the time since the row was last armed (armed_at), read with the
// database's now() in the same statement (TimerStore.Age), and this
// applies it in one transaction:
//
//   - kept (last armed less than UnknownTimerGrace ago): nothing is
//     written, so the row comes back when the pump's claim lapses, within
//     one TimerClaimDuration -- a newer replica's timer loses no more than
//     that, as it always has;
//   - backed off: its fires_at, and nothing else, moves to the database's
//     now plus UnknownTimerBackoff;
//   - deleted (last armed UnknownTimerDeleteAfter ago or more).
//
// Both writes hold only while the row still carries the armed_at the
// decision read (TimerStore.PostponeIfArmedAt, DeleteIfArmedAt): the read
// locks nothing, so a newer replica can re-arm the row between the two,
// and its re-arm wins -- the write then touches no row, and nothing is
// logged at WARN or counted. Each outcome that did write logs one WARN
// naming the kind and counts session_timer_unknown_kind_total, after the
// commit. A row already gone -- a TimerFired sent with no row behind it --
// changes nothing. Until the row is deleted, the session's status counts
// it as scheduled work (TimerCanCreateWork), the safe direction.
func (a *Actor) handleUnknownTimer(ctx context.Context, name string) error {
	var (
		action  UnknownTimerAction
		age     time.Duration
		firesAt time.Time
		written bool
	)
	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		action, written = 0, false
		timers := a.stores.timer.WithTx(tx)
		row, err := timers.Age(ctx, sqlcgen.GetSessionTimerAgeParams{SessionID: a.sessionID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sessionactor: read the age of timer %q: %w", name, err)
		}
		now := row.DbNow.Time
		age = now.Sub(row.ArmedAt.Time)
		action = DecideUnknownTimer(row.ArmedAt.Time, now, a.timeouts.UnknownTimerGrace, a.timeouts.UnknownTimerDeleteAfter)
		var n int64
		switch action {
		case UnknownTimerBackOff:
			firesAt = now.Add(a.timeouts.UnknownTimerBackoff)
			n, err = timers.PostponeIfArmedAt(ctx, sqlcgen.PostponeSessionTimerIfArmedAtParams{
				FiresAt:   pgtype.Timestamptz{Time: firesAt, Valid: true},
				SessionID: a.sessionID,
				Name:      name,
				ArmedAt:   row.ArmedAt,
			})
			if err != nil {
				return fmt.Errorf("sessionactor: back off timer %q: %w", name, err)
			}
		case UnknownTimerDelete:
			n, err = timers.DeleteIfArmedAt(ctx, sqlcgen.DeleteSessionTimerIfArmedAtParams{
				SessionID: a.sessionID,
				Name:      name,
				ArmedAt:   row.ArmedAt,
			})
			if err != nil {
				return fmt.Errorf("sessionactor: delete timer %q: %w", name, err)
			}
		}
		written = n > 0
		return nil
	})
	if err != nil {
		return err
	}

	switch {
	case action == 0:
		a.logger.Info("sessionactor: TimerFired with unknown name has no timer row", "name", name)
	case action == UnknownTimerKeep:
		a.logger.Warn("sessionactor: ignoring TimerFired with unknown name", "name", name, "age", age)
	case !written:
		a.logger.Info("sessionactor: timer kind this binary does not know was re-armed meanwhile; left as it is",
			"name", name, "decided", action.String())
	case action == UnknownTimerBackOff:
		a.logger.Warn("sessionactor: timer kind this binary does not know backed off",
			"name", name, "age", age, "fires_at", firesAt)
		a.recordUnknownTimer(ctx, action)
	case action == UnknownTimerDelete:
		a.logger.Warn("sessionactor: timer kind this binary does not know deleted",
			"name", name, "age", age)
		a.recordUnknownTimer(ctx, action)
	}
	return nil
}

// handleInactivityTimer implements the `inactivity` named timer (§2).
// See this file's own top comment for the ConnectedClientCount
// simplification.
func (a *Actor) handleInactivityTimer(ctx context.Context) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No sandbox yet -- nothing to watch. Re-arm at the
				// minimum check interval so the pump keeps polling until
				// one exists.
				return a.armTimer(ctx, tx, TimerInactivity, now.Add(a.timeouts.InactivityMinCheckInterval))
			}
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: list turns: %w", err)
		}

		state := sandbox.InactivityState{
			LastActivity: pgTimeOrZero(sandboxRow.LastSeenAt),
			Status:       sandbox.State(sandboxRow.Status),
			// ConnectedClientCount is always 0 for now: the client WS
			// hub itself now exists and does track connected
			// participants (internal/adapters/inbound/wshub's *Hub,
			// §6.2), but this actor has no field/port through which
			// to query it for a live count. Until that wiring lands,
			// EvaluateInactivityTimeout can never take its "clients
			// still connected -> extend + warn" branch -- every genuine
			// inactivity timeout goes straight to
			// InactivityActionTimeout. This is a deliberate, documented
			// limitation of what THIS caller can observe, not a gap in
			// the decision function itself, which is used exactly as
			// built.
			ConnectedClientCount: 0,
			IsProcessing:         anyTurnProcessing(turns),
		}
		cfg := sandbox.InactivityConfig{
			Timeout:          a.timeouts.InactivityTimeout,
			Extension:        a.timeouts.InactivityExtension,
			MinCheckInterval: a.timeouts.InactivityMinCheckInterval,
		}
		action := sandbox.EvaluateInactivityTimeout(state, cfg, now)

		switch action.Kind {
		case sandbox.InactivityActionSchedule:
			return a.armTimer(ctx, tx, TimerInactivity, now.Add(action.NextCheck))

		case sandbox.InactivityActionExtend:
			if err := a.armTimer(ctx, tx, TimerInactivity, now.Add(action.Extension)); err != nil {
				return err
			}
			if !action.ShouldWarn {
				return nil
			}
			return a.appendEvent(ctx, tx, "warning", map[string]any{
				"reason": "inactivity_extended",
			})

		case sandbox.InactivityActionTimeout:
			// §3.2's two-phase design: the watchdog's only job is moving
			// the sandbox to Suspect and arming terminal_grace -- NOT
			// calling a provider to actually snapshot/stop it (that's
			// the SandboxProvider's own job, out of scope here).
			// action.ShouldSnapshot is deliberately never consulted.
			//
			// gap: EvaluateInactivityTimeout's own InactivityAction
			// carries no elapsed figure of its own (unlike
			// EvaluateConnectingTimeout/EvaluateHeartbeatHealth below), so
			// it is computed here, inline, from the same state.LastActivity
			// already read into state above -- now.Sub(state.LastActivity)
			// is exactly "how long since this sandbox's last activity",
			// the same quantity InactivityActionTimeout's own trip
			// condition (inactiveTime >= cfg.Timeout) just tested.
			if err := a.transitionSandboxToSuspect(ctx, tx, sandboxRow, now, watchdogInactivity, now.Sub(state.LastActivity)); err != nil {
				return err
			}
			// The sandbox is no longer Ready -- inactivity monitoring no
			// longer applies until (if ever) it recovers.
			return a.deleteTimer(ctx, tx, TimerInactivity)

		default:
			return fmt.Errorf("sessionactor: unknown inactivity action kind %v", action.Kind)
		}
	})
}

// handleConnectingDeadlineTimer implements the `connecting_deadline`
// named timer (§2), sharing EvaluateConnectingTimeout /
// EvaluateHeartbeatHealth's two-budget model with handleLivenessCheckTimer.
func (a *Actor) handleConnectingDeadlineTimer(ctx context.Context) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return a.deleteTimer(ctx, tx, TimerConnectingDeadline)
			}
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		status := sandbox.State(sandboxRow.Status)
		if !isConnectingPhase(status) {
			// Sandbox has already moved past its boot phase (or into
			// Suspect/terminal via some other path) -- this timer no
			// longer applies; drop it rather than re-arming forever.
			return a.deleteTimer(ctx, tx, TimerConnectingDeadline)
		}

		cfg := sandbox.LivenessConfig{
			FirstConnectBudget:    a.timeouts.FirstConnectBudget,
			SteadyHeartbeatBudget: a.timeouts.SteadyHeartbeatBudget,
		}
		result := sandbox.EvaluateConnectingTimeout(status, pgTimeOrZero(sandboxRow.CreatedAt), pgTimeOrZero(sandboxRow.LastSeenAt), cfg, now)

		if !result.IsTimedOut {
			// Re-check at a short interval so the budget's expiry is
			// noticed promptly. platform.Timeouts has no dedicated
			// "connecting recheck interval" (not specified in the plan);
			// InactivityMinCheckInterval is reused here as the
			// platform's existing "how often is it OK to poll a
			// liveness-style condition" constant, rather than inventing
			// a near-duplicate field for the same judgment call.
			return a.armTimer(ctx, tx, TimerConnectingDeadline, now.Add(a.timeouts.InactivityMinCheckInterval))
		}

		if err := a.transitionSandboxToSuspect(ctx, tx, sandboxRow, now, watchdogConnectingDeadline, result.Elapsed); err != nil {
			return err
		}
		return a.deleteTimer(ctx, tx, TimerConnectingDeadline)
	})
}

// handleLivenessCheckTimer implements the `liveness_check` named timer
// (§2): the Ready-state counterpart of handleConnectingDeadlineTimer.
func (a *Actor) handleLivenessCheckTimer(ctx context.Context) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return a.deleteTimer(ctx, tx, TimerLivenessCheck)
			}
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		if sandbox.State(sandboxRow.Status) != sandbox.StateReady {
			// liveness_check only monitors the steady Ready state (§3.2's
			// steady_heartbeat_budget); once the sandbox leaves Ready via
			// any path, this timer no longer applies.
			return a.deleteTimer(ctx, tx, TimerLivenessCheck)
		}

		cfg := sandbox.LivenessConfig{
			FirstConnectBudget:    a.timeouts.FirstConnectBudget,
			SteadyHeartbeatBudget: a.timeouts.SteadyHeartbeatBudget,
		}
		health := sandbox.EvaluateHeartbeatHealth(pgTimeOrZero(sandboxRow.LastSeenAt), cfg, now)

		if !health.IsStale {
			return a.armTimer(ctx, tx, TimerLivenessCheck, now.Add(a.timeouts.SteadyHeartbeatBudget))
		}

		if err := a.transitionSandboxToSuspect(ctx, tx, sandboxRow, now, watchdogLivenessCheck, health.Age); err != nil {
			return err
		}
		return a.deleteTimer(ctx, tx, TimerLivenessCheck)
	})
}

// handleTerminalGraceTimer implements the `terminal_grace` named timer
// (§2, §3.2). Also closes a confirmed redelivery gap (this batch's own
// fix): a session whose sandbox NEVER manages to spawn at all has no
// sandbox event -- and therefore no handleSandboxEvent call -- to ever
// re-trigger EnsureDispatched (command.go's own doc comment names exactly
// these two call sites: httpapi.CreateSession, once, at turn creation, and
// handleSandboxEvent, on every real sandbox event). Reusing THIS timer
// (rather than inventing a 6th named timer -- command.go's own doc
// comment is explicit that the 5 named persistent timers are a closed
// set) as the redelivery trigger is correct because it is the one
// existing handler that transitions a sandbox into its final Suspect->
// Failed state while a turn may still be Pending: once terminal_grace's
// own transact below commits, handleEnsureDispatched is invoked
// unconditionally -- exactly mirroring handleSandboxEvent's own "re-
// evaluate dispatch state right after this handler's own transact
// commits" precedent (sandboxevent.go) -- so tryPlanSpawn/dispatch.go's
// own already-correct "spawn again from Failed via SpawnTrigger" logic
// gets a genuine chance to run again, using platform.Timeouts.
// TerminalGracePeriod's own existing ~60s cadence (plus dispatch.go's own
// SpawnCooldown, which already gates how soon a fresh attempt is actually
// allowed) as the natural retry interval.
//
// Called even on the two early-return branches below (no sandbox row;
// sandbox already moved past Suspect via some other path):
// handleEnsureDispatched is designed as a safe, idempotent "please
// re-evaluate" signal (see its own doc comment) -- calling it there too is
// a harmless no-op, not a bug, and special-casing just the Suspect->Failed
// branch would add complexity for no real benefit.
func (a *Actor) handleTerminalGraceTimer(ctx context.Context) error {
	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return a.deleteTimer(ctx, tx, TimerTerminalGrace)
			}
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		if sandbox.State(sandboxRow.Status) != sandbox.StateSuspect {
			// Already recovered (or already terminal) via some other
			// path before this timer fired -- this specific grace period
			// is moot.
			return a.deleteTimer(ctx, tx, TimerTerminalGrace)
		}

		// §3.2: "Any liveness signal during grace returns to previous
		// state." This Step has no mechanism for a genuine external
		// liveness signal to arrive DURING grace and reach this actor as
		// a command (that requires the sandbox WS hub to
		// exist and deliver one -- e.g. a future LivenessSignal command
		// driving TriggerRecover). Absent that, terminal_grace firing
		// here is UNCONDITIONALLY treated as a genuine timeout:
		// Suspect -> Failed. Stopped (an explicit stop request) and
		// Stale (a GC/reconciler classification) are the two OTHER peer
		// terminal outcomes domain/sandbox.Transition allows for
		// TriggerGraceExpired, but neither is reachable from this
		// handler yet -- Stopped needs an HTTP stop endpoint (a later
		// Step) to have recorded that a stop was explicitly requested.
		// Stale remains genuinely unreachable too, but NOT for lack of a
		// reconciler any more: internal/app/reconciler (§5.3,
		// "reconciler + GC") exists now, but is deliberately PURE
		// cloud-side orphan reaping -- it calls ports.SandboxProvider.
		// StopSandbox on a provider ref with no live Postgres owner, and
		// never writes to any sandboxes row's own status column (see that
		// package's own doc.go). Classifying an ALREADY-Suspect row like
		// THIS one as Stale once a reconciler independently confirms its
		// cloud resource is gone is a distinct, currently-hypothetical
		// concept -- domain/sandbox's own three-way TriggerGraceExpired
		// target already supports it structurally, should some FUTURE
		// Step ever decide to build it, but nothing in this codebase does
		// today.
		to, err := sandbox.Transition(sandbox.StateSuspect, int(sandboxRow.Gen), sandbox.GraceExpiredTrigger(sandbox.StateFailed))
		if err != nil {
			return fmt.Errorf("sessionactor: sandbox transition suspect->failed: %w", err)
		}
		if _, err := a.sandboxWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
			SessionID: a.sessionID,
			Status:    sqlcgen.SandboxStatus(to),
		}); err != nil {
			return fmt.Errorf("sessionactor: update sandbox status to failed: %w", err)
		}

		// The sandbox failing does not, on its own, fail whatever turn
		// might currently be Processing (this handler never touches
		// turns) -- that cascade is turn_deadline's own independent job
		// (handleTurnDeadlineTimer), armed and firing on its own separate
		// schedule. Re-deriving here is still correct and required: it
		// keeps the session row consistent with the turn history as it
		// stands right now, exactly as this Step scopes it.
		if err := a.rederiveSessionStatusUnchanged(ctx, tx); err != nil {
			return err
		}
		return a.deleteTimer(ctx, tx, TimerTerminalGrace)
	})

	if err == nil {
		if dispatchErr := a.handleEnsureDispatched(ctx); dispatchErr != nil {
			a.logger.Warn("sessionactor: ensure-dispatched after terminal grace failed", "error", dispatchErr)
		}
	}
	return err
}

// handleTurnDeadlineTimer implements the `turn_deadline` named timer (§2,
// §3.3).
func (a *Actor) handleTurnDeadlineTimer(ctx context.Context) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: list turns: %w", err)
		}

		processing, ok := findProcessingTurn(turns)
		if !ok {
			// No turn currently Processing: it already reached a
			// terminal state via some other path before this timer
			// fired, or the timer is simply stale. turn_deadline is
			// armed exactly once, at TriggerDispatch (see
			// domain/turn.EvaluateTurnDeadline's own doc), so unlike the
			// liveness timers there is no "reschedule and keep watching"
			// case here -- just drop it.
			return a.deleteTimer(ctx, tx, TimerTurnDeadline)
		}

		result := turn.EvaluateTurnDeadline(pgTimeOrZero(processing.DispatchedAt), now, turn.DeadlineConfig{
			Deadline: a.timeouts.TurnDeadline,
		})

		if !result.IsTimedOut {
			// Re-arm at the remaining time. turn_deadline is
			// conceptually a one-shot deadline, not a recheck-interval
			// timer like the liveness ones -- but re-arming defensively
			// here correctly handles the timer pump's claim-and-
			// redeliver semantics if this ever fires "early" (e.g. a
			// claim-window edge case), by pushing it out to the real
			// remaining deadline instead of treating an early wakeup as
			// the genuine one.
			remaining := a.timeouts.TurnDeadline - result.Elapsed
			return a.armTimer(ctx, tx, TimerTurnDeadline, now.Add(remaining))
		}

		to, err := turn.Transition(turn.StateProcessing, turn.TriggerTimeout)
		if err != nil {
			return fmt.Errorf("sessionactor: turn transition processing->failed: %w", err)
		}

		if _, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
			ID:          processing.ID,
			Status:      sqlcgen.TurnStatus(to),
			CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("sessionactor: update turn status: %w", err)
		}

		// sessionRow is fetched once here, reused below both by
		// OnTurnCompleted (§25.6/§25.9) and by enqueueOutboxNotification
		// (§5.1) further down this same transact.
		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session: %w", err)
		}

		// §25.6/§25.9 ("workflow execution engine" / "workflow HITL gate +
		// circuit breaker", §25.6/§25.9): this turn just reached a real
		// terminal state via its own turn_deadline, exactly like a real
		// execution_complete event would (pushpr.go's completeProcessingTurn)
		// -- see OnTurnCompleted's own doc comment for why ALL THREE
		// terminal-state call sites need this hook, not just that one.
		workflowengine.OnTurnCompleted(ctx, workflowengine.Deps{
			Workflows:             a.stores.workflow.WithTx(tx),
			Turns:                 a.stores.turn.WithTx(tx),
			SlackThreadSessions:   a.stores.slackThreadSession.WithTx(tx),
			LinearAgentSessions:   a.stores.linearAgentSession.WithTx(tx),
			GitHubPRSessions:      a.stores.githubPRSession.WithTx(tx),
			Outbox:                a.stores.outbox.WithTx(tx),
			EpistemicCheckDefault: a.epistemicCheckDefault,
		}, sessionRow, processing.ID, turn.TriggerTimeout)

		// §3.3: "Stop/failure paths emit a synthetic execution_complete
		// event so clients always see one terminal event per turn" --
		// exactly the machinery §3.1 built for this caller (see
		// domain/turn's own doc.go). RequiresSyntheticExecutionComplete
		// and DeriveFailureReason are used exactly as built, not
		// reimplemented.
		if turn.RequiresSyntheticExecutionComplete(turn.TriggerTimeout) {
			if err := a.appendEvent(ctx, tx, "execution_complete", syntheticExecutionComplete(processing.ID, turn.StateProcessing, "timeout")); err != nil {
				return err
			}
		}

		failureReason, _ := turn.DeriveFailureReason(turn.StateProcessing, turn.TriggerTimeout)

		if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverride(turns, processing.ID, to, failureReason)); err != nil {
			return err
		}

		// §5.1 ("outbox delivery", §5.1): a turn that fails HERE, on its
		// own turn_deadline, needs the same outbound notification a turn
		// that fails via a real execution_complete already gets
		// (completeProcessingTurn, pushpr.go). Before this fix, only that
		// real-event path enqueued one -- so a Slack- or Linear-origin
		// session whose turn simply timed out went permanently silent on
		// its originating channel, visible only to the web UI (which reads
		// turn state directly and so needs no notification at all).
		//
		// The plan argument is unconditionally nil: recordPlanIfNeeded
		// (planrecord.go) only ever records a plan for a genuinely
		// COMPLETED plan_mode turn, so a timed-out turn can never have one
		// to route a plan-approval-request notification for.
		//
		// Enqueued inside this handler's own already-open transact, before
		// the deleteTimer that closes it -- §5.1's "written in the same tx
		// as the state change" rule, identical to how completeProcessingTurn
		// enqueues inside handleSandboxEvent's own transact. sessionRow is
		// the SAME row already fetched above.
		if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, turn.TriggerTimeout, failureReason, processing, nil, ""); err != nil {
			return err
		}

		return a.deleteTimer(ctx, tx, TimerTurnDeadline)
	})
}

// transitionSandboxToSuspect is the shared Suspect-transition + arm-grace
// step every watchdog-style timer (inactivity, connecting_deadline,
// liveness_check) performs identically on timeout, per §3.2's two-phase
// design: "a watchdog never writes failed directly. It writes suspect and
// arms terminal_grace." §3.2 ("two-phase terminalization") extends this
// shared step to ALSO persist row.Status -- the live state being left,
// always one of the five states TriggerSuspect's own transition-table
// entries allow (Spawning/Connecting/Booting/Ready/Snapshotting) -- as
// pre_suspect_status, in the SAME statement (UpdateSandboxStatusToSuspect,
// queries/sandboxes.sql): §3.2's own recovery rule ("any liveness signal
// during grace returns to previous state") needs this value later
// (handleSandboxEvent's own recovery branch, sandboxevent.go), and by the
// time a recovery signal arrives, the sandbox row's own status column
// would otherwise already read 'suspect' with no memory of what it
// recovered FROM.
// watchdog identifies which watchdog-style timer drove this call (§5.3:
// watchdog activations / liveness gaps -- see watchdogKind's own doc
// comment, opsmetrics.go) -- "" for recordSpawnFailure's own distinct
// permanent-provider-error path (dispatch.go), which records neither
// instrument. gap is how long the sandbox had shown no sign of life by
// the moment the watchdog fired (ignored when watchdog == "").
func (a *Actor) transitionSandboxToSuspect(ctx context.Context, tx pgx.Tx, row sqlcgen.Sandbox, now time.Time, watchdog watchdogKind, gap time.Duration) error {
	// The returned target is always sandbox.StateSuspect (TriggerSuspect's
	// own single target, state.go) -- this call's own value is discarded;
	// what matters is Transition's validation that (row.Status, Suspect)
	// is a legal edge at all, exactly as before this Step.
	if _, err := sandbox.Transition(sandbox.State(row.Status), int(row.Gen), sandbox.SuspectTrigger()); err != nil {
		return fmt.Errorf("sessionactor: sandbox transition to suspect: %w", err)
	}
	preSuspect := sqlcgen.SandboxStatus(row.Status)
	if _, err := a.sandboxWrites(tx).UpdateStatusToSuspect(ctx, sqlcgen.UpdateSandboxStatusToSuspectParams{
		SessionID:        a.sessionID,
		PreSuspectStatus: &preSuspect,
	}); err != nil {
		return fmt.Errorf("sessionactor: update sandbox status to suspect: %w", err)
	}
	a.recordWatchdogActivation(ctx, watchdog, gap.Seconds())
	return a.armTimer(ctx, tx, TimerTerminalGrace, now.Add(a.timeouts.TerminalGracePeriod))
}

// rederiveSessionStatusUnchanged re-derives and persists session status
// from the CURRENT (unmodified by this call) turn history -- used by
// handleTerminalGraceTimer, which changes the SANDBOX's status but no
// turn's. Since domain/session.DeriveStatus is a pure function of turn
// history alone, and that history hasn't changed, its result here is
// necessarily identical to whatever it was the last time it was
// legitimately derived and persisted (by whichever call DID change a
// turn's status -- in this Step, only handleTurnDeadlineTimer). turns
// carry no failure_reason column by design (see turn.FailureReason's own
// doc: only the (from, trigger) pair that PRODUCED a terminal transition
// knows the reason, and only the caller making that transition can derive
// it) -- so the last turn's failure reason, if it is terminal-failed, is
// read back from the session row's OWN currently-stored failure_reason
// rather than reconstructed from a trigger this call was never given.
func (a *Actor) rederiveSessionStatusUnchanged(ctx context.Context, tx pgx.Tx) error {
	sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: get session: %w", err)
	}
	turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: list turns: %w", err)
	}

	var currentReason turn.FailureReason
	if sessionRow.FailureReason != nil {
		currentReason = turn.FailureReason(*sessionRow.FailureReason)
	}

	return a.persistDerivedSessionStatus(ctx, tx, summariesForRederive(turns, currentReason))
}

// persistDerivedSessionStatus runs domain/session.DeriveStatus over
// summaries and persists the result -- the ONLY way this package ever
// writes sessions.status/failure_reason (§11: "every state transition
// goes through the machine's transition table").
func (a *Actor) persistDerivedSessionStatus(ctx context.Context, tx pgx.Tx, summaries []turn.Summary) error {
	derived := session.DeriveStatus(summaries)

	var failureReason *sqlcgen.SessionFailureReason
	if derived.FailureReason != "" {
		v := sqlcgen.SessionFailureReason(derived.FailureReason)
		failureReason = &v
	}

	if _, err := a.stores.session.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{
		ID:            a.sessionID,
		Status:        sqlcgen.SessionStatus(derived.Status),
		FailureReason: failureReason,
	}); err != nil {
		return fmt.Errorf("sessionactor: update session status: %w", err)
	}
	return nil
}

// armTimer upserts (arms or re-arms) the named timer to fire at fireAt.
func (a *Actor) armTimer(ctx context.Context, tx pgx.Tx, name string, fireAt time.Time) error {
	_, err := a.stores.timer.WithTx(tx).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: a.sessionID,
		Name:      name,
		FiresAt:   pgtype.Timestamptz{Time: fireAt, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("sessionactor: arm timer %q: %w", name, err)
	}
	return nil
}

// deleteTimer removes the named timer -- see this file's top comment for
// why every handler must end in either this or armTimer.
func (a *Actor) deleteTimer(ctx context.Context, tx pgx.Tx, name string) error {
	if err := a.stores.timer.WithTx(tx).Delete(ctx, sqlcgen.DeleteSessionTimerParams{
		SessionID: a.sessionID,
		Name:      name,
	}); err != nil {
		return fmt.Errorf("sessionactor: delete timer %q: %w", name, err)
	}
	return nil
}

// appendEvent inserts a session event row inside tx (§2: appended events
// always commit in the same transaction as the state change they
// describe). Unlike appendRawEvent (actor.go), this caller's events are
// always server-SYNTHESIZED (a fabricated execution_complete on timeout/
// failure, a "warning" event) with no real wire messageId of their own --
// so a fresh one is minted here internally (github.com/google/uuid, an
// already-established import in this package, see dispatch.go/pushpr.go's
// own precedent) purely to satisfy events.message_id's NOT NULL/unique
// constraint; no caller of appendEvent needs to supply or know about it.
func (a *Actor) appendEvent(ctx context.Context, tx pgx.Tx, eventType string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sessionactor: marshal %s event payload: %w", eventType, err)
	}
	row, err := a.stores.event.WithTx(tx).Create(ctx, sqlcgen.CreateEventParams{
		SessionID: a.sessionID,
		Type:      eventType,
		MessageID: uuid.NewString(),
		Payload:   raw,
	})
	if err != nil {
		return fmt.Errorf("sessionactor: append %s event: %w", eventType, err)
	}
	// Queue for broadcast AFTER commit -- see actor.go's transact/
	// broadcastPending doc comments for the full commit-then-broadcast,
	// discard-on-rollback ordering this is part of. A freshly-minted
	// messageId can never collide with an already-persisted row, so
	// row.Inserted is always true here in practice -- checked anyway
	// (rather than assumed) for the same reason appendRawEvent does: this
	// is the one and only gate that decides broadcast delivery.
	if row.Inserted {
		a.pendingBroadcast = append(a.pendingBroadcast, raw)
	}
	return nil
}

// pgTimeOrZero converts a nullable Postgres timestamp to time.Time,
// time.Time{} (the zero value) when it is NULL -- matching how the
// domain decision functions in internal/domain/sandbox already represent
// "no signal yet" (e.g. InactivityState.LastActivity's own doc: "Zero
// (time.Time{}) means never active").
func pgTimeOrZero(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// isConnectingPhase reports whether status is one of the three boot-phase
// states domain/sandbox.EvaluateConnectingTimeout actually evaluates
// (Spawning, Connecting, Booting) -- used here to decide whether
// connecting_deadline still applies at all, distinct from
// EvaluateConnectingTimeout's own IsTimedOut (which only says whether the
// budget has elapsed, not whether the phase is still relevant).
func isConnectingPhase(status sandbox.State) bool {
	switch status {
	case sandbox.StateSpawning, sandbox.StateConnecting, sandbox.StateBooting:
		return true
	default:
		return false
	}
}

// findProcessingTurn returns the turn currently in StateProcessing, if
// any. At most one such turn can exist per session (domain/turn's own
// single-in-flight invariant, enforced additionally at the DB level by
// turns_one_processing_per_session).
func findProcessingTurn(turns []sqlcgen.Turn) (sqlcgen.Turn, bool) {
	for _, t := range turns {
		if turn.State(t.Status) == turn.StateProcessing {
			return t, true
		}
	}
	return sqlcgen.Turn{}, false
}

// anyTurnProcessing reports whether any turn in turns is currently
// StateProcessing -- the IsProcessing input
// domain/sandbox.EvaluateInactivityTimeout needs.
func anyTurnProcessing(turns []sqlcgen.Turn) bool {
	_, ok := findProcessingTurn(turns)
	return ok
}

// summariesWithOverride builds the []turn.Summary domain/session.
// DeriveStatus needs from stored turn rows, substituting overrideStatus/
// overrideReason for the one row whose ID matches overrideID -- used
// right after this actor itself just transitioned that one turn (so its
// new status/reason is known directly, not yet reflected in the turns
// slice fetched at the top of the same transaction).
func summariesWithOverride(turns []sqlcgen.Turn, overrideID pgtype.UUID, overrideStatus turn.State, overrideReason turn.FailureReason) []turn.Summary {
	out := make([]turn.Summary, len(turns))
	for i, t := range turns {
		if t.ID == overrideID {
			out[i] = turn.Summary{Status: overrideStatus, FailureReason: overrideReason}
			continue
		}
		out[i] = storedTurnSummary(t)
	}
	return out
}

// storedTurnSummary is one stored turn row as DeriveStatus reads it: its
// status, and the failure reason that status alone implies
// (turn.ImpliedFailureReason -- a cancelled turn is always cancelled). A
// failed row's reason is not in the row, so it stays empty, exactly as
// before. Since a person's stop can cancel queued turns before an older
// one ends (stop.go), the last turn of a session can be one cancelled
// earlier, and its reason must not be lost when the older one ends. A
// turn with an end reason of its own -- one ended context_moved never ran
// (technical plan §24.9) -- is marked Ignored, so DeriveStatus reads the
// session as if it were not there.
//
// A stated limit: when the turn DeriveStatus then reads last is a failed
// one stored earlier, its reason is not in its row, and the session reads
// failed with no reason recorded. That arises only when an ignored turn is
// the newest, and the turn before it failed: the reason an earlier
// derivation wrote was replaced by active while the ignored turn waited.
func storedTurnSummary(t sqlcgen.Turn) turn.Summary {
	state := turn.State(t.Status)
	reason, _ := turn.ImpliedFailureReason(state)
	return turn.Summary{Status: state, FailureReason: reason, Ignored: turn.IgnoredForEndReason(t.EndReason)}
}

// summariesForRederive builds the []turn.Summary domain/session.
// DeriveStatus needs from stored turn rows, for a re-derivation NOT
// accompanied by a turn status change of this call's own (see
// rederiveSessionStatusUnchanged's doc for why currentFailureReason,
// applied only to the last entry DeriveStatus reads, is the correct source
// of truth here). A turn with an end reason is Ignored (storedTurnSummary),
// so the reason goes to the last turn that is not.
func summariesForRederive(turns []sqlcgen.Turn, currentFailureReason turn.FailureReason) []turn.Summary {
	out := make([]turn.Summary, len(turns))
	for i, t := range turns {
		out[i] = turn.Summary{Status: turn.State(t.Status), Ignored: turn.IgnoredForEndReason(t.EndReason)}
	}
	for i := len(out) - 1; i >= 0; i-- {
		if !out[i].Ignored {
			out[i].FailureReason = currentFailureReason
			break
		}
	}
	return out
}
