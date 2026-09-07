//go:build integration

// Integration tests for §21's own (§21.1) read-only analytics route
// (reviewanalytics.go's own GetReviewAnalytics), against a real Postgres
// instance -- sharing this package's own testRig (httpapi_integration_test.go).
package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// TestGetReviewAnalytics_ViewerAllowed_NothingComputedYet proves TWO
// things at once: (1) authz.ActionViewAnalytics is genuinely a §13.3 row
// 1 action -- a VIEWER, the one role every §21.2 write-side route in this
// package denies outright, can still read this endpoint; (2) a repo with
// no review_verdicts/review_findings rows at all renders every one of
// the three "not yet computed" sentinels as false/nil, never a 500 and
// never a falsely-reassuring real zero.
func TestGetReviewAnalytics_ViewerAllowed_NothingComputedYet(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleViewer)
	rig.markRepoKnown(ctx, t, "acme/analytics-empty")

	var resp restdtos.ReviewAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/repos/acme/analytics-empty/review-analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.TimeseriesComputed {
		t.Errorf("TimeseriesComputed = true, want false (no verdicts posted)")
	}
	if resp.Timeseries != nil {
		t.Errorf("Timeseries = %v, want nil", resp.Timeseries)
	}
	if resp.TopRiskDriversComputed {
		t.Errorf("TopRiskDriversComputed = true, want false (no verdicts posted)")
	}
	if resp.TopRiskDrivers != nil {
		t.Errorf("TopRiskDrivers = %v, want nil", resp.TopRiskDrivers)
	}
	if resp.FindingOutcomesComputed {
		t.Errorf("FindingOutcomesComputed = true, want false (no findings reported)")
	}
	if resp.FindingOutcomes != nil {
		t.Errorf("FindingOutcomes = %v, want nil", resp.FindingOutcomes)
	}
	if resp.DigestContestationRateComputed {
		t.Errorf("DigestContestationRateComputed = true, want false (no deep-path verdicts posted)")
	}
	if resp.DigestContestationRatePercent != nil {
		t.Errorf("DigestContestationRatePercent = %v, want nil", resp.DigestContestationRatePercent)
	}
}

// TestGetReviewAnalytics_RendersComputedRollups seeds one review_verdicts
// row (Shippable=auto, one BlastRadius tag) and one review_findings row,
// then proves all three rollups render as real, computed data -- the
// positive twin of the all-sentinels test above, and the ONE case in this
// file that exercises the actual count/grouping logic end to end through
// the real HTTP surface (internal/domain/reviewverdict's own Timeseries/
// TopRiskDrivers/FindingOutcomes are unit-tested directly; this is the
// wiring proof).
func TestGetReviewAnalytics_RendersComputedRollups(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	const repoFullName = "acme/analytics-populated"
	rig.markRepoKnown(ctx, t, repoFullName)

	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		BlastRadius:       []review.Tag{review.TagAuth},
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      2,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	seededDigest := reviewpost.Digest{
		Summary: "Test-seeded verdict.",
		ArchDecisions: []reviewpost.ArchDecision{
			{Decision: "Use a shared retry helper.", RejectedAlternative: "Inline retry logic per call site.", ConventionConformance: "Matches internal/platform's existing retry helper pattern."},
		},
	}
	insertedRecord, err := appreviewverdict.Insert(ctx, rig.reviewVerdicts, narvipg.NewRepoSettingsStore(rig.pool), false, repoFullName, 7, "deadbeef", pgtype.UUID{}, verdict, seededDigest, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil)
	if err != nil {
		t.Fatalf("seed review_verdicts row: %v", err)
	}
	// Insert's own returned Record is the digest's read-back path
	// (recordFromRow's own ArchDecisions JSON unmarshal, convert.go) --
	// every other call site in this codebase discards it (`if _, err :=
	// ...`), so this is the one place that path is actually exercised
	// end to end: it must round-trip verbatim, not just the write side
	// TestPostReviewVerdict_PersistsDigestColumns already covers.
	if insertedRecord.Digest.Summary != seededDigest.Summary {
		t.Errorf("Insert() returned Digest.Summary = %q, want %q", insertedRecord.Digest.Summary, seededDigest.Summary)
	}
	if len(insertedRecord.Digest.ArchDecisions) != 1 || insertedRecord.Digest.ArchDecisions[0] != seededDigest.ArchDecisions[0] {
		t.Errorf("Insert() returned Digest.ArchDecisions = %+v, want %+v", insertedRecord.Digest.ArchDecisions, seededDigest.ArchDecisions)
	}

	if _, err := rig.reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     7,
		IdentityHash: "finding-hash-1",
		Severity:     "medium",
		FilePath:     "internal/example.go",
		Description:  "example finding",
	}); err != nil {
		t.Fatalf("seed review_findings row: %v", err)
	}

	var resp restdtos.ReviewAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/repos/"+repoFullName+"/review-analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if !resp.TimeseriesComputed {
		t.Fatalf("TimeseriesComputed = false, want true (one verdict was posted)")
	}
	if resp.Timeseries == nil || len(*resp.Timeseries) != 1 {
		t.Fatalf("Timeseries = %v, want exactly 1 bucket", resp.Timeseries)
	}
	if got := (*resp.Timeseries)[0].AutoCount; got != 1 {
		t.Errorf("Timeseries[0].AutoCount = %d, want 1", got)
	}

	if !resp.TopRiskDriversComputed {
		t.Fatalf("TopRiskDriversComputed = false, want true")
	}
	if resp.TopRiskDrivers == nil || len(*resp.TopRiskDrivers) != 1 {
		t.Fatalf("TopRiskDrivers = %v, want exactly 1 tag", resp.TopRiskDrivers)
	}
	if got := (*resp.TopRiskDrivers)[0]; got.Tag != restdtos.ReviewAnalyticsTagCountTagAuth || got.Count != 1 {
		t.Errorf("TopRiskDrivers[0] = %+v, want {auth 1}", got)
	}

	if !resp.FindingOutcomesComputed {
		t.Fatalf("FindingOutcomesComputed = false, want true (one finding was reported)")
	}
	if resp.FindingOutcomes == nil || len(*resp.FindingOutcomes) != 1 {
		t.Fatalf("FindingOutcomes = %v, want exactly 1 status", resp.FindingOutcomes)
	}
	if got := (*resp.FindingOutcomes)[0]; got.Status != restdtos.ReviewAnalyticsFindingStatusCountStatusOpen || got.Count != 1 {
		t.Errorf("FindingOutcomes[0] = %+v, want {open 1}", got)
	}
}

// TestGetReviewAnalytics_DigestContestationRate_ComputedFromDeepPathAndContest
// proves §26.5's own "digest precision (contestation rate)" KPI (§26.4)
// is genuinely wired end to end through this HTTP surface -- not merely
// present in restdtos, the exact defect B4 named (the capture path,
// review_digest_section_feedback, and the pure DigestContestationRate
// function all pre-existed this fix; nothing called it from the analytics
// handler and no DTO field carried it). Seeds ONE deep-path verdict (the
// denominator -- only a deep-path review ever produces an arch recap at
// all, §26.4/§26.9) and ONE arch-recap contest against it (the numerator),
// so the expected rate is unambiguous: 100%.
func TestGetReviewAnalytics_DigestContestationRate_ComputedFromDeepPathAndContest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	const repoFullName = "acme/analytics-contested"
	rig.markRepoKnown(ctx, t, repoFullName)

	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      1,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	digest := reviewpost.Digest{Summary: "Deep-path test-seeded verdict."}
	if _, err := appreviewverdict.Insert(ctx, rig.reviewVerdicts, narvipg.NewRepoSettingsStore(rig.pool), false, repoFullName, 9, "deadbeef2", pgtype.UUID{}, verdict, digest, reviewtriage.DepthDeep, review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil); err != nil {
		t.Fatalf("seed deep-path review_verdicts row: %v", err)
	}

	feedback := narvipg.NewReviewDigestSectionFeedbackStore(rig.pool)
	if _, _, err := feedback.Upsert(ctx, repoFullName, 9, string(reviewpost.DigestSectionArchRecap), "recap-hash-1", "issue_comment", 555, "arch recap wrong: missed the actual migration risk", pgtype.UUID{}); err != nil {
		t.Fatalf("seed review_digest_section_feedback row: %v", err)
	}

	var resp restdtos.ReviewAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/repos/"+repoFullName+"/review-analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if !resp.DigestContestationRateComputed {
		t.Fatalf("DigestContestationRateComputed = false, want true (one deep-path verdict and one contest were seeded)")
	}
	if resp.DigestContestationRatePercent == nil {
		t.Fatalf("DigestContestationRatePercent = nil, want a computed value")
	}
	if got := *resp.DigestContestationRatePercent; got != 100 {
		t.Errorf("DigestContestationRatePercent = %v, want 100 (1 deep-path verdict, 1 contest)", got)
	}
}
