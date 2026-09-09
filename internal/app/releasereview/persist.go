package releasereview

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/review"
)

// This file implements the release-review screen's own durable
// persistence of the manifest check's result (§12.2 item 9, "dedicated
// release-review screen") -- Run's own pre-existing rendered-comment-only
// output (rendermanifestcomment.go) left this data structurally
// unavailable to any UI read endpoint (never re-parsing anything out of
// posted comment text, review/doc.go's own standing invariant).

// ReleaseManifestCheckInserter is the narrow slice of
// *postgres.ReleaseManifestCheckStore this package needs -- mirrors
// OutboxEnqueuer's own identical "narrow interface over one store's
// Create/Insert method" precedent, purely for this package's own unit
// tests.
type ReleaseManifestCheckInserter interface {
	Insert(ctx context.Context, arg sqlcgen.InsertReleaseManifestCheckParams) (sqlcgen.ReleaseManifestCheck, error)
}

// mergedPRJSON is release_manifest_checks.merged_prs' own per-element JSON
// shape -- mirrors internal/app/reviewverdict/convert.go's own
// archDecisionJSON precedent (a plain JSON array of objects, not a
// normalized child table, §12.2 item 9's own "read-mostly, always-read-
// as-a-whole" data).
type mergedPRJSON struct {
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

// manifestFindingJSON is release_manifest_checks.findings' own per-element
// JSON shape, mirroring review.ManifestFinding's own three fields exactly.
type manifestFindingJSON struct {
	Kind     string `json:"kind"`
	PRNumber int    `json:"prNumber"`
	PRTitle  string `json:"prTitle"`
	Detail   string `json:"detail"`
}

// marshalJSONArray marshals v (always a slice) to JSON, degrading a nil
// slice to "[]" rather than a JSON null -- mirrors marshalTags/
// marshalArchDecisions' own identical "a present, empty array, never
// null" guarantee (internal/app/reviewverdict/convert.go).
func marshalJSONArray(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every element type here is a plain struct/string/bool/int --
		// json.Marshal cannot fail on these, mirroring marshalTags' own
		// "cannot happen for this package's own types" precedent. Fail
		// conservative anyway: an empty array persists rather than a
		// panic taking down persistReleaseManifestCheck's own best-effort
		// caller.
		return []byte("[]")
	}
	return b
}

// persistReleaseManifestCheck writes ONE release_manifest_checks row from
// the SAME already-computed typed data Run's own RenderManifestComment
// call renders from -- best-effort, mirroring Run's own established
// "every internal failure is logged and this function simply continues"
// posture: a failure here must never prevent Run's own pre-existing
// outbox-delivered comment from still being enqueued.
//
// Returns the inserted row's own id and ok=true on success -- ok=false
// (a zero-value pgtype.UUID) whenever store is nil or the insert itself
// failed, so this function's own caller (Run) can pass a real id on to
// dispatchCompositionReview (compositiondispatch.go) ONLY when a row
// genuinely exists for it to anchor composition_head_sha/
// composition_diff_truncated against -- never a guessed/zero id that
// would make that later, best-effort UPDATE a silent no-op against the
// wrong (or no) row.
func persistReleaseManifestCheck(ctx context.Context, logger *slog.Logger, store ReleaseManifestCheckInserter, in Input, merged []review.MergedPR, findings []review.ManifestFinding, aggregateReviewTriggered bool, triggerReasons []string, coveragePartial bool) (checkID pgtype.UUID, ok bool) {
	if store == nil {
		return pgtype.UUID{}, false
	}

	mergedWire := make([]mergedPRJSON, len(merged))
	for i, m := range merged {
		mergedWire[i] = mergedPRJSON{
			Number:                      m.Number,
			Title:                       m.Title,
			HasApprovingReview:          m.HasApprovingReview,
			MergedViaAdminOverride:      m.MergedViaAdminOverride,
			CIConclusionAtMergeSHA:      string(m.CIConclusionAtMergeSHA),
			WasReverted:                 m.WasReverted,
			RevertReviewState:           string(m.RevertReviewState),
			RevertedAfterMergeSeconds:   m.RevertedAfterMergeSeconds,
			HadManualConflictResolution: m.HadManualConflictResolution,
			ChangedPathPrefixes:         m.ChangedPathPrefixes,
			HighRiskFlagged:             m.HighRiskFlagged,
		}
	}

	findingsWire := make([]manifestFindingJSON, len(findings))
	for i, f := range findings {
		findingsWire[i] = manifestFindingJSON{
			Kind:     string(f.Kind),
			PRNumber: f.PRNumber,
			PRTitle:  f.PRTitle,
			Detail:   f.Detail,
		}
	}

	created, err := store.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID:                     in.SessionID,
		RepoFullName:                  in.Owner + "/" + in.Repo,
		PrNumber:                      in.PRNumber,
		BaseRef:                       in.BaseRef,
		HeadRef:                       in.HeadRef,
		ConstituentPrCount:            int32(len(merged)),
		CoveragePartial:               coveragePartial,
		AggregateReviewTriggered:      aggregateReviewTriggered,
		AggregateReviewTriggerReasons: marshalJSONArray(triggerReasons),
		Findings:                      marshalJSONArray(findingsWire),
		MergedPrs:                     marshalJSONArray(mergedWire),
	})
	if err != nil {
		logger.Error("releasereview: persist release manifest check failed (the outbox-delivered comment is unaffected)",
			"error", err, "owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		return pgtype.UUID{}, false
	}
	return created.ID, true
}
