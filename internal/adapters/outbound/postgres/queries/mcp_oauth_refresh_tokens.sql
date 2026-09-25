-- Queries backing MCPOAuthGrantStore's refresh-token half (technical plan
-- §43.16, migrations/000142_mcp_oauth_refresh_tokens.up.sql).
--
-- RotateMCPOAuthRefreshToken is the single-use step: it marks a refresh
-- token replaced at most once. When it returns nothing, the token was
-- already rotated (or is gone): presenting it was a replay, and
-- mcpauth's refresh grant then revokes its grant.
-- GetMCPOAuthRefreshTokenByHash reads a token whether or not it was
-- rotated, which is how a replay is recognised as one.

-- name: CreateMCPOAuthRefreshToken :one
INSERT INTO mcp_oauth_refresh_tokens (grant_id, token_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4)
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
