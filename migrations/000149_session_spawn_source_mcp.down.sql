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
--
-- ONE column uses this enum, sessions.spawn_source
-- (migrations/000004_sessions.up.sql); it has no default, index, view or
-- trigger depending on it, so converting it is the whole conversion. The
-- conversion rewrites the sessions table under an ACCESS EXCLUSIVE lock.
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
