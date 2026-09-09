// This file (releasecompositiondecision.go) implements §12.2 item 9's own
// composition-finding actions ("Block release / Acknowledge & ship /
// Unblock"):
//
//   - POST /api/sessions/{sessionID}/release-manifest/block
//   - POST /api/sessions/{sessionID}/release-manifest/acknowledge
//   - POST /api/sessions/{sessionID}/release-manifest/unblock
//
// All three resolve their target release_manifest_checks row from
// sessionID alone (ReleaseManifestCheckStore.GetBySessionID, the
// addition), validate the requested transition via internal/domain/review.
// TransitionCompositionDecision (an already-decided composition, or one
// whose own composition review has not completed yet, rejects all three
// actions), then persist it via a guarded compare-and-swap
// (ReleaseManifestCheckStore.UpdateCompositionDecision) keyed on the SAME
// current value just validated -- a concurrent decision loses the race
// with an honest 409, never a silent overwrite (mirrors this package's
// own established guarded-write discipline, e.g. reviewfindings.go's own
// finding-status transitions).
//
// # RBAC: three different rows for three different risk shapes
//
// Block reuses authz.ActionEditReviewVerdict (§13.3 row 5, admin/
// maintainer) -- blocking is the SAFETY-additive response to a
// composition finding, the same "maintainer-level review-adjacent write"
// shape ActionEditReviewVerdict/ActionRetriggerReview and reviewfindings.
// go's own rebuttal-dismissal action already cover.
//
// Acknowledge gates on the NEW authz.ActionAcknowledgeReleaseComposition
// (§13.3 row 6, admin only) -- see that action's own doc comment
// (internal/domain/authz/action.go) for the full "why admin-only"
// reasoning: this is a human electing to ship AS IS, specifically DESPITE
// an already-computed cross-PR risk signal, an override in the same
// admin-gated class as every other row-6 action.
//
// Unblock (confirmed-major fix: Block used to be terminal, permanently
// voiding Acknowledge & ship once a maintainer had blocked) gates on the
// NEW authz.ActionUnblockReleaseComposition, the SAME admin-only row as
// Acknowledge -- see that action's own doc comment for the full "why
// admin-only" reasoning: only the tier that may ship despite a
// composition finding may also remove the safety block a maintainer
// placed on one.

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

// releaseCompositionDecisionRouteContext resolves {sessionID} into the
// values every decision action (Block/Acknowledge/Unblock) needs: the
// session must exist (404), the caller must pass action (403), this
// session must have a release manifest check on record at all (400), and
// that check's own composition review must have actually completed (400
// -- §15.3's own "not yet available" sentinel: composition_reviewed_at
// IS NULL means there is nothing yet to decide on, distinct from a real,
// empty compositionFindings array). Mirrors findingRouteContext's own
// "404 before 403" / "nothing to act on is a 400" sequencing exactly.
func releaseCompositionDecisionRouteContext(w http.ResponseWriter, r *http.Request, sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, action authz.Action) (sessionID pgtype.UUID, check sqlcgen.ReleaseManifestCheck, actorUserID pgtype.UUID, ok bool) {
	sessionID, ok = parseSessionID(w, r)
	if !ok {
		return
	}
	ctx := platform.WithSessionID(r.Context(), sessionID.String())
	r2 := r.WithContext(ctx)
	logger := platform.Logger(ctx)

	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session not found")
			ok = false
			return
		}
		logger.Error("httpapi: get session for release composition decision failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		ok = false
		return
	}

	if !authorize(w, r2, action, authz.Resource{}) {
		ok = false
		return
	}

	actorUserID, ok = authenticatedUserID(w, r2)
	if !ok {
		return
	}

	check, err := releaseManifestChecks.GetBySessionID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "this session has no release manifest check on record")
			ok = false
			return
		}
		logger.Error("httpapi: get release manifest check for composition decision failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		ok = false
		return
	}
	if !check.CompositionReviewedAt.Valid {
		writeError(w, http.StatusBadRequest, "this release's composition review has not completed yet -- nothing to decide on")
		ok = false
		return
	}

	return sessionID, check, actorUserID, true
}

// decideReleaseComposition is the shared core BlockReleaseComposition/
// AcknowledgeReleaseComposition/UnblockReleaseComposition all call:
// validate the transition, persist it via a guarded compare-and-swap and
// audit-log it IN ONE TRANSACTION, and respond.
//
// Confirmed-major fix (transactional audit): the guarded UPDATE and its
// own audit_log row now run on one pool.Begin transaction, committed only
// once BOTH have succeeded -- mirrors decideplan.go's own DecidePlan/
// DecidePlanOnTx split and audit.go's own top doc comment ("written in
// the same transaction as the change"). BEFORE this fix, these were two
// independent pool-scoped calls: a failure recording the audit row (a
// transient DB error, a connection drop) left the decision UPDATE already
// durably committed on its own -- an irreversible override (Block/
// Acknowledge/Unblock all change what a human can act on next) with NO
// audit trail, exactly the kind of unattributed state change §13.3's own
// audit requirement exists to make impossible.
func decideReleaseComposition(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore, action authz.Action, decisionAction review.CompositionDecisionAction, auditAction string) {
	sessionID, check, actorUserID, ok := releaseCompositionDecisionRouteContext(w, r, sessions, releaseManifestChecks, action)
	if !ok {
		return
	}
	ctx := r.Context()
	logger := platform.Logger(ctx)

	current := review.CompositionDecision(check.CompositionDecision)
	next, err := review.TransitionCompositionDecision(current, decisionAction)
	if err != nil {
		writeError(w, http.StatusConflict, "this release's composition decision is already "+string(current))
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		logger.Error("httpapi: begin release composition decision tx failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	updated, err := decideReleaseCompositionOnTx(ctx, tx, releaseManifestChecks, auditLog, sessionID, check.ID, actorUserID, current, next, auditAction)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "this release's composition decision was changed concurrently")
			return
		}
		logger.Error("httpapi: decide release composition failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("httpapi: commit release composition decision tx failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, restdtos.PostReleaseCompositionDecisionResponse{
		SessionId:             sessionID.String(),
		CompositionDecision:   restdtos.PostReleaseCompositionDecisionResponseCompositionDecision(updated.CompositionDecision),
		CompositionDecisionBy: updated.CompositionDecisionBy.String(),
		CompositionDecisionAt: updated.CompositionDecisionAt.Time,
	})
}

// decideReleaseCompositionOnTx performs the guarded compare-and-swap
// UPDATE and its own audit_log row on tx, an already-open transaction --
// see decideReleaseComposition's own doc comment for why these two writes
// must never commit independently of each other. pgx.ErrNoRows
// (unwrapped) means the guarded UPDATE's own CAS predicate lost a
// concurrent race -- see UpdateReleaseManifestCompositionDecision's own
// doc comment (queries/releasemanifestchecks.sql).
func decideReleaseCompositionOnTx(ctx context.Context, tx pgx.Tx, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore, sessionID, checkID, actorUserID pgtype.UUID, current, next review.CompositionDecision, auditAction string) (sqlcgen.ReleaseManifestCheck, error) {
	updated, err := releaseManifestChecks.WithTx(tx).UpdateCompositionDecision(ctx, checkID, string(current), string(next), actorUserID)
	if err != nil {
		return sqlcgen.ReleaseManifestCheck{}, err
	}

	if err := recordAuditLog(ctx, auditLog.WithTx(tx), actorUserID, auditAction, "release_manifest_check", updated.ID.String(), map[string]any{
		"session_id":     sessionID.String(),
		"repo_full_name": updated.RepoFullName,
		"pr_number":      updated.PrNumber,
		"prior_decision": string(current),
		"new_decision":   string(next),
	}); err != nil {
		return sqlcgen.ReleaseManifestCheck{}, fmt.Errorf("httpapi: record %s audit log: %w", auditAction, err)
	}

	return updated, nil
}

// BlockReleaseComposition backs POST /api/sessions/{sessionID}/
// release-manifest/block -- admin/maintainer (authz.ActionEditReviewVerdict,
// see this file's own top doc comment for why this row).
func BlockReleaseComposition(pool *pgxpool.Pool, sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decideReleaseComposition(w, r, pool, sessions, releaseManifestChecks, auditLog, authz.ActionEditReviewVerdict, review.CompositionDecisionActionBlock, "release_manifest_check.block")
	}
}

// AcknowledgeReleaseComposition backs POST /api/sessions/{sessionID}/
// release-manifest/acknowledge -- admin only
// (authz.ActionAcknowledgeReleaseComposition, see this file's own top doc
// comment for why this stricter row).
func AcknowledgeReleaseComposition(pool *pgxpool.Pool, sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decideReleaseComposition(w, r, pool, sessions, releaseManifestChecks, auditLog, authz.ActionAcknowledgeReleaseComposition, review.CompositionDecisionActionAcknowledge, "release_manifest_check.acknowledge")
	}
}

// UnblockReleaseComposition backs POST /api/sessions/{sessionID}/
// release-manifest/unblock -- admin only
// (authz.ActionUnblockReleaseComposition, see this file's own top doc
// comment for why this stricter row). Confirmed-major fix: closes the
// "Block is terminal, permanently voiding the admin-only Acknowledge &
// ship" gap -- see internal/domain/review.CompositionDecisionActionUnblock's
// own doc comment for why this reopens back to pending rather than
// straight to acknowledged.
func UnblockReleaseComposition(pool *pgxpool.Pool, sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decideReleaseComposition(w, r, pool, sessions, releaseManifestChecks, auditLog, authz.ActionUnblockReleaseComposition, review.CompositionDecisionActionUnblock, "release_manifest_check.unblock")
	}
}
