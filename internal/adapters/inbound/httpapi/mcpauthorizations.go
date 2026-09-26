package httpapi

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
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
		writeJSON(w, http.StatusOK, mcpAuthorizationsResponse(rows))
	}
}

// ListMemberMCPAuthorizations backs GET
// /api/members/{userID}/mcp-authorizations (technical plan §43.18): an
// administrator's view of one member's unexpired MCP authorizations --
// exactly the MCPAuthorization rows the member sees of their own, and
// nothing more. Gated by authz.ActionManageMembers (admin only, like every
// other /api/members route: an administrator already changes roles and
// links identities). 404 when userID names no user. No field is a secret:
// tokens and codes exist only as hashes, so an administrator can never
// see or use one.
func ListMemberMCPAuthorizations(users *postgres.UserStore, grants *postgres.MCPOAuthGrantStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)
		targetUserID, ok := parseUUIDParam(w, r, "userID", "malformed member id")
		if !ok {
			return
		}
		if _, err := users.GetByID(ctx, targetUserID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "member not found")
				return
			}
			logger.Error("httpapi: list member mcp authorizations: get user failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		rows, err := grants.ListGrantsForUser(ctx, targetUserID)
		if err != nil {
			logger.Error("httpapi: list member mcp authorizations failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, mcpAuthorizationsResponse(rows))
	}
}

// mcpAuthorizationsResponse renders one user's grants, as both list routes
// answer them.
func mcpAuthorizationsResponse(rows []sqlcgen.ListMCPOAuthGrantsForUserRow) restdtos.ListMCPAuthorizationsResponse {
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
	return restdtos.ListMCPAuthorizationsResponse{Authorizations: out}
}

// RevokeMyMCPAuthorization backs DELETE
// /api/me/mcp-authorizations/{authorizationID} (technical plan §43.18):
// deletes one of the caller's OWN authorizations -- and, by cascade, every
// code, access token and refresh token issued under it, so the client's
// very next /mcp call is refused and it cannot refresh. Another user's
// authorization id is indistinguishable from a missing one (404). Gated by
// authz.ActionRevokeOwnMCPAuthorization, deliberately not
// ActionConnectMCPClient: disconnecting must stay open to every role even
// if connecting is ever narrowed. Audited as mcp_authorization.revoked,
// reason "user", in the same transaction.
func RevokeMyMCPAuthorization(pool *pgxpool.Pool, grants *postgres.MCPOAuthGrantStore, clients *postgres.MCPOAuthClientStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionRevokeOwnMCPAuthorization, authz.Resource{}) {
			return
		}
		userID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		grantID, ok := parseUUIDParam(w, r, "authorizationID", "malformed authorization id")
		if !ok {
			return
		}
		revokeMCPAuthorization(w, r, mcpRevocation{
			pool: pool, grants: grants, clients: clients, auditLog: auditLog,
			grantID: grantID, ownerID: userID, actorID: userID,
			detail: map[string]any{"reason": "user"},
		})
	}
}

// RevokeMemberMCPAuthorization backs DELETE
// /api/members/{userID}/mcp-authorizations/{authorizationID} (technical
// plan §43.18): an administrator revokes one of a member's authorizations
// on the member's behalf -- the very deletion the member's own route
// makes, so every code, access token and refresh token under it goes with
// it and the client's next /mcp call is refused. Only a grant belonging to
// userID is revoked: any other id -- another member's grant included -- is
// the same 404 as a missing one, so the route can never reach a grant
// through the wrong member. Gated by authz.ActionManageMembers (admin
// only). Audited as mcp_authorization.revoked, reason "admin", attributed
// to the administrator, with detail.target_user_id naming the member.
func RevokeMemberMCPAuthorization(pool *pgxpool.Pool, grants *postgres.MCPOAuthGrantStore, clients *postgres.MCPOAuthClientStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		actorID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		targetUserID, ok := parseUUIDParam(w, r, "userID", "malformed member id")
		if !ok {
			return
		}
		grantID, ok := parseUUIDParam(w, r, "authorizationID", "malformed authorization id")
		if !ok {
			return
		}
		revokeMCPAuthorization(w, r, mcpRevocation{
			pool: pool, grants: grants, clients: clients, auditLog: auditLog,
			grantID: grantID, ownerID: targetUserID, actorID: actorID,
			detail: map[string]any{"reason": "admin", "target_user_id": targetUserID.String()},
		})
	}
}

// mcpRevocation is one Settings revocation of an MCP authorization: the
// grant, the user it must belong to, who is revoking it, and the audit
// detail that says why (the client's own identity is added to it).
type mcpRevocation struct {
	pool     *pgxpool.Pool
	grants   *postgres.MCPOAuthGrantStore
	clients  *postgres.MCPOAuthClientStore
	auditLog *postgres.AuditLogStore
	grantID  pgtype.UUID
	ownerID  pgtype.UUID
	actorID  pgtype.UUID
	detail   map[string]any
}

// revokeMCPAuthorization deletes v.grantID if it belongs to v.ownerID, in
// one transaction with its mcp_authorization.revoked audit row, and
// answers 204; a grant that is missing, gone, or another user's is 404.
func revokeMCPAuthorization(w http.ResponseWriter, r *http.Request, v mcpRevocation) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	tx, err := v.pool.Begin(ctx)
	if err != nil {
		logger.Error("httpapi: revoke mcp authorization: begin tx failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grantsTx := v.grants.WithTx(tx)

	// Locks follow the one order every transaction on these tables
	// follows, parent before child (the top of
	// postgres/mcpoauthgrant_store.go, technical plan §43.16): the grant's
	// client FOR KEY SHARE, then the grant, whose deletion cascades to its
	// codes and tokens. So the grant's client is read first, without a
	// lock. A code exchange or refresh in flight under the grant already
	// holds it FOR KEY SHARE: the deletion waits for it to commit, then
	// takes what it issued too.
	existing, err := grantsTx.GetGrant(ctx, v.grantID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && existing.UserID != v.ownerID) {
		writeError(w, http.StatusNotFound, "authorization not found")
		return
	}
	if err != nil {
		logger.Error("httpapi: revoke mcp authorization: load grant failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	client, err := v.clients.WithTx(tx).LockKeyShare(ctx, existing.ClientID)
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
	deleted, err := grantsTx.DeleteGrantForUser(ctx, v.grantID, v.ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "authorization not found")
		return
	}
	if err != nil {
		logger.Error("httpapi: revoke mcp authorization: delete failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	detail := map[string]any{"client_id": client.ClientID, "client_name": client.ClientName}
	for k, val := range v.detail {
		detail[k] = val
	}
	if err := recordAuditLog(ctx, v.auditLog.WithTx(tx), v.actorID, "mcp_authorization.revoked", "mcp_authorization", deleted.ID.String(), detail); err != nil {
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
