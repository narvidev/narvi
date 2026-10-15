-- Reverses 000171_sandbox_rotation.up.sql. With the columns go any rotation
-- in progress and the start of any snapshot in progress. Nothing a binary
-- without 000171 reads; a binary with it that runs 000171 again finds no
-- rotation in progress -- a gen that was rotating is then rotated afresh
-- once it is ready again -- and gives a snapshot in progress a start at its
-- first look.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000171. Do not run it against live pods of a binary that carries 000171,
-- for two reasons:
--   - Every query of that binary that returns a whole sandbox row names
--     these columns (sqlc writes each SELECT * and RETURNING * out as a
--     column list): creating, reading and updating sandboxes among them,
--     and its runway read, its rotation start and the statements that set
--     and clear a pending snapshot write them. After this down each of them
--     fails with SQLSTATE 42703 (column does not exist), so no sandbox is
--     spawned, restored or snapshotted and no turn is dispatched or
--     completed until the older binary replaces the pod.
--   - A pod that carries 000171 and restarts before the older binary
--     replaces it applies 000171 again at boot, which locks the older binary
--     out again ("no migration found for version 171").
-- Guarded: IF EXISTS, so it also runs when the columns are already gone --
-- this down ran, the recorded version was then forced back to 171, and the
-- down runs again. Each drop is a catalog change: it rewrites nothing, but
-- takes ACCESS EXCLUSIVE on sandboxes for the file's one implicit
-- transaction.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS snapshot_started_at;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS rotation_started_at;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS rotation_gen;
