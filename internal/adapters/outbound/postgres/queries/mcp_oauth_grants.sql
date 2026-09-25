-- Queries backing MCPOAuthGrantStore's grant half (technical plan §43.16,
-- migrations/000141_mcp_oauth.up.sql). A grant row exists iff the
-- authorization is live: deleting it IS revocation, and every code,
-- access token and refresh token issued under it cascades with it.
--
-- TouchMCPOAuthGrantLastUsed is the bearer check's own coalesced write:
-- it only updates a row whose last_used_at is NULL or older than
-- stale_before, so a busy client costs one write per interval, not one per
-- call.

-- UpsertMCPOAuthGrant records a consent: a user's first approval of a
-- client creates its one grant row; a later approval of the same client
-- renews that row's resource and expiry in place and records the scopes
-- just approved (same id, so the Connected apps list names the client
-- once). The row's scopes are the most recent approval, for display
-- only: no authorization decision reads them -- every code, access token
-- and refresh token carries its own scopes, fixed at issuance. Nor does a
-- refresh read the row's resource: a refresh chain carries its own
-- resource and its own end (chain_expires_at), copied when the chain
-- began and never renewed. A refresh does read the row's expiry, only
-- ever as a limit: it refuses once the grant has expired, and caps every
-- token it issues at the grant's expiry as well as the chain's end. So
-- this update can neither widen nor narrow a credential's scopes, and
-- neither extends nor rebinds a refresh chain an earlier consent began:
-- it renews the grant, and the chains this consent's own code will begin.
-- Renewing normally moves the expiry later, past every older chain's own
-- end, so no chain is shortened either; but an expiry written earlier
-- than before -- MCPGrantMaxLifetime lowered since the last consent --
-- caps, and then ends, every chain under the grant (technical plan
-- §43.16). created_at keeps the first approval's time.
-- name: UpsertMCPOAuthGrant :one
INSERT INTO mcp_oauth_grants (user_id, client_id, scopes, resource, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, client_id) DO UPDATE
SET scopes = EXCLUDED.scopes, resource = EXCLUDED.resource, expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: GetMCPOAuthGrant :one
SELECT * FROM mcp_oauth_grants
WHERE id = $1;

-- LockMCPOAuthGrantKeyShare takes the grant row's FOR KEY SHARE lock for
-- the rest of the transaction: the lock a token insert's own foreign-key
-- check would take anyway, taken FIRST, before the code exchange consumes
-- its code or the refresh grant rotates its refresh token (the lock order
-- at the top of mcpoauthgrant_store.go). It conflicts only with the
-- grant's deletion, never with another exchange, another refresh, or a
-- consent renewing the grant.
-- name: LockMCPOAuthGrantKeyShare :one
SELECT * FROM mcp_oauth_grants
WHERE id = $1
FOR KEY SHARE;

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
