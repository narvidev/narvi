-- Technical plan §40.1: a session's spend cap. The next turn of a session
-- that has spent its cap is refused (internal/domain/sessionguard), at
-- the one place every turn is created (postgres.TurnStore's
-- CreateAndArmDispatch takes an admission the guard minted) and again
-- when a queued turn is about to be dispatched. Spending is read from the
-- rows that exist -- SUM(turns.cost_usd) over the session's dispatched
-- turns -- never from a counter column.
--
-- repo_settings.session_spend_cap_usd: the cap of every session that names
-- the repository, through a clone URL in sessions.repos or a pull-request
-- claim keyed to it (a review session's github_pr_sessions row, a sentinel
-- auto-fix child's sentinel_fixes row). A session naming several
-- repositories takes the strictest cap among them.
--
-- automations.session_spend_cap_usd: the cap of every session the
-- automation creates (its automation_runs rows), which takes precedence
-- over the repository's when it is set.
--
-- NULL is "no cap", today's exact behavior, and the only spelling of it:
-- a cap of zero or less permits nothing, so it is refused when written
-- (technical plan §37) -- by the write path's own validator
-- (sessionguard.ValidateSessionSpendCap), which answers 400, and by the
-- CHECK constraints below, which back it. So is a cap that is no number:
-- NUMERIC orders NaN above every number, so "> 0" alone would admit a NaN,
-- which the guard cannot compare and would answer with a failed read on
-- every turn of the sessions it caps; "< 'Infinity'" refuses it, and the
-- infinities, which NUMERIC(10, 2) cannot hold anyway. NUMERIC(10, 2),
-- for the reason migrations/000085 gives for the review cost budget, the
-- first ceiling a person types in dollars: a NUMERIC compares exactly, a
-- float does not. The guard compares in integer micro-dollars.
--
-- automation_runs_session_id_idx: the guard reads a session's automation
-- through its automation_runs row before every turn it admits, and no
-- index of automation_runs leads with session_id. Without one the read
-- scans automation_runs, which grows with every fan-out. Partial: a run
-- whose session is gone, or that never created one, has none.
--
-- No backfill. Every existing repository and automation has no cap, so no
-- turn of any session is refused until a cap is written.
--
-- # Locks
--
-- Each ADD COLUMN is nullable with no default, a catalog change that
-- rewrites nothing; each takes ACCESS EXCLUSIVE on its table for an
-- instant. Each ADD CONSTRAINT scans its table to validate the CHECK under
-- ACCESS EXCLUSIVE: both tables hold one row a repository or an
-- automation, and every value in them is NULL here, so the scan is short.
-- The index is built in this transaction and holds a SHARE lock on
-- automation_runs for the whole build, so every automation run created,
-- started or closed out waits until it ends.
--
-- # Operator guidance for a large automation_runs table
--
-- To avoid that stall, build the index concurrently yourself before
-- deploying this version, from psql, while no control plane is starting:
--
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS automation_runs_session_id_idx
--       ON automation_runs (session_id) WHERE session_id IS NOT NULL;
--
-- then check it is valid:
--
--   SELECT indisvalid FROM pg_index WHERE indexrelid = 'automation_runs_session_id_idx'::regclass;
--
-- This migration then finds the valid index and builds nothing. A failed
-- concurrent build leaves an INVALID index behind, which Postgres never
-- plans with; the block below drops it and builds it again here, as
-- migrations/000158 does for its own -- a drop that takes ACCESS EXCLUSIVE
-- on automation_runs to the end of the transaction, so reads of it wait
-- through the rebuild too. A plain build, never CONCURRENTLY, for the
-- reason 000144 gives: golang-migrate serializes migrators with a blocking
-- advisory lock, a migrator waiting there holds a snapshot, and a
-- concurrent build waits for every older snapshot.
--
-- # Rolling deploy
--
-- The previous binary works with these columns, constraints and index
-- present:
--   - Every statement it sends names its columns (sqlc writes each
--     SELECT * and RETURNING * out as a column list), so it neither reads
--     nor writes them.
--   - It reads no cap and enforces none: its turns are created as before.
--     Every cap is NULL until a write path that sets one ships, so the two
--     binaries behave alike throughout the rollout.
-- migration000163_integration_test.go runs the previous binary's own
-- statements against the columns.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version 163"). A
-- rollback therefore takes one of two steps first, with the control plane
-- scaled to zero:
--   - Keep the columns: with the golang-migrate CLI, `migrate force 162`.
--     The previous binary then boots, since 162 is a version it has, and
--     works with the columns present as above: every cap is lifted, since
--     it reads none. When this release is deployed again, this file runs
--     again and keeps the columns, their values, the constraints and the
--     index.
--   - Drop them: run this migration's down (goto 162) with this release's
--     migrations. The down file says what it removes.
-- Nothing else needs undoing: no timer kind or event type is added.
ALTER TABLE repo_settings ADD COLUMN IF NOT EXISTS session_spend_cap_usd NUMERIC(10, 2);
ALTER TABLE automations ADD COLUMN IF NOT EXISTS session_spend_cap_usd NUMERIC(10, 2);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'repo_settings_session_spend_cap_usd_positive') THEN
        ALTER TABLE repo_settings ADD CONSTRAINT repo_settings_session_spend_cap_usd_positive
            CHECK (session_spend_cap_usd > 0 AND session_spend_cap_usd < 'Infinity');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'automations_session_spend_cap_usd_positive') THEN
        ALTER TABLE automations ADD CONSTRAINT automations_session_spend_cap_usd_positive
            CHECK (session_spend_cap_usd > 0 AND session_spend_cap_usd < 'Infinity');
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = to_regclass('automation_runs_session_id_idx') AND NOT indisvalid
    ) THEN
        DROP INDEX automation_runs_session_id_idx;
    END IF;
    IF to_regclass('automation_runs_session_id_idx') IS NULL THEN
        CREATE INDEX automation_runs_session_id_idx
            ON automation_runs (session_id) WHERE session_id IS NOT NULL;
    END IF;
END $$;
