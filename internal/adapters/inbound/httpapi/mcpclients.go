package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/platform"
)

// mcpClientIDPrefix marks a pre-registered MCP client's public client_id
// (technical plan §43.15). A client_id is an identifier, never a secret.
const mcpClientIDPrefix = "narvi_mcp_c_"

// mcpClientToDTO renders one mcp_oauth_clients row.
func mcpClientToDTO(c sqlcgen.McpOauthClient) restdtos.MCPClient {
	dto := restdtos.MCPClient{
		Id:           c.ID.String(),
		ClientId:     c.ClientID,
		ClientName:   c.ClientName,
		Kind:         restdtos.MCPClientKind(c.Kind),
		RedirectUris: nonNilStrings(c.RedirectUris),
		ClientUri:    restdtos.MCPClientClientUri(c.ClientUri),
		CreatedAt:    c.CreatedAt.Time,
	}
	if c.DisabledAt.Valid {
		disabled := c.DisabledAt.Time
		dto.DisabledAt = &disabled
	}
	return dto
}

// ListMCPClients backs GET /api/mcp-clients (technical plan §43.15): every
// registered MCP client, oldest first. Admin only
// (authz.ActionManageIntegrations): which apps may ask this deployment's
// users for access is an integration decision.
func ListMCPClients(clients *postgres.MCPOAuthClientStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageIntegrations, authz.Resource{}) {
			return
		}
		rows, err := clients.List(r.Context())
		if err != nil {
			platform.Logger(r.Context()).Error("httpapi: list mcp clients failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out := make([]restdtos.MCPClient, 0, len(rows))
		for _, row := range rows {
			out = append(out, mcpClientToDTO(row))
		}
		writeJSON(w, http.StatusOK, restdtos.ListMCPClientsResponse{Clients: out})
	}
}

// CreateMCPClient backs POST /api/mcp-clients (technical plan §43.15):
// pre-register an MCP client. The server generates the public client_id;
// every redirect URI must pass internal/domain/mcpclient's rule -- the
// SAME rule the authorization endpoint matches against, so no URI is ever
// stored that consent could not use -- and the name must be displayable
// as-is on the consent page. Admin only (authz.ActionManageIntegrations).
// Audited as mcp_client.created in the same transaction.
func CreateMCPClient(pool *pgxpool.Pool, clients *postgres.MCPOAuthClientStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageIntegrations, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.CreateMCPClientRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		name, err := mcpclient.ValidateClientName(req.ClientName)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(req.RedirectUris) == 0 || len(req.RedirectUris) > mcpclient.MaxRedirectURIs {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("between 1 and %d redirect URIs are required", mcpclient.MaxRedirectURIs))
			return
		}
		seen := make(map[string]bool, len(req.RedirectUris))
		redirects := make([]string, 0, len(req.RedirectUris))
		for _, uri := range req.RedirectUris {
			if err := mcpclient.ValidateRedirectURI(uri); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			if !seen[uri] {
				seen[uri] = true
				redirects = append(redirects, uri)
			}
		}
		var clientURI *string
		if req.ClientUri != nil && *req.ClientUri != "" {
			if err := mcpclient.ValidateClientURI(*req.ClientUri); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			clientURI = req.ClientUri
		}

		suffix, err := platform.GenerateToken()
		if err != nil {
			logger.Error("httpapi: create mcp client: generate client id failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: create mcp client: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		row, err := clients.WithTx(tx).Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
			ClientID:     mcpClientIDPrefix + suffix,
			Kind:         sqlcgen.McpOauthClientKindPreregistered,
			ClientName:   name,
			ClientUri:    clientURI,
			RedirectUris: redirects,
			CreatedBy:    actorUserID,
		})
		if err != nil {
			logger.Error("httpapi: create mcp client: insert failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := recordAuditLog(ctx, auditLog.WithTx(tx), actorUserID, "mcp_client.created", "mcp_client", row.ID.String(), map[string]any{
			"client_id":     row.ClientID,
			"client_name":   row.ClientName,
			"redirect_uris": redirects,
		}); err != nil {
			logger.Error("httpapi: create mcp client: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: create mcp client: commit failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusCreated, mcpClientToDTO(row))
	}
}

// DeleteMCPClient backs DELETE /api/mcp-clients/{clientID} (technical plan
// §43.15, clientID = the client's internal id): removes the client and, by
// cascade, every authorization, pending request, code and access token
// issued to it -- each affected user's client is refused on its very next
// /mcp call. Admin only (authz.ActionManageIntegrations). Audited in the
// same transaction: mcp_client.deleted, plus one mcp_authorization.revoked
// (reason "client_deleted") per authorization the deletion took with it,
// attributed to the administrator who deleted the client.
func DeleteMCPClient(pool *pgxpool.Pool, clients *postgres.MCPOAuthClientStore, grants *postgres.MCPOAuthGrantStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageIntegrations, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		clientID, ok := parseUUIDParam(w, r, "clientID", "malformed client id")
		if !ok {
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: delete mcp client: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		affected, err := grants.WithTx(tx).ListGrantsForClient(ctx, clientID)
		if err != nil {
			logger.Error("httpapi: delete mcp client: list grants failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		deleted, err := clients.WithTx(tx).Delete(ctx, clientID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "client not found")
			return
		}
		if err != nil {
			logger.Error("httpapi: delete mcp client: delete failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		audit := auditLog.WithTx(tx)
		if err := recordAuditLog(ctx, audit, actorUserID, "mcp_client.deleted", "mcp_client", deleted.ID.String(), map[string]any{
			"client_id":              deleted.ClientID,
			"client_name":            deleted.ClientName,
			"revoked_authorizations": len(affected),
		}); err != nil {
			logger.Error("httpapi: delete mcp client: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, g := range affected {
			if err := recordAuditLog(ctx, audit, actorUserID, "mcp_authorization.revoked", "mcp_authorization", g.ID.String(), map[string]any{
				"reason":         "client_deleted",
				"target_user_id": g.UserID.String(),
				"client_id":      deleted.ClientID,
				"client_name":    deleted.ClientName,
			}); err != nil {
				logger.Error("httpapi: delete mcp client: record revocation audit failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: delete mcp client: commit failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
