package reviewverdict

import "time"

// Acceptance is one review_verdict_acceptances row ("human acceptance of
// a verdict the engine refuses", §21.1b) -- a human's authorisation to
// proceed past a SPECIFIC review_verdicts row's own human-judgment
// eligibility refusal (autoapproval.ReasonNotShippableAuto or
// autoapproval.ReasonDiffTooLarge -- see autoapproval.
// ComputeEligibleWithAcceptance's own doc comment for exactly which two
// criteria an acceptance may waive, and which stay mandatory
// regardless). An AUTHORISATION, never an override: it never rewrites
// VerdictID's own risk level -- Record above is never mutated by an
// Acceptance existing -- and every eligibility criterion that is not
// about human judgment still applies exactly as ComputeEligible already
// enforces it, whether or not an applicable Acceptance exists.
type Acceptance struct {
	ID           string
	RepoFullName string
	PRNumber     int32
	// VerdictID is review_verdicts.id -- the ONE verdict this acceptance
	// binds to. A review_verdicts row is immutable and append-only
	// (§21.1: "never an update-in-place") and already carries its OWN
	// attempt (Record.AttemptID) and its OWN context (Record.Context) on
	// that SAME row -- so VerdictID alone is what "one verdict, one
	// attempt and one context" (§21.1b) reduces to for THIS check:
	// Applicable below needs no separate attempt/context comparison of
	// its own -- see that method's own doc comment for the full
	// reasoning, including the one thing VerdictID equality does NOT by
	// itself catch (a moved base under an unchanged verdict).
	VerdictID string
	// AttemptID/HeadSHA/Context are carried here VERBATIM from the
	// accepted verdict, for display/audit only (this table's own
	// migration doc comment) -- Applicable below never reads them, since
	// VerdictID equality already implies them.
	AttemptID string
	HeadSHA   string
	Context   Context
	// Reason is a best-effort, no-I/O classification (httpapi.
	// AcceptReviewVerdict's own accept-time guess, autoapproval.Reason's
	// own string vocabulary) of which waivable eligibility criterion this
	// acceptance most likely addresses (e.g. "the verdict's shippable
	// classification is not auto") -- NOT itself computed by calling
	// autoapproval.ComputeEligible (finding F8, adversarial review: this
	// comment previously claimed it was) -- display/audit only: never
	// itself consulted to decide whether this acceptance still applies. A
	// caller applying this acceptance always re-runs autoapproval.
	// ComputeEligibleWithAcceptance against the pull request's CURRENT
	// live facts, which independently re-derives whichever reason(s)
	// still apply.
	Reason string
	// Justification is the accepting maintainer's own required free-text
	// explanation (§21.1b: "carries author, justification"). Untrusted,
	// human-authored content -- never rendered anywhere as an
	// instruction, mirroring this codebase's own review_false_positive_
	// patterns.reason discipline.
	Justification    string
	AcceptedByUserID string
	AcceptedAt       time.Time
	// RevokedAt/RevokedByUserID: RevokedAt == nil means this acceptance
	// is still active. Non-nil means a maintainer+ explicitly revoked it
	// -- kept, never deleted, for the audit trail (§21.1b: "auditable
	// revocation").
	RevokedAt       *time.Time
	RevokedByUserID string
}

// Revoked reports whether a has been explicitly revoked.
func (a Acceptance) Revoked() bool {
	return a.RevokedAt != nil
}

// Applicable reports whether a still authorises proceeding past
// currentVerdictID's own eligibility refusal -- §21.1b: "acceptance
// binds to one verdict, one attempt and one context... a new attempt, a
// moved base or a changed ancestor chain makes it inapplicable, exactly
// as it makes the verdict stale."
//
// Because a review_verdicts row is immutable and append-only, and a's
// own VerdictID already names the ONE verdict (and, transitively, the
// ONE attempt and ONE context that SAME immutable row carries --
// VerdictID's own doc comment above), the "new attempt" half of that
// contract reduces entirely to: is currentVerdictID -- the id of the
// LATEST verdict internal/app/reviewverdict.GetLatest would return for
// this pull request RIGHT NOW -- still the SAME id this acceptance was
// granted against? A new attempt (a re-triggered review, or a fresh
// review after a new commit landed) always posts a NEW review_verdicts
// row with a NEW id, so currentVerdictID changes the instant that
// happens, with no separate attempt-id comparison needed here.
//
// A moved base or a changed ancestor chain, by themselves, do NOT change
// which verdict row is latest (nobody re-reviewed) -- so this function
// alone cannot detect those two triggers, and deliberately does not try:
// they are instead caught downstream, by autoapproval.
// ComputeEligibleWithAcceptance's OWN unconditional (never waived)
// base-moved/ancestor-chain-changed checks, which run whether or not
// accepted is true (that function's own doc comment). This function and
// that one TOGETHER implement the full "new attempt, moved base, or
// changed ancestor chain" contract -- neither one alone -- exactly the
// same division of labor ComputeEligible itself already draws between
// "is this the right code" (head/base/ancestor-chain freshness) and "is
// this code good enough" (CI, Shippable, diff size, sensitive path).
//
// A revoked acceptance is never applicable, regardless of
// currentVerdictID.
func (a Acceptance) Applicable(currentVerdictID string) bool {
	if a.Revoked() {
		return false
	}
	return a.VerdictID != "" && a.VerdictID == currentVerdictID
}
