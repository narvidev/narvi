-- Reverses 000167_review_turn_checkout.up.sql. With the columns go every
-- turn's checkout request, its count of sends and failures, the gen its
-- failures retired, the commit it was checked out at, and every sandbox's
-- record of the gen that can check out a review's head. A binary without
-- 000167 reads none of them and dispatches review turns unchecked; a
-- binary with it that runs 000167 again asks every pending review turn for
-- a checkout at its next dispatch evaluation, and reads every sandbox as
-- incapable until its next ready.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000167. Do not run it against live pods of a binary that carries 000167,
-- for two reasons:
--   - Every query of that binary that returns a whole turn or sandbox row
--     names these columns (sqlc writes each SELECT * and RETURNING * out as
--     a column list): creating, reading and updating turns and sandboxes
--     among them. After this down each of them fails with SQLSTATE 42703
--     (column does not exist), so no turn is created, dispatched or
--     completed until the older binary replaces the pod.
--   - A pod that carries 000167 and restarts before the older binary
--     replaces it applies 000167 again at boot, which locks the older binary
--     out again ("no migration found for version 167").
-- Guarded: IF EXISTS, so it also runs when the columns are already gone --
-- this down ran, the recorded version was then forced back to 167, and the
-- down runs again. (With the columns kept, after `migrate force 166`, a
-- plain DROP COLUMN would run anyway.) Each drop is a catalog change: it
-- rewrites nothing, but takes ACCESS EXCLUSIVE on its table for the file's
-- one implicit transaction.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS review_checkout_gen;
ALTER TABLE turns DROP COLUMN IF EXISTS checked_out_sha;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_retired_gen;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_failures;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_sends;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_sent_ready_seq;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_sent_at;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_requested_at;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_gen;
ALTER TABLE turns DROP COLUMN IF EXISTS checkout_message_id;
