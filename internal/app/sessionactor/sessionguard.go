// This file (sessionguard.go) is the session actor's half of the session
// guard (technical plan §40.1, internal/app/turnguard): a session that has
// spent its cap takes no new turn, and no queued turn of it is dispatched.
//
// Every turn this package inserts is admitted by the guard first, in the
// inserting transaction, under the session-row lock transact takes: the
// automatic re-review (reviewretrigger.go), an owed review request's re-run
// (owedreviewrequest.go), and the workflow engine's advance, through
// workflowDeps' Guard. And the guard is asked again for every queued turn
// about to be dispatched (endPendingTurnsIfGuardClosed, from planDispatch).
// That second check is what bounds the overshoot to the one turn in flight
// when the cap is crossed: a mention coalesced while a turn ran, the review
// button, a composition review -- each queues a turn behind the one in
// flight, and each would otherwise run after the crossing.
//
// The turn in flight is never touched: the guard calls no stop, and a
// re-send of the in-flight turn to a new gen or a prompt receipt re-send is
// the same turn, never asked about (planReenqueueOrRespawn,
// tryPlanReceiptResend). A refused turn is never failed: a turn refused at
// creation is never inserted, and a queued turn ended here takes the
// machine's abandon edge with an end reason of its own, which the session's
// status derivation and every reader of attempts skip -- so the session
// reads its last real turn's outcome, waiting on a person.

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// sessionGuard is the session guard bound to tx, writing its warning
// through appendRawEvent, so it is broadcast once tx commits.
func (a *Actor) sessionGuard(tx pgx.Tx) *turnguard.Bound {
	return a.stores.sessionGuard.WithTx(tx, func(ctx context.Context, messageID string, raw json.RawMessage) (bool, error) {
		return a.appendRawEvent(ctx, tx, "warning", messageID, raw)
	})
}

// workflowDeps is what this package hands the workflow engine, every store
// bound to tx, with the session guard and the origin of an automatic
// advance: the engine's next attempt is admitted by the guard first, and a
// refusal escalates the run rather than failing anything
// (workflowengine's admitNextAttempt).
func (a *Actor) workflowDeps(tx pgx.Tx) workflowengine.Deps {
	return workflowengine.Deps{
		Workflows:             a.stores.workflow.WithTx(tx),
		Turns:                 a.stores.turn.WithTx(tx),
		SlackThreadSessions:   a.stores.slackThreadSession.WithTx(tx),
		LinearAgentSessions:   a.stores.linearAgentSession.WithTx(tx),
		GitHubPRSessions:      a.stores.githubPRSession.WithTx(tx),
		Outbox:                a.stores.outbox.WithTx(tx),
		Guard:                 a.sessionGuard(tx),
		Origin:                sessionguard.OriginAutomatic,
		EpistemicCheckDefault: a.epistemicCheckDefault,
	}
}

// admitAutomaticTurn asks the session guard whether this session may take
// a turn an automatic producer of this package is about to insert, in tx,
// under the session-row lock. A refusal is recorded -- the crossing's
// warning and, when it is new, its one notice -- in tx, and returned for
// the caller to decline its turn; a read that failed is an error, never a
// refusal.
func (a *Actor) admitAutomaticTurn(ctx context.Context, tx pgx.Tx, stage turnguard.Stage) (sessionguard.Admission, *sessionguard.Refusal, error) {
	guard := a.sessionGuard(tx)
	admission, refusal, err := guard.Admit(ctx, a.sessionID, sessionguard.OriginAutomatic, stage)
	if err != nil {
		return sessionguard.Admission{}, nil, fmt.Errorf("sessionactor: session guard: %w", err)
	}
	if refusal == nil {
		return admission, nil, nil
	}
	sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		return sessionguard.Admission{}, nil, fmt.Errorf("sessionactor: get session: %w", err)
	}
	if _, err := guard.Record(ctx, sessionRow, refusal, true); err != nil {
		return sessionguard.Admission{}, nil, fmt.Errorf("sessionactor: record the session guard's refusal: %w", err)
	}
	return sessionguard.Admission{}, refusal, nil
}

// endReasonForRefusal is the end reason of a queued turn the guard ended
// for r (turns.end_reason, internal/domain/turn's EndReason values).
func endReasonForRefusal(r *sessionguard.Refusal) (string, error) {
	switch r.Reason {
	case sessionguard.ReasonSpendCap:
		return turn.EndReasonSpendCap, nil
	default:
		return "", fmt.Errorf("sessionactor: no end reason for the session guard's reason %q", r.Reason)
	}
}

// endPendingTurnsIfGuardClosed is planDispatch's second check of the
// session guard, made when a pending turn is about to be dispatched
// (hasPending), in the evaluation's transaction, after the stop gate and
// the personal-link gate and before anything is spawned or sent. It reports
// whether the guard refused, in which case every pending turn of the
// session has been ended and nothing is to be dispatched or spawned this
// round:
//
//   - each pending turn, oldest first, takes the machine's abandon edge,
//     pending to failed, written through the recorder with the guard's end
//     reason (EndReasonSpendCap), so a held re-review debounce wakes as on
//     any turn's end, and the session's status derivation and every reader
//     of attempts skip it; with a synthetic execution_complete marked
//     "delivered": false -- no prompt was sent;
//   - a turn the workflow engine tracks ends its step run through
//     OnTurnRefusedBySessionGuard, which escalates the run with the
//     guard's escalation notice as its one notice -- the one a refused
//     advance sends too;
//   - the session's status is re-derived with every ended turn ignored, so
//     it reads its last real turn's outcome, never failed;
//   - then the crossing's warning, and its one notice when the warning is
//     new and no workflow escalation already told it. Like context_moved,
//     no review check and no turn-completion notice is posted.
//
// The turn in flight, if any, is never reached: a pending turn is
// dispatched only once none is in flight (turn.NextToDispatch). A read
// that failed is returned as an error: the evaluation rolls back and its
// dispatch timer backs off, as for every other read here.
func (a *Actor) endPendingTurnsIfGuardClosed(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, turns []sqlcgen.Turn, now time.Time) (bool, error) {
	guard := a.sessionGuard(tx)
	_, refusal, err := guard.Admit(ctx, a.sessionID, sessionguard.OriginDispatch, turnguard.StageDispatch)
	if err != nil {
		return false, fmt.Errorf("sessionactor: dispatch-time session guard: %w", err)
	}
	if refusal == nil {
		return false, nil
	}
	endReason, err := endReasonForRefusal(refusal)
	if err != nil {
		return false, err
	}
	text := sessionguard.Text(*refusal)

	overrides := map[pgtype.UUID]turn.Summary{}
	escalationNotified := false
	ended := 0
	for _, target := range turns {
		from := turn.State(target.Status)
		if from != turn.StatePending {
			continue
		}
		to, err := turn.Transition(from, turn.TriggerAbandon)
		if err != nil {
			return false, fmt.Errorf("sessionactor: end queued turn %s the session guard refused: %w", target.ID.String(), err)
		}
		reason := endReason
		if _, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
			ID:          target.ID,
			Status:      sqlcgen.TurnStatus(to),
			CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
			EndReason:   &reason,
		}); err != nil {
			return false, fmt.Errorf("sessionactor: update turn status: %w", err)
		}
		if workflowengine.OnTurnRefusedBySessionGuard(ctx, a.workflowDeps(tx), sessionRow, target.ID, *refusal) {
			escalationNotified = true
		}
		if turn.RequiresSyntheticExecutionComplete(turn.TriggerAbandon) {
			payload := syntheticExecutionComplete(target.ID, from, text)
			payload[syntheticUndeliveredKey] = false
			if err := a.appendEvent(ctx, tx, "execution_complete", payload); err != nil {
				return false, err
			}
		}
		failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerAbandon)
		overrides[target.ID] = turn.Summary{Status: to, FailureReason: failureReason, Ignored: true}
		ended++
	}
	if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverrides(turns, overrides)); err != nil {
		return false, err
	}
	if _, err := guard.Record(ctx, sessionRow, refusal, !escalationNotified); err != nil {
		return false, fmt.Errorf("sessionactor: record the session guard's refusal: %w", err)
	}
	a.logger.Warn("sessionactor: queued turns ended before dispatch: the session has reached its spend cap; the session waits on a person",
		"session_id", a.sessionID.String(), "ended", ended, "reason", string(refusal.Reason),
		"spent_usd", refusal.Spent.String(), "cap_usd", refusal.Cap.String())
	return true, nil
}
