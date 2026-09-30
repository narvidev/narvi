package review

// CounterReviewStatus is the deep path's own structural-enforcement signal
// (§26.4, "the deep path: adversarial counter-review"): the
// control plane cannot observe a sandbox's own internals (whether the
// primary reviewer's orchestration actually spawned and adjudicated the
// `counter-reviewer` sub-task via §7.1's engine-native fan-out), so the
// verdict payload itself carries this typed field instead — mirroring
// DescriptionAdequacy's own placement in THIS package (adequacy.go) rather
// than reviewpost, for the identical reason: this is a direct input to a
// raise-only floor (CounterReviewFloor below), not narrative content.
//
// Unlike every other closed enum in this package (doc.go's "fail-
// conservative policy for every closed enum" section), CounterReviewStatus
// has no meaning at all on the LIGHT path — §26.9 is explicit that the
// light path never runs a counter-reviewer, so there is nothing to have
// "skipped". reviewpost.BuildVerdict (this package's one caller) is the
// layer that knows which path a verdict came from (reviewtriage.
// ReviewDepth, which this package cannot import at all — see doc.go's own
// "zero external imports" convention) and is therefore the layer
// responsible for substituting CounterReviewDone in place of whatever this
// field's own zero value would otherwise fail-conservative to, on every
// light-path verdict — see reviewpost.BuildVerdict's own doc comment for
// the exact substitution. CounterReviewFloor itself stays a pure function
// of ONLY this type, with no depth parameter of its own, exactly matching
// AdequacyFloor/PremiseFloor/CoverageFloor's own signatures.
type CounterReviewStatus string

// The CounterReviewStatus values: the two a reviewer reports (§26.4's own
// "typed CounterReview: done|skipped field", verbatim) and the one the
// server resolves (CounterReviewUncorroborated) — see this type's own doc
// comment for why the zero value ("") is never legitimately observed by
// CounterReviewFloor in production (reviewpost.ValidateVerdictInput
// rejects it outright on the deep path, the only path this field is ever
// schema-required on; reviewpost.BuildVerdict substitutes CounterReviewDone
// on the light path, where it is never schema-required at all).
const (
	// CounterReviewDone is the primary reviewer's orchestration having
	// actually spawned and adjudicated the counter-reviewer sub-task
	// (§7.1's fan-out) — no floor of its own.
	CounterReviewDone CounterReviewStatus = "done"
	// CounterReviewSkipped is a deep-path review whose counter-reviewer
	// sub-task did NOT run — for any cause: a provider failure inside the
	// sub-task, a malformed sub-task result, or (§26.7) the per-path cost
	// ceiling already having been reached before this optional pass would
	// have been dispatched. Every cause floors identically: from the
	// verdict's own point of view, a PR whose own routing signal (§26.3)
	// said it needed adversarial review did not get it, whatever the
	// reason — see §26.7's own "each field's already-decided Shippable
	// consequence applies unchanged and un-special-cased" wording. This
	// package's own fail-conservative reference point for an unrecognized
	// CounterReviewStatus value (see CounterReviewFloor).
	CounterReviewSkipped CounterReviewStatus = "skipped"
	// CounterReviewUncorroborated is a deep-path counter-review the
	// reviewer reported done that the server could not corroborate
	// against this turn's own persisted sub-task trace
	// (reviewverdict.CounterReviewCorroborated): the trace showed no
	// counter-reviewer sub-task that started and completed, or it could
	// not be read, or the finish event had not landed when the verdict
	// was posted -- the server cannot tell these apart, so the value
	// claims only that the done was not confirmed. It is the server's
	// finding, never a report: reviewpost.ValidateVerdictInput rejects it
	// from a payload on every path, and reviewpost.BuildVerdict is the one
	// place this value is produced. It floors exactly like
	// CounterReviewSkipped and keeps a name of its own, so the blocker it
	// produces (§26.1) says what happened rather than reading as a skip
	// the reviewer admitted to.
	CounterReviewUncorroborated CounterReviewStatus = "uncorroborated"
)

// CounterReviewFloor is the FOURTH raise-only floor's single exported pure
// function (§26.4, composing alongside CoverageFloor/PremiseFloor/
// AdequacyFloor via the existing max(rank) in ComputeShippable, §8.2's
// own exactly-one-pure-function-per-floor pattern, extended once already by
// §26.2 and now again by this Step): given the deep path's own
// CounterReviewStatus, it returns the MOST CONSERVATIVE Shippable value
// counter-review alone ever forces. This function alone decides nothing
// about whether a PR ships — see ComputeShippable (shippable.go) for the
// actual composition.
//
//   - CounterReviewDone: ShippableAuto — a completed, adjudicated counter-
//     review imposes no floor of its own (whatever findings it surfaced or
//     refuted are already reflected in the verdict's own RiskLevel/
//     findings by the time this field is set).
//   - CounterReviewSkipped: ShippableNeedsHuman — §26.4's own named floor:
//     "skipped raises the Shippable floor to needs_human". A sensitive or
//     sizable PR (§26.3's own deep-routing triggers) that did not
//     actually get an adversarial counter-review must never auto-approve
//     on the strength of a review pass that never happened.
//   - CounterReviewUncorroborated: ShippableNeedsHuman — the same floor as
//     CounterReviewSkipped: a counter-review the server cannot confirm
//     ran counts for no more than one the reviewer says did not.
//   - the zero value or any other unrecognized CounterReviewStatus:
//     ShippableNeedsHuman, FAILING CONSERVATIVE — ranked identically to
//     CounterReviewSkipped, this enum's own worst known legitimate value
//     (doc.go's uniform fail-conservative policy), never as permissive as
//     CounterReviewDone's own ShippableAuto. In practice this branch is
//     unreachable from a real deep-path verdict (reviewpost.
//     ValidateVerdictInput rejects anything but Done/Skipped on the deep
//     path before BuildVerdict ever runs, and BuildVerdict itself only
//     ever adds Uncorroborated) — kept anyway, exactly like
//     every sibling floor's own identical defensive default, so a future
//     caller that skips validation fails toward the SAME safe direction
//     every other floor already does, rather than an unguarded map/switch
//     miss reading as the permissive end.
func CounterReviewFloor(s CounterReviewStatus) Shippable {
	switch s {
	case CounterReviewDone:
		return ShippableAuto
	case CounterReviewSkipped, CounterReviewUncorroborated:
		return ShippableNeedsHuman
	default:
		return ShippableNeedsHuman
	}
}
