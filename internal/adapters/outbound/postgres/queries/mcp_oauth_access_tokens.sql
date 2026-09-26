-- Queries backing MCPOAuthGrantStore's access-token half (technical plan
-- §43.16, migrations/000141_mcp_oauth.up.sql).
--
-- LookupMCPOAuthAccessToken is the /mcp bearer check's ONE query per
-- call: the token, its grant, its client and its user in a single join, so
-- every fact that can revoke the call (token or grant expiry, a deleted
-- grant, a disabled or deleted client, a client whose registration
-- mechanism was switched off, a disabled user, a changed role, a foreign
-- resource) is re-read on every request. There is no cache of any
-- kind in front of it (auth.RequireMCPBearer's own doc comment). The
-- scopes it returns are the TOKEN's own (t.scopes), fixed when the token
-- was issued -- never the grant's, which only records the most recent
-- consent for display (queries/mcp_oauth_grants.sql).

-- name: CreateMCPOAuthAccessToken :one
INSERT INTO mcp_oauth_access_tokens (grant_id, token_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: LookupMCPOAuthAccessToken :one
SELECT t.id AS token_id, t.expires_at AS token_expires_at, t.scopes AS token_scopes,
       g.id AS grant_id, g.resource, g.expires_at AS grant_expires_at, g.last_used_at,
       c.client_id AS client_public_id, c.kind AS client_kind, c.disabled_at AS client_disabled_at,
       u.id AS user_id, u.role AS user_role, u.primary_email, u.disabled AS user_disabled
FROM mcp_oauth_access_tokens t
JOIN mcp_oauth_grants g ON g.id = t.grant_id
JOIN mcp_oauth_clients c ON c.id = g.client_id
JOIN users u ON u.id = g.user_id
WHERE t.token_hash = $1;

-- GetMCPOAuthAccessTokenByHash reads one access token row -- expired or
-- not -- for POST /oauth/revoke, which only needs the grant it belongs to.
-- name: GetMCPOAuthAccessTokenByHash :one
SELECT * FROM mcp_oauth_access_tokens
WHERE token_hash = $1;

-- name: DeleteExpiredMCPOAuthAccessTokens :execrows
DELETE FROM mcp_oauth_access_tokens
WHERE expires_at < now();
