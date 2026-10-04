-- Reverses 000160_owed_review_requests.up.sql. With the table go every
-- request owed when it runs: none of them is re-run, and no requester is
-- told. With the columns go who asked for each turn, the lane's text and
-- the moves a re-run's request met; a binary with 000160 that runs it
-- again reads every earlier turn as recording none of them.
--
-- Every armed owed_review_request timer is deleted too. The requests it
-- stood for go with the table, so it has nothing left to deliver, and a
-- binary without 000160 does not know the kind: it would keep the row
-- under technical plan §2's bound for up to UnknownTimerDeleteAfter after
-- its last arm, the session reading scheduled all that time. The kind is
-- armed due at once, so §2's bound alone would also clear it; deleting it
-- here lets the sessions read settled as soon as the rollback runs.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000160. Do not run it against live pods of a binary that carries 000160,
-- for two reasons:
--   - Every query of that binary that returns a whole turn names these
--     columns (sqlc writes each SELECT * and RETURNING * out as a column
--     list): creating, reading and updating turns among them, and its
--     re-review hold reads owed_review_requests. After this down each of
--     them fails with SQLSTATE 42703 or 42P01 (column or table does not
--     exist), so no turn is created, dispatched or completed until the
--     older binary replaces the pod.
--   - A pod that carries 000160 and restarts before the older binary
--     replaces it applies 000160 again at boot, which locks the older binary
--     out again ("no migration found for version 160").
-- Guarded: IF EXISTS, so it also runs when the table and the columns are
-- already gone -- this down ran, the recorded version was then forced back
-- to 160, and the down runs again. The drops are catalog changes: they
-- rewrite nothing, but take ACCESS EXCLUSIVE on turns and on the table for
-- the file's one implicit transaction.
DELETE FROM session_timers WHERE name = 'owed_review_request';
DROP TABLE IF EXISTS owed_review_requests;
ALTER TABLE turns DROP COLUMN IF EXISTS context_moves;
ALTER TABLE turns DROP COLUMN IF EXISTS request_text;
ALTER TABLE turns DROP COLUMN IF EXISTS requested_by;
