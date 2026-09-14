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
//
// Minor fix (the same defect class the composition-findings blocking fix
// closed, one level over): this function's own doc comment already
// promised the "nil degrades to []" guarantee above, but the
// implementation never actually delivered it -- json.Marshal(nil
// []string) legitimately SUCCEEDS (no error at all) and produces the
// literal 4 bytes `null`, so the previous `if err != nil` guard alone
// never caught it. findingsWire/mergedWire (this file's own two other
// callers) happened to never trigger this in practice, because both are
// always built via `make([]T, len(x))`, which is never nil even for a
// zero-length x -- but triggerReasons (review.AggregateReviewTriggerReasons,
// aggregatereview.go) starts as a bare `var reasons []string` and returns
// it un-touched whenever none of §15.3's three criteria fired, which is
// nil. The result: every NON-triggering manifest check (the common case)
// persisted aggregate_review_trigger_reasons as a literal SQL/JSON null,
// which releasemanifestreadout.go's own json.Unmarshal into `var reasons
// []string` decodes with NO error (unmarshaling JSON null into a slice
// pointer succeeds, leaving it nil) -- so that handler's own initial,
// correct `[]string{}` default got silently overwritten right back to
// nil, and the wire response violated aggregateReviewTriggerReasons' own
// contracts/rest/v1/dtos.schema.json required-array shape on every
// non-triggering release. Checking the marshaled BYTES themselves (never
// trust the absence of an error alone) closes this for every current and
// future caller of this shared helper, not just the one that happened to
// surface it.
func marshalJSONArray(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		// Every element type here is a plain struct/string/bool/int --
		// json.Marshal cannot fail on these, mirroring marshalTags' own
		// "cannot happen for this package's own types" precedent. Fail
		// conservative anyway: an empty array persists rather than a
		// panic taking down persistReleaseManifestCheck's own best-effort
		// caller. The string(b) == "null" arm is the ACTUAL common path
		// this function exists to cover -- see this function's own doc
		// comment above for the real, previously-unfixed bug it closes.
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
