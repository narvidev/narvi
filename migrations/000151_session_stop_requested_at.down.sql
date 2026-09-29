-- Reverses 000151_session_stop_requested_at.up.sql. Every pending stop
-- request goes with the columns: a turn flagged but not yet cancelled then
-- dispatches as if no stop had been asked for, a stopped parent accepts
-- new children again, and a gen whose retirement is owed takes the next
-- turn. Every armed `stop` timer is deleted too: its request
-- is gone, and a binary without 000151 does not know the name, so it would
-- leave the timer armed, redeliver it every claim window, and read it as
-- scheduled work (sessionactor.TimerCanCreateWork).
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000151. Do not run it against live pods of a binary that carries 000151,
-- for two reasons:
--   - Every query of that binary that returns a whole session, turn or
--     sandbox row names these columns (sqlc writes each SELECT * and
--     RETURNING * out as a column list): session, turn and sandbox create,
--     read, list and status change among them. After this down each of them fails with SQLSTATE 42703
--     (column does not exist), and every route and every session actor
--     built on them fails until the older binary replaces the pod.
--   - A pod that carries 000151 and restarts before the older binary
--     replaces it applies 000151 again at boot, which locks the older binary
--     out again ("no migration found for version 151").
-- The drops are catalog changes: they rewrite nothing, but take ACCESS
-- EXCLUSIVE on sessions, turns and sandboxes for the file's one implicit
-- transaction, taken in the up's order.
DELETE FROM session_timers WHERE name = 'stop';

ALTER TABLE sandboxes DROP COLUMN IF EXISTS stop_retire_gen;

ALTER TABLE turns DROP COLUMN IF EXISTS stop_requested_at;

ALTER TABLE sessions DROP COLUMN IF EXISTS stop_requested_at;
