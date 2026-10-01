// This file (reviewanalytics.go) implements §21's own read-only
// analytics surface (§21.1): GET /api/repos/{owner}/{repo}/review-analytics,
// exposing the three rollups that section names explicitly -- timeseries,
// top-risk-driver breakdown, and the "Review finding outcomes" KPI
// (§12.2 item 6) -- over the append-only review_verdicts history plus
// review_findings' own mutable per-finding status. §26.5 adds a
// fourth rollup, the digest contestation rate (appreviewverdict.
// DigestContestationRate, internal/app/reviewverdict/digestcontestation.go)
// -- the "digest precision (contestation rate)" KPI that section names,
// over review_digest_section_feedback (§26.5's per-section contest/confirm
// mechanism). Every rollup is bounded to platform.Timeouts.
// ReviewVerdictAnalyticsWindow (§21.1's own "bounded from day one"
// discipline) and carries its own independent "not yet computed"
// sentinel, distinct from a real, computed zero (§21.1: "a repo with a
// real 0% dismiss rate and a repo with no data yet must never render
// identically").
//
// Gated by the EXISTING authz.ActionViewAnalytics (§13.3 row 1: admin,
// maintainer, member, viewer -- every role, read-only) -- no new Action
// needed, unlike the write-side §21.2 endpoints in reposettings.go, which
// each needed their own narrower gate.

package httpapi

import (
	"net/http"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

// GetReviewAnalytics backs GET /api/repos/{owner}/{repo}/review-analytics:
// 403 if the caller fails authz.ActionViewAnalytics; 200 with
// restdtos.ReviewAnalytics otherwise. A per-rollup fetch error degrades
// that ONE rollup to its own "not yet computed" sentinel rather than
// failing the whole request -- mirrors GetRepoSettings' own identical
// posture for the contradiction-rate read model immediately above it in
// that file ("a degraded calibration read must never block viewing the
// rest of this repo's own settings").
func GetReviewAnalytics(reviewVerdictDeps appreviewverdict.Deps, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionViewAnalytics, authz.Resource{}) {
			return
		}

		// fix/repo-scoped-authorization: role check above is necessary but
		// not sufficient -- see reposettings.go's own resolveKnownRepo doc
		// comment for why the URL's own repo must ALSO be confirmed known
		// to this deployment before any store call below ever runs.
		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		now := time.Now()
		resp := restdtos.ReviewAnalytics{RepoFullName: repoFullName}

		if buckets, computed, err := appreviewverdict.Timeseries(ctx, reviewVerdictDeps, repoFullName, now); err != nil {
			logger.Error("httpapi: compute review-verdict timeseries failed", "error", err)
			// resp.TimeseriesComputed/resp.Timeseries stay false/nil --
			// their own zero values, the SAME rendering a genuine lack of
			// data produces.
		} else if computed {
			resp.TimeseriesComputed = true
			series := make(restdtos.ReviewAnalyticsTimeseries, len(buckets))
			for i, b := range buckets {
				series[i] = restdtos.ReviewAnalyticsDayBucket{
					Day:             b.Day,
					AutoCount:       b.Counts[review.ShippableAuto],
					NeedsHumanCount: b.Counts[review.ShippableNeedsHuman],
					BlockCount:      b.Counts[review.ShippableBlock],
				}
			}
			resp.Timeseries = &series
		}

		if drivers, computed, err := appreviewverdict.TopRiskDrivers(ctx, reviewVerdictDeps, repoFullName, now); err != nil {
			logger.Error("httpapi: compute review-verdict top risk drivers failed", "error", err)
		} else if computed {
			resp.TopRiskDriversComputed = true
			tagCounts := make(restdtos.ReviewAnalyticsTopRiskDrivers, len(drivers))
			for i, d := range drivers {
				tagCounts[i] = restdtos.ReviewAnalyticsTagCount{
					Tag:   restdtos.ReviewAnalyticsTagCountTag(d.Tag),
					Count: d.Count,
				}
			}
			resp.TopRiskDrivers = &tagCounts
		}

		if readout, computed, err := appreviewverdict.FindingOutcomes(ctx, reviewVerdictDeps, repoFullName, now); err != nil {
			logger.Error("httpapi: compute review finding outcomes failed", "error", err)
		} else if computed {
			resp.FindingOutcomesComputed = true
			// Every finding but the unverified counter-review additions
			// (§26.6's amendment), which are counted apart below.
			statusCounts := make(restdtos.ReviewAnalyticsFindingOutcomes, len(readout.Outcomes))
			for i, o := range readout.Outcomes {
				statusCounts[i] = restdtos.ReviewAnalyticsFindingStatusCount{
					Status: restdtos.ReviewAnalyticsFindingStatusCountStatus(o.Status),
					Count:  o.Count,
				}
			}
			resp.FindingOutcomes = &statusCounts
			// §26.5: precision per source is read from this breakdown.
			sourceCounts := make(restdtos.ReviewAnalyticsFindingOutcomesBySource, len(readout.BySource))
			for i, o := range readout.BySource {
				sourceCounts[i] = restdtos.ReviewAnalyticsFindingSourceCount{
					Source: string(o.Source),
					Status: restdtos.ReviewAnalyticsFindingSourceCountStatus(o.Status),
					Count:  o.Count,
				}
			}
			resp.FindingOutcomesBySource = &sourceCounts
		}

		if rate, computed, err := appreviewverdict.DigestContestationRate(ctx, reviewVerdictDeps, repoFullName, now); err != nil {
			logger.Error("httpapi: compute digest contestation rate failed", "error", err)
			// resp.DigestContestationRateComputed/resp.DigestContestationRatePercent
			// stay false/nil -- their own zero values, the SAME rendering a
			// genuine lack of data produces.
		} else if computed {
			resp.DigestContestationRateComputed = true
			percent := rate * 100
			resp.DigestContestationRatePercent = &percent
		}

		writeJSON(w, http.StatusOK, resp)
	}
}
