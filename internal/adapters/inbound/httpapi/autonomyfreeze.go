// This file (autonomyfreeze.go) implements the operator action behind
// technical plan §40.2's freeze: an administrator freezes autonomy
// platform-wide, so no automatic action starts anywhere, and lifts the
// freeze.
//
//   - GET  /api/autonomy           (the freeze in force, every signed-in role)
//   - POST /api/autonomy/freeze    (freeze, with a reason; admin only)
//   - POST /api/autonomy/unfreeze  (lift the freeze; admin only)
//
// The freeze is the one row of platform_settings (migrations/000163). Every
// automatic-action site reads it on every action through
// internal/app/autonomy.Gate; this file only writes it. Reading it is open
// to every role (authz.ActionViewSessions), because the decision inbox shows
// every role the same banner (internal/app/decisioninbox, freeze.go);
// writing it is authz.ActionManageAutonomyFreeze, admin only (§13.3).
//
// Each change and its audit_log row (autonomy.frozen, autonomy.unfrozen,
// resource platform/autonomy -- §13.3's "written in the same transaction as
// the change", on repoentitlement.go's precedent) commit together: an audit
// insert that fails leaves the freeze as it was. A second freeze answers
// 409 and keeps the first freeze's who, when and why; an unfreeze when
// nothing is frozen answers 409 too. Freezing and unfreezing are a
// person's commands, never a site: neither reads the freeze through the
// gate (internal/ops' ScanAutonomyFreezeSites).

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

// maxAutonomyFreezeReasonChars is the longest reason a freeze keeps, in
// characters after trimming -- the same bound as platform_settings' own
// freeze-shape CHECK, enforced here first so a caller gets a 400 that says
// so rather than a 500.
const maxAutonomyFreezeReasonChars = 500

// The audit_log names of the freeze's two changes (§40.2), on the
// auto_merge.merged naming precedent, and the resource both name.
const (
	auditActionAutonomyFrozen   = "autonomy.frozen"
	auditActionAutonomyUnfrozen = "autonomy.unfrozen"
	auditResourceTypePlatform   = "platform"
	auditResourceIDAutonomy     = "autonomy"
)

// GetAutonomyFreeze backs GET /api/autonomy: 403 unless the caller passes
// authz.ActionViewSessions (every signed-in role); 200 with
// restdtos.AutonomyFreeze otherwise, frozen or not. A missing row reads as
// not frozen, as every site reads it.
func GetAutonomyFreeze(settings *postgres.PlatformSettingsStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if !authorize(w, r, authz.ActionViewSessions, authz.Resource{}) {
			return
		}

		row, err := settings.GetAutonomyFreeze(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeJSON(w, http.StatusOK, autonomyFreezeDTO(sqlcgen.GetAutonomyFreezeRow{}))
		case err != nil:
			platform.Logger(ctx).Error("httpapi: get autonomy freeze failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		default:
			writeJSON(w, http.StatusOK, autonomyFreezeDTO(row))
		}
	}
}

// PostFreezeAutonomy backs POST /api/autonomy/freeze: 403 unless the
// caller passes authz.ActionManageAutonomyFreeze (admin only); 400 for a
// reason blank, longer than 500 characters after trimming, or holding a
// NUL byte (restdtos.FreezeAutonomyRequest); 409 when autonomy is already
// frozen (the first freeze is kept); otherwise the freeze and its
// autonomy.frozen audit row commit together, and 200 answers the freeze in
// force. Every site reads it on its next action, on every replica.
func PostFreezeAutonomy(pool *pgxpool.Pool, settings *postgres.PlatformSettingsStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutonomyFreeze, authz.Resource{}) {
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		// Decoded into a local shape, not restdtos.FreezeAutonomyRequest:
		// that type's generated decoder refuses a missing or empty reason
		// with its own error text, and both are the same "reason is
		// required" here, as is a reason of white space alone
		// (PostRevokeRepoEntitlement's precedent).
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
		if utf8.RuneCountInString(reason) > maxAutonomyFreezeReasonChars {
			writeError(w, http.StatusBadRequest, "reason must be at most 500 characters")
			return
		}
		// A Postgres TEXT column refuses a NUL byte outright, which would
		// surface as a 500 (containsNULByte, providercredentials.go).
		if containsNULByte(reason) {
			writeError(w, http.StatusBadRequest, "reason must not contain a NUL byte")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: freeze autonomy: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		txSettings := settings.WithTx(tx)
		if _, err := txSettings.Freeze(ctx, actorUserID, reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "autonomy is already frozen")
				return
			}
			logger.Error("httpapi: freeze autonomy: write failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, auditActionAutonomyFrozen, auditResourceTypePlatform, auditResourceIDAutonomy, map[string]any{
			"reason": reason,
		}); err != nil {
			logger.Error("httpapi: freeze autonomy: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Read back on the same transaction for the freezing user's
		// display name.
		row, err := txSettings.GetAutonomyFreeze(ctx)
		if err != nil {
			logger.Error("httpapi: freeze autonomy: read back failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: freeze autonomy: commit failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		logger.Info("httpapi: autonomy frozen", "actor_user_id", actorUserID.String())
		writeJSON(w, http.StatusOK, autonomyFreezeDTO(row))
	}
}

// PostUnfreezeAutonomy backs POST /api/autonomy/unfreeze (no body): 403
// unless the caller passes authz.ActionManageAutonomyFreeze (admin only);
// 409 when autonomy is not frozen; otherwise the freeze is lifted and an
// autonomy.unfrozen audit row naming what was lifted -- when, by whom, why,
// and for how long -- commits with it, and 200 answers autonomy no longer
// frozen. Every held candidate is still a candidate: each site takes it up
// again on its next tick (§40.2).
func PostUnfreezeAutonomy(pool *pgxpool.Pool, settings *postgres.PlatformSettingsStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutonomyFreeze, authz.Resource{}) {
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: unfreeze autonomy: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		lifted, err := settings.WithTx(tx).Unfreeze(ctx)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "autonomy is not frozen")
				return
			}
			logger.Error("httpapi: unfreeze autonomy: write failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, auditActionAutonomyUnfrozen, auditResourceTypePlatform, auditResourceIDAutonomy,
			unfreezeAuditDetail(lifted)); err != nil {
			logger.Error("httpapi: unfreeze autonomy: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: unfreeze autonomy: commit failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		logger.Info("httpapi: autonomy unfrozen", "actor_user_id", actorUserID.String())
		writeJSON(w, http.StatusOK, autonomyFreezeDTO(sqlcgen.GetAutonomyFreezeRow{}))
	}
}

// unfreezeAuditDetail is the autonomy.unfrozen audit row's detail: the
// freeze that was lifted -- when it was set, by whom (null when that is not
// on record), why -- and how long it held, in whole seconds. That duration
// is the unfreeze statement's own (UnfreezeAutonomy's frozen_seconds),
// measured on the database's clock at both ends, as the audit row's
// created_at is: a replica's clock never enters it.
func unfreezeAuditDetail(lifted sqlcgen.UnfreezeAutonomyRow) map[string]any {
	detail := map[string]any{
		"frozen_at":      nil,
		"frozen_by":      nil,
		"reason":         lifted.Reason,
		"frozen_seconds": lifted.FrozenSeconds,
	}
	if lifted.FrozenAt.Valid {
		detail["frozen_at"] = lifted.FrozenAt.Time
	}
	if lifted.FrozenBy.Valid {
		detail["frozen_by"] = lifted.FrozenBy.String()
	}
	return detail
}

// autonomyFreezeDTO maps the freeze in force -- the zero row when nothing
// is frozen -- onto restdtos.AutonomyFreeze: frozen, and when, by whom and
// why, each null when not frozen. Shared by the three autonomy routes and
// the decision inbox's banner (decisionInboxResultToDTO), so both say the
// same thing.
func autonomyFreezeDTO(row sqlcgen.GetAutonomyFreezeRow) restdtos.AutonomyFreeze {
	dto := restdtos.AutonomyFreeze{Frozen: row.AutonomyFrozen}
	if !row.AutonomyFrozen {
		return dto
	}
	if row.AutonomyFrozenAt.Valid {
		at := row.AutonomyFrozenAt.Time
		dto.FrozenAt = &at
	}
	if row.AutonomyFrozenBy.Valid {
		id := row.AutonomyFrozenBy.String()
		dto.FrozenByUserId = &id
		dto.FrozenByDisplayName = row.AutonomyFrozenByDisplayName
	}
	dto.Reason = row.AutonomyFreezeReason
	return dto
}
