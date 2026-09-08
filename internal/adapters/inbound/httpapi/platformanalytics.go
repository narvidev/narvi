// This file (platformanalytics.go) implements §12.2 item 6's own (and
// §34.6.1's own boundary) read-only analytics surface: GET /api/analytics, the
// platform-wide sibling of reviewanalytics.go's own repo-scoped GET
// /api/repos/{owner}/{repo}/review-analytics -- five KPI tiles (sessions,
// success rate, false failures, cost, boot p95) and three charts
// (sessions per day by outcome, cost by model, top failure reasons), each
// bounded to platform.Timeouts.PlatformAnalyticsWindow and, where a real
// sample-size gate applies, carrying its own independent "not yet
// computed"/"too few samples" sentinel -- see internal/app/
// platformanalytics and internal/domain/platformanalytics for the full
// read model and why sessionsTotal/falseFailureCount carry none.
//
// Gated by the SAME authz.ActionViewAnalytics (§13.3 row 1: admin,
// maintainer, member, viewer -- every role, read-only) GetReviewAnalytics
// already uses -- no new Action needed.

package httpapi

import (
	"net/http"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	apppa "github.com/narvidev/narvi/internal/app/platformanalytics"
	"github.com/narvidev/narvi/internal/domain/authz"
	domainpa "github.com/narvidev/narvi/internal/domain/platformanalytics"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/platform"
)

// GetPlatformAnalytics backs GET /api/analytics: 403 if the caller fails
// authz.ActionViewAnalytics; 200 with restdtos.PlatformAnalytics
// otherwise. A per-rollup fetch error degrades that rollup (or, for the
// single shared session-outcome-counts read, the four fields it feeds)
// to its own "not yet computed" sentinel rather than failing the whole
// request -- mirrors GetReviewAnalytics' own identical posture.
func GetPlatformAnalytics(deps apppa.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionViewAnalytics, authz.Resource{}) {
			return
		}

		now := time.Now()
		resp := restdtos.PlatformAnalytics{
			// Hours()/24 rather than a time.Hour-literal division -- §5.4/§11's
			// notimeliteral lint forbids a time.Duration unit literal outside
			// internal/platform, mirroring httpapi/digestscope.go's own
			// identical LookbackDays conversion.
			WindowDays: int(deps.Timeouts.PlatformAnalyticsWindow.Hours() / 24),
		}

		// sessionsTotal, sessionsPerDay, successRate, topFailureReasons
		// are ALL reductions over the SAME ONE Postgres read (see
		// internal/domain/platformanalytics.OutcomeCount's own doc
		// comment) -- a fetch failure here degrades all four together,
		// honestly, rather than partially rendering from stale/zero data.
		if counts, err := apppa.SessionOutcomeCountsInWindow(ctx, deps, now); err != nil {
			logger.Error("httpapi: fetch session outcome counts failed", "error", err)
		} else {
			resp.SessionsTotal = domainpa.TotalSessions(counts)

			if buckets, computed := domainpa.SessionsPerDay(counts); computed {
				resp.SessionsPerDayComputed = true
				days := make(restdtos.PlatformAnalyticsSessionsPerDay, len(buckets))
				for i, b := range buckets {
					days[i] = restdtos.PlatformAnalyticsDayOutcomeBucket{
						Day:            b.Day,
						CreatedCount:   b.Counts[session.StatusCreated],
						ActiveCount:    b.Counts[session.StatusActive],
						CompletedCount: b.Counts[session.StatusCompleted],
						FailedCount:    b.Counts[session.StatusFailed],
						CancelledCount: b.Counts[session.StatusCancelled],
					}
				}
				resp.SessionsPerDay = &days
			}

			if rate, sampleSize, computed := domainpa.SuccessRate(counts); computed {
				resp.SuccessRateComputed = true
				resp.SuccessRatePercent = &rate
				resp.SuccessRateSampleSize = sampleSize
			}

			if reasons, computed := domainpa.TopFailureReasons(counts); computed {
				resp.TopFailureReasonsComputed = true
				items := make(restdtos.PlatformAnalyticsTopFailureReasons, len(reasons))
				for i, fr := range reasons {
					items[i] = restdtos.PlatformAnalyticsFailureReasonCount{
						Reason: restdtos.PlatformAnalyticsFailureReasonCountReason(fr.Reason),
						Count:  fr.Count,
					}
				}
				resp.TopFailureReasons = &items
			}
		}

		if count, err := apppa.FalseFailureCountInWindow(ctx, deps, now); err != nil {
			logger.Error("httpapi: count false failures failed", "error", err)
		} else {
			resp.FalseFailureCount = int(count)
		}

		if cost, err := apppa.CostSummaryInWindow(ctx, deps, now); err != nil {
			logger.Error("httpapi: fetch platform cost summary failed", "error", err)
		} else if cost.Computed {
			resp.CostComputed = true
			total := cost.TotalUSD
			median := cost.MedianPerSessionUSD
			resp.CostTotalUsd = &total
			resp.CostMedianPerSessionUsd = &median
			resp.CostSampleSize = int(cost.CostedSessionCount)
		}

		if costs, computed, err := apppa.CostByModelInWindow(ctx, deps, now); err != nil {
			logger.Error("httpapi: fetch cost by model failed", "error", err)
		} else if computed {
			resp.CostByModelComputed = true
			items := make(restdtos.PlatformAnalyticsCostByModel, len(costs))
			for i, c := range costs {
				items[i] = restdtos.PlatformAnalyticsModelCost{ModelId: c.ModelID, TotalUsd: c.TotalUSD}
			}
			resp.CostByModel = &items
		}

		if boot, err := apppa.BootP95InWindow(ctx, deps, now); err != nil {
			logger.Error("httpapi: fetch boot p95 failed", "error", err)
		} else {
			resp.BootP95SampleSize = int(boot.SampleSize)
			if boot.Computed {
				resp.BootP95Computed = true
				seconds := boot.Seconds
				resp.BootP95Seconds = &seconds
			}
		}

		writeJSON(w, http.StatusOK, resp)
	}
}
