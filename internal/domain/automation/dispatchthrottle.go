package automation

// DispatchThrottleThreshold is D8's own audit fix (confirmed, SECURITY
// finding: "unbounded invocations" -- every matching GitHub/Linear webhook
// delivery created a brand-new invocation with no per-automation
// throttle, coalescing, or in-flight cap at all, so an attacker-controlled
// event stream on a public repo -- or simply a single authorized-but-noisy
// actor, or a CI system posting comments -- could create unbounded
// invocations, each fanning out into up to MaxFanOutTargets (10) sandboxed
// agent sessions).
//
// Reuses the SAME count-within-a-window-against-a-threshold SHAPE
// domain/sandbox.EvaluateCircuitBreaker and domain/imagebuild.
// EvaluateBackoff already establish for this codebase's other two
// "something keeps happening too often, stop" decisions -- deliberately
// NOT domain/sandbox.EvaluateCircuitBreaker itself (a cross-domain import
// between two unrelated domain packages, each already independent of the
// other by design), and deliberately NOT a CONSECUTIVE-failure count the
// way that breaker's own FailureCount is: this throttle counts
// INVOCATIONS regardless of outcome (a string of successes is exactly as
// capable of spawning unbounded sandboxed agent runs as a string of
// failures), so "3 consecutive failures" is the wrong shape to reuse
// verbatim here, only its GENERAL "count, window, threshold" pattern.
//
// 20 is not specified by the plan -- chosen so that, combined with
// MaxFanOutTargets (10), a single automation is bounded to at most 200
// sandboxed agent sessions per AutomationDispatchThrottleWindow (5
// minutes, platform.Timeouts) -- generous enough that a legitimately busy
// repository's own ordinary event volume is never throttled in practice,
// while still being a REAL, finite ceiling instead of none at all.
const DispatchThrottleThreshold = 20

// EvaluateDispatchThrottle reports whether ANOTHER invocation may be
// created for an automation that has already created countInWindow
// invocations within the caller's own trailing window (app/automation's
// own githubdispatch.go/lineardispatch.go: a COUNT(*) ... WHERE created_at
// >= now()-AutomationDispatchThrottleWindow query, done in the impure
// layer -- this function stays pure, taking only the already-computed
// count, per §11). false means the caller must skip creating a new
// invocation and log a named, loud reason instead.
func EvaluateDispatchThrottle(countInWindow int) bool {
	return countInWindow < DispatchThrottleThreshold
}
