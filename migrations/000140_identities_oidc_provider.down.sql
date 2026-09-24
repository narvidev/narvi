-- Postgres has no ALTER TYPE ... DROP VALUE -- removing an enum value
-- requires recreating the type. Guarded exactly like
-- migrations/000061_provider_credentials_user_scope.down.sql's own
-- identical precedent: this down migration is only ever safe on an
-- environment that added the 'oidc' value but never actually started
-- using it (a local dev/CI rollback) -- it fails loudly rather than
-- silently orphaning a real OIDC-linked identity row if one exists.
--
-- TWO tables use this enum -- identities (migrations/000003) AND
-- identity_link_prompts (migrations/000036, "reuses the identity_provider
-- enum migrations/000003_identities.up.sql already defines") -- both
-- columns must be converted to the recreated type before the old one is
-- dropped, or DROP TYPE fails with "cannot drop type ... because other
-- objects depend on it".
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM identities WHERE provider = 'oidc') THEN
        RAISE EXCEPTION 'cannot drop identity_provider value ''oidc'': rows still reference it';
    END IF;
    IF EXISTS (SELECT 1 FROM identity_link_prompts WHERE provider = 'oidc') THEN
        RAISE EXCEPTION 'cannot drop identity_provider value ''oidc'': pending identity_link_prompts rows still reference it';
    END IF;
END $$;

ALTER TYPE identity_provider RENAME TO identity_provider_old;
CREATE TYPE identity_provider AS ENUM ('github', 'slack', 'linear', 'google');
ALTER TABLE identities
    ALTER COLUMN provider TYPE identity_provider
    USING provider::text::identity_provider;
ALTER TABLE identity_link_prompts
    ALTER COLUMN provider TYPE identity_provider
    USING provider::text::identity_provider;
DROP TYPE identity_provider_old;
