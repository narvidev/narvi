-- Client ID metadata documents (technical plan §43.15): a client that
-- identifies itself by an https URL is described by the JSON document at
-- that URL, which Narvi fetches and caches in the client's own
-- mcp_oauth_clients row (kind 'metadata_document', client_id = the URL).
--
-- metadata_fetched_at is when the document was last fetched and validated
-- successfully. metadata_stale_at is when it must next be re-fetched:
-- fetched + MCPClientMetadataCacheTTL, shortened -- never extended -- by
-- the document's own Cache-Control. metadata_refetch_failed_at is when a
-- re-fetch of the stale document first failed since that last successful
-- fetch (NULL while none has): the cached document is then kept for ONE
-- more MCPClientMetadataCacheTTL measured from that first failure -- so a
-- document host that is down does not break every authorization at once
-- -- and metadata_stale_at moves to the end of that grace; past it, the
-- client is refused until a fetch succeeds again, which clears the
-- failure. The cache only ever decides what the NEXT authorization sees:
-- every code, token and refresh chain already issued carries its own
-- redirect URI, scopes and resource, so re-fetching a document never
-- changes what an issued credential can do.
--
-- The stamps describe a metadata-document client and no other kind; the
-- CHECK keeps them from ever describing a client whose registration they
-- are not.
ALTER TABLE mcp_oauth_clients
    ADD COLUMN metadata_fetched_at        TIMESTAMPTZ,
    ADD COLUMN metadata_stale_at          TIMESTAMPTZ,
    ADD COLUMN metadata_refetch_failed_at TIMESTAMPTZ;

ALTER TABLE mcp_oauth_clients
    ADD CONSTRAINT mcp_oauth_clients_metadata_stamps_check CHECK (
        (kind = 'metadata_document' AND metadata_fetched_at IS NOT NULL AND metadata_stale_at IS NOT NULL)
        OR (kind <> 'metadata_document' AND metadata_fetched_at IS NULL AND metadata_stale_at IS NULL
            AND metadata_refetch_failed_at IS NULL)
    );
