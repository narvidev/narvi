// This file (stop.go) is the actor's half of a person's stop (technical plan
// §3.3, POST /api/sessions/{sessionID}/stop). The request itself is data,
// written by the REST transaction under this session's actor-epoch lock:
// turns.stop_requested_at on every turn open at that instant,
// sessions.stop_requested_at, and the `stop` timer upserted to now. Every
// state transition stays here, and each is §3.3's existing cancel edge --
// there is no new turn state, trigger or session status:
//
//   - A flagged pending turn is cancelled (TriggerCancel) with the
//     synthetic execution_complete §3.3 requires, by the stop timer or --
//     whichever runs first -- by planDispatch's gate, so it never
//     dispatches.
//   - A flagged turn in flight is sent the sandbox `stop` command after the
//     commit, and the timer is re-armed for the end of its StopGrace. The
//     agent's execution_complete{cancelled} then takes the ordinary path
//     (pushpr.go's completeProcessingTurn: cancelled, nothing pushed), and
//     the sandbox is kept. If the timer fires again with the turn still in
//     flight -- the agent stayed silent, or there was no sandbox to tell --
//     the turn is cancelled here with a synthetic event, as turn_deadline
//     ends one, and the sandbox generation it was dispatched to is retired
//     (retireStoppedGen): nothing more is dispatched to that gen, its
//     provider object is stopped, and the next turn runs on a new gen, so
//     the gen fence (handleSandboxEvent) drops whatever the stopped work
//     still reports. An execution_complete names a gen, never a turn:
//     without the retirement, the stopped work's late one would be booked
//     against the next turn dispatched to the same gen.
//   - The retirement waits for a delivery (deliveryHold), the cancel never
//     does: while the gen it would retire is still delivering a completed
//     turn's push and pull request (sandboxes.pr_delivery_started_at,
//     within MCPStatusDeliveryWindow of its start), stopping its sandbox
//     would kill that delivery. The stopped turn is cancelled all the same,
//     and the retirement is left owed (sandboxes.stop_retire_gen): nothing
//     is dispatched to that gen and no snapshot of it is started meanwhile
//     (planDispatch, triggerSnapshotBestEffort), since the agent may still
//     be running the stopped work there -- it runs a prompt while it
//     handles a push, and a gen that takes no snapshot (a Docker-required
//     session's) is sent the next turn's prompt before the previous turn's
//     push. The stopped work's own late execution_complete then finds
//     nothing processing, completes nothing and pushes nothing. The timer
//     is re-armed to look again every StopGrace, never past the window's
//     end, and the first fire after the delivery ends, or after its window
//     has run, retires the gen and dispatches what is queued to a new one.
//   - While the session's own request stands, the session's work-creating
//     timers (ClassifyTimer: TimerWorkCreatesTurn) armed at or before the
//     request -- its latest instant, which a repeated request moves forward
//     -- are deleted; one armed after it is new input, and stays.
//   - By the same rule, a workflow advance the autonomy freeze held at or
//     before the request (technical plan §40.2) is dropped, and its run
//     ends cancelled (cancelHeldWorkflowAdvancesForStop). The stop request
//     itself does this, in its own transaction (httpapi's stop route), so
//     the advance is never applied, even after a person resumes the
//     session; this handler catches a stop recorded without it, by a
//     replica of the previous release during a rolling deploy.
//
// A turn created after the request carries no flag and runs normally. A
// person's next act that sets the session going again also clears the
// session's flag: the next turn a person creates (httpapi's
// createTurnLocked), the approval of its plan (DecidePlanOnTx), or a
// decision to approve or revise a workflow step awaiting it
// (DecideWorkflowStep). Children are the REST handler's to reach: it
// applies the same request to each descendant, whose own actor runs this
// same file.

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
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// stopSignal is what handleStopTimer's transaction hands back when a
// flagged turn in flight should be told to stop: the sandbox gen the
// command is fenced with. The send itself happens after the commit, outside
// any transaction, like every other network call this package makes.
type stopSignal struct {
	gen int
}

// retiredGen is what handleStopTimer's transaction hands back when it
// retired the sandbox generation a stopped turn was still running on: the
// provider object stopSandboxOfRetiredGen asks the provider to stop after
// the commit ("" when the spawn that created it never recorded one).
type retiredGen struct {
	gen        int
	providerID string
}

// handleStopTimer implements the `stop` named timer -- see this file's own
// top comment. Ends, like every handler, by re-arming or deleting the timer
// that fired: re-armed only while a flagged turn is still in flight within
// its grace, or while the retirement of a stopped turn's gen waits for a
// delivery (deliveryHold).
func (a *Actor) handleStopTimer(ctx context.Context) error {
	var signal *stopSignal
	var retired *retiredGen
	var cancelled bool

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		signal, retired, cancelled = nil, nil, false
		now := time.Now()

		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session: %w", err)
		}

		flagged, err := a.stores.turn.WithTx(tx).ListStopRequestedOpen(ctx, a.sessionID, a.timeouts.StopGrace)
		if err != nil {
			return fmt.Errorf("sessionactor: list stop-requested turns: %w", err)
		}

		var toCancel []pgtype.UUID
		var rearmAt time.Time
		for _, f := range flagged {
			if turn.State(f.Status) == turn.StatePending || f.GraceElapsed {
				toCancel = append(toCancel, f.ID)
				continue
			}
			// Dispatched or processing, inside its grace: told to stop, and
			// looked at again once the grace -- measured from its own flag,
			// on the database's clock -- has run.
			if ends := f.StopRequestedAt.Time.Add(a.timeouts.StopGrace); rearmAt.IsZero() || ends.Before(rearmAt) {
				rearmAt = ends
			}
		}

		var unconfirmed []sqlcgen.Turn
		if len(toCancel) > 0 {
			if unconfirmed, err = a.cancelStoppedTurns(ctx, tx, sessionRow, toCancel, now); err != nil {
				return err
			}
			cancelled = true
		}

		// A turn in flight cancelled here ended with no word from the agent,
		// which may still be running it: its sandbox gen is retired in this
		// same transaction, before anything can be dispatched to it -- or,
		// while that gen still delivers a completed turn's push and pull
		// request, the retirement is left owed, and so is one an earlier
		// fire left owed.
		var heldUntil time.Time
		if retired, heldUntil, err = a.retireOrHold(ctx, tx, unconfirmed); err != nil {
			return err
		}
		if !heldUntil.IsZero() && (rearmAt.IsZero() || heldUntil.Before(rearmAt)) {
			rearmAt = heldUntil
		}

		if err := a.disarmWorkCreatingTimers(ctx, tx, sessionRow.StopRequestedAt); err != nil {
			return err
		}
		if err := a.dropOwedReviewRequestsForStop(ctx, tx, sessionRow); err != nil {
			return err
		}
		if err := a.cancelHeldWorkflowAdvancesForStop(ctx, tx, sessionRow); err != nil {
			return err
		}

		if rearmAt.IsZero() {
			return a.deleteTimer(ctx, tx, TimerStop)
		}
		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		switch {
		case err == nil:
			signal = &stopSignal{gen: int(sandboxRow.Gen)}
		case errors.Is(err, pgx.ErrNoRows):
			// No sandbox to tell: the grace alone ends the turn.
		default:
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}
		return a.armTimer(ctx, tx, TimerStop, rearmAt)
	})
	if err != nil {
		return err
	}

	if retired != nil {
		a.stopSandboxOfRetiredGen(ctx, *retired)
	}
	if signal != nil {
		a.sendStop(*signal)
	}
	if cancelled || retired != nil {
		// §3.3: "on terminal event ... dispatch next pending" -- a turn
		// created after the stop, which carries no flag, runs now: on the
		// same sandbox when the stop cancelled only queued turns, on a new
		// gen (a restore or a respawn) when it retired the old one, and not
		// yet while that retirement is owed (planDispatch holds it).
		if dispatchErr := a.handleEnsureDispatched(ctx); dispatchErr != nil {
			a.logger.Warn("sessionactor: ensure-dispatched after stop failed", "error", dispatchErr)
		}
	}
	return nil
}

// retireOrHold retires the sandbox generation a stopped turn was cancelled
// on with no word from the agent -- one of unconfirmed's, or the one an
// earlier fire left owed (sandboxes.stop_retire_gen) -- or, while that gen
// still delivers a completed turn's push and pull request, leaves the
// retirement owed and returns when to look again (deliveryHold). The owed
// gen is recorded only while the row is at it; nothing is dispatched to it
// and no snapshot of it is started meanwhile (planDispatch,
// triggerSnapshotBestEffort). Once not held, the record is cleared,
// whether or not there is still something to retire (retireStoppedGen's
// own conditions). Returns (nil, zero, nil) when nothing is owed.
func (a *Actor) retireOrHold(ctx context.Context, tx pgx.Tx, unconfirmed []sqlcgen.Turn) (*retiredGen, time.Time, error) {
	row, err := a.stores.sandbox.WithTx(tx).PRDelivery(ctx, a.sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No sandbox: nothing to retire, nothing to wait for.
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, fmt.Errorf("sessionactor: read sandbox push/PR delivery: %w", err)
	}
	gens := make([]*int32, 0, len(unconfirmed)+1)
	for _, t := range unconfirmed {
		gens = append(gens, t.DispatchedSandboxGen)
	}
	if row.StopRetireGen != nil {
		gens = append(gens, row.StopRetireGen)
	}
	if len(gens) == 0 {
		return nil, time.Time{}, nil
	}

	if heldUntil := a.deliveryHold(row, gens); !heldUntil.IsZero() {
		if row.StopRetireGen == nil || *row.StopRetireGen != row.Gen {
			if err := a.stores.sandbox.WithTx(tx).SetStopRetireGen(ctx, a.sessionID, row.Gen); err != nil {
				return nil, time.Time{}, fmt.Errorf("sessionactor: record a stopped turn's owed gen retirement: %w", err)
			}
		}
		return nil, heldUntil, nil
	}

	if row.StopRetireGen != nil {
		if err := a.stores.sandbox.WithTx(tx).ClearStopRetireGen(ctx, a.sessionID); err != nil {
			return nil, time.Time{}, fmt.Errorf("sessionactor: clear a stopped turn's owed gen retirement: %w", err)
		}
	}
	retired, err := a.retireStoppedGen(ctx, tx, gens)
	return retired, time.Time{}, err
}

// retirementOwed reports whether row's current gen owes a person's stop its
// retirement (sandboxes.stop_retire_gen, retireOrHold): nothing is
// dispatched to that gen, and no snapshot of it is started, until the stop
// timer retires it.
func retirementOwed(row sqlcgen.Sandbox) bool {
	return row.StopRetireGen != nil && *row.StopRetireGen == row.Gen
}

// cancelStoppedTurns moves each turn of ids -- every one flagged by a
// person's stop and still open -- to cancelled through the machine's own
// table (TriggerCancel, legal from pending, dispatched and processing), and
// writes, in tx, everything a terminal write owes for each: the workflow
// hook (which ends a tracked attempt's run cancelled without consulting
// NextStep), the synthetic execution_complete §3.3 requires since no real
// one will arrive for it, and the outbox notice. turn_deadline is deleted
// when a turn in flight is cancelled, since it was that turn's. The session
// status is re-derived once, over every cancel. A turn no longer open (it
// ended some other way first) is skipped. Returns the turns it cancelled
// that were in flight (dispatched or processing), as they stood before the
// cancel: for handleStopTimer, each is a turn the agent never confirmed
// ending. Shared by handleStopTimer and planDispatch's gate, which cancels
// only pending turns.
func (a *Actor) cancelStoppedTurns(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, ids []pgtype.UUID, now time.Time) ([]sqlcgen.Turn, error) {
	turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: list turns: %w", err)
	}

	var inFlight []sqlcgen.Turn
	overrides := make(map[pgtype.UUID]turn.Summary, len(ids))
	for _, id := range ids {
		target, ok := findTurnByID(turns, id)
		if !ok {
			continue
		}
		from := turn.State(target.Status)
		to, err := turn.Transition(from, turn.TriggerCancel)
		if err != nil {
			// Already terminal: it ended some other way first.
			a.logger.Info("sessionactor: stopped turn no longer open; nothing to cancel",
				"turn_id", id.String(), "status", target.Status)
			continue
		}

		if _, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
			ID:          id,
			Status:      sqlcgen.TurnStatus(to),
			CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return nil, fmt.Errorf("sessionactor: update turn status: %w", err)
		}

		workflowengine.OnTurnCompleted(ctx, a.workflowDeps(tx), sessionRow, id, turn.TriggerCancel)

		if turn.RequiresSyntheticExecutionComplete(turn.TriggerCancel) {
			if err := a.appendEvent(ctx, tx, "execution_complete", syntheticExecutionComplete(id, from, "stopped")); err != nil {
				return nil, err
			}
		}

		failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerCancel)
		if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, turn.TriggerCancel, failureReason, target, nil, ""); err != nil {
			return nil, err
		}
		if from != turn.StatePending {
			if err := a.deleteTimer(ctx, tx, TimerTurnDeadline); err != nil {
				return nil, err
			}
			inFlight = append(inFlight, target)
		}
		overrides[id] = turn.Summary{Status: to, FailureReason: failureReason}
		a.logger.Info("sessionactor: turn cancelled by a stop", "turn_id", id.String(), "from", string(from))
	}

	if len(overrides) == 0 {
		return nil, nil
	}
	if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverrides(turns, overrides)); err != nil {
		return nil, err
	}
	return inFlight, nil
}

// retireStoppedGen retires the sandbox generation a stopped turn was still
// running on when handleStopTimer cancelled it with no word from the agent
// -- dispatched, the dispatched_sandbox_gen of each such turn (retireOrHold).
// The agent may still be running that work -- the one `stop` it was sent
// found no live connection, or its abort outlasted the grace -- and its
// eventual execution_complete names a gen, never a turn (pushpr.go's
// completeProcessingTurn books it against whichever turn is processing).
// Left live, the sandbox would take the next turn at that same gen, and the
// stopped work's late end would complete or cancel it; the agent would also
// be running both at once. Retired, it takes nothing more: the next
// dispatch finds a dead sandbox and restores or respawns it under a new gen
// (planDispatch, tryPlanSpawn), and handleSandboxEvent's gen fence drops
// every event the old gen still sends, while wshub refuses its reconnect
// (410, then 403 once the gen moves on). The transition is the machine's
// own two edges, each validated by sandbox.Transition and written in tx:
// the live state to suspect, then suspect to stopped by
// TriggerGraceExpired, stopped being the outcome it reserves for an
// explicit stop. The stop's own StopGrace stood in for terminal_grace, so
// none is armed, and one a watchdog armed earlier is deleted.
//
// Returns nil when there is nothing to retire: no sandbox, one already
// dead, or one whose gen has moved past every one of dispatched -- a
// respawn since then already fenced that gen off, and a flagged turn is
// never re-sent (planReenqueueOrRespawn). A turn with no
// dispatched_sandbox_gen recorded (nil) is taken to have run on the
// current gen.
func (a *Actor) retireStoppedGen(ctx context.Context, tx pgx.Tx, dispatched []*int32) (*retiredGen, error) {
	row, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionactor: get sandbox: %w", err)
	}
	from := sandbox.State(row.Status)
	if sandbox.IsDeadSandboxStatus(from) || !ranOnGen(dispatched, int(row.Gen)) {
		return nil, nil
	}

	if from != sandbox.StateSuspect {
		if _, err := sandbox.Transition(from, int(row.Gen), sandbox.SuspectTrigger()); err != nil {
			return nil, fmt.Errorf("sessionactor: retire stopped sandbox gen: %w", err)
		}
		preSuspect := sqlcgen.SandboxStatus(from)
		if _, err := a.sandboxWrites(tx).UpdateStatusToSuspect(ctx, sqlcgen.UpdateSandboxStatusToSuspectParams{
			SessionID:        a.sessionID,
			PreSuspectStatus: &preSuspect,
		}); err != nil {
			return nil, fmt.Errorf("sessionactor: update sandbox status to suspect: %w", err)
		}
	}
	to, err := sandbox.Transition(sandbox.StateSuspect, int(row.Gen), sandbox.GraceExpiredTrigger(sandbox.StateStopped))
	if err != nil {
		return nil, fmt.Errorf("sessionactor: retire stopped sandbox gen: %w", err)
	}
	if _, err := a.sandboxWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: a.sessionID,
		Status:    sqlcgen.SandboxStatus(to),
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: update sandbox status to stopped: %w", err)
	}
	if err := a.deleteTimer(ctx, tx, TimerTerminalGrace); err != nil {
		return nil, err
	}

	providerID := ""
	if row.ProviderID != nil {
		providerID = *row.ProviderID
	}
	a.logger.Warn("sessionactor: a stopped turn's grace ended with no word from the agent; its sandbox gen is retired, and the next turn runs on a new one",
		"gen", row.Gen, "from", string(from), "provider_id", providerID)
	return &retiredGen{gen: int(row.Gen), providerID: providerID}, nil
}

// ranOnGen reports whether any of dispatched -- the dispatched_sandbox_gen
// of each of a set of turns -- is gen, or is not recorded (nil).
func ranOnGen(dispatched []*int32, gen int) bool {
	for _, g := range dispatched {
		if g == nil || int(*g) == gen {
			return true
		}
	}
	return false
}

// deliveryHold decides whether retiring the sandbox gen of dispatched --
// the dispatched_sandbox_gen of each turn a stop cancelled with no word
// from the agent, and of the one an earlier fire left owed -- waits, and
// until when. row is the sandbox's delivery read (GetSandboxPRDelivery).
// Stopping that gen's provider object would kill a completed turn's push
// and pull request it is still delivering: §3.3 keeps those, as that
// turn's result. The turns themselves are cancelled either way.
//
// Returns the zero time -- retire now -- when nothing would be retired (a
// sandbox already dead, or one whose gen has moved past every one of
// dispatched: retireStoppedGen's own conditions), or when no delivery is
// under way: none stamped (sandboxes.pr_delivery_started_at, cleared once
// the pull request is created, the push fails or the push cannot be sent),
// or one stamped MCPStatusDeliveryWindow ago or more,
// session.PRDeliveryOpen's bound, which the session's status also reads.
// Otherwise returns when to look again: StopGrace from now, so a delivery
// that ends is noticed within one grace, and never later than the window's
// end, so a push that never reports back holds the retirement no longer
// than it holds the session's status. Every instant is on the database's
// clock.
func (a *Actor) deliveryHold(row sqlcgen.GetSandboxPRDeliveryRow, dispatched []*int32) time.Time {
	if sandbox.IsDeadSandboxStatus(sandbox.State(row.Status)) || !ranOnGen(dispatched, int(row.Gen)) || !row.PrDeliveryStartedAt.Valid {
		return time.Time{}
	}
	started, observed := row.PrDeliveryStartedAt.Time, row.ObservedAt.Time
	if !session.PRDeliveryOpen(started, observed, a.timeouts.MCPStatusDeliveryWindow) {
		return time.Time{}
	}
	until := started.Add(a.timeouts.MCPStatusDeliveryWindow)
	if next := observed.Add(a.timeouts.StopGrace); next.Before(until) {
		until = next
	}
	a.logger.Info("sessionactor: a stopped turn was cancelled while its sandbox gen still delivers a completed turn's push and pull request; the gen's retirement, and every dispatch to it, wait for that delivery",
		"gen", row.Gen, "delivery_started_at", started, "next_look", until)
	return until
}

// stopSandboxOfRetiredGen asks the provider, after the commit, to stop the
// object behind a gen retireStoppedGen retired, so the work the person
// stopped ends rather than running on unseen. Best effort, like every
// provider call here: with no provider, one without an explicit stop, no
// provider object recorded, or a failed call, the reconciler stops it -- a
// stopped row no longer lists its provider object among those expected
// alive (SandboxStore.ListLiveProviderIDs), so it is reaped as an orphan
// once ReconcilerOrphanConfirmationPeriod has run.
func (a *Actor) stopSandboxOfRetiredGen(ctx context.Context, r retiredGen) {
	switch {
	case a.provider == nil:
		a.logger.Warn("sessionactor: retired sandbox not stopped here: no SandboxProvider is configured; the reconciler reaps it", "gen", r.gen)
		return
	case r.providerID == "":
		a.logger.Warn("sessionactor: retired sandbox not stopped here: no provider object was recorded for it; the reconciler reaps any it finds", "gen", r.gen)
		return
	case !a.provider.Capabilities().ExplicitStop:
		a.logger.Warn("sessionactor: retired sandbox not stopped here: the provider has no explicit stop; the reconciler reaps it", "gen", r.gen, "provider_id", r.providerID)
		return
	}
	if err := a.provider.StopSandbox(ctx, ports.SandboxRef{ProviderID: r.providerID}); err != nil {
		a.logger.Warn("sessionactor: stop of a retired sandbox failed; the reconciler reaps it", "gen", r.gen, "provider_id", r.providerID, "error", err)
		return
	}
	a.logger.Info("sessionactor: retired sandbox stopped at the provider", "gen", r.gen, "provider_id", r.providerID)
}

// stopFlaggedPendingTurnIDs returns the pending turns of turns a person's
// stop flagged -- planDispatch's gate cancels them instead of dispatching.
func stopFlaggedPendingTurnIDs(turns []sqlcgen.Turn) []pgtype.UUID {
	var ids []pgtype.UUID
	for _, t := range turns {
		if t.StopRequestedAt.Valid && turn.State(t.Status) == turn.StatePending {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// disarmWorkCreatingTimers deletes every timer of the session whose firing
// creates a turn with no new input (ClassifyTimer's TimerWorkCreatesTurn --
// today the re-review debounce and the owed review request's timer, whose
// requests dropOwedReviewRequestsForStop drops by the same rule) and that
// was armed at or before
// requestedAt, the session's standing stop request: its latest request's
// instant, which a repeated request moves forward (RequestSessionStop), so
// a debounce a push armed between two requests goes with the second.
// session_timers.created_at and sessions.stop_requested_at are both the
// database's now(), so the comparison involves no replica's clock. A timer
// armed after the request -- a push after the stop -- is new input, and
// stays (technical plan §3.3's effects table), however late this timer
// fires. A NULL requestedAt -- a person resumed the session -- deletes
// nothing: a timer armed since is new work of theirs.
//
// A re-arm updates a timer's row in place and keeps its created_at
// (UpsertSessionTimer), so a push that lands after the request but before
// this timer's first fire, while a debounce armed before the request still
// exists, loses that debounce with it. The route wakes the actor as it
// answers, so that window is normally the actor's own queue.
//
// The re-review debounce's hold and the wake-up a turn's end owes it
// (technical plan §24.9: holdReviewRetrigger,
// wakeReviewRetriggerIfTurnEnded) keep to the same rule, by created_at,
// which no re-arm moves. Both only move a debounce that exists and never
// insert one, so a debounce deleted here stays deleted: a firing claimed
// before this timer ran and handled after it holds without re-creating it,
// and the cancelled turn's end wakes nothing. Neither moves one armed at
// or before the standing request: the hold drops it, as this would, and
// the wake-up leaves it, so a turn the dispatch gate cancels before this
// timer runs cannot launch the review this timer is about to delete. A
// firing that finds no hold to keep -- the session's turns all ended -- is
// not covered: it inserts its review as it always did, the stop
// notwithstanding.
//
// A kind ClassifyTimer does not know is left armed: this binary cannot tell
// what it does, and handleTimerFired leaves one armed too, until the time
// since its last arm reaches UnknownTimerDeleteAfter (technical plan §2).
func (a *Actor) disarmWorkCreatingTimers(ctx context.Context, tx pgx.Tx, requestedAt pgtype.Timestamptz) error {
	if !requestedAt.Valid {
		return nil
	}
	timers, err := a.stores.timer.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: list session timers: %w", err)
	}
	for _, t := range timers {
		work, ok := ClassifyTimer(t.Name)
		if !ok || work != TimerWorkCreatesTurn {
			continue
		}
		if t.CreatedAt.Time.After(requestedAt.Time) {
			a.logger.Info("sessionactor: work-creating timer armed after the stop kept", "timer", t.Name)
			continue
		}
		if err := a.deleteTimer(ctx, tx, t.Name); err != nil {
			return err
		}
		a.logger.Info("sessionactor: work-creating timer disarmed by a stop", "timer", t.Name)
	}
	return nil
}

// dropOwedReviewRequestsForStop drops every review request the session
// owed when a person's standing stop was made (technical plan §24.9,
// §3.3): the owed_review_requests rows created at or before
// sessionRow.StopRequestedAt, the rule disarmWorkCreatingTimers deletes
// the owed_review_request timer by -- the database's clock on both sides.
// A stop is the person's answer, so none of them is re-run and no
// requester is told; each moved turn's workflow run, whose attempt the
// re-run would have taken over, ends cancelled as a stopped run ends
// (withdrawOwedRequestWorkflow), and the hold's owed term is released
// (wakeHeldReviewRetrigger, under the stop's own rule). A request owed
// after the stop was made by a turn created after it -- the dispatch's
// stop gate cancels every flagged one before it is checked -- so it is new
// work and stays, and the owed_review_request timer the disarm may have
// deleted with the older requests is armed again, due at once, for it.
// Run by the stop timer after its disarm, and by the owed timer's consumer
// first, so a request the stop predates is never re-run whichever fires
// first. A NULL stop request -- none standing, or a person resumed the
// session -- drops nothing.
func (a *Actor) dropOwedReviewRequestsForStop(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) error {
	if !sessionRow.StopRequestedAt.Valid {
		return nil
	}
	owed := a.stores.owedReviewRequest.WithTx(tx)
	dropped, err := owed.DeleteForStop(ctx, a.sessionID, sessionRow.StopRequestedAt)
	if err != nil {
		return fmt.Errorf("sessionactor: drop the review requests a stop predates: %w", err)
	}
	if len(dropped) == 0 {
		return nil
	}
	for _, d := range dropped {
		a.withdrawOwedRequestWorkflow(ctx, tx, d.MovedTurnID)
		a.logger.Info("sessionactor: an owed review request dropped by a person's stop",
			"owed_id", d.ID.String(), "moved_turn_id", d.MovedTurnID.String(), "request_trigger", d.Trigger)
	}
	if err := a.wakeHeldReviewRetrigger(ctx, tx, "a stop dropped the owed review requests"); err != nil {
		return err
	}
	left, err := owed.Exists(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: read whether a review request is still owed: %w", err)
	}
	if !left {
		return nil
	}
	if err := a.stores.timer.WithTx(tx).ArmOwedReviewRequest(ctx, a.sessionID); err != nil {
		return fmt.Errorf("sessionactor: arm the owed review request timer: %w", err)
	}
	return nil
}

// cancelHeldWorkflowAdvancesForStop drops every workflow advance the
// autonomy freeze held on this session at or before the person's standing
// stop (technical plan §40.2, §3.3) -- the rule disarmWorkCreatingTimers
// deletes by, the database's clock on both sides -- and ends each held
// run cancelled (workflowengine.CancelHeldAdvancesForStop). A stop is the
// person's answer, so the held advance is never applied, and nobody is
// told. The stop route drops them in the request's own transaction; this,
// run by the stop timer after the owed requests are dropped, catches a stop
// a replica of the previous release recorded without dropping them, as
// does the releaser, which takes the same actor-epoch lock and cancels the
// run itself when it reaches such a hold first. A NULL stop request -- none
// standing, or a person resumed the session -- drops nothing.
func (a *Actor) cancelHeldWorkflowAdvancesForStop(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) error {
	if err := workflowengine.CancelHeldAdvancesForStop(ctx, a.stores.workflow.WithTx(tx), a.sessionID, sessionRow.StopRequestedAt); err != nil {
		return fmt.Errorf("sessionactor: %w", err)
	}
	return nil
}

// sendStop sends the sandbox `stop` command (sandboxws.Stop) fenced with
// gen, best effort: no live connection, or a failed write, is logged and
// left to the grace, which cancels the turn anyway and retires its gen. The
// agent aborts whatever turn it is running; a repeated stop finds it
// aborted already.
func (a *Actor) sendStop(sig stopSignal) {
	if a.commander == nil {
		a.logger.Warn("sessionactor: stop would be sent but no SandboxCommander is configured; the grace will cancel the turn")
		return
	}
	payload, err := json.Marshal(sandboxws.Stop{
		Type:      "stop",
		MessageId: uuid.NewString(),
		SessionId: a.sessionID.String(),
		Gen:       sig.gen,
	})
	if err != nil {
		a.logger.Error("sessionactor: marshal stop command failed", "error", err)
		return
	}
	if err := a.commander.SendCommand(a.sessionID.String(), payload); err != nil {
		if errors.Is(err, ports.ErrNoLiveSandboxConnection) {
			a.logger.Info("sessionactor: stop not sent: no live sandbox connection; the grace will cancel the turn", "gen", sig.gen)
			return
		}
		a.logger.Warn("sessionactor: send stop command failed; the grace will cancel the turn", "gen", sig.gen, "error", err)
		return
	}
	a.logger.Info("sessionactor: stop sent to the sandbox", "gen", sig.gen)
}

// summariesWithOverrides is summariesWithOverride (timerfired.go) for
// several turns at once: each turn whose ID is in overrides takes that
// status and failure reason.
func summariesWithOverrides(turns []sqlcgen.Turn, overrides map[pgtype.UUID]turn.Summary) []turn.Summary {
	out := make([]turn.Summary, len(turns))
	for i, t := range turns {
		if o, ok := overrides[t.ID]; ok {
			out[i] = o
			continue
		}
		out[i] = storedTurnSummary(t)
	}
	return out
}
