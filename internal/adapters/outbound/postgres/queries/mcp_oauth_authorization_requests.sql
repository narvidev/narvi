-- Queries backing MCPOAuthGrantStore's authorization-request half
-- (technical plan §43.14, migrations/000141_mcp_oauth.up.sql): one row per
-- validated GET /oauth/authorize, waiting for the consent decision.
--
-- BindMCPOAuthAuthorizationRequest is the consent page's own render step:
-- it binds the request to the rendering user the FIRST time (user_id IS
-- NULL) and refuses any other user afterwards, and it replaces the stored
-- CSRF nonce hash with the hash of the nonce this render embeds -- in one
-- statement, so two users racing to render the same request cannot both
-- bind it. ConsumeMCPOAuthAuthorizationRequest is the decision's own
-- single-use step: it succeeds at most once, only for the bound user,
-- only once a nonce was ever minted, and only before expiry.

-- name: CreateMCPOAuthAuthorizationRequest :one
INSERT INTO mcp_oauth_authorization_requests
    (client_id, redirect_uri, scopes, state, code_challenge, code_challenge_method, resource, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- CountPendingMCPOAuthAuthorizationRequests counts the client's requests
-- still waiting for a decision -- unexpired and not consumed, bound to a
-- user or not -- which the pending-request cap bounds (technical plan
-- §43.14). Run it under LockMCPOAuthClientForNewRequest, as a statement of
-- its own: its snapshot is then taken once the lock is granted, so it
-- counts every request an authorization that held the lock before it
-- committed.
-- name: CountPendingMCPOAuthAuthorizationRequests :one
SELECT count(*) FROM mcp_oauth_authorization_requests
WHERE client_id = $1
  AND consumed_at IS NULL
  AND expires_at > now();

-- name: GetMCPOAuthAuthorizationRequest :one
SELECT * FROM mcp_oauth_authorization_requests
WHERE id = $1;

-- name: BindMCPOAuthAuthorizationRequest :one
UPDATE mcp_oauth_authorization_requests
SET user_id = sqlc.arg(user_id), csrf_nonce_hash = sqlc.arg(csrf_nonce_hash)
WHERE id = sqlc.arg(id)
  AND (user_id IS NULL OR user_id = sqlc.arg(user_id))
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: ConsumeMCPOAuthAuthorizationRequest :one
UPDATE mcp_oauth_authorization_requests
SET consumed_at = now()
WHERE id = sqlc.arg(id)
  AND user_id = sqlc.arg(user_id)
  AND csrf_nonce_hash IS NOT NULL
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredMCPOAuthAuthorizationRequests :execrows
DELETE FROM mcp_oauth_authorization_requests
WHERE expires_at < now();
