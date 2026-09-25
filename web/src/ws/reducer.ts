import type { EventEnvelope } from './types'

// reducer.ts is the "reducer" stage of §12.1's "WS transport -> event log
// -> reducer -> query invalidation" pipeline.
//
// # The ordering assumption this module makes, and why it is safe
//
// A reducer that folds events in NETWORK ARRIVAL order implicitly assumes
// arrival order == causal order. That assumption is false here: a
// reconnect's fresh replay, an in-flight fetch_history backfill page, and
// a live-broadcast-triggered backfill page (sessionStream.ts) are three
// independent request/response round-trips that can complete in any
// relative order -- e.g. a backfill page covering ids 40-60 can resolve
// AFTER a later live-broadcast-triggered page already delivered id 61,
// simply because the first request was slower. Folding "whatever arrived
// most recently" onto the end of a running fold would apply id 61 before
// id 55 and silently corrupt any state that depends on causal order
// (running totals are fine either way; anything that depends on "what
// happened before what" is not).
//
// This module never sees arrival order at all: reduceLog's only input is
// EventLog.entries() (eventLog.ts), which is ALWAYS id-sorted ascending
// by construction, regardless of the order appendMany was called in. So
// the one assumption this module actually makes is narrower and is
// something the server genuinely guarantees: within one session, event
// ids are allocated in commit order. The sequence alone does not give
// that: events.id is a bigserial (migrations/*_events*.up.sql) drawn from
// one table-wide sequence when the row is inserted, not when it commits.
// What gives it is that every insert (CreateEvent, internal/adapters/
// outbound/postgres/queries/events.sql) takes the session row lock
// BEFORE drawing its id, so a session's inserts draw their ids one at a
// time, in the order they commit. That lock is load-bearing, not
// redundant: an insert path without it can commit a lower id after a
// higher one of the same session, and every `id > cursor` reader (this
// client's backfill included) then skips that event for good. Every wire
// representation this client trusts as log data (SubscribedPayload.
// events, FetchHistoryResponse.events) carries that same id verbatim
// (internal/adapters/inbound/wshub/client.go's own eventWireMap: "id (for
// client-side de-dup against live broadcasts)"). Folding by id order is
// therefore folding by causal order, exactly.
//
// # Why a full re-fold on every change, not incremental folding
//
// reduceLog re-derives SessionActivityState from scratch over the WHOLE
// log every time it is called, rather than trying to fold only the
// newly-appended tail. That is deliberate: a newly-appended event does
// not necessarily belong at the tail (a backfill page can insert events
// BEHIND the current high-water mark -- see EventLog's own lowerBound-
// based splice), so "fold only what's new, onto the end" would be simply
// wrong here, not just less efficient. A full re-fold is trivially
// correct at the event volumes this pipeline handles today (client.go's
// own initialReplayLimit=200 is the right order of magnitude to have in
// mind); a future Step is free to replace this with real incremental
// folding once volume ever justifies the added complexity -- this module
// is small and self-contained specifically so that swap is easy to make
// in one place.

export interface SessionActivityState {
  /** Total distinct events folded so far (dedup already happened in EventLog -- this count can never be inflated by a redelivered replay). */
  eventCount: number
  /** Per-type running counts, e.g. {"tool_call": 12, "execution_complete": 1}. */
  countsByType: Record<string, number>
  /** The highest event id folded, or null if none yet -- always the log's own highestId() once reduceLog has run over it. */
  lastEventId: number | null
  /** createdAt of the causally-latest (highest-id) event folded, or null if none yet. */
  lastEventAt: string | null
}

export const initialSessionActivityState: SessionActivityState = {
  eventCount: 0,
  countsByType: {},
  lastEventId: null,
  lastEventAt: null,
}

/** reduceEvent folds exactly one event into state. Exported (not just an internal helper of reduceLog) so a test can drive it directly against hand-built EventEnvelopes without going through EventLog/SessionStream, alongside the pipeline-level tests that DO go through the real transport/log (see this Step's own PR description on why both kinds of test earn their place: this one pins the fold's own logic in isolation, the pipeline tests pin that dedup/ordering/invalidation actually compose correctly end to end). */
export function reduceEvent(state: SessionActivityState, event: EventEnvelope): SessionActivityState {
  return {
    eventCount: state.eventCount + 1,
    countsByType: {
      ...state.countsByType,
      [event.type]: (state.countsByType[event.type] ?? 0) + 1,
    },
    lastEventId: event.id,
    lastEventAt: event.createdAt,
  }
}

/** reduceLog folds an id-ordered event array (always EventLog.entries() in real use) into SessionActivityState from scratch. */
export function reduceLog(events: readonly EventEnvelope[]): SessionActivityState {
  return events.reduce(reduceEvent, initialSessionActivityState)
}
