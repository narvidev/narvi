package mcpauth

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/platform"
)

// resolveClient finds the client an authorization request names (technical
// plan §43.14/§43.15): a metadata-document client -- an https client_id,
// while that mechanism is on -- through its cached or freshly fetched
// document (metadataDocumentClient); any other through its stored row. It
// renders the error page itself when it cannot: a client_id is never
// answered with a redirect. Whether the client found may be used is the
// caller's question (clientUsable).
func (s *Server) resolveClient(w http.ResponseWriter, r *http.Request, clientID string) (sqlcgen.McpOauthClient, bool) {
	if s.cfg.Mechanisms.MetadataDocuments && mcpclient.IsClientIDURL(clientID) {
		return s.metadataDocumentClient(w, r, clientID)
	}
	ctx := r.Context()
	client, err := s.deps.Clients.GetByClientID(ctx, clientID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		platform.Logger(ctx).Warn("mcpauth: authorize refused", "outcome", "unknown_client")
		s.renderError(w, r, http.StatusBadRequest, "This app is not registered", "This deployment does not know the app that sent you here.")
		return sqlcgen.McpOauthClient{}, false
	case err != nil:
		platform.Logger(ctx).Error("mcpauth: authorize: load client failed", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be processed.")
		return sqlcgen.McpOauthClient{}, false
	}
	return client, true
}

// metadataDocumentClient resolves a metadata-document client (technical
// plan §43.15). A cached document still fresh is used as stored. Otherwise
// the document is fetched through the SSRF-guarded fetcher and validated
// (mcpclient.ParseMetadataDocument: its client_id must be clientIDURL
// byte for byte), and the client row is created or refreshed with it,
// trusted for MCPClientMetadataCacheTTL -- or less, if the document's own
// Cache-Control says so (mcpclient.MetadataCacheTTL), never more. When a
// fetch fails:
//
//   - a document never fetched before refuses the authorization with an
//     error page, never a redirect: nothing about the client is known,
//     least of all where it may be sent;
//   - a stale cached document is kept for one more TTL (logged), so a
//     document host that is down does not break every authorization --
//     and is not asked again on every one.
//
// Refreshing the row changes what the NEXT authorization sees and nothing
// else: every request, code, token and refresh chain already issued
// carries its own redirect URI, scopes and resource (technical plan
// §43.16), and none of them re-reads the client's.
func (s *Server) metadataDocumentClient(w http.ResponseWriter, r *http.Request, clientIDURL string) (sqlcgen.McpOauthClient, bool) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	fail := func(msg string, err error) (sqlcgen.McpOauthClient, bool) {
		logger.Error("mcpauth: authorize: "+msg, "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization request could not be processed.")
		return sqlcgen.McpOauthClient{}, false
	}

	if err := mcpclient.ValidateClientIDURL(clientIDURL); err != nil {
		logger.Warn("mcpauth: authorize refused", "outcome", "invalid_client_id_url", "error", err)
		s.renderError(w, r, http.StatusBadRequest, "This app could not be identified", "The app identified itself by an address this deployment cannot use.")
		return sqlcgen.McpOauthClient{}, false
	}

	cached, err := s.deps.Clients.GetByClientID(ctx, clientIDURL)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fail("load client failed", err)
	}
	if found && mcpclient.Kind(cached.Kind) != mcpclient.KindMetadataDocument {
		// Unreachable: no other kind's client_id is an https URL. Refused
		// rather than treated as the kind it claims.
		logger.Error("mcpauth: authorize: an https client_id belongs to a client of another kind", "client_id", clientIDURL, "kind", cached.Kind)
		s.renderError(w, r, http.StatusBadRequest, "This app is not available", "This deployment cannot use the app that sent you here.")
		return sqlcgen.McpOauthClient{}, false
	}
	now := time.Now()
	if found && s.metadataFresh(cached, now) {
		return cached, true
	}

	md, ttl, ferr := s.fetchMetadataDocument(r, clientIDURL)
	if ferr == nil {
		row, err := s.deps.Clients.UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
			ClientID:          clientIDURL,
			ClientName:        md.ClientName,
			RedirectUris:      md.RedirectURIs,
			MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
			MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(ttl), Valid: true},
		})
		if err != nil {
			return fail("record the metadata document failed", err)
		}
		return row, true
	}

	if !found {
		logger.Warn("mcpauth: authorize refused", "outcome", "metadata_document_unusable", "client_id", clientIDURL, "error", ferr)
		if errors.Is(ferr, mcpclient.ErrConfidentialClient) {
			// invalid_client: this server takes public clients only.
			s.renderError(w, r, http.StatusBadRequest, "This app is not supported", "The app's description asks to sign in with a secret or a key, and this deployment accepts only apps that use neither.")
			return sqlcgen.McpOauthClient{}, false
		}
		s.renderError(w, r, http.StatusBadRequest, "This app's description could not be used", "This deployment fetched the app's description from the address it identified itself by, and could not use it. Nothing was sent anywhere.")
		return sqlcgen.McpOauthClient{}, false
	}

	logger.Warn("mcpauth: metadata document re-fetch failed; keeping the cached document for one more cache lifetime", "client_id", clientIDURL, "error", ferr)
	kept, err := s.deps.Clients.ExtendMetadataStale(ctx, cached.ID, cached.MetadataFetchedAt.Time, now.Add(s.cfg.Timeouts.MCPClientMetadataCacheTTL))
	switch {
	case err == nil:
		return kept, true
	case errors.Is(err, pgx.ErrNoRows):
		// Another fetch succeeded, or the client was deleted, since the
		// read above: use what is stored now.
		current, gerr := s.deps.Clients.GetByClientID(ctx, clientIDURL)
		if errors.Is(gerr, pgx.ErrNoRows) {
			s.renderError(w, r, http.StatusBadRequest, "This app is not registered", "This deployment does not know the app that sent you here.")
			return sqlcgen.McpOauthClient{}, false
		}
		if gerr != nil {
			return fail("reload client failed", gerr)
		}
		return current, true
	default:
		return fail("keep the cached metadata document failed", err)
	}
}

// metadataFresh reports whether a metadata-document client's cached
// document may still be used without a re-fetch: before its stale time,
// and -- should the stored stale time lie further out than the current
// ceiling allows from now, because an operator lowered
// MCPClientMetadataCacheTTL -- never trusted past the current ceiling.
func (s *Server) metadataFresh(client sqlcgen.McpOauthClient, now time.Time) bool {
	stale := client.MetadataStaleAt.Time
	return client.MetadataStaleAt.Valid && now.Before(stale) && !stale.After(now.Add(s.cfg.Timeouts.MCPClientMetadataCacheTTL))
}

// fetchMetadataDocument fetches and validates the document at clientIDURL,
// returning what it registers and how long it may be trusted.
func (s *Server) fetchMetadataDocument(r *http.Request, clientIDURL string) (mcpclient.Metadata, time.Duration, error) {
	res, err := s.deps.Metadata.Fetch(r.Context(), clientIDURL)
	if err != nil {
		return mcpclient.Metadata{}, 0, err
	}
	md, err := mcpclient.ParseMetadataDocument(clientIDURL, res.Body)
	if err != nil {
		return mcpclient.Metadata{}, 0, err
	}
	return md, mcpclient.MetadataCacheTTL(s.cfg.Timeouts.MCPClientMetadataCacheTTL, res.MaxAge, res.HasMaxAge), nil
}
