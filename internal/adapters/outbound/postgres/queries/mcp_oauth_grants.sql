-- Queries backing MCPOAuthGrantStore's grant half (technical plan §43.16,
-- migrations/000141_mcp_oauth.up.sql). A grant row exists iff the
-- authorization is live: deleting it IS revocation, and every code and
-- access token issued under it cascades with it.
--
-- TouchMCPOAuthGrantLastUsed is the bearer check's own coalesced write:
-- it only updates a row whose last_used_at is NULL or older than
-- stale_before, so a busy client costs one write per interval, not one per
-- call.

-- UpsertMCPOAuthGrant records a consent: a user's first approval of a
-- client creates its one grant row; a later approval of the same client
-- replaces that row's scopes, resource and expiry in place (same id, so
-- tokens already issued under it keep working, now under the scopes just
-- consented to). created_at keeps the first approval's time.
-- name: UpsertMCPOAuthGrant :one
INSERT INTO mcp_oauth_grants (user_id, client_id, scopes, resource, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, client_id) DO UPDATE
SET scopes = EXCLUDED.scopes, resource = EXCLUDED.resource, expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: GetMCPOAuthGrant :one
SELECT * FROM mcp_oauth_grants
WHERE id = $1;

-- name: DeleteMCPOAuthGrant :execrows
DELETE FROM mcp_oauth_grants
WHERE id = $1;

-- name: DeleteMCPOAuthGrantForUser :one
DELETE FROM mcp_oauth_grants
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
RETURNING *;

-- name: ListMCPOAuthGrantsForUser :many
SELECT g.id, g.scopes, g.created_at, g.expires_at, g.last_used_at,
       c.client_id AS client_public_id, c.client_name, c.kind AS client_kind
FROM mcp_oauth_grants g
JOIN mcp_oauth_clients c ON c.id = g.client_id
WHERE g.user_id = $1 AND g.expires_at > now()
ORDER BY g.created_at DESC, g.id;

-- name: ListMCPOAuthGrantsForClient :many
SELECT id, user_id FROM mcp_oauth_grants
WHERE client_id = $1
ORDER BY created_at, id;

-- name: TouchMCPOAuthGrantLastUsed :exec
UPDATE mcp_oauth_grants
SET last_used_at = now()
WHERE id = sqlc.arg(id)
  AND (last_used_at IS NULL OR last_used_at < sqlc.arg(stale_before));

-- name: DeleteExpiredMCPOAuthGrants :execrows
DELETE FROM mcp_oauth_grants
WHERE expires_at < now();
