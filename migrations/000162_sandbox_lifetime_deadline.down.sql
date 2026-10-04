-- Reverses 000162_sandbox_lifetime_deadline.up.sql. With the columns goes
-- every sandbox's recorded lifetime deadline. Nothing a binary without
-- 000162 reads; a binary with it that runs 000162 again reads every
-- sandbox's deadline as unknown until its next spawn or restore.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000162. Do not run it against live pods of a binary that carries 000162,
-- for two reasons:
--   - Every query of that binary that returns a whole sandbox row names
--     these columns (sqlc writes each SELECT * and RETURNING * out as a
--     column list): creating, reading and updating sandboxes among them,
--     and its UpsertSandboxForSpawn writes them. After this down each of
--     them fails with SQLSTATE 42703 (column does not exist), so no sandbox
--     is spawned or restored and no turn is dispatched or completed until
--     the older binary replaces the pod.
--   - A pod that carries 000162 and restarts before the older binary
--     replaces it applies 000162 again at boot, which locks the older binary
--     out again ("no migration found for version 162").
-- Guarded: IF EXISTS, so it also runs when the columns are already gone --
-- this down ran, the recorded version was then forced back to 162, and the
-- down runs again. (With the columns kept, after `migrate force 161`, a
-- plain DROP COLUMN would run anyway.) Each drop is a catalog change: it
-- rewrites nothing, but takes ACCESS EXCLUSIVE on sandboxes for the file's
-- one implicit transaction.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS lifetime_deadline_gen;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS lifetime_seconds;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS lifetime_deadline_at;
