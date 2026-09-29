-- Step 183 (§43.8, "plan, revision and stop over MCP"): an idempotent session
-- create. POST /api/sessions accepts an optional idempotencyKey (contracts
-- 1.11.0), and the narvi_create_session MCP tool always sends one, so a
-- client that retries a create whose answer it lost does not start, and pay
-- for, a second session.
--
-- The key is kept with the session it created, beside the SHA-256 of the
-- request it came with (the CreateSessionRequest re-encoded without the
-- key, httpapi.createRequestSHA256). A later create by the same user with
-- the same key is a replay: the same hash answers that session, a different
-- hash is refused 409. Keys are scoped per user: the unique index is on
-- (created_by, key), so two users choosing the same UUID never collide.
-- Sessions with no creator (bot ingress) never carry a key.
--
-- The CHECK keeps the pair together: a key is never stored without the
-- 32-byte hash a replay is compared against, and a hash never without its
-- key. The hash's IS NOT NULL is spelled out: a CHECK passes when it
-- evaluates to NULL, and octet_length(NULL) = 32 is NULL, so without it a
-- key with no hash would pass.
--
-- Two concurrent creates with the same key both find no row, and both
-- insert; the second insert waits on the unique index until the first
-- commits, then fails with 23505 on sessions_create_idempotency_key_uniq,
-- and the handler reads the winner back. One row results either way.
--
-- # Locks
--
-- ADD COLUMN with no default is a catalog change. The CHECK is verified
-- against every existing row (all NULL here) under the ALTER's ACCESS
-- EXCLUSIVE lock, and the index build scans sessions under a SHARE lock --
-- one row per session, far smaller than events (compare 000144, whose
-- reasoning for a plain build over CONCURRENTLY under golang-migrate's
-- advisory lock applies here unchanged). The partial index holds only rows
-- that carry a key, so it starts empty.
--
-- Reversible: the down drops the index, the constraint and both columns.
-- Rolling back loses the stored keys, so a retry after a rollback can start
-- a second session; nothing else reads them.
ALTER TABLE sessions
    ADD COLUMN create_idempotency_key uuid,
    ADD COLUMN create_request_sha256 bytea,
    ADD CONSTRAINT sessions_create_idempotency_pair_check CHECK (
        (create_idempotency_key IS NULL AND create_request_sha256 IS NULL)
        OR (create_idempotency_key IS NOT NULL AND create_request_sha256 IS NOT NULL
            AND octet_length(create_request_sha256) = 32)
    );

CREATE UNIQUE INDEX sessions_create_idempotency_key_uniq
    ON sessions (created_by, create_idempotency_key)
    WHERE create_idempotency_key IS NOT NULL;
