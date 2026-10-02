package reviewverdict

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// maxAnalyticsRows bounds every analytics rollup's own Postgres read --
// §21.1's own "bounded from day one" discipline, applied here the same
// way maxRecentlyDecidedPlans already bounds internal/app/decisioninbox.
// Metrics' own comparable window scan. Generous for any single repo's
// real verdict/finding volume within platform.Timeouts.
// ReviewVerdictAnalyticsWindow.
const maxAnalyticsRows = 5000

// ListRecordsSince fetches repoFullName's own review_verdicts history
// posted after sinceTime, converted to the pure domain shape -- the ONE
// Postgres read every rollup function below shares, mirroring
// internal/domain/reviewverdict's own doc comment ("caller fetches, pure
// package reduces"). Exported (unlike an unexported windowRecords helper)
// so a DIFFERENT caller with its own window -- internal/app/digest's own
// much narrower one-day rollup, §21.3 -- can reuse the exact same
// fetch-and-convert logic rather than re-deriving it.
func ListRecordsSince(ctx context.Context, deps Deps, repoFullName string, sinceTime time.Time) ([]reviewverdict.Record, error) {
	rows, err := deps.ReviewVerdicts.ListInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: sinceTime, Valid: true}, maxAnalyticsRows)
	if err != nil {
		return nil, err
	}
	records := make([]reviewverdict.Record, len(rows))
	for i, row := range rows {
		records[i] = recordFromRow(row)
	}
	return records, nil
}

// ListNonShadowRecordsSince is ListRecordsSince's own §30.8 customer-
// consequential sibling -- excludes any verdict whose own
// suppressed_in_shadow stamp is true, or that predates repoFullName's
// own live_egress_promoted_at fence (ListNonShadowReviewVerdictsInWindow's
// own generated doc comment). internal/app/digest's own daily rollup
// (§21.3) is this function's one caller: a shadow-era verdict must never
// surface as a phantom review in a customer-facing digest, even though
// Timeseries/TopRiskDrivers below deliberately stay on ListRecordsSince's
// own unfiltered read (§30.6: those feed Narvi's own internal analytics
// surface, never a customer's own channel).
func ListNonShadowRecordsSince(ctx context.Context, deps Deps, repoFullName string, sinceTime time.Time) ([]reviewverdict.Record, error) {
	rows, err := deps.ReviewVerdicts.ListNonShadowInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: sinceTime, Valid: true}, maxAnalyticsRows)
	if err != nil {
		return nil, err
	}
	records := make([]reviewverdict.Record, len(rows))
	for i, row := range rows {
		records[i] = recordFromRow(row)
	}
	return records, nil
}

// maxHistoryRows bounds the merge readout's own PR-scoped "History" rail
// (§26.1 item 5, §12.2 item 2) -- generous for any single PR's real
// verdict volume, mirroring maxAnalyticsRows' own identical "bounded from
// day one" discipline (§21.1) at a per-PR rather than per-repo scale.
const maxHistoryRows = 200

// GetLatestRecord fetches (repoFullName, prNumber)'s own LATEST verdict
// (round-11 finding E: never "most-recently-posted" -- see GetLatest's
// own doc comment, latest.go, for the actual ordering), converted to the
// pure domain shape -- ok=false (never an error) when no verdict has ever
// been posted for this PR, mirroring ReviewVerdictStore.GetLatest's own
// "pgx.ErrNoRows means no verdict yet, never an error condition"
// contract.
func GetLatestRecord(ctx context.Context, deps Deps, repoFullName string, prNumber int32) (reviewverdict.Record, bool, error) {
	row, err := deps.ReviewVerdicts.GetLatest(ctx, repoFullName, prNumber)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reviewverdict.Record{}, false, nil
		}
		return reviewverdict.Record{}, false, err
	}
	return recordFromRow(row), true, nil
}

// ListRecordsForPR fetches every verdict ever posted for ONE
// (repoFullName, prNumber) pair, newest first, bounded by maxHistoryRows
// -- the merge readout's own "History" rail (§26.1 item 5), converted to
// the pure domain shape exactly like ListRecordsSince above.
func ListRecordsForPR(ctx context.Context, deps Deps, repoFullName string, prNumber int32) ([]reviewverdict.Record, error) {
	rows, err := deps.ReviewVerdicts.ListForPR(ctx, repoFullName, prNumber, maxHistoryRows)
	if err != nil {
		return nil, err
	}
	records := make([]reviewverdict.Record, len(rows))
	for i, row := range rows {
		records[i] = recordFromRow(row)
	}
	return records, nil
}

// Timeseries computes repoFullName's own §21.1 timeseries rollup, bounded
// to deps.Timeouts.ReviewVerdictAnalyticsWindow -- see
// internal/domain/reviewverdict.Timeseries' own doc comment for the
// "not yet computed" sentinel this forwards verbatim (ok=false).
func Timeseries(ctx context.Context, deps Deps, repoFullName string, now time.Time) ([]reviewverdict.DayBucket, bool, error) {
	records, err := ListRecordsSince(ctx, deps, repoFullName, now.Add(-deps.Timeouts.ReviewVerdictAnalyticsWindow))
	if err != nil {
		return nil, false, err
	}
	buckets, ok := reviewverdict.Timeseries(records)
	return buckets, ok, nil
}

// TopRiskDrivers computes repoFullName's own §21.1 top-risk-driver
// breakdown, bounded the same way Timeseries above is.
func TopRiskDrivers(ctx context.Context, deps Deps, repoFullName string, now time.Time) ([]reviewverdict.TagCount, bool, error) {
	records, err := ListRecordsSince(ctx, deps, repoFullName, now.Add(-deps.Timeouts.ReviewVerdictAnalyticsWindow))
	if err != nil {
		return nil, false, err
	}
	drivers, ok := reviewverdict.TopRiskDrivers(records)
	return drivers, ok, nil
}

// FindingOutcomeReadout is the "Review finding outcomes" KPI as
// FindingOutcomes computes it (§21.1, §26.5/§26.6's amendment).
type FindingOutcomeReadout struct {
	// Outcomes is the status distribution of every finding in the window
	// except the counter-review additions the server could not count as
	// checked (reviewverdict.VerifiedStatuses) -- empty, never nil, when
	// every finding in the window is one.
	Outcomes []reviewverdict.FindingStatusCount
	// BySource counts every finding in the window per source and status,
	// unverified additions in a bucket of their own -- the breakdown
	// precision per source is read from (§26.5).
	BySource []reviewverdict.FindingSourceStatusCount
}

// FindingOutcomes computes repoFullName's own §21.1/§12.2 item 6
// "Review finding outcomes" KPI, read from review_findings (§8.2) --
// see internal/domain/reviewverdict.FindingOutcomes' own doc comment for
// why this reads a DIFFERENT table than Timeseries/TopRiskDrivers above.
// ok is false when no finding was first seen in the window, the KPI's own
// "not yet computed" state; an unverified addition is a finding, so a
// window holding only those is computed, with an empty Outcomes and the
// additions counted apart in BySource.
func FindingOutcomes(ctx context.Context, deps Deps, repoFullName string, now time.Time) (FindingOutcomeReadout, bool, error) {
	since := now.Add(-deps.Timeouts.ReviewVerdictAnalyticsWindow)
	raw, err := deps.ReviewFindings.ListStatusesInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: since, Valid: true}, maxAnalyticsRows)
	if err != nil {
		return FindingOutcomeReadout{}, false, err
	}
	if len(raw) == 0 {
		return FindingOutcomeReadout{}, false, nil
	}
	rows := make([]reviewverdict.FindingOutcomeRow, len(raw))
	for i, r := range raw {
		rows[i] = reviewverdict.FindingOutcomeRow{Status: reviewpost.FindingStatus(r.Status)}
		if r.ReportedSource != nil {
			rows[i].Source = reviewpost.FindingSource(*r.ReportedSource)
		}
		if r.AdditionCheck != nil {
			rows[i].AdditionCheck = reviewpost.AdditionCheck(*r.AdditionCheck)
		}
	}
	outcomes, _ := reviewverdict.FindingOutcomes(reviewverdict.VerifiedStatuses(rows))
	if outcomes == nil {
		outcomes = []reviewverdict.FindingStatusCount{}
	}
	return FindingOutcomeReadout{Outcomes: outcomes, BySource: reviewverdict.FindingOutcomesBySource(rows)}, true, nil
}
