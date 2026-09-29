-- Reverses 000150_session_create_idempotency_key.up.sql. The stored keys go
-- with the columns, so a create retried after this runs is not recognised as
-- a replay.
DROP INDEX IF EXISTS sessions_create_idempotency_key_uniq;

ALTER TABLE sessions
    DROP CONSTRAINT IF EXISTS sessions_create_idempotency_pair_check,
    DROP COLUMN IF EXISTS create_request_sha256,
    DROP COLUMN IF EXISTS create_idempotency_key;
