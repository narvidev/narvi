-- Technical plan §2: a timer kind a binary does not know is bounded by the
-- time since its row was last armed. created_at cannot say that: it is set
-- by the first insert and nothing moves it, so a kind that re-arms itself in
-- place (inactivity, liveness_check and stop do) reads as old as the session
-- after a day, however recently the binary that handles it re-armed it.
--
-- armed_at is the instant of the row's latest arm: the insert and every
-- re-arm through UpsertSessionTimer set it to now(). The pump's claim
-- (ClaimDueTimer) and the session actor's backoff of a kind it does not
-- know move only fires_at and leave it alone, so it keeps counting while
-- nothing that knows the kind touches the row.
--
-- Existing rows are backfilled from created_at: their last arm is unknown,
-- and their first arm is the latest instant this migration can vouch for.
-- A row of a kind the migrated binary knows is never aged, so the backfill
-- matters only to a kind stranded by an earlier rollback, which then keeps
-- the age it had before this migration.
--
-- NOT NULL DEFAULT now(), set after the backfill: a row the previous binary
-- inserts during the rolling deploy that ships this file names no armed_at
-- and gets its insert's instant, which is exactly its last arm.
--
-- IF NOT EXISTS and the NULL-only backfill: see "Rolling back" -- a
-- rollback that keeps the column leaves it in place, with its values, when
-- this file runs again.
--
-- # Locks
--
-- The ADD COLUMN, the backfill and SET NOT NULL run in the file's one
-- implicit transaction under ACCESS EXCLUSIVE on session_timers: the
-- backfill rewrites every row and SET NOT NULL scans them. The table holds
-- at most a few rows per live session (UNIQUE (session_id, name)), so every
-- timer arm and claim waits for an instant.
--
-- # Rolling deploy
--
-- The previous binary works with the column present. Every statement it
-- sends to session_timers names its columns (sqlc writes each SELECT * and
-- RETURNING * out as a column list), and its one INSERT leaves armed_at to
-- its default. Its re-arm (UpsertSessionTimer's ON CONFLICT) moves only
-- fires_at, so a row the previous binary re-arms keeps the armed_at of its
-- last arm by this release or of its insert. That matters only to a kind
-- this release does not know and the previous one re-arms -- a kind this
-- release retires -- which this release may then back off or delete during
-- the deploy that retires it. migration000153_integration_test.go runs the
-- previous binary's own statements against the column.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version 153"): not
-- a rolled-back pod, and not an older pod restarting in the middle of a
-- rolling deploy. A rollback therefore takes one of two steps first, with
-- the control plane scaled to zero:
--   - Keep the column: with the golang-migrate CLI, `migrate force 152`.
--     The previous binary then boots, since 152 is a version it has, and
--     works with the column present as above; the rows it re-arms keep
--     their armed_at. When this release is deployed again, this file runs
--     again and leaves the column and its values as they are.
--   - Drop it: run this migration's down (goto 152) with this release's
--     migrations. The down file says what it removes.
ALTER TABLE session_timers ADD COLUMN IF NOT EXISTS armed_at TIMESTAMPTZ;

UPDATE session_timers SET armed_at = created_at WHERE armed_at IS NULL;

ALTER TABLE session_timers ALTER COLUMN armed_at SET DEFAULT now();

ALTER TABLE session_timers ALTER COLUMN armed_at SET NOT NULL;
