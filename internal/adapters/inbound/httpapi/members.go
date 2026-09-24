// This file (members.go) implements §13.2's ("identities + full RBAC")
// own second deliverable: the backend-only members API (§13.2/§13.3) --
// list members (with role, linked identities, and system-wide pending
// link-prompt state), an admin-only role-change endpoint, admin manual
// link/unlink of an identity (§13.2 point 5, "admin can force-link"), and
// a read endpoint over audit_log. The actual Settings -> Members UI is
// Phase 7 (§13.4) and explicitly out of scope here -- these are plain
// REST endpoints for whatever future Step builds that screen, following
// this package's own existing conventions (parseSessionID-style path-
// param parsing, writeError/writeJSON, the authorize() helper) rather
// than inventing new ones.
//
// Every handler below is gated by domain/authz.Authorize(actor,
// authz.ActionManageMembers, ...) -- §13.3's own matrix bundles "members &
// roles" as ONE admin-only row (no separate read-vs-write split named in
// the spec table itself), so every route here, including the two plain
// read endpoints (ListMembers, ListAuditLog), reuses that SAME action
// rather than inventing finer-grained Actions the spec doesn't call for.
//
// Routes (mounted by cmd/control-plane/main.go, behind auth.Middleware,
// like every other route in this package):
//
//   - GET    /api/members                              -- ListMembers
//   - PATCH  /api/members/{userID}/role                 -- UpdateMemberRole
//   - POST   /api/members/{userID}/identities            -- LinkMemberIdentity
//   - DELETE /api/members/{userID}/identities/{identityID} -- UnlinkMemberIdentity
//   - GET    /api/audit-log                              -- ListAuditLog

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// uniqueViolationCode is Postgres' own SQLSTATE for a unique-constraint
// violation -- mirrors internal/app/identitylink/service.go's own
// identical constant/isUniqueViolation pair (that package's own doc
// comment on isUniqueViolation explains the general pattern; not reused
// directly from there since it's unexported and this is a different
// package). LinkMemberIdentity's own race fix below is this file's one
// use of it.
const uniqueViolationCode = "23505"

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode
}

// identityToDTO converts one identities row to its own REST wire shape
// (contracts/rest/v1/dtos.schema.json's own Identity, generated as
// restdtos.Identity -- audit finding: wire-contract, this file's own DTOs
// used to be hand-written outside /contracts' schema-driven codegen
// pipeline; see contracts/gen/go/restdtos/restdtos.go for the generated
// type this now returns).
func identityToDTO(i sqlcgen.Identity) restdtos.Identity {
	return restdtos.Identity{
		Id:         i.ID.String(),
		Provider:   restdtos.IdentityProvider(i.Provider),
		ExternalId: i.ExternalID,
		LinkedVia:  restdtos.IdentityLinkedVia(i.LinkedVia),
		CreatedAt:  i.CreatedAt.Time,
	}
}

// ListMembers backs GET /api/members: admin-only (authz.ActionManageMembers),
// returns every user with role/disabled and their own currently-linked
// identities, plus every system-wide still-pending link prompt (§13.2's
// own "pending-link state" -- not yet associated with any member, by
// definition, since resolving THAT is exactly what a prompt is still
// waiting on).
func ListMembers(users *postgres.UserStore, identities *postgres.IdentityStore, linkPrompts *postgres.IdentityLinkPromptStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)

		userRows, err := users.List(ctx)
		if err != nil {
			logger.Error("httpapi: list members: list users failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		memberDTOs := make([]restdtos.Member, 0, len(userRows))
		for _, u := range userRows {
			identityRows, err := identities.ListForUser(ctx, u.ID)
			if err != nil {
				logger.Error("httpapi: list members: list identities failed", "error", err, "user_id", u.ID.String())
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			identityDTOs := make([]restdtos.Identity, 0, len(identityRows))
			for _, i := range identityRows {
				identityDTOs = append(identityDTOs, identityToDTO(i))
			}
			memberDTOs = append(memberDTOs, restdtos.Member{
				Id:          u.ID.String(),
				Email:       u.PrimaryEmail,
				DisplayName: u.DisplayName,
				Role:        restdtos.MemberRole(u.Role),
				Disabled:    u.Disabled,
				CreatedAt:   u.CreatedAt.Time,
				Identities:  identityDTOs,
			})
		}

		promptRows, err := linkPrompts.List(ctx)
		if err != nil {
			logger.Error("httpapi: list members: list link prompts failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		promptDTOs := make([]restdtos.PendingLinkPrompt, 0, len(promptRows))
		for _, p := range promptRows {
			promptDTOs = append(promptDTOs, restdtos.PendingLinkPrompt{
				Provider:   restdtos.PendingLinkPromptProvider(p.Provider),
				ExternalId: p.ExternalID,
				ExpiresAt:  p.ExpiresAt.Time,
				CreatedAt:  p.CreatedAt.Time,
			})
		}

		writeJSON(w, http.StatusOK, restdtos.ListMembersResponse{Members: memberDTOs, PendingLinkPrompts: promptDTOs})
	}
}

// validRoles is the closed set of role strings UpdateMemberRole accepts --
// mirrors authz.AllRoles exactly (converted to plain strings once here,
// rather than re-deriving it per request).
var validRoles = map[string]sqlcgen.UserRole{
	string(authz.RoleAdmin):      sqlcgen.UserRoleAdmin,
	string(authz.RoleMaintainer): sqlcgen.UserRoleMaintainer,
	string(authz.RoleMember):     sqlcgen.UserRoleMember,
	string(authz.RoleViewer):     sqlcgen.UserRoleViewer,
}

// UpdateMemberRole backs PATCH /api/members/{userID}/role: admin-only
// (authz.ActionManageMembers, §13.3's own "members & roles: admin only"
// row), body {"role": "admin"|"maintainer"|"member"|"viewer"}. 400 on a
// malformed body or an unrecognized role string; 404 if userID doesn't
// name a real user; 409 if the target is CURRENTLY an active (non-
// disabled) admin and this change would leave zero active admins left
// (the last-admin guard, an audit finding: H8 -- demoting the sole
// remaining admin would permanently lock the deployment out of every
// admin-only endpoint, including this one, with no recovery path short
// of direct DB surgery); else 200 with the updated restdtos.Member,
// including the target's own actual currently-linked identities (audit
// finding: wire-contract -- this response used to leave Identities nil,
// which was merely sloppy before Member.identities became a schema-
// required, non-nullable array in this same batch's /contracts migration,
// but is a genuine contract violation now; fetched the same way
// ListMembers already does for each member, just for this one target,
// inside the SAME transaction as the role update itself since the
// identities table is unaffected by it either way).
func UpdateMemberRole(pool *pgxpool.Pool, users *postgres.UserStore, identities *postgres.IdentityStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		targetUserID, ok := parseUUIDParam(w, r, "userID", "malformed member id")
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateMemberRoleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		newRole, ok := validRoles[req.Role]
		if !ok {
			writeError(w, http.StatusBadRequest, "unrecognized role")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: update member role: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		existing, err := users.WithTx(tx).GetByID(ctx, targetUserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "member not found")
				return
			}
			logger.Error("httpapi: update member role: get user failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Last-admin guard (audit finding H8): only relevant when this
		// change actually TAKES admin away from someone who currently HAS
		// it (a disabled admin, or a no-op admin->admin "change", never
		// counted as active in the first place, so neither can trip this).
		if existing.Role == sqlcgen.UserRoleAdmin && !existing.Disabled && newRole != sqlcgen.UserRoleAdmin {
			activeAdminIDs, err := users.WithTx(tx).ListActiveAdminIDsForUpdate(ctx)
			if err != nil {
				logger.Error("httpapi: update member role: list active admins failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			// existing is itself one of the rows ListActiveAdminIDsForUpdate
			// just locked (its own role/disabled columns, read a moment ago
			// in this SAME transaction, already satisfy that query's own
			// WHERE clause) -- so len<=1 here can only mean targetUserID IS
			// that one remaining active admin.
			if len(activeAdminIDs) <= 1 {
				writeError(w, http.StatusConflict, "cannot demote the last remaining admin")
				return
			}
		}

		updated, err := users.WithTx(tx).UpdateRole(ctx, targetUserID, newRole)
		if err != nil {
			logger.Error("httpapi: update member role: update failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, "member.role_changed", "user", targetUserID.String(), map[string]any{
			"from_role": string(existing.Role),
			"to_role":   string(updated.Role),
		}); err != nil {
			logger.Error("httpapi: update member role: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Same query ListMembers itself uses to build each member's own
		// identities list (see this file's own ListMembers above) -- run
		// inside this same tx (harmless: the identities table is untouched
		// by this transaction either way) so the response is assembled
		// before commit, not as a separate post-commit query.
		identityRows, err := identities.WithTx(tx).ListForUser(ctx, targetUserID)
		if err != nil {
			logger.Error("httpapi: update member role: list identities failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		identityDTOs := make([]restdtos.Identity, 0, len(identityRows))
		for _, i := range identityRows {
			identityDTOs = append(identityDTOs, identityToDTO(i))
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: update member role: commit tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, restdtos.Member{
			Id:          updated.ID.String(),
			Email:       updated.PrimaryEmail,
			DisplayName: updated.DisplayName,
			Role:        restdtos.MemberRole(updated.Role),
			Disabled:    updated.Disabled,
			CreatedAt:   updated.CreatedAt.Time,
			Identities:  identityDTOs,
		})
	}
}

// validProviders is the closed set of provider strings LinkMemberIdentity
// accepts -- mirrors the identity_provider Postgres enum
// (migrations/000003_identities.up.sql) exactly.
var validProviders = map[string]sqlcgen.IdentityProvider{
	string(sqlcgen.IdentityProviderGithub): sqlcgen.IdentityProviderGithub,
	string(sqlcgen.IdentityProviderSlack):  sqlcgen.IdentityProviderSlack,
	string(sqlcgen.IdentityProviderLinear): sqlcgen.IdentityProviderLinear,
	string(sqlcgen.IdentityProviderGoogle): sqlcgen.IdentityProviderGoogle,
	string(sqlcgen.IdentityProviderOidc):   sqlcgen.IdentityProviderOidc,
}

// LinkMemberIdentity backs POST /api/members/{userID}/identities:
// admin-only (authz.ActionManageMembers), body {"provider":...,
// "externalId":...} -- §13.2 point 5's own "admin can force-link".
// linked_via is always "admin" (sqlcgen.IdentityLinkedViaAdmin) here,
// regardless of what created the row this endpoint is being used to
// correct/establish -- an admin explicitly choosing to link two specific
// ids together, through this endpoint, is exactly what that enum value
// means (mirrors internal/adapters/inbound/auth's own identical
// first-time-sign-in overload of the same value, see that package's own
// createUserAndIdentity doc comment).
//
// 404 if userID doesn't name a real user. 409 if (provider, externalId)
// is ALREADY linked to a DIFFERENT user (an admin must unlink it first,
// via UnlinkMemberIdentity, rather than this endpoint silently
// reassigning it). 200 (not 201) if it's already linked to THIS SAME
// user -- idempotent, not an error. Else 201 with the new restdtos.Identity.
//
// The already-linked check (identities.GetByProviderAndExternalID) and
// the subsequent Create both run INSIDE the same transaction (an audit
// finding, H8: they used to straddle two separate transactions/
// connections, so a concurrent duplicate-link request could race past
// the check and hit the identities.(provider, external_id) unique
// constraint directly, surfacing as a raw 500 instead of this handler's
// own intended 409/200). Running the check inside the transaction
// narrows that window; the Create call's own isUniqueViolation branch
// below closes it completely -- mirrors internal/app/identitylink's own
// autoLink "lost the race, resolve the winner" precedent (service.go).
func LinkMemberIdentity(pool *pgxpool.Pool, users *postgres.UserStore, identities *postgres.IdentityStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		targetUserID, ok := parseUUIDParam(w, r, "userID", "malformed member id")
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.LinkMemberIdentityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		provider, ok := validProviders[req.Provider]
		if !ok {
			writeError(w, http.StatusBadRequest, "unrecognized provider")
			return
		}
		if req.ExternalId == "" {
			writeError(w, http.StatusBadRequest, "externalId is required")
			return
		}

		if _, err := users.GetByID(ctx, targetUserID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "member not found")
				return
			}
			logger.Error("httpapi: link member identity: get user failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: link member identity: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		existing, err := identities.WithTx(tx).GetByProviderAndExternalID(ctx, provider, req.ExternalId)
		if err == nil {
			if existing.UserID != targetUserID {
				writeError(w, http.StatusConflict, "this identity is already linked to a different member")
				return
			}
			writeJSON(w, http.StatusOK, identityToDTO(existing))
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("httpapi: link member identity: lookup existing identity failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		created, err := identities.WithTx(tx).Create(ctx, sqlcgen.CreateIdentityParams{
			UserID:     targetUserID,
			Provider:   provider,
			ExternalID: req.ExternalId,
			LinkedVia:  sqlcgen.IdentityLinkedViaAdmin,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Lost the race: a concurrent LinkMemberIdentity call for
				// the SAME (provider, externalId) committed its own Create
				// between our GetByProviderAndExternalID check above and
				// this INSERT. This tx is now aborted (Postgres refuses
				// further commands on it after a constraint violation), so
				// resolve the winner via a fresh, pool-scoped lookup --
				// mirrors internal/app/identitylink's own autoLink "lost
				// the race, resolve the winner" precedent (service.go) --
				// and answer with the exact same 409-vs-200 split the
				// check above would have given had it merely run a moment
				// later.
				winner, getErr := identities.GetByProviderAndExternalID(ctx, provider, req.ExternalId)
				if getErr != nil {
					logger.Error("httpapi: link member identity: resolve winner after lost race failed", "error", getErr)
					writeError(w, http.StatusInternalServerError, "internal error")
					return
				}
				if winner.UserID != targetUserID {
					writeError(w, http.StatusConflict, "this identity is already linked to a different member")
					return
				}
				writeJSON(w, http.StatusOK, identityToDTO(winner))
				return
			}
			logger.Error("httpapi: link member identity: create failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, "identity.force_linked", "identity", created.ID.String(), map[string]any{
			"user_id":     targetUserID.String(),
			"provider":    string(provider),
			"external_id": req.ExternalId,
		}); err != nil {
			logger.Error("httpapi: link member identity: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: link member identity: commit tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusCreated, identityToDTO(created))
	}
}

// UnlinkMemberIdentity backs DELETE
// /api/members/{userID}/identities/{identityID}: admin-only
// (authz.ActionManageMembers). 404 (never distinguished from a genuine
// "no such identity at all") if identityID doesn't exist OR exists but
// belongs to a DIFFERENT user than userID -- mirrors auth.Middleware's own
// "no differential signal" precedent: a caller probing identity ids
// belonging to other members learns nothing extra from the response.
// Else 204, with an audit-log entry recorded first, in the SAME
// transaction as the delete.
func UnlinkMemberIdentity(pool *pgxpool.Pool, identities *postgres.IdentityStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		targetUserID, ok := parseUUIDParam(w, r, "userID", "malformed member id")
		if !ok {
			return
		}
		identityID, ok := parseUUIDParam(w, r, "identityID", "malformed identity id")
		if !ok {
			return
		}

		existing, err := identities.ListForUser(ctx, targetUserID)
		if err != nil {
			logger.Error("httpapi: unlink member identity: list identities failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var target sqlcgen.Identity
		found := false
		for _, i := range existing {
			if i.ID == identityID {
				target = i
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "identity not found")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: unlink member identity: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, "identity.unlinked", "identity", identityID.String(), map[string]any{
			"user_id":     targetUserID.String(),
			"provider":    string(target.Provider),
			"external_id": target.ExternalID,
		}); err != nil {
			logger.Error("httpapi: unlink member identity: record audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if _, err := identities.WithTx(tx).Delete(ctx, identityID); err != nil {
			logger.Error("httpapi: unlink member identity: delete failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: unlink member identity: commit tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// defaultAuditLogPageSize/maxAuditLogPageSize bound GET /api/audit-log's
// own ?limit= query param -- a plain, small page size (see queries/
// audit_log.sql's own doc comment on ListAuditLogEntries for why this is
// deliberately NOT cursor-paginated).
const (
	defaultAuditLogPageSize = 50
	maxAuditLogPageSize     = 200
)

// ListAuditLog backs GET /api/audit-log?limit=&offset=: admin-only
// (authz.ActionManageMembers, §13.3: "surfaced in Settings -> Members
// ('Audit log')"). limit defaults to defaultAuditLogPageSize, capped at
// maxAuditLogPageSize; offset defaults to 0. Malformed limit/offset query
// values are treated as absent (defaulted), never a 400 -- this is a
// read-only convenience endpoint for an admin-facing view, not a strict
// API contract a machine client depends on getting a validation error
// from.
//
// Each row's own detail_json is passed through as opaque raw JSON (see
// this loop's own inline comment below) rather than decoded into a typed
// map -- and a single row whose detail_json isn't even a well-formed JSON
// object (no CHECK constraint at the DB layer rules this out for some
// bad/legacy row) degrades in isolation -- an empty object substituted,
// logged with that row's own id -- rather than failing this entire page
// for every admin (an audit finding: LOW, page-wide 500 on one bad row).
func ListAuditLog(auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionManageMembers, authz.Resource{}) {
			return
		}
		ctx := r.Context()
		logger := platform.Logger(ctx)

		limit := int32(defaultAuditLogPageSize)
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= maxAuditLogPageSize {
				limit = int32(parsed)
			}
		}
		offset := int32(0)
		if raw := r.URL.Query().Get("offset"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
				offset = int32(parsed)
			}
		}

		rows, err := auditLog.List(ctx, limit, offset)
		if err != nil {
			logger.Error("httpapi: list audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		entries := make([]restdtos.AuditLogEntry, 0, len(rows))
		for _, row := range rows {
			var actorUserID *string
			if row.ActorUserID.Valid {
				s := row.ActorUserID.String()
				actorUserID = &s
			}
			// detail_json is EXPECTED to always be a well-formed JSON object
			// at the DB layer (migrations/000013_audit_log.up.sql's own
			// "JSONB NOT NULL DEFAULT '{}'::jsonb", and auditlog.Record's own
			// signature only ever accepts a map[string]any) -- but nothing
			// enforces that shape at the column level (no CHECK constraint),
			// so a bad/legacy row can't be ruled out. restdtos.AuditLogEntry.
			// Detail is json.RawMessage, an opaque raw-JSON passthrough (see
			// dtos.schema.json's own "detail" field doc comment, audit
			// finding: LOW, decode-then-re-encode precision loss) rather than
			// a decoded map -- row.DetailJson's own bytes are used verbatim
			// below, never rebuilt from a decoded value, so a legitimate
			// row's own large integers/key order survive untouched end to
			// end. shapeProbe below decodes ONLY to verify detail_json really
			// is a JSON object (its own decoded values are discarded
			// immediately, never used to build the response) -- a row that
			// fails this check degrades in isolation: logged with its own
			// row id and substituted with an empty object, rather than
			// failing this entire page for every admin (an audit finding:
			// LOW, page-wide 500 on one bad row).
			detail := json.RawMessage(row.DetailJson)
			var shapeProbe map[string]json.RawMessage
			// A top-level JSON `null` unmarshals into a map with err == nil
			// (encoding/json treats it as a no-op success, not a type
			// mismatch) -- shapeProbe == nil catches that case too, since a
			// genuinely-object detail_json ('{}' included) always leaves
			// shapeProbe non-nil.
			if err := json.Unmarshal(row.DetailJson, &shapeProbe); err != nil || shapeProbe == nil {
				logger.Warn("httpapi: list audit log: detail_json is not a well-formed JSON object; substituting {} for this row only", "error", err, "audit_log_id", row.ID.String())
				detail = json.RawMessage(`{}`)
			}
			entries = append(entries, restdtos.AuditLogEntry{
				Id:            row.ID.String(),
				ActorUserId:   actorUserID,
				Action:        row.Action,
				ResourceType:  row.ResourceType,
				ResourceId:    row.ResourceID,
				Detail:        detail,
				CorrelationId: row.CorrelationID,
				CreatedAt:     row.CreatedAt.Time,
			})
		}

		writeJSON(w, http.StatusOK, restdtos.ListAuditLogResponse{Entries: entries})
	}
}

// parseUUIDParam parses chi's own paramName URL path param as a UUID,
// writing a 400 response and returning ok=false on failure -- mirrors
// parseSessionID's own identical shape, generalized to an arbitrary
// param name since this file's own routes carry two different id kinds
// (userID, identityID) neither of which is a sessionID.
func parseUUIDParam(w http.ResponseWriter, r *http.Request, paramName, errMessage string) (pgtype.UUID, bool) {
	raw := chi.URLParam(r, paramName)
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		writeError(w, http.StatusBadRequest, errMessage)
		return pgtype.UUID{}, false
	}
	return id, true
}
