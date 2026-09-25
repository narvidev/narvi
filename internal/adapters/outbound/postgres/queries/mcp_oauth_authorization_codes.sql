-- Queries backing MCPOAuthGrantStore's authorization-code half (technical
-- plan §43.16, migrations/000141_mcp_oauth.up.sql).
-- ConsumeMCPOAuthAuthorizationCode is the single-use step: it returns a
-- row at most once per code. When it returns nothing,
-- GetMCPOAuthAuthorizationCodeByHash tells a replayed code (row present,
-- already consumed -- its grant is then revoked) from one that never
-- existed.

-- name: CreateMCPOAuthAuthorizationCode :one
INSERT INTO mcp_oauth_authorization_codes (grant_id, code_hash, code_challenge, redirect_uri, resource, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ConsumeMCPOAuthAuthorizationCode :one
UPDATE mcp_oauth_authorization_codes
SET consumed_at = now()
WHERE code_hash = $1 AND consumed_at IS NULL
RETURNING *;

-- name: GetMCPOAuthAuthorizationCodeByHash :one
SELECT * FROM mcp_oauth_authorization_codes
WHERE code_hash = $1;

-- name: DeleteExpiredMCPOAuthAuthorizationCodes :execrows
DELETE FROM mcp_oauth_authorization_codes
WHERE expires_at < now();
