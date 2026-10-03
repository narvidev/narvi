package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TimerStore is a thin, pass-through wrapper around the sqlc-generated
// session_timers queries (§4.3 TimerScheduler, §2 named persistent
// timers). No caching, no retries, no business rules — the timer pump
// lands in §2.
type TimerStore struct {
	q *sqlcgen.Queries
}

// NewTimerStore builds a TimerStore backed by pool.
func NewTimerStore(pool *pgxpool.Pool) *TimerStore {
	return &TimerStore{q: sqlcgen.New(pool)}
}

// WithTx returns a TimerStore whose queries run on tx instead of the pool
// this store was built with — used both by the timer pump's claim
// transaction and by app/sessionactor's transactional-write helper (§2).
func (s *TimerStore) WithTx(tx pgx.Tx) *TimerStore {
	return &TimerStore{q: s.q.WithTx(tx)}
}

// Upsert arms (or re-arms) a named timer for a session: ON CONFLICT
// (session_id, name) DO UPDATE, per §2 ("each is armed/re-armed
// independently") — never a duplicate row.
func (s *TimerStore) Upsert(ctx context.Context, arg sqlcgen.UpsertSessionTimerParams) (sqlcgen.SessionTimer, error) {
	return s.q.UpsertSessionTimer(ctx, arg)
}

// BackOffDispatch moves the session's dispatch timer to the database's
// now plus its age since the first arm of its chain of failures -- its
// created_at, or arg.Since when earlier, which created_at then takes --
// held between arg.BaseSeconds and arg.MaxSeconds, only while the row
// still carries arg.ArmedAt, and reports how many rows it moved: zero when
// the session has no dispatch timer, or a turn re-armed it since. armed_at
// is not moved. The session actor's backoff after a dispatch evaluation
// that failed, or a prompt its sandbox never received (technical plan §2).
func (s *TimerStore) BackOffDispatch(ctx context.Context, arg sqlcgen.BackOffSessionDispatchTimerParams) (int64, error) {
	return s.q.BackOffSessionDispatchTimer(ctx, arg)
}

// HoldReviewRetriggerDebounce re-arms the session's re-review debounce
// backstop ahead of the database's now, stamping armed_at with that now and
// keeping created_at, and reports how many rows it moved: zero when the
// session has no debounce, or only one armed at or before its standing stop
// request. It never inserts one. The session actor's held firing
// (technical plan §24.9); see HoldReviewRetriggerDebounce's doc comment in
// queries/session_timers.sql.
func (s *TimerStore) HoldReviewRetriggerDebounce(ctx context.Context, sessionID pgtype.UUID, backstop time.Duration) (int64, error) {
	return s.q.HoldReviewRetriggerDebounce(ctx, sqlcgen.HoldReviewRetriggerDebounceParams{
		BackstopSeconds: backstop.Seconds(),
		SessionID:       sessionID,
	})
}

// WakeReviewRetriggerDebounce moves the session's held re-review debounce
// -- one last armed at least heldLead before its fires_at, as only the hold
// arms it -- to the database's now, stamping armed_at and keeping
// created_at, and reports how many rows it moved: zero when the session
// has no debounce, only a push's quiet window, or only one armed at or
// before its standing stop request. It never inserts one. The session
// actor runs it in the transaction of every write that ends a turn
// (technical plan §24.9); see WakeReviewRetriggerDebounce's doc comment in
// queries/session_timers.sql.
func (s *TimerStore) WakeReviewRetriggerDebounce(ctx context.Context, sessionID pgtype.UUID, heldLead time.Duration) (int64, error) {
	return s.q.WakeReviewRetriggerDebounce(ctx, sqlcgen.WakeReviewRetriggerDebounceParams{
		SessionID:       sessionID,
		HeldLeadSeconds: heldLead.Seconds(),
	})
}

// DeleteDispatch deletes the session's dispatch timer and returns the
// armed_at and created_at it carried, both invalid when the session had
// none. The first write of every dispatch evaluation (technical plan §2).
func (s *TimerStore) DeleteDispatch(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.DeleteSessionDispatchTimerRow, error) {
	row, err := s.q.DeleteSessionDispatchTimer(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.DeleteSessionDispatchTimerRow{}, nil
	}
	return row, err
}

// Get fetches a named timer for a session.
func (s *TimerStore) Get(ctx context.Context, arg sqlcgen.GetSessionTimerParams) (sqlcgen.SessionTimer, error) {
	return s.q.GetSessionTimer(ctx, arg)
}

// Age fetches a named timer's armed_at -- its last arm -- together with the
// database's now(): the session actor measures the age of a timer kind it
// does not know on the database's clock (sessionactor.DecideUnknownTimer).
func (s *TimerStore) Age(ctx context.Context, arg sqlcgen.GetSessionTimerAgeParams) (sqlcgen.GetSessionTimerAgeRow, error) {
	return s.q.GetSessionTimerAge(ctx, arg)
}

// PostponeIfArmedAt moves a named timer's fires_at, and nothing else, only
// while the row still carries arg.ArmedAt, and reports how many rows it
// moved: zero when the row is gone or was re-armed since that armed_at was
// read. The session actor's backoff of a timer kind it does not know.
func (s *TimerStore) PostponeIfArmedAt(ctx context.Context, arg sqlcgen.PostponeSessionTimerIfArmedAtParams) (int64, error) {
	return s.q.PostponeSessionTimerIfArmedAt(ctx, arg)
}

// DeleteIfArmedAt deletes a named timer only while it still carries
// arg.ArmedAt, and reports how many rows it deleted: zero when the row is
// gone or was re-armed since. The session actor's deletion of a timer kind
// it does not know.
func (s *TimerStore) DeleteIfArmedAt(ctx context.Context, arg sqlcgen.DeleteSessionTimerIfArmedAtParams) (int64, error) {
	return s.q.DeleteSessionTimerIfArmedAt(ctx, arg)
}

// ListDue fetches up to limit due timers, locking each row (FOR UPDATE
// SKIP LOCKED) so concurrent pump ticks never select the same row — the
// timer pump's poll query (§2).
func (s *TimerStore) ListDue(ctx context.Context, limit int32) ([]sqlcgen.SessionTimer, error) {
	return s.q.ListDueTimers(ctx, limit)
}

// Claim pushes an already-locked (via ListDue, same transaction) timer's
// fires_at forward, so it won't be re-selected as due until the claim
// window elapses (redelivery-safety, §2). It never moves armed_at: a claim
// is not an arm.
func (s *TimerStore) Claim(ctx context.Context, arg sqlcgen.ClaimDueTimerParams) (sqlcgen.SessionTimer, error) {
	return s.q.ClaimDueTimer(ctx, arg)
}

// Delete removes a named timer for a session.
func (s *TimerStore) Delete(ctx context.Context, arg sqlcgen.DeleteSessionTimerParams) error {
	return s.q.DeleteSessionTimer(ctx, arg)
}

// ListForSession returns every timer armed on sessionID, by name -- the
// stop timer's handler reads it to delete the ones whose firing creates a
// turn (technical plan §3.3).
func (s *TimerStore) ListForSession(ctx context.Context, sessionID pgtype.UUID) ([]sqlcgen.SessionTimer, error) {
	return s.q.ListSessionTimers(ctx, sessionID)
}
