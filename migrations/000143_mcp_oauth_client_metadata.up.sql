-- Client ID metadata documents (technical plan §43.15): a client that
-- identifies itself by an https URL is described by the JSON document at
-- that URL, which Narvi fetches and caches in the client's own
-- mcp_oauth_clients row (kind 'metadata_document', client_id = the URL).
--
-- metadata_fetched_at is when the document was last fetched and validated
-- successfully. metadata_stale_at is when it must next be re-fetched:
-- fetched + MCPClientMetadataCacheTTL, shortened -- never extended -- by
-- the document's own Cache-Control, and pushed one more TTL out when a
-- re-fetch of a stale document fails (the cached document is kept rather
-- than breaking every authorization while its host is down). The cache
-- only ever decides what the NEXT authorization sees: every code, token
-- and refresh chain already issued carries its own redirect URI, scopes
-- and resource, so re-fetching a document never changes what an issued
-- credential can do.
--
-- Both are set for a metadata-document client and for no other kind; the
-- CHECK keeps the cache stamps from ever describing a client whose
-- registration they are not.
ALTER TABLE mcp_oauth_clients
    ADD COLUMN metadata_fetched_at TIMESTAMPTZ,
    ADD COLUMN metadata_stale_at   TIMESTAMPTZ;

ALTER TABLE mcp_oauth_clients
    ADD CONSTRAINT mcp_oauth_clients_metadata_stamps_check CHECK (
        (kind = 'metadata_document' AND metadata_fetched_at IS NOT NULL AND metadata_stale_at IS NOT NULL)
        OR (kind <> 'metadata_document' AND metadata_fetched_at IS NULL AND metadata_stale_at IS NULL)
    );
