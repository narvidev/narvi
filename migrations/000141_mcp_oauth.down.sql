-- Reverse of 000141_mcp_oauth.up.sql: tables in reverse dependency order,
-- the enum last.
DROP TABLE IF EXISTS mcp_oauth_access_tokens;
DROP TABLE IF EXISTS mcp_oauth_authorization_codes;
DROP TABLE IF EXISTS mcp_oauth_grants;
DROP TABLE IF EXISTS mcp_oauth_authorization_requests;
DROP TABLE IF EXISTS mcp_oauth_clients;
DROP TYPE IF EXISTS mcp_oauth_client_kind;
