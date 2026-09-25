package httpapi

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// ListMyMCPAuthorizations backs GET /api/me/mcp-authorizations (technical
// plan §43.18): the caller's own unexpired MCP authorizations -- which
// client, which scopes, when granted, last used and expiring. Gated by
// authz.ActionViewOwnProfile (every role, including viewer): this is a
// view of the caller's own account, never anyone else's. No field is a
// secret: tokens and codes exist only as hashes.
func ListMyMCPAuthorizations(grants *postgres.MCPOAuthGrantStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionViewOwnProfile, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		userID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		rows, err := grants.ListGrantsForUser(ctx, userID)
		if err != nil {
			platform.Logger(ctx).Error("httpapi: list mcp authorizations failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out := make([]restdtos.MCPAuthorization, 0, len(rows))
		for _, row := range rows {
			a := restdtos.MCPAuthorization{
				Id:         row.ID.String(),
				ClientId:   row.ClientPublicID,
				ClientName: row.ClientName,
				ClientKind: restdtos.MCPAuthorizationClientKind(row.ClientKind),
				Scopes:     nonNilStrings(row.Scopes),
				CreatedAt:  row.CreatedAt.Time,
				ExpiresAt:  row.ExpiresAt.Time,
			}
			if row.LastUsedAt.Valid {
				lastUsed := row.LastUsedAt.Time
				a.LastUsedAt = &lastUsed
			}
			out = append(out, a)
		}
		writeJSON(w, http.StatusOK, restdtos.ListMCPAuthorizationsResponse{Authorizations: out})
	}
}

// RevokeMyMCPAuthorization backs DELETE
// /api/me/mcp-authorizations/{authorizationID} (technical plan §43.18):
// deletes one of the caller's OWN authorizations -- and, by cascade, every
// code and access token issued under it, so the client's very next /mcp
// call is refused. Another user's authorization id is indistinguishable
// from a missing one (404). Gated by authz.ActionRevokeOwnMCPAuthorization,
// deliberately not ActionConnectMCPClient: disconnecting must stay open to
// every role even if connecting is ever narrowed. Audited as
// mcp_authorization.revoked, reason "user", in the same transaction.
func RevokeMyMCPAuthorization(pool *pgxpool.Pool, grants *postgres.MCPOAuthGrantStore, clients *postgres.MCPOAuthClientStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionRevokeOwnMCPAuthorization, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)
		userID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		grantID, ok := parseUUIDParam(w, r, "authorizationID", "malformed authorization id")
		if !ok {
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: revoke mcp authorization: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		grantsTx := grants.WithTx(tx)

		// Locks follow the one order every transaction on these tables
		// follows, parent before child (the top of
		// postgres/mcpoauthgrant_store.go, technical plan §43.16): the
		// grant's client FOR KEY SHARE, then the grant, whose deletion
		// cascades to its codes and tokens. So the grant's client is read
		// first, without a lock. A code exchange in flight under the grant
		// already holds it FOR KEY SHARE: the deletion waits for it to
		// commit, then takes the token it issued too.
		existing, err := grantsTx.GetGrant(ctx, grantID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && existing.UserID != userID) {
			writeError(w, http.StatusNotFound, "authorization not found")
			return
		}
		if err != nil {
			logger.Error("httpapi: revoke mcp authorization: load grant failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		client, err := clients.WithTx(tx).LockKeyShare(ctx, existing.ClientID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The client was deleted since, and this grant with it.
			writeError(w, http.StatusNotFound, "authorization not found")
			return
		}
		if err != nil {
			logger.Error("httpapi: revoke mcp authorization: lock client failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		deleted, err := grantsTx.DeleteGrantForUser(ctx, grantID, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "authorization not found")
			return
		}
		if err != nil {
			logger.Error("httpapi: revoke mcp authorization: delete failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := recordAuditLog(ctx, auditLog.WithTx(tx), userID, "mcp_authorization.revoked", "mcp_authorization", deleted.ID.String(), map[string]any{
			"reason":      "user",
			"client_id":   client.ClientID,
			"client_name": client.ClientName,
		}); err != nil {
			logger.Error("httpapi: revoke mcp authorization: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: revoke mcp authorization: commit failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
