package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// EventStore is a thin, pass-through wrapper around the sqlc-generated
// events queries (§4.3, §6.1's append-only per-session event log). No
// caching, no retries, no business rules — appending an event as part of
// a state transition is app/sessionactor's job (§2), always inside
// that transition's own transaction (§2: "state transition + appended
// event + outbox entries commit in ONE Postgres transaction"). Reading
// back a page of that log (ListForSession, §6.2) has no such
// transactional requirement — it always runs against the pool, never
// WithTx.
type EventStore struct {
	q *sqlcgen.Queries
}

// NewEventStore builds an EventStore backed by pool.
func NewEventStore(pool *pgxpool.Pool) *EventStore {
	return &EventStore{q: sqlcgen.New(pool)}
}

// WithTx returns an EventStore whose queries run on tx instead of the
// pool this store was built with — used by app/sessionactor's
// transactional-write helper (§2).
func (s *EventStore) WithTx(tx pgx.Tx) *EventStore {
	return &EventStore{q: s.q.WithTx(tx)}
}

// Create upserts an event row on (session_id, message_id) and returns it
// -- CreateEventRow.Inserted reports whether this call actually inserted a
// fresh row (true) or found an already-persisted one from an earlier call
// with the same messageId (false, a genuine resend/dedupe, §6.1).
func (s *EventStore) Create(ctx context.Context, arg sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error) {
	return s.q.CreateEvent(ctx, arg)
}

// ListForSession returns up to limit events for sessionID with id >
// afterID, oldest first (afterID = 0 means "from the beginning" -- a null
// fetch_history cursor / the REST endpoint's own ?cursor= default). Shared
// by both the client WS hub's fetch_history/initial-replay handling and
// the REST GET .../events endpoint (§6.2, §6.3) -- one implementation,
// two callers, no duplication.
func (s *EventStore) ListForSession(ctx context.Context, sessionID pgtype.UUID, afterID int64, limit int32) ([]sqlcgen.Event, error) {
	return s.q.ListEventsForSession(ctx, sqlcgen.ListEventsForSessionParams{
		SessionID: sessionID,
		ID:        afterID,
		Limit:     limit,
	})
}

// ListRecentForSession returns up to limit of sessionID's own MOST RECENT
// events, newest id first -- the mirror-image pagination direction of
// ListForSession's own oldest-first cursor page. Used when a caller needs
// only the TAIL of a possibly-long event log (e.g. sessionactor's own
// best-effort plan-content extraction, §8.1) rather than a paginated
// walk from the very beginning of a session's entire history.
func (s *EventStore) ListRecentForSession(ctx context.Context, sessionID pgtype.UUID, limit int32) ([]sqlcgen.Event, error) {
	return s.q.ListRecentEventsForSession(ctx, sqlcgen.ListRecentEventsForSessionParams{
		SessionID: sessionID,
		Limit:     limit,
	})
}

// ListSubTaskStartsForTurn and ListSubTaskFinishesForTurn (§26.4/
// §7.1; renamed from ...ForGen after an adversarial review of this same PR
// caught a real cross-turn contamination gap -- see queries/events.sql's
// own doc comment on these two queries for the full "why") back post-hoc
// sub-task corroboration: reading back this session's own already-persisted
// sub_task_start/sub_task_finish trace, scoped to BOTH ONE sandbox gen
// (the gen the turn being verdicted was actually dispatched at,
// turns.dispatched_sandbox_gen) AND an events.id lower bound at that SAME
// turn's own turns.dispatched_event_id. Neither alone is sufficient -- gen
// alone cannot distinguish two turns dispatched to the SAME still-live
// sandbox incarnation (gen is bumped only on spawn/restore/resume, never on
// an ordinary dispatch to an already-Ready/Suspect sandbox); the id bound
// alone cannot distinguish a genuinely different, now-dead sandbox
// incarnation's stale, late-arriving event.
//
// The lower bound is a monotonic events.id, NOT a timestamp: it was
// originally turns.dispatched_at, which compared a Postgres-stamped
// events.created_at against an application-stamped Go time.Time -- two
// different clocks, sound only to whatever precision NTP happened to hold.
// See migrations/000089_turns_dispatched_event_id.up.sql for the full "why"
// and for why turns.dispatched_at itself is deliberately left untouched.
//
// Both callers already validate their own respective NULL case before
// reaching here (dispatchedEventID is never derived from a NULL column on a
// real call -- see corroborateCounterReview,
// internal/adapters/inbound/httpapi/reviewverdict.go, for that NULL
// handling and the fuller "why" this pair of conditions is required, not
// merely an optimization).
func (s *EventStore) ListSubTaskStartsForTurn(ctx context.Context, sessionID pgtype.UUID, gen int32, dispatchedEventID int64) ([]sqlcgen.Event, error) {
	return s.q.ListSubTaskStartEventsForTurn(ctx, sqlcgen.ListSubTaskStartEventsForTurnParams{
		SessionID:         sessionID,
		Gen:               gen,
		DispatchedEventID: dispatchedEventID,
	})
}

// ListSubTaskFinishesForTurn is ListSubTaskStartsForTurn's own sibling --
// see that method's doc comment immediately above for the full "why".
func (s *EventStore) ListSubTaskFinishesForTurn(ctx context.Context, sessionID pgtype.UUID, gen int32, dispatchedEventID int64) ([]sqlcgen.Event, error) {
	return s.q.ListSubTaskFinishEventsForTurn(ctx, sqlcgen.ListSubTaskFinishEventsForTurnParams{
		SessionID:         sessionID,
		Gen:               gen,
		DispatchedEventID: dispatchedEventID,
	})
}

// MaxEventIDForSession returns this session's own events-log high-water
// mark -- MAX(events.id), 0 when the session has no events yet. Stamped
// into turns.dispatched_event_id at dispatch (tryPlanDispatch/
// tryPlanReenqueue, internal/app/sessionactor/dispatch.go) so the two
// corroboration queries above have a clock-free lower bound identifying
// this turn's own dispatch.
func (s *EventStore) MaxEventIDForSession(ctx context.Context, sessionID pgtype.UUID) (int64, error) {
	return s.q.MaxEventIDForSession(ctx, sessionID)
}

// GetBootP95InWindow returns sinceTime's own platform-wide boot-duration
// p95 (successful boots only) plus the sample size behind it -- §12.2
// item 6's own "Boot p95" KPI tile. See GetBootP95InWindow's own generated
// doc comment (sqlcgen/events.sql.go, sourced from queries/events.sql)
// for the full definition.
func (s *EventStore) GetBootP95InWindow(ctx context.Context, sinceTime pgtype.Timestamptz) (sqlcgen.GetBootP95InWindowRow, error) {
	return s.q.GetBootP95InWindow(ctx, sinceTime)
}
