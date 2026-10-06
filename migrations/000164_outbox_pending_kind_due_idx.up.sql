-- outbox_pending_kind_due_idx: backs ListDuePendingOutboxEntriesOfKinds
-- (queries/outbox.sql), the claim of one lane of an outbox pump tick while
-- the autonomy freeze holds some kinds (technical plan §40.2): the kinds
-- the freeze holds in one lane, every other kind in the other, so the rows
-- the freeze keeps due again every minute never take a notification's slot
-- (outboxworker's claimBatch).
--
-- No index held that read. The outbox has its primary key and the
-- dead-letter index (000065) only, and its delivered rows are never
-- deleted, so each lane planned as a scan of the whole table, sorted --
-- two such scans a tick while frozen where one ran before, on every
-- replica every five seconds, growing with every row the outbox ever
-- delivered. This partial index holds the pending rows only, by kind and
-- then by when each is due, so a lane seeks its kinds' due range and reads
-- those rows and no other: under a custom plan and the generic plan alike,
-- whatever the table holds. outbox_lanes_plan_integration_test.go holds
-- every plan to that, on an outbox of 50,000 and of 400,000 delivered rows
-- beside 9,000 pending ones: each lane read 2,469 and then 17,052 buffers
-- before this index, a sequential scan removing every other row, and reads
-- 161 to 169 on both outboxes with it, through this index, removing none.
--
-- # What it costs a write
--
-- Every pending row adds an entry, a kind and a timestamp; a delivered or
-- dead-lettered row leaves the index. Each claim, renewal and outcome of a
-- delivery changes next_attempt_at or status, both now indexed, so none of
-- those updates is heap-only any more: each new row version also gets an
-- entry in the primary key, and in this index while it stays pending.
--
-- # A plain build, not CONCURRENTLY
--
-- This builds the index in the migration's own transaction and holds a
-- SHARE lock on outbox for the whole build, so every enqueue and every
-- outcome the outbox worker records waits until it ends; reads of outbox
-- go on. The build scans the whole table whatever the partial predicate:
-- measured on Postgres 17 with the outbox at 409,000 rows (400,000
-- delivered, each with a payload of about 200 bytes), warm cache, about
-- 80 ms, an ANALYZE of the table included. The stall grows linearly with
-- the table, and more on a cold cache; and with no lock_timeout, a
-- transaction already holding ROW EXCLUSIVE on outbox when the build starts
-- delays its lock, with every later write queued behind it.
--
-- One path blocks reads as well: an INVALID leftover of this index (below).
-- The block drops it with a plain DROP INDEX, which takes ACCESS EXCLUSIVE
-- on outbox and holds it until the migration commits, through the whole
-- build that follows.
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
-- # Operator guidance for a large outbox
--
-- To avoid the stall, build the index concurrently yourself before
-- deploying this version, from psql, while no control plane is starting:
--
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_pending_kind_due_idx
--       ON outbox (kind, next_attempt_at) WHERE status = 'pending';
--
-- then check it is valid:
--
--   SELECT indisvalid FROM pg_index WHERE indexrelid = 'outbox_pending_kind_due_idx'::regclass;
--
-- This migration then finds the valid index and builds nothing, and takes
-- no lock on outbox: the block below looks the index up in the catalog and
-- runs the CREATE only when there is none. Type the statement above as it
-- is: the block checks that an index it finds under this name is that
-- index, and fails the migration if it is not (below).
--
-- If that concurrent build fails it leaves an INVALID index behind. Drop it
-- yourself, without blocking anything --
--
--   DROP INDEX CONCURRENTLY outbox_pending_kind_due_idx;
--
-- -- and retry the concurrent build before deploying. Left in place, the
-- leftover is dropped and the index rebuilt by the block below, in this
-- migration's transaction, which blocks every read and write of outbox for
-- the whole build (see above).
--
-- # An INVALID leftover
--
-- A name check alone would keep an INVALID index of this name -- one a
-- failed concurrent build left -- and Postgres never plans with an invalid
-- index, so every lane would scan the table instead. It is dropped first,
-- in the same transaction, and built again.
--
-- # A relation of this name that is not this index
--
-- A valid index of this name with another definition -- next_attempt_at
-- alone, say, or the columns without the predicate -- would leave the
-- lanes reading other kinds' rows or every row ever delivered. The block
-- does not keep such a relation, and it does not drop it either: it fails
-- the migration, naming what it found and what to do, and golang-migrate
-- leaves this version recorded as dirty, so no control plane boots until
-- the operator has dropped the relation and forced the version back (the
-- message says how). The definition is compared as pg_get_indexdef prints
-- it, with quote_all_identifiers off for the block's own transaction and
-- the table's schema-qualified name taken out exactly as pg_get_indexdef
-- writes it, as 000161 does.
--
-- # Rolling deploy
--
-- The previous binary works with the index present: it adds no column and
-- changes no statement's result, only the plans the planner may choose.
-- Its outbox reads -- the claim of a tick's one batch and the pending
-- count -- read no more with it, and much less: measured by the plan test
-- on the same outboxes, from 2,469 and 17,052 buffers to 301 for the
-- claim, and from 2,449 and 17,032 to 401 for the count, each now a
-- bitmap scan of this index's pending rows.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version N", N being
-- this file's own number). A rollback therefore takes one of two steps
-- first, with the control plane scaled to zero, V being the version before
-- this file's:
--   - Drop the index: run this migration's down (`migrate goto V`) with
--     this release's migrations. The previous binary then reads exactly as
--     it did before this release.
--   - Keep the index: with the golang-migrate CLI, `migrate force V`. The
--     previous binary then boots and reads with the index present, as
--     above. When this release is deployed again, this file runs again and
--     keeps the valid index.
-- Nothing else needs undoing: no column, timer kind or event type is added.
DO $$
DECLARE
    existing regclass := to_regclass('outbox_pending_kind_due_idx');
    found_as text;
    is_this_index boolean;
BEGIN
    IF existing IS NOT NULL AND EXISTS (
        SELECT 1 FROM pg_index WHERE indexrelid = existing AND NOT indisvalid
    ) THEN
        DROP INDEX outbox_pending_kind_due_idx;
        existing := NULL;
    END IF;
    IF existing IS NULL THEN
        CREATE INDEX outbox_pending_kind_due_idx
            ON outbox (kind, next_attempt_at) WHERE status = 'pending';
        RETURN;
    END IF;
    PERFORM set_config('quote_all_identifiers', 'off', true);
    SELECT CASE WHEN i.indexrelid IS NULL
                THEN format('a relation that is not an index (pg_class.relkind %L)', c.relkind)
                ELSE pg_get_indexdef(c.oid) END,
           i.indexrelid IS NOT NULL
           AND i.indrelid = to_regclass('outbox')
           AND replace(pg_get_indexdef(c.oid),
                       ' ON ' || format('%I.%I', tn.nspname, t.relname) || ' USING ',
                       ' ON outbox USING ')
               = 'CREATE INDEX outbox_pending_kind_due_idx ON outbox USING btree (kind, next_attempt_at) WHERE (status = ''pending''::outbox_status)'
    INTO found_as, is_this_index
    FROM pg_class c
    LEFT JOIN pg_index i ON i.indexrelid = c.oid
    LEFT JOIN pg_class t ON t.oid = i.indrelid
    LEFT JOIN pg_namespace tn ON tn.oid = t.relnamespace
    WHERE c.oid = existing;
    IF is_this_index IS NOT TRUE THEN
        RAISE EXCEPTION 'outbox_pending_kind_due_idx exists but is not the index this migration builds: found %, want CREATE INDEX outbox_pending_kind_due_idx ON outbox (kind, next_attempt_at) WHERE status = ''pending''. Drop it (DROP INDEX CONCURRENTLY outbox_pending_kind_due_idx for an index, or the DROP statement of its kind), clear this dirty version with `migrate force` to the version before this one, and deploy again: this migration then builds the index, or finds the one you build first as its operator guidance says.', found_as;
    END IF;
END $$;
