package reviewverdict

import (
	"encoding/json"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// marshalTags converts tags into JSONB bytes -- review_verdicts.
// blast_radius/repo_settings.sensitive_blast_radius_tags' own shared
// wire shape (migrations/000067's own doc comment: "a plain JSON array
// of tag strings", mirroring sessions.repos' own JSON-array-column
// precedent over a native Postgres array type). A nil/empty tags always
// marshals to "[]", never a JSON null -- both review_verdicts.blast_radius
// (NOT NULL DEFAULT '[]'::jsonb) and this package's own readers expect a
// present, empty array to mean "no tags", not an absent column. make's
// own "always non-nil, even at length zero" guarantee is what makes this
// true with no further nil-check needed: json.Marshal renders a non-nil,
// zero-length []string as "[]", never "null".
func marshalTags(tags []review.Tag) ([]byte, error) {
	strs := make([]string, len(tags))
	for i, t := range tags {
		strs[i] = string(t)
	}
	return json.Marshal(strs)
}

// unmarshalTags is marshalTags' own inverse -- a malformed/empty input
// (a NULL column read as a nil []byte, or genuinely invalid JSON, which
// should never happen for a column ONLY ever written by marshalTags
// above, but defended against anyway) degrades to an empty tag list
// rather than propagating a decode error up through every caller of
// recordFromRow -- an empty BlastRadius is always a SAFE, conservative
// reading (autoapproval.ComputeEligible's own sensitive-path check finds
// no overlap against an empty list, which can only ever make a PR LESS
// likely to be flagged sensitive, never more -- so this is the one place
// in this package's own read path where the fail-conservative direction
// legitimately favors "assume nothing tagged" over "fail closed", since
// the alternative -- treating a decode failure as though EVERY tag were
// present -- has no principled meaning at all, unlike domain/review's
// own enum-level fail-conservative convention, which always has a real
// worst-known-value to fall back to).
func unmarshalTags(raw []byte) []review.Tag {
	if len(raw) == 0 {
		return nil
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err != nil {
		return nil
	}
	tags := make([]review.Tag, len(strs))
	for i, s := range strs {
		tags[i] = review.Tag(s)
	}
	return tags
}

// marshalStrings converts ss into JSONB bytes -- review_verdicts.
// arch_decision_tags/arch_decision_roots's own shared wire shape
// (migrations/000113: "JSONB, mirroring blast_radius's own... precedent"),
// mirroring marshalTags' own identical "a nil/empty input always
// marshals to '[]', never a JSON null" guarantee one function above --
// both columns are NOT NULL DEFAULT '[]'::jsonb, and a bare
// json.Marshal(ss) on a genuinely nil ss would encode the JSON scalar
// null (a valid, non-NULL jsonb VALUE, but the wrong one: it would make
// this row's own tags/roots unmatchable by the gate's own ?| operator in
// a way indistinguishable from a real decode failure, rather than the
// intended, honestly-empty "[]"). make's own "always non-nil, even at
// length zero" property is what closes that gap with no extra branch.
func marshalStrings(ss []string) ([]byte, error) {
	out := make([]string, len(ss))
	copy(out, ss)
	return json.Marshal(out)
}

// recordFromRow converts a freshly-inserted-or-read sqlcgen.ReviewVerdict
// into the pure reviewverdict.Record shape -- the one seam every domain-
// layer analytics/eligibility caller in this package goes through, never
// hand-built ad hoc at each call site.
func recordFromRow(row sqlcgen.ReviewVerdict) reviewverdict.Record {
	return reviewverdict.Record{
		RepoFullName: row.RepoFullName,
		PRNumber:     row.PrNumber,
		HeadSHA:      row.HeadSha,
		CreatedAt:    row.CreatedAt.Time,
		Verdict: review.Verdict{
			RiskLevel:         review.RiskLevel(row.RiskLevel),
			Premise:           review.PremiseState(row.Premise),
			BlastRadius:       unmarshalTags(row.BlastRadius),
			FilesChanged:      int(row.FilesChanged),
			TestsCoverage:     review.TestsCoverageState(row.TestsCoverage),
			DocsDrift:         review.DocsDriftState(row.DocsDrift),
			ProposedShippable: review.ProposedShippable(row.ProposedShippable),
			Shippable:         review.Shippable(row.Shippable),
		},
		Digest:          digestFromRow(row),
		ReviewPath:      reviewPathFromRow(row),
		CounterReview:   counterReviewFromRow(row),
		FactCheck:       factCheckFromRow(row),
		FactCheckKilled: factCheckKilledFromRow(row),
	}
}

// counterReviewFromRow reads row's own counter_review column (
// §26.4) into review.CounterReviewStatus -- row.CounterReview == nil (a
// pre-existing row, or any light-path row -- §26.9) degrades to the zero
// value, mirroring reviewPathFromRow's own identical "absent column ->
// zero value" precedent immediately above.
func counterReviewFromRow(row sqlcgen.ReviewVerdict) review.CounterReviewStatus {
	if row.CounterReview == nil {
		return ""
	}
	return review.CounterReviewStatus(*row.CounterReview)
}

// factCheckFromRow reads row's own fact_check column (§26.6)
// into reviewpost.FactCheckStatus -- row.FactCheck == nil (a pre-existing
// row) degrades to the zero value, mirroring counterReviewFromRow's own
// identical precedent immediately above.
func factCheckFromRow(row sqlcgen.ReviewVerdict) reviewpost.FactCheckStatus {
	if row.FactCheck == nil {
		return ""
	}
	return reviewpost.FactCheckStatus(*row.FactCheck)
}

// factCheckKilledFromRow reads row's own fact_check_killed column
// (§26.6) -- row.FactCheckKilled == nil (a pre-existing row) degrades
// to 0, indistinguishable from a real fact-check pass that killed
// nothing (the SAME "0 either way" ambiguity §26.6's own FactCheckKilled
// doc comment already accepts for a skipped pass -- neither case is a
// safety-relevant distinction this package's readers need to make).
func factCheckKilledFromRow(row sqlcgen.ReviewVerdict) int {
	if row.FactCheckKilled == nil {
		return 0
	}
	return int(*row.FactCheckKilled)
}

// reviewPathFromRow reads row's own review_path column (§26.3)
// into reviewtriage.ReviewDepth -- row.ReviewPath == nil (a pre-existing
// row, or a verdict whose own turn never resolved a depth) degrades to
// the zero value ReviewDepth(""), mirroring digestFromRow's own identical
// "absent column -> zero value" precedent immediately above, never a
// fabricated depth.
func reviewPathFromRow(row sqlcgen.ReviewVerdict) reviewtriage.ReviewDepth {
	if row.ReviewPath == nil {
		return ""
	}
	return reviewtriage.ReviewDepth(*row.ReviewPath)
}

// digestFromRow builds reviewverdict.Record.Digest from row's own eight
// digest_* columns (migrations/000077_review_verdicts_digest.up.sql,
// 000078_review_verdicts_description_adequacy.up.sql,
// 000084_review_verdicts_counter_review.up.sql's own
// digest_contested_points) -- row.DigestSummary
// == nil means either "posted before digest tracking existed" or (in principle,
// never in practice once ValidateVerdictInput's own ErrEmptyDigestSummary
// check is live) "no digest recorded" -- either way this returns the
// zero-value reviewpost.Digest{}, exactly like unmarshalTags' own
// "malformed/absent degrades to an empty, safe value" precedent
// immediately above. row.DigestDescriptionAdequacy == nil (a pre-existing
// row) degrades to review.DescriptionAdequacy(""), the SAME zero value
// AdequacyFloor's own documented fail-conservative policy already treats
// as ranking with DescriptionAdequacyMisleading -- never silently read as
// "ok".
func digestFromRow(row sqlcgen.ReviewVerdict) reviewpost.Digest {
	var d reviewpost.Digest
	if row.DigestSummary != nil {
		d.Summary = *row.DigestSummary
	}
	if row.DigestStackRisks != nil {
		d.StackRisks = *row.DigestStackRisks
	}
	if row.DigestUnverifiedLimits != nil {
		d.UnverifiedLimits = *row.DigestUnverifiedLimits
	}
	if row.DigestDescriptionAdequacy != nil {
		d.DescriptionAdequacy = review.DescriptionAdequacy(*row.DigestDescriptionAdequacy)
	}
	if row.DigestAdequacyExplanation != nil {
		d.AdequacyExplanation = *row.DigestAdequacyExplanation
	}
	if row.DigestProposedBody != nil {
		d.ProposedBody = *row.DigestProposedBody
	}
	if row.DigestContestedPoints != nil {
		d.ContestedPoints = *row.DigestContestedPoints
	}
	d.ArchDecisions = unmarshalArchDecisions(row.DigestArchDecisions)
	return d
}

// archDecisionJSON is digest_arch_decisions' own JSON-array element shape
// -- mirrors marshalTags/unmarshalTags' own "a plain JSON array, not a
// native Postgres array or composite type" precedent (this file, above),
// applied here to a per-element OBJECT rather than a bare string.
type archDecisionJSON struct {
	Decision              string `json:"decision"`
	RejectedAlternative   string `json:"rejectedAlternative"`
	ConventionConformance string `json:"conventionConformance"`
}

// marshalArchDecisions converts decisions into digest_arch_decisions'
// own JSONB bytes -- a nil/empty decisions marshals to "[]", never a JSON
// null, mirroring marshalTags' own identical "always a present, empty
// array" guarantee immediately above (make's own "non-nil even at length
// zero" property is what makes this true with no extra nil-check).
func marshalArchDecisions(decisions []reviewpost.ArchDecision) ([]byte, error) {
	out := make([]archDecisionJSON, len(decisions))
	for i, ad := range decisions {
		out[i] = archDecisionJSON{
			Decision:              ad.Decision,
			RejectedAlternative:   ad.RejectedAlternative,
			ConventionConformance: ad.ConventionConformance,
		}
	}
	return json.Marshal(out)
}

// unmarshalArchDecisions is marshalArchDecisions' own inverse -- a NULL
// column (raw == nil) or genuinely invalid JSON (should never happen for a
// column only ever written by marshalArchDecisions above, defended against
// anyway) both degrade to nil, never a decode error propagated up through
// recordFromRow, mirroring unmarshalTags' own identical fail-conservative
// posture for the SAME reason: there is no principled "worse than nil"
// value to invent for architecture-decision prose.
func unmarshalArchDecisions(raw []byte) []reviewpost.ArchDecision {
	if len(raw) == 0 {
		return nil
	}
	var decoded []archDecisionJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	if len(decoded) == 0 {
		return nil
	}
	decisions := make([]reviewpost.ArchDecision, len(decoded))
	for i, d := range decoded {
		decisions[i] = reviewpost.ArchDecision{
			Decision:              d.Decision,
			RejectedAlternative:   d.RejectedAlternative,
			ConventionConformance: d.ConventionConformance,
		}
	}
	return decisions
}
