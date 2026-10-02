// This file (repoentitlement.go) implements the operator action behind
// technical plan §31.4's "Un-entitlement": an administrator revokes a
// repository's eligibility for new sessions, and restores it.
//
//   - GET  /api/repos/{owner}/{repo}/entitlement          (status)
//   - POST /api/repos/{owner}/{repo}/entitlement/revoke   (revoke, with a reason)
//   - POST /api/repos/{owner}/{repo}/entitlement/restore  (restore)
//
// All three are gated by one admin-only action,
// authz.ActionManageRepoEntitlement (§13.3), then scoped like every other
// repository-scoped route (resolveKnownRepo: 404 for a repository this
// deployment has never seen a pull-request session for, so a revocation is
// always keyed by a name a webhook payload used). That scoping reads
// eligibility's Known alone, so a revoked repository stays reachable here --
// which is how it is restored.
//
// A revocation is a row in repo_entitlement_revocations; restore deletes
// it. Each change and its audit_log row (repo_entitlement.revoked,
// repo_entitlement.restored -- §13.3's "written in the same transaction as
// the change") commit together. A second revoke and a restore of a
// repository that is not revoked both answer 409, and a second revoke
// keeps the first revocation's who, when and why. What a revocation does is
// elsewhere: session creation refuses the repository on every surface
// (repoentitlementgate.go), and the session actor refuses its sessions'
// pending turns before they reach a sandbox (internal/app/sessionactor,
// repoentitlement.go).

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// maxRepoEntitlementReasonChars is the longest reason a revocation keeps,
// in characters after trimming -- the same bound as
// repo_entitlement_revocations' own CHECK, enforced here
// first so a caller gets a 400 that says so rather than a 500.
const maxRepoEntitlementReasonChars = 500

// GetRepoEntitlement backs GET /api/repos/{owner}/{repo}/entitlement: 403
// unless the caller passes authz.ActionManageRepoEntitlement (admin only);
// 404 for a repository this deployment does not know; 200 with
// restdtos.RepoEntitlement otherwise, revoked or not.
func GetRepoEntitlement(revocations *postgres.RepoEntitlementRevocationStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if !authorize(w, r, authz.ActionManageRepoEntitlement, authz.Resource{}) {
			return
		}
		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		row, err := revocations.Get(ctx, repoFullName)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeJSON(w, http.StatusOK, repoEntitlementDTO(repoFullName, nil))
		case err != nil:
			platform.Logger(ctx).Error("httpapi: get repo entitlement failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
		default:
			writeJSON(w, http.StatusOK, repoEntitlementDTO(repoFullName, &row))
		}
	}
}

// PostRevokeRepoEntitlement backs POST
// /api/repos/{owner}/{repo}/entitlement/revoke: 403 unless the caller
// passes authz.ActionManageRepoEntitlement (admin only); 404 for a
// repository this deployment does not know; 400 for a reason blank or
// longer than 500 characters after trimming (restdtos.
// RevokeRepoEntitlementRequest); 409 when the repository is already
// revoked (the first revocation is kept); otherwise the revocation and its
// repo_entitlement.revoked audit row commit together, and 200 answers the
// repository's restdtos.RepoEntitlement.
func PostRevokeRepoEntitlement(pool *pgxpool.Pool, revocations *postgres.RepoEntitlementRevocationStore, auditLog *postgres.AuditLogStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageRepoEntitlement, authz.Resource{}) {
			return
		}
		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		// Decoded into a local shape, not restdtos.RevokeRepoEntitlementRequest:
		// that type's generated decoder refuses a missing or empty reason
		// with its own error text, and both are the same "reason is
		// required" here, as is a reason of white space alone.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var body struct {
			Reason *string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		reason := ""
		if body.Reason != nil {
			reason = strings.TrimSpace(*body.Reason)
		}
		if reason == "" {
			writeError(w, http.StatusBadRequest, "reason is required")
			return
		}
		if utf8.RuneCountInString(reason) > maxRepoEntitlementReasonChars {
			writeError(w, http.StatusBadRequest, "reason must be at most 500 characters")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: revoke repo entitlement: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := revocations.WithTx(tx).Revoke(ctx, repoFullName, actorUserID, reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "repository entitlement is already revoked")
				return
			}
			logger.Error("httpapi: revoke repo entitlement: insert failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, "repo_entitlement.revoked", "repo", repoFullName, map[string]any{
			"reason": reason,
		}); err != nil {
			logger.Error("httpapi: revoke repo entitlement: record audit log failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Read back on the same transaction for the revoking user's
		// display name.
		row, err := revocations.WithTx(tx).Get(ctx, repoFullName)
		if err != nil {
			logger.Error("httpapi: revoke repo entitlement: read back failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: revoke repo entitlement: commit failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		logger.Info("httpapi: repository entitlement revoked", "repo", repoFullName, "actor_user_id", actorUserID.String())
		writeJSON(w, http.StatusOK, repoEntitlementDTO(repoFullName, &row))
	}
}

// PostRestoreRepoEntitlement backs POST
// /api/repos/{owner}/{repo}/entitlement/restore (no body): 403 unless the
// caller passes authz.ActionManageRepoEntitlement (admin only); 404 for a
// repository this deployment does not know; 409 when the repository is not
// revoked; otherwise the revocation is deleted and a
// repo_entitlement.restored audit row naming what was lifted (when, by
// whom, why) commits with it, and 200 answers the repository's
// restdtos.RepoEntitlement, no longer revoked. Restoring re-opens only what
// the deployment already knows; it never grants eligibility.
func PostRestoreRepoEntitlement(pool *pgxpool.Pool, revocations *postgres.RepoEntitlementRevocationStore, auditLog *postgres.AuditLogStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageRepoEntitlement, authz.Resource{}) {
			return
		}
		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: restore repo entitlement: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		lifted, err := revocations.WithTx(tx).Restore(ctx, repoFullName)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "repository entitlement is not revoked")
				return
			}
			logger.Error("httpapi: restore repo entitlement: delete failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		detail := map[string]any{
			"revoked_at": lifted.RevokedAt.Time,
			"revoked_by": nil,
			"reason":     lifted.Reason,
		}
		if lifted.RevokedBy.Valid {
			detail["revoked_by"] = lifted.RevokedBy.String()
		}
		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, "repo_entitlement.restored", "repo", repoFullName, detail); err != nil {
			logger.Error("httpapi: restore repo entitlement: record audit log failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: restore repo entitlement: commit failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		logger.Info("httpapi: repository entitlement restored", "repo", repoFullName, "actor_user_id", actorUserID.String())
		writeJSON(w, http.StatusOK, repoEntitlementDTO(repoFullName, nil))
	}
}

// repoEntitlementDTO maps a repository's revocation, nil when it is not
// revoked, onto restdtos.RepoEntitlement.
func repoEntitlementDTO(repoFullName string, row *sqlcgen.GetRepoEntitlementRevocationRow) restdtos.RepoEntitlement {
	dto := restdtos.RepoEntitlement{RepoFullName: repoFullName}
	if row == nil {
		return dto
	}
	dto.Revoked = true
	if row.RevokedAt.Valid {
		at := row.RevokedAt.Time
		dto.RevokedAt = &at
	}
	if row.RevokedBy.Valid {
		id := row.RevokedBy.String()
		dto.RevokedByUserId = &id
	}
	dto.RevokedByDisplayName = row.RevokedByDisplayName
	reason := row.Reason
	dto.Reason = &reason
	return dto
}
