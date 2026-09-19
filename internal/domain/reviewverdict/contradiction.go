package reviewverdict

// Outcome is one auto_approval_outcomes row's own outcome column (§21.2
// stage 2, migrations/000070_auto_approval_outcomes.up.sql) -- a closed
// vocabulary living here in Go, never a Postgres ENUM, mirroring
// review_findings.status/sentinel_kind's own established "the schema
// stays TEXT, a sibling Go package is the source of truth" precedent
// (that table's own migration doc comment).
type Outcome string

const (
	// OutcomeConfirmed is an auto-approved PR that was actually merged
	// (human 1-click confirm, or the armed auto-merge worker) -- the
	// engine's own judgment stood. NEVER recorded for a merge that only
	// happened because an acceptance ("human acceptance of a verdict the
	// engine refuses", §21.1b) waived a refusal -- see
	// OutcomeAcceptedOverride below, adversarial-review finding F1: this
	// distinction is the entire reason that outcome exists as its own
	// value rather than folding into this one.
	OutcomeConfirmed Outcome = "confirmed"
	// OutcomeOverridden is a human disagreeing with an auto-approved
	// verdict BEFORE any merge happened (GitHub's own HasChangesRequested
	// became true, or a review:needs-human label was applied).
	OutcomeOverridden Outcome = "overridden"
	// OutcomeAcceptedOverride (finding F1, adversarial review) is a PR
	// that merged ONLY because an applicable review_verdict_acceptances
	// row waived the engine's own refusal (autoapproval.
	// ComputeEligibleWithAcceptance's own viaAcceptance=true) -- NEITHER
	// "confirmed" (the engine did not approve this PR; a human overrode
	// its refusal) NOR "overridden" in the existing sense (that value
	// means a human disagreed with an auto-approval BEFORE any merge;
	// this one means a human authorised proceeding past a REFUSAL, and
	// the PR then actually merged). Recorded from the SAME two
	// merge-completion call sites as OutcomeConfirmed (httpapi.
	// MergePullRequest, internal/app/automerge's own worker), gated on
	// RevalidateForMerge/RevalidateForAutoMerge's own viaAcceptance
	// return value (internal/app/decisioninbox/revalidate.go) --
	// EXCLUDED from both total and contested by ContradictionRate below
	// (round-5 adversarial review, finding V1, correcting this comment's
	// own prior claim that it counted as contested): this PR was never
	// auto-approved at all -- the engine refused it -- so it does not
	// belong in a metric §21.2 itself defines over auto-approved PRs. See
	// internal/adapters/outbound/postgres/queries/autoapprovaloutcomes.sql's
	// own CountAutoApprovalOutcomesInWindow comment for the worked
	// arithmetic. It is still recorded, and still visible per-PR in the
	// decision inbox -- only this aggregate's population excludes it.
	OutcomeAcceptedOverride Outcome = "accepted_override"
)

// ContradictionRate computes §21.2's own calibration metric -- "the
// fraction of auto-approved PRs a human later disagreed with" -- as
// contested/total. ok=false when total is zero: no auto-approval outcome
// has been recorded yet in the queried window, a legitimate "not yet
// computed" answer a caller must never render identically to a real,
// computed 0% contradiction rate (§21.1's own sentinel discipline,
// doc.go) -- the whole reason an admin arming the auto-merge toggle for a
// brand-new repo must see "no data yet", never a falsely reassuring "0%
// so far".
//
// contested is expected to be <= total (every OutcomeOverridden row is
// also counted in total; OutcomeAcceptedOverride rows are excluded from
// BOTH, since the underlying query -- internal/app/reviewverdict's own
// CountAutoApprovalOutcomesInWindow -- filters them out entirely, round-5
// finding V1); this function does not itself validate that relationship
// (a pure arithmetic reduction over values the caller's own query already
// guarantees are consistent, mirroring MedianLatency's own "caller
// fetches, this package only reduces" split), but the returned rate is
// naturally in [0, 1] whenever the caller's own invariant holds.
func ContradictionRate(total, contested int) (rate float64, ok bool) {
	if total == 0 {
		return 0, false
	}
	return float64(contested) / float64(total), true
}
