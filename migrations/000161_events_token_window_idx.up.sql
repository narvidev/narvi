-- events_token_window_idx: backs ListTokenFramesInWindow
-- (queries/events.sql), the read every reader of a turn's final text makes
-- (sessionactor.ReadWindowFinal): the approval's cut check and its
-- snapshot, the decision inbox, the Slack and Linear replies to an awaiting
-- plan, GET /plans' fallback, the session result's summary and the plan
-- notices. It reads one turn's window of the session's `token` frames --
-- id above the turn's dispatch watermark and, for any turn but the
-- session's last, at or below the next dispatched turn's -- newest first,
-- at most 2000.
--
-- No index held that read to the window. events_token_part_idx (000144)
-- keys a session's frames by part id ahead of id, so id is no seek key in
-- it: under a generic plan the read walked every `token` frame of the
-- session and fetched the heap row of each one above the window, to test
-- an upper bound written `upper_id IS NULL OR id <= upper_id`, which no
-- plan can seek on. A custom plan took that walk or, by the log's
-- statistics, events_pkey across the window's id range, reading every
-- event every session logged inside it. Both grew with what the session or
-- the deployment had logged, not with the turn.
--
-- This partial index holds a session's `token` frames by id, written
-- id + 0, and the read puts both its bounds and its order on that
-- expression, the upper bound as a range even when the window is open
-- above (COALESCE(upper_id, the largest bigint)). The expression is the
-- point: neither events_pkey nor events_session_id_id_idx can serve a
-- range or an order on id + 0, so no plan can trade the read for a scan of
-- the window's range across every session, or of the session's whole log,
-- which an index on plain (session_id, id) still left open to the planner.
-- Every plan then reads this index's range from the window's upper bound
-- to its lower one and nothing else: a generic plan as a backward index
-- scan, a custom plan as that scan or as a bitmap scan of the same range
-- whose rows -- the window's frames, no others -- it then sorts. Either
-- way it reads a few index pages and the heap pages of the window's own
-- frames, and removes no row by a filter.
-- event_tokenwindow_plan_integration_test.go holds every plan to that
-- across a matrix of logs: at most 45 buffers for a window of 40 frames,
-- where the read before this index took up to 3,225 and removed up to
-- 52,360 rows on the same logs.
--
-- # What it costs a write
--
-- Every `token` row stored adds one entry here, a session id and a bigint,
-- as it does to events_token_part_idx; a row of any other type adds
-- nothing.
--
-- # A plain build, not CONCURRENTLY
--
-- This builds the index in the migration's own transaction and holds a
-- SHARE lock on events for the whole build, so every insert into events
-- -- every session's sandbox events, across all sessions -- waits until it
-- ends. 000144 measured the same kind of build, a partial index of the
-- `token` rows, on Postgres 17 with events at 3M rows (2 GB, 600k of them
-- `token`), warm cache: about 0.6 s, with concurrent inserts waiting up to
-- 0.65 s. The build scans the whole table whatever the partial predicate,
-- so the stall grows linearly with the table, and more on a cold cache;
-- and with no lock_timeout, a transaction already holding ROW EXCLUSIVE on
-- events when the build starts delays its lock, with every later insert
-- queued behind it. Reads of events go on meanwhile: a SHARE lock blocks
-- writes only.
--
-- One path blocks reads as well: an INVALID leftover of this index (below).
-- The block drops it with a plain DROP INDEX, which takes ACCESS EXCLUSIVE
-- on events and holds it until the migration commits, through the whole
-- build that follows. Every read of events then waits for the build too --
-- the timeline and its page walk, every read of a turn's text, the
-- actor's own reads before it stores a frame -- and the lock request first
-- queues behind every open transaction on events, reads included, with
-- every later read and write queued behind it.
--
-- CREATE INDEX CONCURRENTLY is not safe under this runner, for the reason
-- 000144 gives: golang-migrate serializes migrators with a blocking
-- pg_advisory_lock, a migrator waiting there holds a snapshot, and a
-- concurrent build waits for every older snapshot -- a deadlock Postgres
-- breaks by cancelling the build (an INVALID index, this version dirty,
-- every control plane refusing to boot) or the waiter (its boot fails). A
-- fresh install of deploy/control-plane (replicas: 2) runs two migrators
-- at once. A plain build takes no snapshot wait, so the waiter simply
-- waits.
--
-- # Operator guidance for a large events table
--
-- To avoid the stall, build the index concurrently yourself before
-- deploying this version, from psql, while no control plane is starting:
--
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS events_token_window_idx
--       ON events (session_id, (id + 0)) WHERE type = 'token';
--
-- then check it is valid:
--
--   SELECT indisvalid FROM pg_index WHERE indexrelid = 'events_token_window_idx'::regclass;
--
-- This migration then finds the valid index and builds nothing, and takes
-- no lock on events: the block below looks the index up in the catalog and
-- runs the CREATE only when there is none. (A bare CREATE INDEX IF NOT
-- EXISTS would not do: it queues for the SHARE lock on events before it
-- sees that the index exists, so it would still wait behind every open
-- insert into events, and stall every new one behind it.) Type the
-- statement above as it is: the block checks that an index it finds under
-- this name is that index, and fails the migration if it is not (below).
--
-- If that concurrent build fails it leaves an INVALID index behind. Drop it
-- yourself, without blocking anything --
--
--   DROP INDEX CONCURRENTLY events_token_window_idx;
--
-- -- and retry the concurrent build before deploying. Left in place, the
-- leftover is dropped and the index rebuilt by the block below, in this
-- migration's transaction, and that blocks every read and write of events
-- for the whole build (see above), not only inserts.
--
-- # An INVALID leftover
--
-- A name check alone would keep an INVALID index of this name -- one a
-- failed concurrent build left -- and Postgres never plans with an invalid
-- index, so every read of a turn's text would walk the session's text
-- instead. It is dropped first, in the same transaction, and built again,
-- under the ACCESS EXCLUSIVE lock described above.
--
-- # A relation of this name that is not this index
--
-- A valid index of this name with another definition -- (session_id, id),
-- say, or the expression without its predicate -- would leave the read's
-- bounds on id + 0 with no index to seek on, and the read would then fetch
-- every `token` frame of the session, more than the previous binary's text
-- reads. So would any other relation of this name. The block does not keep
-- such a relation, and it does not drop it either: dropping it here would
-- block every read and write of events for the rebuild, and it may be
-- something an operator built on purpose. It fails the migration instead,
-- naming what it found and what to do, and golang-migrate leaves this
-- version recorded as dirty, so no control plane boots until the operator
-- has dropped the relation and forced the version back (the message says
-- how). The definition is compared as pg_get_indexdef gives it, with the
-- table's schema left out, so the check holds in any schema.
--
-- # Rolling deploy
--
-- The previous binary works with the index present: it adds no column and
-- changes no statement's result, only the plans the planner may choose. But
-- one of its reads gets slower. Its text of the read keeps its bounds and
-- order on plain id, and creating the index invalidates its cached plans,
-- so its pods plan the read again and a generic plan may now take this
-- index for the session_id equality -- where the bound on id is a filter on
-- each heap row fetched, not a seek, so the read fetches every `token` frame
-- of the session. On events_token_part_idx, which that text read before,
-- the bound on id is a seek behind the part id.
--
-- Measured cell by cell on the plan test's matrix, that text with and
-- without the index: on the two shapes whose windows were logged after the
-- last ANALYZE, the generic plan of a window open above -- the session's
-- latest turn, which the approval's cut check, the decision inbox and the
-- Slack and Linear replies to an awaiting plan read -- went from about 150
-- buffers and no row removed to 463, 805 and 1,662 buffers at 8, 300 and
-- 1,000 characters of text, removing all 9,040 of the session's frames
-- outside the window. On the same shapes the window bounded above read
-- fewer buffers than before (942 to 1,763 became 463 to 1,662) but removed
-- 9,040 rows instead of 6,040; the other 84 cells read about what they
-- read without the index. Results are unchanged, and the worst cell stays
-- under that text's own worst without the index (3,225 buffers, where two
-- sessions share the log), but a long session's latest turn can cost the
-- old pods three to eleven times what it did. A rolling deploy bounds that
-- to the old pods' remaining life, within RollingDeployCeiling.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version 161"). A
-- rollback therefore takes one of two steps first, with the control plane
-- scaled to zero:
--   - Drop the index: run this migration's down (goto 160) with this
--     release's migrations. The previous binary then reads exactly as it
--     did before this release. Take this step for a rollback that will
--     run for long. The down file says what it removes.
--   - Keep the index, for a short rollback only: with the golang-migrate
--     CLI, `migrate force 160`. The previous binary then boots, since 160
--     is a version it has, but reads with the index present as above: the
--     latest turn's text of a long session can cost it three to eleven
--     times what it did, for as long as the rollback lasts. When this
--     release is deployed again, this file runs again and keeps the valid
--     index.
-- Nothing else needs undoing: no column, timer kind or event type is added.
DO $$
DECLARE
    existing regclass := to_regclass('events_token_window_idx');
    found_as text;
    is_this_index boolean;
BEGIN
    IF existing IS NOT NULL AND EXISTS (
        SELECT 1 FROM pg_index WHERE indexrelid = existing AND NOT indisvalid
    ) THEN
        DROP INDEX events_token_window_idx;
        existing := NULL;
    END IF;
    IF existing IS NULL THEN
        CREATE INDEX events_token_window_idx
            ON events (session_id, (id + 0)) WHERE type = 'token';
        RETURN;
    END IF;
    SELECT CASE WHEN i.indexrelid IS NULL
                THEN format('a relation that is not an index (pg_class.relkind %L)', c.relkind)
                ELSE pg_get_indexdef(c.oid) END,
           i.indexrelid IS NOT NULL
           AND i.indrelid = to_regclass('events')
           AND regexp_replace(pg_get_indexdef(c.oid), ' ON [^ ]+ USING ', ' ON events USING ')
               = 'CREATE INDEX events_token_window_idx ON events USING btree (session_id, ((id + 0))) WHERE (type = ''token''::text)'
    INTO found_as, is_this_index
    FROM pg_class c LEFT JOIN pg_index i ON i.indexrelid = c.oid
    WHERE c.oid = existing;
    IF is_this_index IS NOT TRUE THEN
        RAISE EXCEPTION 'events_token_window_idx exists but is not the index this migration builds: found %, want CREATE INDEX events_token_window_idx ON events (session_id, (id + 0)) WHERE type = ''token''. Drop it (DROP INDEX CONCURRENTLY events_token_window_idx for an index, or the DROP statement of its kind), clear this dirty version with `migrate force 160`, and deploy again: this migration then builds the index, or finds the one you build first as its operator guidance says.', found_as;
    END IF;
END $$;
