package platformanalytics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/platformanalytics"
	"github.com/narvidev/narvi/internal/domain/session"
)

// windowStart resolves deps.Timeouts.PlatformAnalyticsWindow against now
// -- the one shared boundary every function below queries from, mirroring
// internal/app/reviewverdict.ListRecordsSince's own identical role.
func windowStart(deps Deps, now time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: now.Add(-deps.Timeouts.PlatformAnalyticsWindow), Valid: true}
}

// SessionOutcomeCountsInWindow fetches and converts the ONE Postgres read
// behind four distinct §12.2 item 6 rollups (see internal/domain/
// platformanalytics.OutcomeCount's own doc comment): the caller applies
// TotalSessions/SessionsPerDay/SuccessRate/TopFailureReasons to the
// returned slice directly, never re-fetching.
func SessionOutcomeCountsInWindow(ctx context.Context, deps Deps, now time.Time) ([]platformanalytics.OutcomeCount, error) {
	rows, err := deps.Sessions.ListOutcomeCountsInWindow(ctx, windowStart(deps, now))
	if err != nil {
		return nil, err
	}
	counts := make([]platformanalytics.OutcomeCount, len(rows))
	for i, row := range rows {
		counts[i] = outcomeCountFromRow(row)
	}
	return counts, nil
}

// outcomeCountFromRow converts one ListSessionOutcomeCountsInWindowRow to
// the pure domain shape. row.Day is re-truncated to a canonical UTC
// midnight time.Time here (rather than trusted verbatim from pgx's own
// decode) so map-keyed grouping in internal/domain/platformanalytics
// compares by VALUE, never by two time.Time values that represent the
// same instant but carry different internal Location representations --
// mirrors internal/domain/reviewverdict's own truncateToUTCDay output
// contract at the one seam that actually needs it.
func outcomeCountFromRow(row sqlcgen.ListSessionOutcomeCountsInWindowRow) platformanalytics.OutcomeCount {
	day := row.Day.Time.UTC()
	reason := session.FailureReason("")
	if row.FailureReason != nil {
		reason = session.FailureReason(*row.FailureReason)
	}
	return platformanalytics.OutcomeCount{
		Day:           time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
		Status:        session.Status(row.Status),
		FailureReason: reason,
		Count:         int(row.SessionCount),
	}
}

// CostSummary is §12.2 item 6's own "Cost" KPI tile's already-reduced result
// (GetPlatformCostSummaryInWindow does the SUM/percentile_cont reduction
// in SQL -- see that query's own generated doc comment for the full
// definition). Computed reports whether CostedSessionCount is nonzero
// (platformanalytics.CostComputed); TotalUSD/MedianPerSessionUSD are only
// meaningful when it is true.
type CostSummary struct {
	TotalUSD            float64
	MedianPerSessionUSD float64
	CostedSessionCount  int64
	Computed            bool
}

// CostSummaryInWindow fetches and converts §12.2 item 6's own "Cost" KPI
// tile.
func CostSummaryInWindow(ctx context.Context, deps Deps, now time.Time) (CostSummary, error) {
	row, err := deps.Turns.GetPlatformCostSummaryInWindow(ctx, windowStart(deps, now))
	if err != nil {
		return CostSummary{}, err
	}
	totalUSD, _ := reviewtriage.NumericToFloat64(row.TotalCostUsd)
	return CostSummary{
		TotalUSD:            totalUSD,
		MedianPerSessionUSD: row.MedianCostUsdPerSession,
		CostedSessionCount:  row.CostedSessionCount,
		Computed:            platformanalytics.CostComputed(row.CostedSessionCount),
	}, nil
}

// ModelCost is one model's own spend across the window -- CostByModelInWindow's
// own per-model output row.
type ModelCost struct {
	ModelID  string
	TotalUSD float64
}

// CostByModelInWindow fetches and converts §12.2 item 6's own "cost by
// model" chart, spend descending (ListCostByModelInWindow's own SQL-side
// ORDER BY). ok=false when no turn anywhere in the window ever recorded a
// cost figure -- mirrors CostSummaryInWindow's own identical sentinel,
// since both read the same underlying "has any cost data arrived" fact.
func CostByModelInWindow(ctx context.Context, deps Deps, now time.Time) (costs []ModelCost, ok bool, err error) {
	rows, err := deps.Turns.ListCostByModelInWindow(ctx, windowStart(deps, now))
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	costs = make([]ModelCost, len(rows))
	for i, row := range rows {
		usd, _ := reviewtriage.NumericToFloat64(row.TotalCostUsd)
		costs[i] = ModelCost{ModelID: row.ModelID, TotalUSD: usd}
	}
	return costs, true, nil
}

// BootP95 is §12.2 item 6's own "Boot p95" KPI tile's already-reduced result.
// Computed reports whether SampleSize meets platformanalytics.
// BootP95MinSamples; Seconds is only meaningful when it is true.
// SampleSize is always populated regardless, so a caller can distinguish
// "no data yet" (SampleSize == 0) from "too few samples so far"
// (0 < SampleSize < BootP95MinSamples) even though both render as
// !Computed.
type BootP95 struct {
	Seconds    float64
	SampleSize int64
	Computed   bool
}

// BootP95InWindow fetches and converts §12.2 item 6's own "Boot p95" KPI
// tile.
func BootP95InWindow(ctx context.Context, deps Deps, now time.Time) (BootP95, error) {
	row, err := deps.Events.GetBootP95InWindow(ctx, windowStart(deps, now))
	if err != nil {
		return BootP95{}, err
	}
	return BootP95{
		Seconds:    row.P95Seconds,
		SampleSize: row.SampleSize,
		Computed:   platformanalytics.BootP95Computed(row.SampleSize),
	}, nil
}

// FalseFailureCountInWindow fetches §12.2 item 6's own "False failures"
// KPI tile (target 0) -- always a real, meaningful count (see
// internal/domain/platformanalytics's own doc comment for why this tile
// carries no not-yet-computed sentinel).
func FalseFailureCountInWindow(ctx context.Context, deps Deps, now time.Time) (int64, error) {
	return deps.FalseFailures.CountInWindow(ctx, windowStart(deps, now))
}
