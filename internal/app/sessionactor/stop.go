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
//   - While the session's own request stands, the session's work-creating
//     timers (ClassifyTimer: TimerWorkCreatesTurn) armed at or before the
//     request are deleted; one armed after it is new input, and stays.
//
// A turn created after the request carries no flag and runs normally; the
// next turn a person creates also clears the session's flag (httpapi's
// createTurnLocked, DecidePlanOnTx). Children are the REST handler's to
// reach: it applies the same request to each descendant, whose own actor
// runs this same file.

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
// its grace.
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
		var graceEnds time.Time
		for _, f := range flagged {
			if turn.State(f.Status) == turn.StatePending || f.GraceElapsed {
				toCancel = append(toCancel, f.ID)
				continue
			}
			// Dispatched or processing, inside its grace: told to stop, and
			// looked at again once the grace -- measured from its own flag,
			// on the database's clock -- has run.
			if ends := f.StopRequestedAt.Time.Add(a.timeouts.StopGrace); graceEnds.IsZero() || ends.Before(graceEnds) {
				graceEnds = ends
			}
		}

		if len(toCancel) > 0 {
			unconfirmed, err := a.cancelStoppedTurns(ctx, tx, sessionRow, toCancel, now)
			if err != nil {
				return err
			}
			cancelled = true
			// A turn in flight cancelled here ended with no word from the
			// agent, which may still be running it: its sandbox gen is
			// retired in this same transaction, before anything can be
			// dispatched to it.
			if len(unconfirmed) > 0 {
				if retired, err = a.retireStoppedGen(ctx, tx, unconfirmed); err != nil {
					return err
				}
			}
		}

		if err := a.disarmWorkCreatingTimers(ctx, tx, sessionRow.StopRequestedAt); err != nil {
			return err
		}

		if graceEnds.IsZero() {
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
		return a.armTimer(ctx, tx, TimerStop, graceEnds)
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
	if cancelled {
		// §3.3: "on terminal event ... dispatch next pending" -- a turn
		// created after the stop, which carries no flag, runs now: on the
		// same sandbox when the stop cancelled only queued turns, on a new
		// gen (a restore or a respawn) when it retired the old one.
		if dispatchErr := a.handleEnsureDispatched(ctx); dispatchErr != nil {
			a.logger.Warn("sessionactor: ensure-dispatched after stop failed", "error", dispatchErr)
		}
	}
	return nil
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

		if _, err := a.stores.turn.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
			ID:          id,
			Status:      sqlcgen.TurnStatus(to),
			CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return nil, fmt.Errorf("sessionactor: update turn status: %w", err)
		}

		workflowengine.OnTurnCompleted(ctx, workflowengine.Deps{
			Workflows:             a.stores.workflow.WithTx(tx),
			Turns:                 a.stores.turn.WithTx(tx),
			SlackThreadSessions:   a.stores.slackThreadSession.WithTx(tx),
			LinearAgentSessions:   a.stores.linearAgentSession.WithTx(tx),
			GitHubPRSessions:      a.stores.githubPRSession.WithTx(tx),
			Outbox:                a.stores.outbox.WithTx(tx),
			EpistemicCheckDefault: a.epistemicCheckDefault,
		}, sessionRow, id, turn.TriggerCancel)

		if turn.RequiresSyntheticExecutionComplete(turn.TriggerCancel) {
			if err := a.appendEvent(ctx, tx, "execution_complete", map[string]any{
				"turn_id":   id.String(),
				"synthetic": true,
				"reason":    "stopped",
			}); err != nil {
				return nil, err
			}
		}

		failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerCancel)
		if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, turn.TriggerCancel, failureReason, target, nil); err != nil {
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
// running on when handleStopTimer cancelled it with no word from the agent.
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
// dead, or one whose gen has moved past every one of unconfirmed's own
// dispatches -- a respawn since then already fenced that gen off, and a
// flagged turn is never re-sent (planReenqueueOrRespawn). A turn with no
// dispatched_sandbox_gen recorded is taken to have run on the current gen.
func (a *Actor) retireStoppedGen(ctx context.Context, tx pgx.Tx, unconfirmed []sqlcgen.Turn) (*retiredGen, error) {
	row, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionactor: get sandbox: %w", err)
	}
	from := sandbox.State(row.Status)
	if sandbox.IsDeadSandboxStatus(from) || !ranOnGen(unconfirmed, int(row.Gen)) {
		return nil, nil
	}

	if from != sandbox.StateSuspect {
		if _, err := sandbox.Transition(from, int(row.Gen), sandbox.SuspectTrigger()); err != nil {
			return nil, fmt.Errorf("sessionactor: retire stopped sandbox gen: %w", err)
		}
		preSuspect := sqlcgen.SandboxStatus(from)
		if _, err := a.stores.sandbox.WithTx(tx).UpdateStatusToSuspect(ctx, sqlcgen.UpdateSandboxStatusToSuspectParams{
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
	if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
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

// ranOnGen reports whether any of turns was dispatched to gen, or has no
// dispatched gen recorded.
func ranOnGen(turns []sqlcgen.Turn, gen int) bool {
	for _, t := range turns {
		if t.DispatchedSandboxGen == nil || int(*t.DispatchedSandboxGen) == gen {
			return true
		}
	}
	return false
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
// today the re-review debounce) and that was armed at or before
// requestedAt, the session's standing stop request. session_timers.
// created_at and sessions.stop_requested_at are both the database's now(),
// so the comparison involves no replica's clock. A timer armed after the
// request -- a push after the stop -- is new input, and stays (technical
// plan §3.3's effects table), however late this timer fires. A NULL
// requestedAt -- a person resumed the session -- deletes nothing: a timer
// armed since is new work of theirs.
//
// A re-arm updates a timer's row in place and keeps its created_at
// (UpsertSessionTimer), so a push that lands after the request but before
// this timer's first fire, while a debounce armed before the request still
// exists, loses that debounce with it. The route wakes the actor as it
// answers, so that window is normally the actor's own queue.
//
// A kind ClassifyTimer does not know is left armed: this binary cannot tell
// what it does, and handleTimerFired leaves one armed too.
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
