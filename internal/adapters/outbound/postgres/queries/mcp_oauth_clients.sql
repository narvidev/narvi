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

-- UpsertMCPOAuthMetadataDocumentClient records one successfully fetched
-- and validated client ID metadata document (technical plan §43.15,
-- migrations/000143_mcp_oauth_client_metadata.up.sql): it creates the
-- client the URL identifies, or replaces its cached name, redirect URIs
-- and cache stamps in place (same id, so every grant, request, code and
-- token under it is untouched -- and none of them reads the client's
-- redirect URIs or name to decide anything it was issued for). The WHERE
-- keeps it from ever rewriting a client of another kind; no other kind's
-- client_id can be an https URL anyway. disabled_at is never touched: a
-- re-fetch does not re-enable a client an operator disabled.
--
-- Its locks, in the order at the top of mcpoauthgrant_store.go: ONE
-- statement, ONE row -- the client -- taken FOR NO KEY UPDATE (no key
-- column changes). That conflicts with a client's deletion and the
-- unused-client sweep (FOR UPDATE), never with the FOR KEY SHARE every
-- consent, code exchange, refresh and grant revocation takes on the
-- client; and holding nothing else, it can never be part of a wait cycle.
-- If the row it would update is deleted while it waits, it inserts a
-- fresh client instead (INSERT ... ON CONFLICT's own retry).
-- name: UpsertMCPOAuthMetadataDocumentClient :one
INSERT INTO mcp_oauth_clients (client_id, kind, client_name, redirect_uris, metadata_fetched_at, metadata_stale_at)
VALUES ($1, 'metadata_document', $2, $3, $4, $5)
ON CONFLICT (client_id) DO UPDATE
SET client_name         = EXCLUDED.client_name,
    redirect_uris       = EXCLUDED.redirect_uris,
    metadata_fetched_at = EXCLUDED.metadata_fetched_at,
    metadata_stale_at   = EXCLUDED.metadata_stale_at
WHERE mcp_oauth_clients.kind = 'metadata_document'
RETURNING *;

-- ExtendMCPOAuthMetadataDocumentStale keeps a metadata-document client's
-- cached document for one more TTL after a re-fetch failed -- only if no
-- other fetch has succeeded since the caller read the row (the
-- metadata_fetched_at guard), so a failure can never push out the
-- shorter lifetime a newer successful fetch set. One statement, one row
-- lock (FOR NO KEY UPDATE on the client), like the upsert above.
-- pgx.ErrNoRows means another fetch got there first.
-- name: ExtendMCPOAuthMetadataDocumentStale :one
UPDATE mcp_oauth_clients
SET metadata_stale_at = sqlc.arg(metadata_stale_at)
WHERE id = sqlc.arg(id)
  AND kind = 'metadata_document'
  AND metadata_fetched_at = sqlc.arg(metadata_fetched_at)
RETURNING *;

-- The expired-credential sweep's unused-client pass (technical plan
-- §43.15) is these two statements, in one transaction
-- (MCPOAuthClientStore.DeleteUnused). A candidate is a dynamically
-- registered or metadata-document client that no operator disabled -- a
-- disabled row IS the block: deleting it would let the next authorization
-- register the same URL afresh, enabled -- holding no grant and no
-- authorization request (pending, or consumed and not yet swept), last
-- used before unused_before (now - MCPDynamicClientUnusedTTL): the latest
-- of its registration, its last successful fetch and the end of the time
-- its cached document may be used without a fetch -- which a failed
-- re-fetch's grace pushes out, so a client still served from its cache is
-- never a candidate. Pre-registered clients never are.
--
-- LockUnusedMCPOAuthClients takes every candidate FOR UPDATE, in id order
-- (the lock order at the top of mcpoauthgrant_store.go: the client
-- first). A candidate a re-fetch is updating is re-checked once the
-- re-fetch commits, and skipped. A candidate an authorization request's
-- (or a grant's) insert holds FOR KEY SHARE -- its foreign-key check --
-- is waited for, and returned: a row only locked, not updated, is not
-- re-checked.
-- name: LockUnusedMCPOAuthClients :many
SELECT c.id FROM mcp_oauth_clients c
WHERE c.kind IN ('dynamic', 'metadata_document')
  AND c.disabled_at IS NULL
  AND GREATEST(c.created_at, c.metadata_fetched_at, c.metadata_stale_at) < sqlc.arg(unused_before)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_authorization_requests r WHERE r.client_id = c.id)
ORDER BY c.id
FOR UPDATE OF c;

-- DeleteUnusedMCPOAuthClients deletes the candidates the transaction
-- locked above that are STILL candidates: a new statement, so a new
-- snapshot, which sees every request or grant committed before the lock
-- was taken -- the one that committed while the lock waited included --
-- and, the client held FOR UPDATE, none can be added until the
-- transaction ends (its insert's foreign-key check waits, then finds the
-- client gone). Its cascade therefore reaches no row.
-- name: DeleteUnusedMCPOAuthClients :execrows
DELETE FROM mcp_oauth_clients c
WHERE c.id = ANY(sqlc.arg(ids)::uuid[])
  AND c.kind IN ('dynamic', 'metadata_document')
  AND c.disabled_at IS NULL
  AND GREATEST(c.created_at, c.metadata_fetched_at, c.metadata_stale_at) < sqlc.arg(unused_before)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM mcp_oauth_authorization_requests r WHERE r.client_id = c.id);
