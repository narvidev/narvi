// This file (releasemanifestreadout.go) implements §12.2 item 9's
// dedicated release-review screen's own read model: GET
// /api/sessions/{sessionID}/release-manifest. internal/app/releasereview.
// Run (§15.2/§15.3) computes this data already, on session creation, but
// before this file existed it only ever rendered that data into ONE
// outbox-delivered GitHub comment (reviewpost.RenderManifestComment) --
// never re-parsed back out of posted comment text anywhere in this
// codebase (review/doc.go's own standing invariant), which left it
// structurally unavailable to a UI read endpoint. run.go's own
// persistReleaseManifestCheck (persist.go) now ALSO writes the same
// typed data to release_manifest_checks (migrations/
// 000097_release_manifest_checks.up.sql); this handler reads it back.
//
// §15.3's own actual composition-focused aggregate-diff-review LLM pass
// is NOT dispatched anywhere in this codebase today -- only its own
// TRIGGER decision (review.ShouldRunAggregateReview) is computed and
// rendered. This handler therefore never fabricates "composition
// findings": ReleaseManifestReadout.findings carries §15.2's own
// mechanical manifest findings only (admin overrides, red-at-merge,
// unreviewed reverts), and the release-review view renders an honest
// "not yet available" state for composition findings rather than
// inventing a shape nothing on the backend produces.
//
// Gated by the EXISTING authz.ActionViewAnalytics (§13.3 row 1: every
// role including viewer, read-only), mirroring reviewreadout.go's own
// identical choice.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// mergedPRJSON/manifestFindingJSON mirror internal/app/releasereview/
// persist.go's own identical, unexported JSON shapes byte-for-byte (field
// names and JSON tags) -- this package cannot import that one directly
// (unexported), so the read side re-declares the same wire shape rather
// than exporting internal persistence-layer types across a package
// boundary they were never designed to cross.
type releaseManifestMergedPRJSON struct {
	Number                      int      `json:"number"`
	Title                       string   `json:"title"`
	HasApprovingReview          bool     `json:"hasApprovingReview"`
	MergedViaAdminOverride      bool     `json:"mergedViaAdminOverride"`
	CIConclusionAtMergeSHA      string   `json:"ciConclusionAtMergeSha"`
	WasReverted                 bool     `json:"wasReverted"`
	RevertReviewState           string   `json:"revertReviewState"`
	RevertedAfterMergeSeconds   *int64   `json:"revertedAfterMergeSeconds"`
	HadManualConflictResolution bool     `json:"hadManualConflictResolution"`
	ChangedPathPrefixes         []string `json:"changedPathPrefixes"`
	HighRiskFlagged             bool     `json:"highRiskFlagged"`
}

type releaseManifestFindingJSON struct {
	Kind     string `json:"kind"`
	PRNumber int    `json:"prNumber"`
	PRTitle  string `json:"prTitle"`
	Detail   string `json:"detail"`
}

// releaseCompositionFindingJSON mirrors restdtos.ReleaseCompositionFinding's
// own wire shape byte-for-byte -- the SAME shape
// releasecompositionfindings.go's own PostReleaseCompositionFindings
// marshals verbatim (json.Marshal(req.Findings)) into
// release_manifest_checks.composition_findings; this read side re-declares
// it rather than importing restdtos' own struct directly, mirroring
// releaseManifestFindingJSON/releaseManifestMergedPRJSON's own identical
// "re-declare the wire shape, don't reach into an internal package" choice
// immediately above.
type releaseCompositionFindingJSON struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// GetReleaseManifestReadout backs GET
// /api/sessions/{sessionID}/release-manifest. 404 if sessionID doesn't
// exist; 400 if it exists but was never created via a GitHub PR mention;
// 403 if the caller fails authz.ActionViewAnalytics; otherwise 200 with
// restdtos.ReleaseManifestReadout, computed=false when no check has been
// persisted for this PR yet (an honest "not available" state, never a
// 404 -- mirrors GetReviewReadout's own identical "renders empty, never
// missing" posture for a PR with no verdict yet).
func GetReleaseManifestReadout(
	sessions *postgres.SessionStore,
	prSessions *postgres.GitHubPRSessionStore,
	releaseManifestChecks *postgres.ReleaseManifestCheckStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		if _, err := sessions.Get(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session for release manifest readout failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if !authorize(w, r, authz.ActionViewAnalytics, authz.Resource{}) {
			return
		}

		prSession, err := prSessions.GetBySessionID(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusBadRequest, "this session has no associated GitHub pull request")
				return
			}
			logger.Error("httpapi: look up github_pr_sessions by session id failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		repoFullName := prSession.RepoFullName
		prNumber := prSession.PrNumber

		resp := restdtos.ReleaseManifestReadout{
			RepoFullName:                  repoFullName,
			PrNumber:                      int(prNumber),
			AggregateReviewTriggerReasons: []string{},
			Findings:                      []restdtos.ReleaseManifestFinding{},
			MergedPrs:                     []restdtos.ReleaseManifestPR{},
			// CompositionFindings/CompositionDecision are BOTH required,
			// closed-shape fields on the wire (contracts/rest/v1/dtos.schema.
			// json) -- never the Go zero value of either (nil, which
			// marshals to JSON null for the array; "", which is not a
			// legal enum member for the string). "pending" plus an empty
			// (never nil) array is the honest default even in the
			// computed=false branch immediately below: release_manifest_
			// checks.composition_decision/composition_findings themselves
			// default to exactly these same two values at the schema level
			// (migrations/000127_release_manifest_checks_composition.up.
			// sql), so a session with no persisted check at all is reporting
			// precisely what a freshly-inserted row would also report --
			// "nothing decided yet" -- never an out-of-enum sentinel a
			// strict client would reject.
			CompositionFindings: []restdtos.ReleaseCompositionFinding{},
			CompositionDecision: restdtos.ReleaseManifestReadoutCompositionDecisionPending,
		}

		row, err := releaseManifestChecks.GetLatest(ctx, repoFullName, prNumber)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Never computed for this PR -- computed stays false, every
				// other field its own zero value, per this handler's own
				// doc comment.
				writeJSON(w, http.StatusOK, resp)
				return
			}
			logger.Error("httpapi: get latest release manifest check failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp.Computed = true
		baseRef := row.BaseRef
		resp.BaseRef = &baseRef
		headRef := row.HeadRef
		resp.HeadRef = &headRef
		computedAt := row.CreatedAt.Time
		resp.ComputedAt = &computedAt
		resp.ConstituentPrCount = int(row.ConstituentPrCount)
		resp.CoveragePartial = row.CoveragePartial
		resp.AggregateReviewTriggered = row.AggregateReviewTriggered

		// §15.3/§12.2 item 9's own composition-review fields -- the ONE
		// omission a real adversarial review of this branch caught: this
		// handler used to stop at AggregateReviewTriggered above and never
		// read composition_reviewed_at/composition_findings/
		// composition_decision/composition_decision_by/
		// composition_decision_at back out of row at all, which meant the
		// composition pass could run, post real findings, and have a real
		// human decision recorded against it, and this endpoint would still
		// render the "pending: has been dispatched but has not completed
		// yet" sentinel forever. CompositionReviewedAt is left nil (its own
		// "not yet available" sentinel, restdtos.
		// ReleaseManifestReadoutCompositionReviewedAt's own doc comment)
		// exactly when row.CompositionReviewedAt itself is not Valid --
		// never fabricated -- mirroring ComputedAt/BaseRef/HeadRef's own
		// identical pgtype-Valid-gated pattern immediately above.
		if row.CompositionReviewedAt.Valid {
			compositionReviewedAt := row.CompositionReviewedAt.Time
			resp.CompositionReviewedAt = &compositionReviewedAt
		}
		// CompositionHeadSha/CompositionDiffTruncated (migrations/
		// 000128_release_manifest_checks_composition_anchor.up.sql,
		// confirmed-major auditability fix): row.CompositionHeadSha is
		// already nil exactly when dispatchCompositionReview never recorded
		// an anchor for this row (never dispatched, or declined to dispatch)
		// -- forwarded verbatim, no extra Valid-gate needed the way a
		// pgtype column would require.
		resp.CompositionHeadSha = row.CompositionHeadSha
		resp.CompositionDiffTruncated = row.CompositionDiffTruncated
		var compositionFindings []releaseCompositionFindingJSON
		if err := json.Unmarshal(row.CompositionFindings, &compositionFindings); err == nil {
			// A present, empty array on the wire, never JSON null -- mirrors
			// resp's own top-of-function default (this function's own doc
			// comment on that struct literal): compositionFindings is a
			// required array field, so it must never regress to nil (which
			// encoding/json marshals as `null`) merely because this release
			// has zero composition findings to report.
			resp.CompositionFindings = make([]restdtos.ReleaseCompositionFinding, 0, len(compositionFindings))
			for _, f := range compositionFindings {
				resp.CompositionFindings = append(resp.CompositionFindings, restdtos.ReleaseCompositionFinding{
					Kind:   restdtos.ReleaseCompositionFindingKind(f.Kind),
					Detail: f.Detail,
				})
			}
		} else {
			logger.Warn("httpapi: unmarshal composition findings failed, rendering as empty", "error", err)
		}
		resp.CompositionDecision = restdtos.ReleaseManifestReadoutCompositionDecision(row.CompositionDecision)
		if row.CompositionDecisionBy.Valid {
			decisionBy := row.CompositionDecisionBy.String()
			resp.CompositionDecisionBy = &decisionBy
		}
		if row.CompositionDecisionAt.Valid {
			decisionAt := row.CompositionDecisionAt.Time
			resp.CompositionDecisionAt = &decisionAt
		}

		var reasons []string
		if err := json.Unmarshal(row.AggregateReviewTriggerReasons, &reasons); err == nil {
			resp.AggregateReviewTriggerReasons = reasons
		} else {
			logger.Warn("httpapi: unmarshal aggregate_review_trigger_reasons failed, rendering as empty", "error", err)
		}

		var findings []releaseManifestFindingJSON
		if err := json.Unmarshal(row.Findings, &findings); err == nil {
			for _, f := range findings {
				resp.Findings = append(resp.Findings, restdtos.ReleaseManifestFinding{
					Kind:     restdtos.ReleaseManifestFindingKind(f.Kind),
					PrNumber: f.PRNumber,
					PrTitle:  f.PRTitle,
					Detail:   f.Detail,
				})
			}
		} else {
			logger.Warn("httpapi: unmarshal release manifest findings failed, rendering as empty", "error", err)
		}

		var mergedPRs []releaseManifestMergedPRJSON
		if err := json.Unmarshal(row.MergedPrs, &mergedPRs); err == nil {
			for _, m := range mergedPRs {
				var revertedAfter restdtos.ReleaseManifestPRRevertedAfterMergeSeconds
				if m.RevertedAfterMergeSeconds != nil {
					seconds := int(*m.RevertedAfterMergeSeconds)
					revertedAfter = &seconds
				}
				resp.MergedPrs = append(resp.MergedPrs, restdtos.ReleaseManifestPR{
					Number:                      m.Number,
					Title:                       m.Title,
					HasApprovingReview:          m.HasApprovingReview,
					MergedViaAdminOverride:      m.MergedViaAdminOverride,
					CiConclusion:                restdtos.ReleaseManifestPRCiConclusion(m.CIConclusionAtMergeSHA),
					WasReverted:                 m.WasReverted,
					RevertReviewState:           restdtos.ReleaseManifestPRRevertReviewState(m.RevertReviewState),
					RevertedAfterMergeSeconds:   revertedAfter,
					HadManualConflictResolution: m.HadManualConflictResolution,
					HighRiskFlagged:             m.HighRiskFlagged,
				})
			}
		} else {
			logger.Warn("httpapi: unmarshal release manifest merged PRs failed, rendering as empty", "error", err)
		}

		writeJSON(w, http.StatusOK, resp)
	}
}
