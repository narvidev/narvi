// This file (releasecompositiondecision.go) implements §12.2 item 9's own
// two composition-finding actions ("Block release / Acknowledge & ship"):
//
//   - POST /api/sessions/{sessionID}/release-manifest/block
//   - POST /api/sessions/{sessionID}/release-manifest/acknowledge
//
// Both resolve their target release_manifest_checks row from sessionID
// alone (ReleaseManifestCheckStore.GetBySessionID, the
// addition), validate the requested transition via internal/domain/review.
// TransitionCompositionDecision (an already-decided composition, or one
// whose own composition review has not completed yet, rejects both
// actions), then persist it via a guarded compare-and-swap
// (ReleaseManifestCheckStore.UpdateCompositionDecision) keyed on the SAME
// current value just validated -- a concurrent decision loses the race
// with an honest 409, never a silent overwrite (mirrors this package's
// own established guarded-write discipline, e.g. reviewfindings.go's own
// finding-status transitions).
//
// # RBAC: two different rows for two different risk shapes
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

package httpapi

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

// releaseCompositionDecisionRouteContext resolves {sessionID} into the
// values BlockReleaseComposition/AcknowledgeReleaseComposition both need:
// the session must exist (404), the caller must pass action (403), this
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
// AcknowledgeReleaseComposition both call: validate the transition,
// persist it via a guarded compare-and-swap, audit-log it, and respond.
func decideReleaseComposition(w http.ResponseWriter, r *http.Request, sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore, action authz.Action, decisionAction review.CompositionDecisionAction, auditAction string) {
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

	updated, err := releaseManifestChecks.UpdateCompositionDecision(ctx, check.ID, string(current), string(next), actorUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "this release's composition decision was changed concurrently")
			return
		}
		logger.Error("httpapi: update release composition decision failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := recordAuditLog(ctx, auditLog, actorUserID, auditAction, "release_manifest_check", updated.ID.String(), map[string]any{
		"session_id":     sessionID.String(),
		"repo_full_name": updated.RepoFullName,
		"pr_number":      updated.PrNumber,
		"prior_decision": string(current),
		"new_decision":   string(next),
	}); err != nil {
		logger.Error("httpapi: record "+auditAction+" audit log failed", "error", err)
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

// BlockReleaseComposition backs POST /api/sessions/{sessionID}/
// release-manifest/block -- admin/maintainer (authz.ActionEditReviewVerdict,
// see this file's own top doc comment for why this row).
func BlockReleaseComposition(sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decideReleaseComposition(w, r, sessions, releaseManifestChecks, auditLog, authz.ActionEditReviewVerdict, review.CompositionDecisionActionBlock, "release_manifest_check.block")
	}
}

// AcknowledgeReleaseComposition backs POST /api/sessions/{sessionID}/
// release-manifest/acknowledge -- admin only
// (authz.ActionAcknowledgeReleaseComposition, see this file's own top doc
// comment for why this stricter row).
func AcknowledgeReleaseComposition(sessions *postgres.SessionStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decideReleaseComposition(w, r, sessions, releaseManifestChecks, auditLog, authz.ActionAcknowledgeReleaseComposition, review.CompositionDecisionActionAcknowledge, "release_manifest_check.acknowledge")
	}
}
