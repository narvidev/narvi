package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// This file backs internal/app/reviewcontext.ArchDecisionsFetcher (a
// following commit's exact sibling of FalsePositivePatternsFetcher/
// FindingsFetcher) directly off *ReviewVerdictStore -- the ONE store
// every other review_verdicts reader in this package already shares.
// Unlike this file's siblings (Insert/GetLatest/... above, all thin,
// zero-conversion pass-throughs to a raw sqlcgen.ReviewVerdict), the
// conversion done here is real: technical plan §31.6's own boundaries
// design (docs/design/boundaries-design.md, section 2.2) fixes
// ArchDecisionsFetcher's own return type at []knowledge.Candidate, not a
// raw row, because ONE review_verdicts row's own digest_arch_decisions
// JSONB array can hold MORE THAN ONE decision -- each becomes its own
// Candidate, sharing that row's own tags/roots/provenance -- and because
// Candidate.ID ("<verdict id>:<index>") is a fact about that array's own
// shape, not something a caller one layer up (which never sees the raw
// row) could construct.

// archDecisionJSON mirrors internal/app/reviewverdict's own identical,
// unexported archDecisionJSON (convert.go) byte for byte -- duplicated
// rather than imported: this package is an OUTBOUND ADAPTER, and
// internal/app/reviewverdict is an APP-layer package one layer up the
// hexagon from here (this package's own callers, never the reverse) --
// importing it from here would invert that direction. The shape is three
// string fields; keeping a second copy in sync costs far less than the
// layering violation avoiding it would cost.
type archDecisionJSON struct {
	Decision              string `json:"decision"`
	RejectedAlternative   string `json:"rejectedAlternative"`
	ConventionConformance string `json:"conventionConformance"`
}

// candidatesFromReviewVerdict explodes one already-gated review_verdicts
// row into zero or more knowledge.Candidate values, one per entry in its
// own digest_arch_decisions array -- every Candidate field other than
// ID/Decision/RejectedAlternative/ConventionConformance is copied
// verbatim from the SAME row (Tags/Roots/CreatedAt/KnowledgeInfluenced
// are per-VERDICT facts, not per-decision ones, so every candidate a
// single row produces shares them identically). A malformed or empty
// digest_arch_decisions/arch_decision_tags/arch_decision_roots column
// (should not happen for a row only ever written by
// internal/app/reviewverdict.Insert, defended against anyway) degrades
// to nil/no candidates from that row rather than propagating a decode
// error -- mirrors reviewverdict.unmarshalArchDecisions/unmarshalTags'
// own identical "fail-conservative, never fail the whole fetch over one
// bad row" posture one layer up.
func candidatesFromReviewVerdict(row sqlcgen.ReviewVerdict) []knowledge.Candidate {
	if len(row.DigestArchDecisions) == 0 {
		return nil
	}
	var decisions []archDecisionJSON
	if err := json.Unmarshal(row.DigestArchDecisions, &decisions); err != nil || len(decisions) == 0 {
		return nil
	}

	var tags, roots []string
	if len(row.ArchDecisionTags) > 0 {
		_ = json.Unmarshal(row.ArchDecisionTags, &tags)
	}
	if len(row.ArchDecisionRoots) > 0 {
		_ = json.Unmarshal(row.ArchDecisionRoots, &roots)
	}

	verdictID := row.ID.String()
	cands := make([]knowledge.Candidate, 0, len(decisions))
	for i, d := range decisions {
		cands = append(cands, knowledge.Candidate{
			ID:                    fmt.Sprintf("%s:%d", verdictID, i),
			VerdictID:             verdictID,
			RepoFullName:          row.RepoFullName,
			PRNumber:              row.PrNumber,
			HeadSHA:               row.HeadSha,
			Decision:              d.Decision,
			RejectedAlternative:   d.RejectedAlternative,
			ConventionConformance: d.ConventionConformance,
			Tags:                  tags,
			Roots:                 roots,
			CreatedAt:             row.CreatedAt.Time,
			KnowledgeInfluenced:   row.KnowledgeInfluenced,
		})
	}
	return cands
}

// candidatesFromReviewVerdicts flattens rows (already gated/ordered by
// the caller's own SQL) into the combined candidate list, preserving
// row order and, within a row, digest_arch_decisions' own array order.
func candidatesFromReviewVerdicts(rows []sqlcgen.ReviewVerdict) []knowledge.Candidate {
	cands := make([]knowledge.Candidate, 0, len(rows))
	for _, row := range rows {
		cands = append(cands, candidatesFromReviewVerdict(row)...)
	}
	return cands
}

// ListGatedArchDecisions implements the read half of
// internal/app/reviewcontext.ArchDecisionsFetcher: the knowledge-
// retrieval GATE itself (§31.6) -- verdicts whose own INSERT-time
// arch_decision_tags/arch_decision_roots overlap tags/roots, live-epoch
// and uncontested only, newest first, bounded by limit. See
// ListGatedArchDecisions' own generated doc comment (queries/
// reviewverdicts.sql) for the full exclusion/overlap reasoning.
func (s *ReviewVerdictStore) ListGatedArchDecisions(ctx context.Context, repoFullName string, tags, roots []string, limit int32) ([]knowledge.Candidate, error) {
	rows, err := s.q.ListGatedArchDecisions(ctx, sqlcgen.ListGatedArchDecisionsParams{
		RepoFullName: repoFullName,
		Tags:         tags,
		Roots:        roots,
		ResultLimit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return candidatesFromReviewVerdicts(rows), nil
}

// ListRecentArchDecisions implements the OTHER read half of
// ArchDecisionsFetcher: the gate's own recency fallback, fired on empty
// overlap -- the IDENTICAL two exclusions as ListGatedArchDecisions
// above, no tag/root predicate. See ListRecentArchDecisions' own
// generated doc comment (queries/reviewverdicts.sql).
func (s *ReviewVerdictStore) ListRecentArchDecisions(ctx context.Context, repoFullName string, limit int32) ([]knowledge.Candidate, error) {
	rows, err := s.q.ListRecentArchDecisions(ctx, sqlcgen.ListRecentArchDecisionsParams{
		RepoFullName: repoFullName,
		ResultLimit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return candidatesFromReviewVerdicts(rows), nil
}
