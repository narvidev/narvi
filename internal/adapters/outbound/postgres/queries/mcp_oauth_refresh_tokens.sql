-- Queries backing MCPOAuthGrantStore's refresh-token half (technical plan
-- §43.16, migrations/000142_mcp_oauth_refresh_tokens.up.sql).
--
-- RotateMCPOAuthRefreshToken is the single-use step: it marks a refresh
-- token replaced at most once. When it returns nothing, the token was
-- already rotated (or is gone): presenting it was a replay, and
-- mcpauth's refresh grant then revokes its grant.
-- GetMCPOAuthRefreshTokenByHash reads a token whether or not it was
-- rotated, which is how a replay is recognised as one.

-- CreateMCPOAuthRefreshToken inserts one refresh token. resource and
-- chain_expires_at are its chain's, fixed when the chain began (the code
-- exchange) and copied unchanged by every rotation -- never re-read from
-- the grant (migrations/000142_mcp_oauth_refresh_tokens.up.sql).
-- name: CreateMCPOAuthRefreshToken :one
INSERT INTO mcp_oauth_refresh_tokens (grant_id, token_hash, scopes, resource, expires_at, chain_expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMCPOAuthRefreshTokenByHash :one
SELECT * FROM mcp_oauth_refresh_tokens
WHERE token_hash = $1;

-- name: RotateMCPOAuthRefreshToken :one
UPDATE mcp_oauth_refresh_tokens
SET rotated_at = now(), superseded_by = sqlc.arg(superseded_by)
WHERE id = sqlc.arg(id) AND rotated_at IS NULL
RETURNING *;

-- name: DeleteExpiredMCPOAuthRefreshTokens :execrows
DELETE FROM mcp_oauth_refresh_tokens
WHERE expires_at < now();
