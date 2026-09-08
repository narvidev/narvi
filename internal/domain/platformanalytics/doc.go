// Package platformanalytics holds §12.2 item 6's own pure reduction math
// for the analytics view's platform-wide rollup (its five KPI
// tiles and its "sessions per day by outcome"/"cost by model"/"top
// failure reasons" charts -- the fourth chart, "Review finding outcomes",
// is repo-scoped and already served by internal/domain/reviewverdict).
// No I/O, no time.Now(), no randomness (CLAUDE.md/§11): every function
// here is a pure transform of already-fetched/already-aggregated rows,
// mirroring internal/domain/reviewverdict's own identical split between
// "pure decision functions here" and "the actual Postgres aggregation one
// layer up, in internal/app/platformanalytics".
//
// # The boundary this package stays inside (§34.6.1)
//
// Every value this package computes is answerable from THIS deployment's
// own rows alone -- sessions, success rate, false failures, cost by
// model, boot p95, top failure reasons. None of it aggregates across
// repositories in a comparative sense (a per-model or per-day BREAKDOWN
// of this deployment's own sessions is not the same thing as comparing
// one repository against another), and none of it aggregates across
// deployments or organizations -- the cross-cutting kind §34.6.1 draws
// the line at does not exist anywhere in this codebase and this package
// builds no hook toward it.
//
// # "Computed in SQL", not "bounded rows reduced in Go"
//
// Unlike internal/domain/reviewverdict (repo-scoped, so a bounded raw-row
// fetch reduced here in Go is safe) or internal/domain/decisioninbox
// (scoped to one window of DECISIONS, not every session this deployment
// has ever run), this rollup is explicitly platform-wide: its own true
// row count has no natural per-entity bound, so internal/app/
// platformanalytics's own Postgres queries do the GROUP BY/SUM/
// percentile_cont reduction in SQL and this package receives only the
// already-small aggregated result. What stays here, still pure and still
// worth naming and testing in isolation, is the SECOND reduction: turning
// day+status+failure_reason counts into the five distinct facts the UI
// renders (sessions-per-day buckets, success rate, top failure reasons,
// total sessions), and the "is this sample big enough to trust" gates for
// the two SQL-side percentiles (cost median, boot p95).
//
// # The "not yet computed" sentinel, applied selectively
//
// A COUNT is always a real, meaningful answer -- 0 sessions in the window
// is a true fact, never "unknown" (unlike a RATE or a PERCENTILE, which
// need a nonzero denominator/sample to mean anything). "Sessions" and
// "False failures" therefore carry no SAMPLE-SIZE/denominator sentinel of
// the kind SuccessRate/boot p95/the cost median carry (their own
// computed-or-not reflects whether enough data exists to make the VALUE
// meaningful, not whether the fetch itself succeeded). That is a
// statement about the shape of the DATA once fetched, not a license to
// skip degrading it honestly when the fetch never happens: a query that
// errors produces no count at all, and internal/adapters/inbound/httpapi.
// GetPlatformAnalytics carries its OWN fetch-outcome sentinel for exactly
// that case (sessionsTotalComputed/falseFailureCountComputed on
// restdtos.PlatformAnalytics) -- this package stays pure and untouched by
// that concern, mirroring internal/domain/reviewverdict's own identical
// (value, ok bool) discipline for SuccessRate/TopFailureReasons: ok=false
// means the denominator/sample was empty, never collapsed into the same
// shape as "the data says zero".
package platformanalytics
