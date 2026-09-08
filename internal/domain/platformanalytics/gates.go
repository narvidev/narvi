package platformanalytics

// BootP95MinSamples is the smallest number of boot_timing samples §12.2
// item 6's "Boot p95" tile will render a computed percentile over. A
// percentile computed from a handful of points is not a reliable
// estimate of the underlying distribution's 95th percentile -- with
// fewer than 20 samples, the nearest-rank/interpolated "p95" is close
// enough to the single slowest observed boot that it misrepresents
// "typical" latency as "worst observed" latency, which is a materially
// different, more alarming claim than the tile's own label makes. 20 is
// a conventional statistical floor for this reason (below it, even the
// SHAPE of a percentile estimate is dominated by which few points
// happened to land in the tail) -- chosen deliberately generously rather
// than tuned, since this codebase has no real production boot-volume
// data yet to calibrate against; a future Step with that data is free to
// revise it.
//
// Distinct from "zero samples" (events.sql's own GetBootP95InWindow
// returns sample_size=0 for a brand-new deployment with no boot_timing
// events at all): both render as "not available" to a caller that only
// checks BootP95Computed, but internal/adapters/inbound/httpapi's own
// GetPlatformAnalytics keeps the raw sample_size on the wire regardless,
// so the UI can distinguish "no data yet" (sampleSize == 0) from "too few
// samples so far" (0 < sampleSize < BootP95MinSamples) rather than
// collapsing both into one indistinguishable "not available" state.
const BootP95MinSamples = 20

// BootP95Computed reports whether sampleSize is large enough for §12.2
// item 6's "Boot p95" tile to render its own percentile_cont(0.95)
// result (computed SQL-side, GetBootP95InWindow) as a trustworthy number
// rather than "not available yet" / "too few samples".
func BootP95Computed(sampleSize int64) bool {
	return sampleSize >= BootP95MinSamples
}

// CostComputed reports whether §12.2 item 6's "Cost" KPI tile (total
// spend + median per session) has any real data to render.
// costedSessionCount is GetPlatformCostSummaryInWindow's own sample
// size -- how many DISTINCT sessions had at least one turn record a cost
// figure in the window. Unlike BootP95Computed, there is no separate
// "too few to trust" threshold here: a SUM and a MEDIAN over even one
// real data point are both still real, honest answers (a percentile over
// a handful of points can misrepresent a DISTRIBUTION's tail, but a
// dollar total and a median of however many real dollar figures exist
// cannot mislead the same way) -- the only question this tile needs
// answered is "has any cost data arrived at all".
func CostComputed(costedSessionCount int64) bool {
	return costedSessionCount > 0
}
