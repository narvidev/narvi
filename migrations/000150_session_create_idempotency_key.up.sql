-- Step 183 (§43.8, "plan, revision and stop over MCP"): an idempotent session
-- create. POST /api/sessions accepts an optional idempotencyKey (contracts
-- 1.11.0), and the narvi_create_session MCP tool always sends one, so a
-- client that retries a create whose answer it lost does not start, and pay
-- for, a second session.
--
-- The key is kept with the session it created, beside the SHA-256 of the
-- request it came with (the request's canonical form, without the key,
-- httpapi.createRequestSHA256). A later create by the same user with the
-- same key is a replay: the same hash answers that session, a different
-- hash is refused 409, and so is a key whose session records another
-- source (web or mcp) than the replay would. Keys are scoped per user: the
-- unique index is on (created_by, key), so two users choosing the same UUID
-- never collide. Sessions with no creator (bot ingress) never carry a key.
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
-- golang-migrate sends this whole file as one batch, which Postgres runs as
-- one implicit transaction (000149 says the same of its own file). ADD
-- COLUMN with no default is a catalog change, but the ALTER TABLE takes
-- ACCESS EXCLUSIVE on sessions and the transaction holds it until the file
-- ends: through the CHECK's scan of every row (all NULL here), and through
-- the index build too, which on its own would need only SHARE. So every
-- query on sessions, reads as well as writes -- every session list, read,
-- create and status change -- waits for the whole migration. The table has
-- one row per session, far smaller than events (compare 000144), but the
-- stall still grows with it: the build scans the whole table whatever its
-- partial predicate, although the index starts empty, since it holds only
-- rows that carry a key. With no lock_timeout, a transaction already
-- holding a lock on sessions when the migration starts delays the ALTER,
-- and every later query on sessions queues behind it.
--
-- The build is plain, not CONCURRENTLY, for 000144's reason: a concurrent
-- build waits for every older snapshot, that of a second control plane
-- waiting on golang-migrate's advisory lock among them, which waits for the
-- build in turn -- a deadlock. A plain build waits for no snapshot, so no
-- wait here leads back to the migrator: a second migrator waits on the
-- advisory lock, a query on sessions waits for this transaction, and this
-- transaction waits at most for a transaction that already held a lock on
-- sessions, which waits for nothing the migrator holds.
-- TestMigration000150_ConcurrentMigrators runs two migrators at once.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied:
--   - An older pod that is already running keeps working: every query it
--     makes on sessions names its columns, and none names these two.
--   - An older pod that restarts does not boot ("no migration found for
--     version 150"). That covers a rollback and an old pod restarting in
--     the middle of a rolling deploy.
--   - Rolling the binary back is safe only to a binary that carries
--     000150; 000149's same rule now names this version.
--   - Rolling back further first needs the down migration. The control
--     plane only ever migrates up, so run it with the golang-migrate CLI
--     and this release's migrations (goto 149), with the control plane
--     scaled to zero, then deploy the older binary. The down file says why
--     not against live pods.
--   - The down drops the stored keys with their columns, so a create
--     retried after it is not recognised as a replay and can start a
--     second session.
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
