-- The MCP authorization server's refresh tokens (technical plan §43.16),
-- beside the access tokens migrations/000141_mcp_oauth.up.sql stores.
-- Like every secret column there, token_hash holds
-- platform.HashToken(plaintext) -- irreversible, looked up by equality,
-- never encrypted.
--
-- A refresh token is a child of its grant, exactly like a code or an
-- access token: ON DELETE CASCADE, so revocation -- deleting the grant --
-- takes every refresh token with it, and the one lock order at the top of
-- internal/adapters/outbound/postgres/mcpoauthgrant_store.go holds
-- unchanged (a refresh token row is locked only after its client and its
-- grant).
--
-- scopes is fixed at issuance, like an access token's: copied from the
-- authorization code on the code exchange, or from the refresh token it
-- replaces -- possibly narrowed, never widened -- on a refresh. Never from
-- the grant, whose scopes only record the most recent consent for
-- display: a later consent can never widen a refresh chain.
--
-- A refresh chain -- the tokens one code exchange begins and every
-- rotation continues -- is fixed at issuance in everything else it can do
-- too. resource is copied from the authorization code, chain_expires_at
-- from the grant's expires_at at that code exchange, and a rotation copies
-- both unchanged into the token it issues; every token a refresh issues
-- expires by chain_expires_at (or by the grant's own expiry, if that comes
-- first). A later consent for the same client renews the grant row in
-- place -- its expiry and its resource -- so taking either from the grant
-- at a refresh would let that consent extend, or rebind, a chain an
-- earlier consent began. Neither is taken from it: the grant's resource
-- is never read at a refresh, and its expiry only ever limits the chain.
--
-- Rotation: every refresh replaces the token presented. rotated_at marks
-- it used and superseded_by names the token that replaced it (NULL once
-- that one is itself deleted). A rotated row is kept until its own
-- expires_at, so presenting it again is recognised as a replay -- which
-- deletes the grant (reuse detection, with no grace window).
CREATE TABLE mcp_oauth_refresh_tokens (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grant_id         UUID NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    token_hash       TEXT NOT NULL UNIQUE,
    scopes           TEXT[] NOT NULL,
    resource         TEXT NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    chain_expires_at TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at       TIMESTAMPTZ,
    superseded_by    UUID REFERENCES mcp_oauth_refresh_tokens(id) ON DELETE SET NULL,
    -- A token names its successor only once it has been rotated.
    CONSTRAINT mcp_oauth_refresh_tokens_superseded_only_when_rotated
        CHECK (superseded_by IS NULL OR rotated_at IS NOT NULL)
);
CREATE INDEX mcp_oauth_refresh_tokens_grant_id_idx ON mcp_oauth_refresh_tokens (grant_id);
CREATE INDEX mcp_oauth_refresh_tokens_expires_at_idx ON mcp_oauth_refresh_tokens (expires_at);
CREATE INDEX mcp_oauth_refresh_tokens_superseded_by_idx ON mcp_oauth_refresh_tokens (superseded_by);
