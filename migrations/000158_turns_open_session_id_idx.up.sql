-- turns_open_session_id_idx: backs ReviewRetriggerHeld (queries/turns.sql),
-- the read the re-review debounce makes before it inserts an automatic
-- review (technical plan §24.9): does any turn of this session sit outside
-- completed, failed and cancelled? While one does, the automatic review
-- holds -- the pushed head stays the target, no budget is spent, and the
-- debounce re-arms itself -- and every write that ends a turn moves the
-- debounce to now in its own transaction.
--
-- The read is an EXISTS, and no index answers it today: turns has its
-- primary key, turns_one_processing_per_session (000005, processing only),
-- (session_id, dispatched_message_id) (000131) and a cost index (000125).
-- Through the second, the read walks every turn of the session through the
-- heap, and a custom plan for a long session, whose session_id the
-- statistics know, can pick a sequential scan of the whole table: an
-- EXISTS is costed to its first match, and open turns are a tiny fraction
-- of any table. This partial index holds the open turns only -- a few per
-- session at most, whatever the table's size -- so the read is one probe.
-- The query names the same literal status list as the predicate below, so
-- the planner proves it under a generic plan too.
--
-- The predicate is a deny list of the three terminal states, the one
-- turn.IsTerminal reads: a state added to turn_status later counts as open,
-- the safe direction for a hold. The statements that already read a
-- session's open or in-flight turns (RequestStopOpenTurns,
-- ListStopRequestedOpenTurns, GetProcessingTurnForSession, and
-- GetSessionActivityFacts' in-flight lateral) may plan with it too;
-- reviewretriggerhold_plan_integration_test.go measures each before and
-- after this migration.
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
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS turns_open_session_id_idx
--       ON turns (session_id) WHERE status NOT IN ('completed', 'failed', 'cancelled');
--
-- then check it is valid:
--
--   SELECT indisvalid FROM pg_index WHERE indexrelid = 'turns_open_session_id_idx'::regclass;
--
-- This migration then finds the valid index and builds nothing. If that
-- concurrent build fails it leaves an INVALID index behind, which the
-- block below drops so the index is rebuilt here, in this transaction --
-- or drop it yourself (DROP INDEX CONCURRENTLY turns_open_session_id_idx)
-- and retry the concurrent build first.
--
-- # An INVALID leftover
--
-- IF NOT EXISTS alone would keep an INVALID index of this name -- one a
-- failed concurrent build left -- and Postgres never plans with an invalid
-- index, so every hold read would walk the session's turns instead. It is
-- dropped first, in the same transaction, and built again below.
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
-- previous binary cannot boot ("no migration found for version 158"). A
-- rollback therefore takes one of two steps first, with the control plane
-- scaled to zero:
--   - Keep the index: with the golang-migrate CLI, `migrate force 157`.
--     The previous binary then boots, since 157 is a version it has, and
--     works with the index present as above. When this release is deployed
--     again, this file runs again and keeps the valid index.
--   - Drop it: run this migration's down (goto 157) with this release's
--     migrations. The down file says what it removes.
-- Nothing else needs undoing: no column, timer kind or event type is added.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = to_regclass('turns_open_session_id_idx') AND NOT indisvalid
    ) THEN
        DROP INDEX turns_open_session_id_idx;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS turns_open_session_id_idx
    ON turns (session_id) WHERE status NOT IN ('completed', 'failed', 'cancelled');
