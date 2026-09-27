-- events_token_part_idx: backs GetLatestTokenFrameForPart
-- (queries/events.sql), the one lookup the session actor makes before it
-- stores a streamed `token` frame.
--
-- A `token` event carries the CUMULATIVE text of one assistant text part,
-- and the agent runtime sends several frames for the same part, all under
-- the same wire messageId (the part id; the pinned runtime sends two, an
-- empty one when the part opens and the full text when it closes).
-- events_session_id_message_id_idx (migrations/000019) makes the first
-- frame win, so from 2026-07-20 until this migration every stored text
-- part was its first frame: an empty string or a prefix. The premise
-- written into 000019 -- "a single session can never legitimately see the
-- same messageId twice from two DIFFERENT genuine events" -- does not hold
-- for `token`. The actor now stores each DISTINCT frame as its own row,
-- under the storage key messageId + "#" + a hash of the frame's text
-- (sessionactor/tokenframe.go), so the log stays append-only and a
-- byte-identical resend still dedupes on 000019's index.
--
-- Before it stores a frame, the actor reads the part's newest stored frame
-- (by id) and its first one: it adds no row when the incoming frame is
-- that newest frame again or an older one replayed late, nor when the
-- part's first frame lies at or below the Processing turn's
-- dispatched_event_id -- a frame of a part from an earlier turn. Both
-- reads are keyed by the payload's own messageId -- the storage key has a
-- per-frame suffix, and the frames stored before this migration have none
-- -- which no existing index covers: this one does, and only for `token`
-- rows, so the rest of the table (every other event type) pays nothing
-- for it. id is the last column, so a part's newest frame is the first
-- entry of a backward scan and its first frame the first of a forward one.
--
-- Frames stored before this migration stay as they were. Their later
-- frames were never stored, and a sandbox that still holds them in its
-- outbound buffer replays them only after their turn has ended, when the
-- actor adds no row for them: that history stays truncated.
--
-- # A plain build, not CONCURRENTLY
--
-- This builds the index in the migration's own transaction and holds a
-- SHARE lock on events for the whole build, so every insert into events
-- -- every session's sandbox events, across all sessions -- waits until it
-- ends. Measured on Postgres 17 with events at 3M rows (2 GB, 600k of them
-- `token`), warm cache: the build took about 0.6 s and concurrent inserts
-- waited up to 0.65 s. The build scans the whole table whatever the
-- partial predicate, so the stall grows linearly with the table and more
-- on a cold cache; and with no lock_timeout, a transaction already
-- holding ROW EXCLUSIVE on events when the build starts delays its lock,
-- with every later insert queued behind it.
--
-- CREATE INDEX CONCURRENTLY would not block inserts, and golang-migrate
-- (v4.19.1) could send it -- one ExecContext per file, no transaction of
-- its own -- but it is not safe under this runner. golang-migrate
-- serializes migrators with a blocking pg_advisory_lock, and a migrator
-- waiting there holds a snapshot; a concurrent build waits for every
-- older snapshot before it finishes, so it waits for the waiter while the
-- waiter waits for its lock. Postgres detects the cycle after
-- deadlock_timeout and cancels one side (both reproduced on Postgres 17):
-- the build, which leaves an INVALID index and this version dirty, so
-- every control plane then refuses to boot until an operator intervenes;
-- or the waiter's lock request, which fails that control plane's boot.
-- Two migrators at once is not a corner case: a fresh install of
-- deploy/control-plane (replicas: 2) boots both replicas together, and
-- both run the migrations. TestMigration000144_ConcurrentMigrators runs
-- exactly that. A plain build takes no snapshot wait, so the waiter simply
-- waits for it.
--
-- # Operator guidance for a large events table
--
-- To avoid the stall, build the index concurrently yourself before
-- deploying this version, from psql, while no control plane is starting:
--
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS events_token_part_idx
--       ON events (session_id, (payload->>'messageId'), id) WHERE type = 'token';
--
-- then check it is valid:
--
--   SELECT indisvalid FROM pg_index WHERE indexrelid = 'events_token_part_idx'::regclass;
--
-- This migration then finds the valid index and builds nothing. If that
-- concurrent build fails it leaves an INVALID index behind, which the
-- block below drops so the index is rebuilt here, in this transaction --
-- or drop it yourself (DROP INDEX CONCURRENTLY events_token_part_idx) and
-- retry the concurrent build first.
--
-- # An INVALID leftover
--
-- IF NOT EXISTS alone would keep an INVALID index of this name -- one a
-- failed concurrent build left -- and Postgres never plans with an
-- invalid index, so every `token` frame would scan instead. It is dropped
-- first, in the same transaction, and built again below.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = to_regclass('events_token_part_idx') AND NOT indisvalid
    ) THEN
        DROP INDEX events_token_part_idx;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS events_token_part_idx
    ON events (session_id, (payload->>'messageId'), id) WHERE type = 'token';
