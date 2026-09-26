-- Reverse of 000143_mcp_oauth_client_metadata.up.sql.
ALTER TABLE mcp_oauth_clients DROP CONSTRAINT IF EXISTS mcp_oauth_clients_metadata_stamps_check;
ALTER TABLE mcp_oauth_clients
    DROP COLUMN IF EXISTS metadata_refetch_failed_at,
    DROP COLUMN IF EXISTS metadata_stale_at,
    DROP COLUMN IF EXISTS metadata_fetched_at;
