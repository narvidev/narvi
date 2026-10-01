-- Reverses 000154_prompt_receipts.up.sql. With the columns go every
-- turn's record of the receipt its dispatch asked for, and every sandbox's
-- ready count and capability gen. Nothing a binary without 000154 reads; a
-- binary with it that runs 000154 again reads every turn as not asked and
-- every sandbox as not capable until its next ready, which is the
-- behavior before this release -- a lost prompt then waits out
-- turn_deadline, never re-sent. Stored prompt_received events stay: they
-- are inert to every binary.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000154. Do not run it against live pods of a binary that carries 000154,
-- for two reasons:
--   - Every query of that binary that returns a whole turn or sandbox row
--     names these columns (sqlc writes each SELECT * and RETURNING * out as
--     a column list): creating, reading and updating turns and sandboxes
--     among them. After this down each of them fails with SQLSTATE 42703
--     (column does not exist), so no turn is dispatched or completed until
--     the older binary replaces the pod.
--   - A pod that carries 000154 and restarts before the older binary
--     replaces it applies 000154 again at boot, which locks the older binary
--     out again ("no migration found for version 154").
-- Guarded: IF EXISTS, so it also runs on a database a rollback already
-- brought back to 153 with the columns kept (`migrate force 153`, the up
-- file's "Rolling back") and then forced to 154 again. Each drop is a
-- catalog change: it rewrites nothing, but takes ACCESS EXCLUSIVE on
-- sandboxes, then on turns, for the file's one implicit transaction.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS ready_seq;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS prompt_receipt_gen;
ALTER TABLE turns DROP COLUMN IF EXISTS receipt_checked_ready_seq;
ALTER TABLE turns DROP COLUMN IF EXISTS receipt_requested_at;
ALTER TABLE turns DROP COLUMN IF EXISTS receipt_requested_message_id;
