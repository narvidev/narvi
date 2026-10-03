package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/framecut"
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
//
// It takes arg.SessionID's session row lock (FOR NO KEY UPDATE) before
// drawing the event id, and the caller's transaction holds it until it
// ends: that is what keeps a session's ids in commit order for every
// `id > cursor` reader -- see CreateEvent's doc comment in
// queries/events.sql. Keep a transaction that calls this short, and do
// not wait inside it on anything that itself appends to the same session
// from another connection. A session that does not exist returns
// pgx.ErrNoRows.
func (s *EventStore) Create(ctx context.Context, arg sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error) {
	return s.q.CreateEvent(ctx, arg)
}

// StoredTokenPart is what is already stored of one streamed text part:
// the id, text and cut of its first stored `token` frame and the text and
// cut of its newest one.
type StoredTokenPart struct {
	// FirstFrameID is the lowest events.id among the part's stored frames:
	// where the part entered the log, which places it in a turn's window.
	FirstFrameID int64
	// FirstText is the text of that first frame -- the one stored under
	// the bare part id, which a resend of it no longer matches by key.
	FirstText string
	// LatestText is the text of the part's highest-id stored frame, the
	// newest one stored.
	LatestText string
	// FirstCut and LatestCut are those two frames' `cut` properties
	// (technical plan §6.1), nil for a whole frame, decoded fail-closed by
	// framecut.DecodeCut: a `cut` present but unreadable is
	// framecut.Malformed, never nil.
	FirstCut  *framecut.Cut
	LatestCut *framecut.Cut
}

// StoredTokenPart returns what is stored of the `token` part whose payload
// messageId is partID in sessionID, and whether any frame of it is stored
// at all -- none is found=false, never an error. The session actor calls
// it inside its own transaction before storing a frame
// (sessionactor/tokenframe.go); events_token_part_idx keeps both halves
// of it index probes however long the session's log is.
func (s *EventStore) StoredTokenPart(ctx context.Context, sessionID pgtype.UUID, partID string) (part StoredTokenPart, found bool, err error) {
	row, err := s.q.GetLatestTokenFrameForPart(ctx, sqlcgen.GetLatestTokenFrameForPartParams{
		SessionID: sessionID,
		PartID:    partID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredTokenPart{}, false, nil
	}
	if err != nil {
		return StoredTokenPart{}, false, err
	}
	return StoredTokenPart{
		FirstFrameID: row.FirstID,
		FirstText:    row.FirstText,
		LatestText:   row.Text,
		FirstCut:     framecut.DecodeCut(row.FirstCut),
		LatestCut:    framecut.DecodeCut(row.Cut),
	}, true, nil
}

// ListTokenFramesInWindow returns up to limit of sessionID's `token`
// frames with id above lowerID and, when upperID is not nil, at or below
// it, newest id first -- one turn's frames, in the window
// sessionactor.TurnContentBounds gives and plan.FinalText reads. The
// decision inbox reads an awaiting plan's final text through it, a range on
// the session's own index rather than the tail of its whole log
// (ListTokenFramesInWindow's doc comment, queries/events.sql).
func (s *EventStore) ListTokenFramesInWindow(ctx context.Context, sessionID pgtype.UUID, lowerID int64, upperID *int64, limit int32) ([]sqlcgen.Event, error) {
	return s.q.ListTokenFramesInWindow(ctx, sqlcgen.ListTokenFramesInWindowParams{
		SessionID: sessionID,
		LowerID:   lowerID,
		UpperID:   upperID,
		RowLimit:  limit,
	})
}

// ListForSession returns up to limit events for sessionID with id >
// afterID, oldest first (afterID = 0 means "from the beginning"), whatever
// their size. It bounds a read by count alone, so no page a client reads
// goes through it: the client WS hub's subscribe replay and fetch_history,
// and the REST GET .../events endpoint (§6.2, §6.3), read through
// ListPageForSession, which bounds the read by bytes too.
func (s *EventStore) ListForSession(ctx context.Context, sessionID pgtype.UUID, afterID int64, limit int32) ([]sqlcgen.Event, error) {
	return s.q.ListEventsForSession(ctx, sqlcgen.ListEventsForSessionParams{
		SessionID: sessionID,
		ID:        afterID,
		Limit:     limit,
	})
}

// EventPage is one page of a session's event log, read by
// ListPageForSession.
type EventPage struct {
	// Events are the page's events, oldest first.
	Events []sqlcgen.Event
	// StoppedAtBudget reports that the page stopped at its byte budget: at
	// least one more event follows the last one in Events.
	StoppedAtBudget bool
}

// ListPageForSession reads one page of sessionID's events after afterID,
// oldest first: at most maxRows events, and no more of them than fit in
// maxBytes as a page carries them -- always at least one, so a page always
// moves its reader on (technical plan §6.2, §6.3). The client WS hub's
// subscribe replay and fetch_history, and the REST events route, read
// every page through it.
//
// The budget bounds the read, not only the reply. The database measures
// each event as it walks the page (ListEventPageExtentForSession, an upper
// bound on the event's size in a page), stops at the first one that does
// not fit, and the page then reads only the events that do, by their ids
// (ListEventsForSessionByIDs). An event that does not fit is never read
// into this process, so the next page, which starts after the last event
// this one holds, reads it once: paging through a log reads each event
// once, however large. What one page holds is at most maxBytes, or one
// event when the first alone is larger. Reading a whole maxRows page and
// cutting it after held up to maxRows events of up to
// platform.MaxEventFrameBytes each, and read every dropped event again on
// the next page.
//
// The two statements need no transaction: no query updates or deletes an
// event (only a deleted session takes its events with it, and then the
// page is simply shorter), so the events the walk measured are the events
// read by their ids. Each lookup of the walk starts at the cursor on
// events_session_id_id_idx and reads a few buffers, and the read takes
// exact ids, under a custom plan and the generic plan a cached statement
// settles on alike (TestEventPage_WalkIsPositionedOnTheSessionIndex):
// neither reads another session's events, nor the session's own before
// the cursor.
func (s *EventStore) ListPageForSession(ctx context.Context, sessionID pgtype.UUID, afterID int64, maxRows int32, maxBytes int64) (EventPage, error) {
	if maxRows <= 0 {
		return EventPage{}, nil
	}
	extent, err := s.q.ListEventPageExtentForSession(ctx, sqlcgen.ListEventPageExtentForSessionParams{
		SessionID: sessionID,
		AfterID:   afterID,
		MaxRows:   int64(maxRows),
		MaxBytes:  maxBytes,
	})
	if err != nil {
		return EventPage{}, err
	}
	fit, stopped := eventPageFit(extent, maxBytes)
	if fit == 0 {
		return EventPage{}, nil
	}
	ids := make([]int64, fit)
	for i, row := range extent[:fit] {
		ids[i] = row.ID
	}
	events, err := s.q.ListEventsForSessionByIDs(ctx, sqlcgen.ListEventsForSessionByIDsParams{
		Ids:       ids,
		SessionID: sessionID,
	})
	if err != nil {
		return EventPage{}, err
	}
	return EventPage{Events: events, StoppedAtBudget: stopped}, nil
}

// eventPageFit returns how many of the walked events a page holds -- every
// one whose running size is within maxBytes, and the first whatever its
// size -- and whether the walk stopped at the budget: an event after them
// that does not fit.
func eventPageFit(extent []sqlcgen.ListEventPageExtentForSessionRow, maxBytes int64) (fit int, stopped bool) {
	for _, row := range extent {
		if row.N > 1 && row.Running > maxBytes {
			return fit, true
		}
		fit++
	}
	return fit, false
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
// real call -- see readSubTaskTrace,
// internal/adapters/inbound/httpapi/reviewverdict.go, for that NULL
// handling and the fuller "why" this pair of conditions is required, not
// merely an optimization).
//
// nextDispatchedEventID (§26.6's amendment) bounds the read from above:
// the watermark of the next turn dispatched on the session
// (TurnStore.NextDispatchedEventID), so a later turn's events never read
// as this turn's; nil when no turn was dispatched after this one. See
// queries/events.sql's "The upper bound".
func (s *EventStore) ListSubTaskStartsForTurn(ctx context.Context, sessionID pgtype.UUID, gen int32, dispatchedEventID int64, nextDispatchedEventID *int64) ([]sqlcgen.Event, error) {
	return s.q.ListSubTaskStartEventsForTurn(ctx, sqlcgen.ListSubTaskStartEventsForTurnParams{
		SessionID:             sessionID,
		Gen:                   gen,
		DispatchedEventID:     dispatchedEventID,
		NextDispatchedEventID: nextDispatchedEventID,
	})
}

// ListSubTaskFinishesForTurn is ListSubTaskStartsForTurn's own sibling --
// see that method's doc comment immediately above for the full "why".
func (s *EventStore) ListSubTaskFinishesForTurn(ctx context.Context, sessionID pgtype.UUID, gen int32, dispatchedEventID int64, nextDispatchedEventID *int64) ([]sqlcgen.Event, error) {
	return s.q.ListSubTaskFinishEventsForTurn(ctx, sqlcgen.ListSubTaskFinishEventsForTurnParams{
		SessionID:             sessionID,
		Gen:                   gen,
		DispatchedEventID:     dispatchedEventID,
		NextDispatchedEventID: nextDispatchedEventID,
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
