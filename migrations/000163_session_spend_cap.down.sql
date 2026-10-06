-- Reverses 000163_session_spend_cap.up.sql. With the columns goes every
-- session spend cap a repository or an automation was given, and with them
-- every refusal they would cause: nothing a binary without 000163 reads. A
-- binary with it that runs 000163 again reads every cap as NULL, no cap,
-- until one is written again.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000163. Do not run it against live pods of a binary that carries 000163,
-- for two reasons:
--   - Every query of that binary that returns a whole repo_settings or
--     automations row names these columns (sqlc writes each SELECT * and
--     RETURNING * out as a column list), and its turn admission reads them.
--     After this down each of those fails with SQLSTATE 42703 (column does
--     not exist), so no turn is created and no queued turn is dispatched
--     until the older binary replaces the pod.
--   - A pod that carries 000163 and restarts before the older binary
--     replaces it applies 000163 again at boot, which locks the older binary
--     out again ("no migration found for version 163").
-- Guarded: IF EXISTS, so it also runs when the index, constraints or
-- columns are already gone -- this down ran, the recorded version was then
-- forced back to 163, and the down runs again. Dropping the index takes
-- ACCESS EXCLUSIVE on automation_runs for an instant; each other drop is a
-- catalog change that rewrites nothing, under ACCESS EXCLUSIVE on its table
-- for the file's one implicit transaction.
DROP INDEX IF EXISTS automation_runs_session_id_idx;
ALTER TABLE automations DROP CONSTRAINT IF EXISTS automations_session_spend_cap_usd_positive;
ALTER TABLE repo_settings DROP CONSTRAINT IF EXISTS repo_settings_session_spend_cap_usd_positive;
ALTER TABLE automations DROP COLUMN IF EXISTS session_spend_cap_usd;
ALTER TABLE repo_settings DROP COLUMN IF EXISTS session_spend_cap_usd;
