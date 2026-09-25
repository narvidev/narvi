-- Queries backing MCPOAuthClientStore (technical plan §43.15,
-- migrations/000141_mcp_oauth.up.sql): the OAuth clients an MCP client
-- program identifies itself as. client_id is the public identifier a
-- client sends; id is the internal key every other mcp_oauth_* table
-- references. DeleteMCPOAuthClient cascades every grant, pending
-- authorization request, code, access token and refresh token issued to
-- the client.

-- name: CreateMCPOAuthClient :one
INSERT INTO mcp_oauth_clients (client_id, kind, client_name, client_uri, redirect_uris, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMCPOAuthClientByClientID :one
SELECT * FROM mcp_oauth_clients
WHERE client_id = $1;

-- name: GetMCPOAuthClientByID :one
SELECT * FROM mcp_oauth_clients
WHERE id = $1;

-- name: ListMCPOAuthClients :many
SELECT * FROM mcp_oauth_clients
ORDER BY created_at, id;

-- LockMCPOAuthClient takes the client row's FOR UPDATE lock for the rest
-- of the transaction. It conflicts with the FOR KEY SHARE lock every
-- consent decision, code exchange, refresh and grant revocation takes on
-- the client before touching anything under it
-- (LockMCPOAuthClientKeyShare; the lock order is at the top of
-- mcpoauthgrant_store.go), and with the one a grant insert's
-- foreign-key check takes: once it returns, an issuance that got there
-- first has committed (and is visible to the next statement) or rolled
-- back, and one that arrives later waits until this transaction ends.
-- DELETE /api/mcp-clients/{clientID} takes it before listing the grants
-- it audits, so the list is exactly what the cascade removes.
-- name: LockMCPOAuthClient :one
SELECT * FROM mcp_oauth_clients
WHERE id = $1
FOR UPDATE;

-- LockMCPOAuthClientKeyShare takes the client row's FOR KEY SHARE lock for
-- the rest of the transaction: the lock a grant or request insert's own
-- foreign-key check would take anyway, taken FIRST, before the
-- transaction locks any row under the client (the lock order at the top
-- of mcpoauthgrant_store.go). It conflicts only with the client's
-- deletion (and LockMCPOAuthClient before it), never with another
-- issuance.
-- name: LockMCPOAuthClientKeyShare :one
SELECT * FROM mcp_oauth_clients
WHERE id = $1
FOR KEY SHARE;

-- name: DeleteMCPOAuthClient :one
DELETE FROM mcp_oauth_clients
WHERE id = $1
RETURNING *;
