-- The MCP authorization server's own storage (technical plan §43.13-§43.16):
-- Narvi issues and verifies these credentials itself, so every secret
-- column below holds platform.HashToken(plaintext) -- a SHA-256 digest,
-- irreversible, looked up by equality. Nothing here is ever encrypted
-- (there is no plaintext anyone needs back: Narvi is both the issuer and
-- the verifier), and nothing here is a provider_credentials row: the
-- model-provider OAuth family (reversible, replayed upstream) and the
-- deployment's own secrets are different credential families that fail
-- differently, and they stay in their own tables (§43.13).
--
-- Revocation is structural: a grant row exists iff the authorization is
-- live, and every code and access token cascades from it. Deleting the
-- grant is the whole revocation; the /mcp bearer check re-reads the grant
-- on every call, so the next call fails with no cache to invalidate.

-- All three client kinds are declared now even though this migration's
-- own routes only ever create 'preregistered' rows: ALTER TYPE ... ADD
-- VALUE cannot run in the same transaction as a statement that uses the
-- new value (the 000061/000062 split is the precedent), so a later
-- migration adding client-ID-metadata-document or dynamic-registration
-- clients would otherwise need a separate enum-only migration first.
CREATE TYPE mcp_oauth_client_kind AS ENUM ('preregistered', 'dynamic', 'metadata_document');

-- One OAuth client an MCP client program identifies itself as. client_id
-- is the public identifier the client sends (never a secret: public
-- clients with PKCE only, §43.15); id is the internal key every other
-- table references. disabled_at is an operator kill-switch the bearer
-- check re-reads on every call; deleting the row instead cascades every
-- grant, request, code and token issued to it.
CREATE TABLE mcp_oauth_clients (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id     TEXT NOT NULL UNIQUE,
    kind          mcp_oauth_client_kind NOT NULL,
    client_name   TEXT NOT NULL,
    client_uri    TEXT,
    redirect_uris TEXT[] NOT NULL,
    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at   TIMESTAMPTZ
);

-- One validated GET /oauth/authorize request, waiting for the user's
-- decision on the consent page. user_id is NULL until the consent page is
-- first rendered, then bound to that signed-in user for good (another
-- user can never decide it). csrf_nonce_hash is likewise NULL until that
-- first render: the consent page mints a fresh nonce on every render and
-- stores only its hash, so the plaintext lives in the rendered form and
-- nowhere else. consumed_at makes the decision single-use.
CREATE TABLE mcp_oauth_authorization_requests (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id             UUID NOT NULL REFERENCES mcp_oauth_clients(id) ON DELETE CASCADE,
    user_id               UUID REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri          TEXT NOT NULL,
    scopes                TEXT[] NOT NULL,
    state                 TEXT,
    code_challenge        TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL CHECK (code_challenge_method = 'S256'),
    resource              TEXT NOT NULL,
    csrf_nonce_hash       TEXT,
    expires_at            TIMESTAMPTZ NOT NULL,
    consumed_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mcp_oauth_authorization_requests_client_id_idx ON mcp_oauth_authorization_requests (client_id);
CREATE INDEX mcp_oauth_authorization_requests_expires_at_idx ON mcp_oauth_authorization_requests (expires_at);

-- One user's authorization of one client: at most one row per (user,
-- client) -- consenting again renews the row's lifetime rather than
-- adding a second row, so the Connected apps list names each app once and
-- re-consent cannot grow this table without bound. scopes records the
-- most recent approval, for display only: it is never read to authorize
-- anything, because what a credential may do is fixed when it is issued
-- -- each authorization code and access token below carries the scopes
-- approved in the flow that produced it, and a later consent can neither
-- widen nor narrow a token already issued (withdrawing access is
-- revocation: deleting this row). resource is the canonical /mcp URI the
-- grant was issued for; the bearer check compares it against this
-- deployment's own on every call (audience binding). expires_at is the
-- absolute lifetime after which the user must consent again.
CREATE TABLE mcp_oauth_grants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id    UUID NOT NULL REFERENCES mcp_oauth_clients(id) ON DELETE CASCADE,
    scopes       TEXT[] NOT NULL,
    resource     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX mcp_oauth_grants_user_client_idx ON mcp_oauth_grants (user_id, client_id);
CREATE INDEX mcp_oauth_grants_client_id_idx ON mcp_oauth_grants (client_id);
CREATE INDEX mcp_oauth_grants_expires_at_idx ON mcp_oauth_grants (expires_at);

-- One authorization code, single-use (consumed_at), bound to the PKCE
-- challenge, redirect URI and resource of the request that produced it,
-- and carrying the scopes the user approved in that one consent decision
-- -- the scopes the access token it is exchanged for will hold. A consumed
-- row is kept until it expires so a replay can be recognised as a replay
-- -- and a replay deletes the grant (§43.16).
CREATE TABLE mcp_oauth_authorization_codes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grant_id       UUID NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    code_hash      TEXT NOT NULL UNIQUE,
    code_challenge TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL,
    resource       TEXT NOT NULL,
    scopes         TEXT[] NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    consumed_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mcp_oauth_authorization_codes_grant_id_idx ON mcp_oauth_authorization_codes (grant_id);
CREATE INDEX mcp_oauth_authorization_codes_expires_at_idx ON mcp_oauth_authorization_codes (expires_at);

-- One bearer access token, looked up by token_hash on every /mcp call.
-- scopes is copied from the code it was exchanged for and never changes:
-- the bearer check reads the token's own scopes, never the grant's.
CREATE TABLE mcp_oauth_access_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grant_id   UUID NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    scopes     TEXT[] NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mcp_oauth_access_tokens_grant_id_idx ON mcp_oauth_access_tokens (grant_id);
CREATE INDEX mcp_oauth_access_tokens_expires_at_idx ON mcp_oauth_access_tokens (expires_at);
