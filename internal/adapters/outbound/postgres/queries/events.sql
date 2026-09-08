-- Queries backing EventStore (§4.3, §6.1's append-only per-session event
-- log). CreateEvent appends; ListEventsForSession is the cursor-paginated
-- read this same table's own migration comment predicted ("reads
-- (cursor-paginated fetch_history) land with the client WS hub") -- this
-- is that work, backing both the client WS hub's own
-- fetch_history/replay and the REST GET .../events endpoint (one
-- implementation, two callers).

-- name: CreateEvent :one
-- Upsert-on-(session_id, message_id) (§6.1: "receiver dedupes by
-- upsert-on-messageId") -- a resend of an already-seen messageId hits the
-- unique index (migrations/000019_events_message_id.up.sql) and this
-- DO UPDATE (a deliberate, self-referential no-op: type is set back to
-- its own current value) guarantees RETURNING always yields exactly one
-- row either way, so callers never need a separate "0 rows means
-- duplicate" branch. `(xmax = 0) AS inserted` is the standard Postgres
-- idiom for "was this row just inserted by THIS statement" (xmax is 0
-- only immediately after a fresh insert, non-zero after any update) --
-- callers use it to decide whether to (re-)broadcast this event to live
-- subscribers.
INSERT INTO events (session_id, type, message_id, payload) VALUES ($1, $2, $3, $4)
ON CONFLICT (session_id, message_id) DO UPDATE SET type = events.type
RETURNING *, (xmax = 0) AS inserted;

-- name: ListEventsForSession :many
-- afterID = 0 means "from the beginning" -- matches a null fetch_history
-- cursor / a REST ?cursor= default. The monotonic BIGSERIAL id is the
-- natural pagination cursor (events_session_id_id_idx,
-- migrations/000008_events.up.sql's own doc comment).
SELECT * FROM events
WHERE session_id = $1 AND id > $2
ORDER BY id ASC
LIMIT $3;

-- name: ListRecentEventsForSession :many
-- The mirror-image pagination direction from ListEventsForSession's own
-- oldest-first cursor page: returns up to $2 of session_id's own MOST
-- RECENT events, newest id first -- for a caller that needs only the TAIL
-- of a possibly-long event log (e.g. sessionactor.planContentText's own
-- best-effort plan-content extraction, §8.1) without scanning forward
-- from the very beginning of a session's entire history, which for a
-- long-lived session (many prior turns) could leave the CURRENT turn's
-- own events entirely outside a bounded oldest-first window. Same
-- events_session_id_id_idx index (migrations/000008_events.up.sql) serves
-- this DESC scan equally well.
SELECT * FROM events
WHERE session_id = $1
ORDER BY id DESC
LIMIT $2;

-- (§26.4/§7.1's own post-hoc sub-task corroboration): the two
-- queries below are this codebase's FIRST use of a payload->>'gen' JSONB
-- extraction filter -- checked against the rest of this file and every
-- other file in this directory before writing these, since there is no
-- existing precedent for filtering events by a field INSIDE payload
-- rather than by one of the table's own real columns (session_id, type,
-- id). Every sandbox-ws wire event, sub_task_start and sub_task_finish
-- included, carries its own "gen" field in the common envelope
-- (contracts/sandbox-ws/v1/events.schema.json) -- the SAME sandbox
-- generation the emitting sandbox process was live at when it produced
-- the event, distinct from the events TABLE's own session-scoped id
-- column, which has no gen concept of its own at all.
--
-- # gen alone is NOT sufficient -- see the dispatched_event_id bound below
--
-- An earlier version of these two queries (this same Step, before an
-- adversarial review of the PR caught this) scoped strictly to
-- (session_id, type, gen) on the theory that gen alone uniquely identifies
-- a turn's own dispatch. It does not: turns.dispatched_sandbox_gen
-- (migrations/000026_turn_dispatch_gen.up.sql) reuses sandboxes.gen, the
-- fencing idiom bumped ONLY on a fresh spawn/restore/resume
-- (internal/app/sessionactor's own planFreshSpawn/planRestore/planResume,
-- `gen = gen + 1`) -- NEVER on an ordinary dispatch to an already-live
-- (Ready/Suspect) sandbox. tryPlanDispatch (dispatch.go), the path used
-- whenever a Pending turn is dispatched to a sandbox that is already up,
-- stamps dispatched_sandbox_gen := sandboxRow.Gen VERBATIM, with no bump.
-- So a session whose sandbox stays alive across multiple review turns --
-- the ORDINARY case §24's automatic re-review on new commits describes,
-- not a rare corner case -- dispatches every one of those turns at the
-- SAME gen. Scoping by gen alone therefore lets an EARLIER turn's own
-- real, genuinely-completed counter-review trace spuriously corroborate a
-- LATER turn's self-report, simply because both turns happened to run on
-- the same still-live sandbox incarnation: exactly the unverified-self-
-- report bypass this whole Step exists to close, reopened.
--
-- The fix: an ADDITIONAL `id > sqlc.arg('dispatched_event_id')` lower
-- bound, using the turn-being-verdicted's own turns.dispatched_event_id
-- (migrations/000089_turns_dispatched_event_id.up.sql): the events-log
-- high-water mark, MAX(events.id) for this session, stamped in the same
-- transaction as the dispatch itself. turns_one_processing_per_session's
-- own unique partial index (migrations/000005_turns.up.sql) guarantees
-- turns execute strictly sequentially per session, so every event
-- genuinely belonging to THIS turn has an id above this turn's own
-- watermark, and every event belonging to an EARLIER turn on the same
-- session was inserted before this turn was ever dispatched -- excluded by
-- this bound regardless of whether gen happens to match.
--
-- This bound was originally written as `created_at >= dispatched_at`, and
-- that version was NOT sound: it compared events.created_at (stamped by
-- the POSTGRES server's own now(), migrations/000008_events.up.sql)
-- against turns.dispatched_at (a Go time.Time supplied by the APPLICATION
-- process, on a different host in any real deployment). Two clocks, agreeing
-- only to whatever precision NTP happens to hold -- and asymmetric in their
-- failure: an app clock running BEHIND the database widens the window and
-- readmits an earlier turn's trace, eroding exactly the cross-turn guard
-- this bound exists to provide. events.id is a BIGSERIAL assigned by the
-- one database (migrations/000008_events.up.sql), so this comparison now
-- has no clock in it at all. See that migration's own doc comment for why
-- turns.dispatched_at is deliberately left in place and untouched (it has
-- a genuine same-clock consumer in turn.EvaluateTurnDeadline).
--
-- Neither condition alone is enough; both stay, because each closes a
-- DIFFERENT, independent gap:
--   - gen alone: as above, cannot distinguish two turns dispatched to the
--     SAME live sandbox incarnation (no gen bump between them).
--   - dispatched_event_id alone: cannot distinguish a genuinely different,
--     now-dead sandbox INCARNATION's stale, late-arriving event from a
--     current one -- a sub_task_finish sent by an old sandbox process
--     right before it died can be delivered late over the network and land
--     in Postgres (and so receive an id) AFTER a LATER turn's own
--     watermark, even though it carries the OLD gen in its own payload.
--     The gen filter is what excludes that stale cross-incarnation row;
--     the id bound alone would not.
-- Together: gen excludes cross-INCARNATION contamination (a different,
-- dead sandbox process); dispatched_event_id excludes cross-TURN-same-
-- incarnation contamination (an earlier turn dispatched to the SAME
-- still-live sandbox). The `::int` cast on gen is safe here specifically
-- because "gen" is schema-typed as a JSON integer on every event this
-- filters (never a string, unlike some other payload fields elsewhere in
-- this schema) -- a malformed/absent "gen" on some hypothetical future
-- producer would fail the cast (or NULL-compare against the arg, matching
-- nothing) rather than silently matching every gen, so this filter fails
-- toward "matches nothing", never toward "over-matches" -- the same
-- fail-conservative direction this codebase's own closed-enum defaults
-- already commit to elsewhere (review/doc.go's "fail-conservative policy
-- for every closed enum" section). The caller (corroborateCounterReview,
-- internal/adapters/inbound/httpapi/reviewverdict.go) applies the
-- identical fail-conservative treatment when dispatched_event_id itself is
-- NULL (should be unreachable -- a turn being verdicted is by definition
-- already dispatched -- but never assumed).

-- name: ListSubTaskStartEventsForTurn :many
SELECT * FROM events
WHERE session_id = $1
  AND type = 'sub_task_start'
  AND (payload->>'gen')::int = sqlc.arg('gen')::int
  AND id > sqlc.arg('dispatched_event_id')::bigint
ORDER BY id ASC;

-- name: ListSubTaskFinishEventsForTurn :many
SELECT * FROM events
WHERE session_id = $1
  AND type = 'sub_task_finish'
  AND (payload->>'gen')::int = sqlc.arg('gen')::int
  AND id > sqlc.arg('dispatched_event_id')::bigint
ORDER BY id ASC;

-- name: MaxEventIDForSession :one
-- The events-log high-water mark for one session, stamped into
-- turns.dispatched_event_id at dispatch (tryPlanDispatch/tryPlanReenqueue,
-- internal/app/sessionactor/dispatch.go) and used as the lower bound of
-- the two corroboration queries above. COALESCE to 0 so a session with no
-- events yet yields a real watermark rather than NULL -- 0 is below every
-- BIGSERIAL id (which starts at 1), so a turn dispatched before any event
-- exists correctly admits every event that follows it.
SELECT COALESCE(MAX(id), 0)::bigint AS max_event_id FROM events WHERE session_id = $1;

-- name: GetBootP95InWindow :one
-- §12.2 item 6's own "Boot p95" KPI tile. Reads the SAME
-- best-effort "boot_timing" sandbox-ws event (§33.3, contracts/sandbox-ws/
-- v1/events.schema.json's own BootTiming def) internal/app/sessionactor's
-- own recordBootTiming (boottiming.go) already relays into the
-- sandbox_agent_boot_duration_seconds OTel histogram -- this is a
-- DIFFERENT consumer of the identical already-persisted fact (every
-- recognized event type is persisted verbatim, unconditionally,
-- appendRawEvent's own "persist ALWAYS" contract), not a second
-- measurement.
--
-- metric = 'boot_duration' selects the ONE of the four boot_timing
-- metrics that measures a whole boot-to-ready sequence (the other three
-- -- hook_rerun/git_fetch/git_checkout -- are sub-phases, not what "Boot
-- p95" names). failed IS NULL OR failed = 'false' excludes a boot attempt
-- that measured a duration but did not actually succeed -- "p95 boot
-- latency" answers "how long does a boot normally take", which a failed
-- attempt's own elapsed time (however long the sandbox spent failing to
-- boot) does not honestly represent; a failed-boot rate is a separate,
-- NOT-yet-built concern this tile does not conflate itself with.
--
-- p95_seconds is percentile_cont(0.95) over every admitted sample's own
-- (payload->>'seconds')::float8, COALESCEd to 0 rather than left NULL --
-- mirrors GetPlatformCostSummaryInWindow's own identical
-- median_cost_usd_per_session treatment (queries/turns.sql) and for the
-- same reason: the caller gates on sample_size alone and must never read
-- this column when the gate fails, so collapsing NULL to a real,
-- always-scannable 0::float8 here avoids depending on sqlc's own
-- nullability inference for an aggregate expression (which does not
-- reliably mark this NULLable) and the runtime "cannot scan NULL into
-- *float64" it would otherwise risk for the empty/too-few-samples case.
-- sample_size is additionally the input to a SECOND gate the caller
-- applies on top of "zero samples" -- a real but small sample count
-- (fewer than platformanalytics.MinBootP95Samples) still renders "too
-- few samples", because a percentile computed over a handful of points
-- is not a percentile anyone should trust, even though Postgres will
-- always compute SOME number for it.
--
-- Bounded by events_boot_timing_created_at_idx (migrations/
-- 000125_platform_analytics_indexes.up.sql).
SELECT
    COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY (payload->>'seconds')::float8), 0)::float8 AS p95_seconds,
    COUNT(*)::bigint AS sample_size
FROM events
WHERE type = 'boot_timing'
  AND payload->>'metric' = 'boot_duration'
  AND created_at >= $1
  AND (payload->>'failed' IS NULL OR payload->>'failed' = 'false');
