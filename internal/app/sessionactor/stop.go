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
//     (pushpr.go's completeProcessingTurn: cancelled, nothing pushed). If
//     the timer fires again with the turn still in flight -- the agent
//     stayed silent, or there was no sandbox to tell -- the turn is
//     cancelled here with a synthetic event, as turn_deadline ends one; a
//     late real execution_complete is then discarded as §3.2 discards one.
//   - While the session's own request stands, the session's work-creating
//     timers (ClassifyTimer: TimerWorkCreatesTurn) are deleted.
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
	"github.com/narvidev/narvi/internal/domain/turn"
)

// stopSignal is what handleStopTimer's transaction hands back when a
// flagged turn in flight should be told to stop: the sandbox gen the
// command is fenced with. The send itself happens after the commit, outside
// any transaction, like every other network call this package makes.
type stopSignal struct {
	gen int
}

// handleStopTimer implements the `stop` named timer -- see this file's own
// top comment. Ends, like every handler, by re-arming or deleting the timer
// that fired: re-armed only while a flagged turn is still in flight within
// its grace.
func (a *Actor) handleStopTimer(ctx context.Context) error {
	var signal *stopSignal
	var cancelled bool

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		signal, cancelled = nil, false
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
			if err := a.cancelStoppedTurns(ctx, tx, sessionRow, toCancel, now); err != nil {
				return err
			}
			cancelled = true
		}

		// Scheduled work is disarmed only while the session's own request
		// stands: once a person has resumed the session (the flag cleared),
		// a timer armed since is new work of theirs, and a late fire of this
		// timer for a turn still in its grace must not take it away.
		if sessionRow.StopRequestedAt.Valid {
			if err := a.disarmWorkCreatingTimers(ctx, tx); err != nil {
				return err
			}
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

	if signal != nil {
		a.sendStop(*signal)
	}
	if cancelled {
		// §3.3: "on terminal event ... dispatch next pending" -- a turn
		// created after the stop, which carries no flag, runs now.
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
// ended some other way first) is skipped. Shared by handleStopTimer and
// planDispatch's gate.
func (a *Actor) cancelStoppedTurns(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, ids []pgtype.UUID, now time.Time) error {
	turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: list turns: %w", err)
	}

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
			return fmt.Errorf("sessionactor: update turn status: %w", err)
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
				return err
			}
		}

		failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerCancel)
		if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, turn.TriggerCancel, failureReason, target, nil); err != nil {
			return err
		}
		if from != turn.StatePending {
			if err := a.deleteTimer(ctx, tx, TimerTurnDeadline); err != nil {
				return err
			}
		}
		overrides[id] = turn.Summary{Status: to, FailureReason: failureReason}
		a.logger.Info("sessionactor: turn cancelled by a stop", "turn_id", id.String(), "from", string(from))
	}

	if len(overrides) == 0 {
		return nil
	}
	return a.persistDerivedSessionStatus(ctx, tx, summariesWithOverrides(turns, overrides))
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
// today the re-review debounce). A kind ClassifyTimer does not know is left
// armed: this binary cannot tell what it does, and handleTimerFired leaves
// one armed too.
func (a *Actor) disarmWorkCreatingTimers(ctx context.Context, tx pgx.Tx) error {
	timers, err := a.stores.timer.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: list session timers: %w", err)
	}
	for _, t := range timers {
		if work, ok := ClassifyTimer(t.Name); ok && work == TimerWorkCreatesTurn {
			if err := a.deleteTimer(ctx, tx, t.Name); err != nil {
				return err
			}
			a.logger.Info("sessionactor: work-creating timer disarmed by a stop", "timer", t.Name)
		}
	}
	return nil
}

// sendStop sends the sandbox `stop` command (sandboxws.Stop) fenced with
// gen, best effort: no live connection, or a failed write, is logged and
// left to the grace, which cancels the turn anyway. The agent aborts
// whatever turn it is running; a repeated stop finds it aborted already.
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
