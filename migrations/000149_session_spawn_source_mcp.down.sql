-- Postgres has no ALTER TYPE ... DROP VALUE -- removing an enum value
-- requires recreating the type. Guarded exactly like
-- migrations/000140_identities_oidc_provider.down.sql's own precedent: this
-- down migration is only ever safe on an environment that added the 'mcp'
-- value but never used it (a local dev/CI rollback, or a production
-- rollback before the first MCP session exists). It fails loudly rather
-- than rewriting a real session's recorded source.
--
-- When it refuses, golang-migrate has already marked the target version
-- (148) dirty, while the schema is still 149's: the whole file ran as one
-- implicit transaction, so the RAISE rolled every statement back. Recover
-- with `migrate force 149`; the schema then matches its version again.
-- Then scale the control plane back up on a binary that carries 000149:
-- no binary without it can boot against this database any more.
--
-- ONE column uses this enum, sessions.spawn_source
-- (migrations/000004_sessions.up.sql); it has no default, index, view or
-- trigger depending on it, so converting it is the whole conversion. The
-- conversion rewrites the sessions table under an ACCESS EXCLUSIVE lock.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy the older
-- binary. Do not run it against live pods, for two reasons:
--   - The recreated type has a new OID. A live pod's pooled connections
--     cache the statements that bind or return sessions.spawn_source,
--     session create and read among them. After this down, each such
--     statement fails once per connection ("cache lookup failed for type"
--     or "cached plan must not change result type"). pgx then drops it
--     from its cache, so the next call succeeds, but a failure inside a
--     transaction aborts that whole transaction.
--   - A pod that carries 000149 and restarts before the older binary
--     replaces it applies 000149 again at boot, which locks the older
--     binary out again.
-- Keeping the pods live would not avoid a stall anyway: every sessions
-- query waits on the ACCESS EXCLUSIVE lock while the rewrite runs.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM sessions WHERE spawn_source = 'mcp') THEN
        RAISE EXCEPTION 'cannot drop session_spawn_source value ''mcp'': sessions rows still reference it';
    END IF;
END $$;

ALTER TYPE session_spawn_source RENAME TO session_spawn_source_old;
CREATE TYPE session_spawn_source AS ENUM ('web', 'slack', 'linear', 'github');
ALTER TABLE sessions
    ALTER COLUMN spawn_source TYPE session_spawn_source
    USING spawn_source::text::session_spawn_source;
DROP TYPE session_spawn_source_old;
