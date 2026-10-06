-- turns_session_dispatched_idx: backs the session guard's read of what a
-- session has spent (GetSessionGuardFacts, queries/sessionguard.sql),
-- made before every turn the guard admits and again before a queued turn
-- is dispatched (technical plan §40.1): SUM(cost_usd) over the session's
-- dispatched turns.
--
-- Only a dispatched turn carries cost. A step's cost lands on the turn
-- processing when its step_finish arrives (RecordTurnStepCost), and a turn
-- reaches processing only through dispatched, which stamps dispatched_at
-- once (UpdateTurnStatus); a re-send to a new sandbox gen keeps it. So the
-- sum over dispatched_at IS NOT NULL is the sum over every turn, and the
-- read can name the predicate this partial index carries, which the
-- planner then proves under a generic plan as well as a custom one.
--
-- No index answers the read today: turns has its primary key,
-- turns_one_processing_per_session (000005, processing only),
-- (session_id, dispatched_message_id) (000131), a cost index on created_at
-- (000125) and turns_open_session_id_idx (000158, open turns only).
-- Through the third, the read fetches every turn of the session from the
-- heap for its cost, and 000158's own header records that a custom plan
-- for a long session, whose session_id the statistics know, can pick a
-- sequential scan of the whole table instead. This index leads with
-- session_id and carries cost_usd and completed_at, so the read is one
-- range of the session's own dispatched turns, answered from the index
-- when the visibility map allows; dispatched_at second keeps that range
-- ordered by when each turn was dispatched, for a read of the turns
-- dispatched since a given instant. sessionguard_plan_integration_test.go
-- measures the read, and the statements that already read turns, across a
-- matrix of table shapes under custom and generic plans.
--
-- # What it costs a write
--
-- A write adds an entry here when it makes a new row version of a
-- dispatched turn and is not heap-only. None of the writes that do is
-- heap-only today: a dispatch and a turn's end change status, which the
-- predicates of turns_one_processing_per_session and
-- turns_open_session_id_idx read; a step's cost changes cost_usd, which
-- turns_cost_created_at_idx's predicate reads. So each such write adds one
-- entry to this index -- a descent from its root to a leaf, which
-- sessionguard_plan_integration_test.go measures -- and never turns a
-- heap-only update into one that is not. A turn inserted pending, and every
-- write while it stays pending, adds nothing: it is outside the predicate.
--
-- # A plain build, not CONCURRENTLY
--
-- This builds the index in the migration's own transaction and holds a
-- SHARE lock on turns for the whole build, so every write to turns -- a
-- turn inserted, dispatched or ended, across all sessions -- waits until it
-- ends. The build scans the whole table whatever the partial predicate, so
-- the stall grows linearly with the table, and more on a cold cache; with
-- no lock_timeout, a transaction already holding ROW EXCLUSIVE on turns
-- when the build starts delays its lock, with every later write queued
-- behind it. turns is far smaller than events (one row a prompt, against
-- one a streamed frame), and 000144 measured 3M events rows in about 0.6 s.
--
-- CREATE INDEX CONCURRENTLY is not safe under this runner, for the reason
-- 000144 gives: golang-migrate serializes migrators with a blocking
-- pg_advisory_lock, a migrator waiting there holds a snapshot, and a
-- concurrent build waits for every older snapshot -- a deadlock Postgres
-- breaks by cancelling the build (an INVALID index, this version dirty,
-- every control plane refusing to boot) or the waiter (its boot fails). A
-- fresh install of deploy/control-plane (replicas: 2) runs two migrators at
-- once. A plain build takes no snapshot wait, so the waiter simply waits.
--
-- # Operator guidance for a large turns table
--
-- To avoid the stall, build the index concurrently yourself before
-- deploying this version, from psql, while no control plane is starting:
--
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS turns_session_dispatched_idx
--       ON turns (session_id, dispatched_at) INCLUDE (cost_usd, completed_at)
--       WHERE dispatched_at IS NOT NULL;
--
-- then check it is valid:
--
--   SELECT indisvalid FROM pg_index WHERE indexrelid = 'turns_session_dispatched_idx'::regclass;
--
-- This migration then finds the valid index and builds nothing, and takes
-- no lock on turns: the block below looks the index up in the catalog and
-- runs the CREATE only when there is none. (A bare CREATE INDEX IF NOT
-- EXISTS would not do: it queues for the SHARE lock on turns before it
-- sees that the index exists, so it would still wait behind every open
-- write to turns, and stall every new one behind it.) If that concurrent
-- build fails it leaves an INVALID index behind, which the block below
-- drops so the index is rebuilt here, in this transaction -- or drop it
-- yourself (DROP INDEX CONCURRENTLY turns_session_dispatched_idx) and
-- retry the concurrent build first.
--
-- # An INVALID leftover
--
-- A name check alone would keep an INVALID index of this name -- one a
-- failed concurrent build left -- and Postgres never plans with an invalid
-- index, so every guard read would walk the session's turns through the
-- heap instead. It is dropped first, in the same transaction, and built
-- again. That plain drop takes ACCESS EXCLUSIVE on turns, held to the end
-- of the transaction, so through the rebuild reads of turns wait too, not
-- only writes.
--
-- # Rolling deploy
--
-- The previous binary works with the index present: it adds no column and
-- changes no statement's result, only the plans the planner may choose.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version 166"). A
-- rollback therefore takes one of two steps first, with the control plane
-- scaled to zero:
--   - Keep the index: with the golang-migrate CLI, `migrate force 164`, the
--     previous binary's last version: 000165 ships in this release too,
--     and its own header says what forcing past it keeps. The previous
--     binary then boots, and works with the index present as above. When
--     this release is deployed again, this file runs again and keeps the
--     valid index.
--   - Drop it: run this migration's down (goto 165) with this release's
--     migrations, then 000165's as its header says. The down file says what
--     it removes.
-- Nothing else needs undoing: no column, timer kind or event type is added.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = to_regclass('turns_session_dispatched_idx') AND NOT indisvalid
    ) THEN
        DROP INDEX turns_session_dispatched_idx;
    END IF;
    IF to_regclass('turns_session_dispatched_idx') IS NULL THEN
        CREATE INDEX turns_session_dispatched_idx
            ON turns (session_id, dispatched_at) INCLUDE (cost_usd, completed_at)
            WHERE dispatched_at IS NOT NULL;
    END IF;
END $$;
