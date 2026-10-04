-- Reverses 000159_turns_end_reason.up.sql. With the columns go which turns
-- ended context_moved, which started with their context unconfirmed, which
-- the automatic re-review asked for, and every pull request's count of
-- moves and record of a drop. A binary without 000159 reads none of them,
-- and reads a context_moved turn as the failed turn its status is; a binary
-- with it that runs 000159 again reads every earlier turn as having no
-- end reason -- counted as the attempt or turn it was -- and no trigger,
-- and every pull request at no move and no drop.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000159. Do not run it against live pods of a binary that carries 000159,
-- for two reasons:
--   - Every query of that binary that returns a whole turn or
--     github_pr_sessions row names these columns (sqlc writes each
--     SELECT * and RETURNING * out as a column list): creating, reading and
--     updating turns among them. After this down each of them fails with
--     SQLSTATE 42703 (column does not exist), so no turn is created,
--     dispatched or completed until the older binary replaces the pod.
--   - A pod that carries 000159 and restarts before the older binary
--     replaces it applies 000159 again at boot, which locks the older binary
--     out again ("no migration found for version 159").
-- Guarded: IF EXISTS, so it also runs when the columns are already gone --
-- this down ran, the recorded version was then forced back to 159, and the
-- down runs again. (With the columns kept, after `migrate force 158`, a
-- plain DROP COLUMN would run anyway.) Each drop is a catalog change: it
-- rewrites nothing, but takes ACCESS EXCLUSIVE on its table for the file's
-- one implicit transaction.
ALTER TABLE github_pr_sessions DROP COLUMN IF EXISTS auto_retrigger_dropped_head_sha;
ALTER TABLE github_pr_sessions DROP COLUMN IF EXISTS auto_retrigger_dropped_at;
ALTER TABLE github_pr_sessions DROP COLUMN IF EXISTS auto_retrigger_context_moves;
ALTER TABLE turns DROP COLUMN IF EXISTS request_trigger;
ALTER TABLE turns DROP COLUMN IF EXISTS context_unconfirmed_at;
ALTER TABLE turns DROP COLUMN IF EXISTS end_reason;
