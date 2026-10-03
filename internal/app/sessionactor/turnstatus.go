package sessionactor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// turnWriter is the one way this package writes a turn's status: every
// write that leaves a turn completed, failed or cancelled is noted on the
// actor, so transact wakes the re-review debounce in the same transaction
// as that write (wakeReviewRetriggerIfTurnEnded, technical plan §24.9).
// While a turn of a review session is open the debounce holds
// (reviewretrigger.go); every way a turn ends today goes through here.
// TestTurnStatusWritesGoThroughTheRecorder keeps every status write the Go
// type checker can see on it -- the store's UpdateStatus, the query only
// inside it, the params only in a literal handed here -- and
// TestTurnStatusQueriesAreTheRecordedOnes keeps UpdateTurnStatus the one
// sqlc query, and no migration, writing turns.status in the shapes
// TestSQLWritesTurnStatus pins. SQL assembled at run time is not seen; a
// wake-up lost that way costs ReviewRetriggerHoldBackstop at most. A turn
// is never inserted terminal (postgres.ErrTurnNotPending).
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

// wakeReviewRetriggerIfTurnEnded is the wake-up a turn's end owes a held
// re-review debounce (technical plan §24.9): when the current transact
// attempt wrote a terminal status through turnWrites, the session's
// debounce, if the hold re-armed it, is moved to now inside tx, the
// transaction of that write, so the end and the wake-up commit together or
// not at all. A debounce a push armed that has not held is left to run out
// its quiet window (§24.2), so a burst of pushes that straddles a turn's
// end still reviews once, at its last head. It runs whether or not another
// turn is still open -- the debounce then holds again at its next firing
// -- so the statement stays one probe of the timer's unique index. It
// never inserts a debounce, and never wakes one armed at or before a
// standing stop request (TimerStore.WakeReviewRetriggerDebounce). A
// session that is no review session has no debounce, and nothing is
// written.
func (a *Actor) wakeReviewRetriggerIfTurnEnded(ctx context.Context, tx pgx.Tx) error {
	if !a.turnEnded {
		return nil
	}
	woken, err := a.stores.timer.WithTx(tx).WakeReviewRetriggerDebounce(ctx, a.sessionID, heldDebounceLead(a.timeouts))
	if err != nil {
		return fmt.Errorf("sessionactor: wake the re-review debounce after a turn ended: %w", err)
	}
	if woken > 0 {
		a.logger.Info("sessionactor: review_retrigger_debounce: a turn ended; the held re-review is due now")
	}
	return nil
}

// heldDebounceLead is how far past its last arm a re-review debounce's
// fires_at must sit for the wake-up to read it as held. The hold re-arms
// it ReviewRetriggerHoldBackstop ahead, fires_at and armed_at from one
// now() on the database's clock (TimerStore.HoldReviewRetriggerDebounce); a
// push arms it ReviewRetriggerDebounce ahead on its replica's clock, a few
// milliseconds after its transaction stamped armed_at, so a push's lead is
// a little over the window. Validate keeps the backstop at least
// MinTimeoutMargin above the window, so a push's row whose window is still
// running never reads as held. Two kinds of row do: one the hold re-armed,
// and one the timer pump claimed after its window ran out -- a claim moves
// fires_at TimerClaimDuration ahead and leaves armed_at, so the claimed
// row's lead is at least the window plus the claim. Waking the second kind
// is harmless: its quiet window is already over, and the firing the claim
// delivered either enqueued the review and cleared the pending head, so a
// second delivery only deletes the row, or has yet to run. A row's lead
// does not shrink as it ages, so a held row is woken however close to its
// backstop the turn ends.
func heldDebounceLead(t platform.Timeouts) time.Duration {
	return t.ReviewRetriggerDebounce + platform.MinTimeoutMargin
}
