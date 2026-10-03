package sessionactor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// turnWriter is the one way this package writes a turn's status: every
// write that leaves a turn completed, failed or cancelled is noted on the
// actor, so transact wakes the re-review debounce in the same transaction
// as that write (wakeReviewRetriggerIfTurnEnded, technical plan §24.9).
// While a turn of a review session is open the debounce holds
// (reviewretrigger.go); every way a turn ends goes through here, so no
// ending can lose the wake-up. TestTurnStatusWritesGoThroughTheRecorder
// keeps every such write on it, and TestTurnStatusQueriesAreTheRecordedOnes
// keeps UpdateTurnStatus the one query that writes turns.status. A turn is
// never inserted terminal (postgres.ErrTurnNotPending).
//
// The writes it carries today, by what they do:
//
//   - completeProcessingTurn (pushpr.go): a real execution_complete;
//   - handleTurnDeadlineTimer (timerfired.go): the turn's deadline;
//   - failDispatchedTurn (dispatch.go): a dispatch-time revocation, a
//     cohort rollout refusal, a prompt over its gen's frame bound, or a
//     send that failed;
//   - endTurnsOnSpawnRefusal (dispatch.go): a spawn refused on policy;
//   - cancelStoppedTurns (stop.go): a person's stop, from the stop timer
//     or planDispatch's gate;
//   - refusePersonalLinkOnly (credentialgate.go): a model only a personal
//     provider link could run;
//   - and three that end nothing: tryPlanDispatch's two (pending to
//     dispatched to processing) and tryPlanReenqueue's re-stamp, which
//     passes the status back unchanged.
type turnWriter struct {
	a *Actor
	s *postgres.TurnStore
}

// turnWrites returns the recorder for tx, the transaction transact opened.
func (a *Actor) turnWrites(tx pgx.Tx) turnWriter {
	return turnWriter{a: a, s: a.stores.turn.WithTx(tx)}
}

// UpdateStatus is postgres.TurnStore.UpdateStatus, noted: a write that
// commits a terminal status sets turnEnded, which transact reads before it
// commits. turn.IsTerminal is the deny list every reader of an open turn
// shares, so a status added to the machine later is never read as an end.
func (w turnWriter) UpdateStatus(ctx context.Context, arg sqlcgen.UpdateTurnStatusParams) (sqlcgen.Turn, error) {
	row, err := w.s.UpdateStatus(ctx, arg)
	if err == nil && turn.IsTerminal(turn.State(row.Status)) {
		w.a.turnEnded = true
	}
	return row, err
}

// wakeReviewRetriggerIfTurnEnded is the wake-up a turn's end owes the
// re-review debounce (technical plan §24.9): when the current transact
// attempt wrote a terminal status through turnWrites, the session's
// debounce, if it has one, is moved to now inside tx, the transaction of
// that write, so the end and the wake-up commit together or not at all. It
// runs whether or not another turn is still open -- the debounce then
// holds again at its next firing -- so the statement stays one probe of
// the timer's unique index. It never inserts a debounce, and never wakes
// one armed at or before a standing stop request (TimerStore.
// WakeReviewRetriggerDebounce). A session that is no review session has no
// debounce, and nothing is written.
func (a *Actor) wakeReviewRetriggerIfTurnEnded(ctx context.Context, tx pgx.Tx) error {
	if !a.turnEnded {
		return nil
	}
	woken, err := a.stores.timer.WithTx(tx).WakeReviewRetriggerDebounce(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: wake the re-review debounce after a turn ended: %w", err)
	}
	if woken > 0 {
		a.logger.Info("sessionactor: review_retrigger_debounce: a turn ended; the held re-review is due now")
	}
	return nil
}
