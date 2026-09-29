-- Reverses 000150_session_create_idempotency_key.up.sql. The stored keys go
-- with the columns, so a create retried after this runs is not recognised as
-- a replay, and can start a second session.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000150. Do not run it against live pods of a binary that carries 000150,
-- for two reasons:
--   - Every query of that binary that returns a whole session row names
--     these columns (sqlc writes each SELECT * and RETURNING * out as a
--     column list): session create, read, list and status change among
--     them. After this down each of them fails with SQLSTATE 42703
--     (column does not exist), and every route built on them answers 500
--     until the older binary replaces the pod.
--   - A pod that carries 000150 and restarts before the older binary
--     replaces it applies 000150 again at boot, which locks the older binary
--     out again ("no migration found for version 150").
-- The drops are catalog changes: they rewrite nothing, but take ACCESS
-- EXCLUSIVE on sessions for the file's one implicit transaction.
DROP INDEX IF EXISTS sessions_create_idempotency_key_uniq;

ALTER TABLE sessions
    DROP CONSTRAINT IF EXISTS sessions_create_idempotency_pair_check,
    DROP COLUMN IF EXISTS create_request_sha256,
    DROP COLUMN IF EXISTS create_idempotency_key;
