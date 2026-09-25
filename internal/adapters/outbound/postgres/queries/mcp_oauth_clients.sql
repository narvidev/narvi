-- Queries backing MCPOAuthClientStore (technical plan §43.15,
-- migrations/000141_mcp_oauth.up.sql): the OAuth clients an MCP client
-- program identifies itself as. client_id is the public identifier a
-- client sends; id is the internal key every other mcp_oauth_* table
-- references. DeleteMCPOAuthClient cascades every grant, pending
-- authorization request, code and access token issued to the client.

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

-- name: DeleteMCPOAuthClient :one
DELETE FROM mcp_oauth_clients
WHERE id = $1
RETURNING *;
