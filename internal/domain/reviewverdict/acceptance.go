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
	// that SAME row. VerdictID equality alone is NOT sufficient to prove
	// "one attempt" still holds, though (finding F1, adversarial review,
	// corrected -- a previous version of this comment claimed it was): an
	// attempt that ends not_assessed posts no review_verdicts row at all,
	// so a NEWER attempt can exist with currentVerdictID still unchanged.
	// Applicable below therefore ALSO takes hasNewerAttempt, a
	// caller-supplied fact (internal/app/reviewverdict.
	// HasNewerReviewAttempt) -- see that method's own doc comment for the
	// full reasoning, including the one thing neither check by itself
	// catches (a moved base under an unchanged verdict and no new
	// attempt).
	VerdictID string
	// AttemptID/HeadSHA/Context are carried here VERBATIM from the
	// accepted verdict, for display/audit only (this table's own
	// migration doc comment) AND as the key Applicable's caller uses to
	// resolve hasNewerAttempt (internal/app/reviewverdict.
	// HasNewerReviewAttempt reads THIS field, via TurnStore.Get, to find
	// the accepted attempt's own turns.created_at) -- Applicable itself
	// never reads this field directly, since hasNewerAttempt is already
	// resolved by the time Applicable is called.
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
	// is still active. Non-nil means either a maintainer+ explicitly
	// revoked it, or a fresh Accept for the same pull request superseded
	// it -- RevocationReason (below) is what tells the two apart; kept,
	// never deleted, for the audit trail (§21.1b: "auditable revocation").
	RevokedAt       *time.Time
	RevokedByUserID string
	// RevocationReason is "explicit" (a maintainer+'s own
	// /revoke-verdict-acceptance click, RevokeReviewVerdictAcceptance) or
	// "superseded" (a fresh Accept for the SAME pull request revoked this
	// row automatically, SupersedeActiveReviewVerdictAcceptances) --
	// display/audit only, empty while RevokedAt is nil (finding F4,
	// adversarial review: before this column existed, both cases wrote the
	// IDENTICAL revoked_at/revoked_by shape, so a superseded acceptance
	// was indistinguishable, on this row alone, from an explicit
	// revocation the accepting user never performed).
	RevocationReason string
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
// CORRECTED (finding F1, adversarial review, reproduced end to end
// against real Postgres): a previous version of this doc comment claimed
// the "new attempt" half of that contract "reduces entirely to"
// currentVerdictID equality, reasoning that a new attempt always posts a
// NEW review_verdicts row. That is false for the one attempt outcome
// this package's own sibling, review's "not_assessed" state, exists to
// name: a review attempt that ends without ever calling the
// verdict-posting tool posts NO review_verdicts row at all
// (sessionactor.enqueueReviewCheckNotAssessed fires PRECISELY because
// reviewverdict.ExistsForAttempt is false for that attempt) -- so
// internal/app/reviewverdict.GetLatest still returns the OLD, accepted
// verdict, currentVerdictID still equals a.VerdictID, and the acceptance
// stayed applicable through an attempt it was never granted against.
// Proven: after a second attempt at the same head ended
// terminal_not_assessed, RevalidateForMerge still returned ok=true,
// viaAcceptance=true, with the PR's own narvi/review check reading
// action_required -- nothing else catches this (githubapi.
// fetchCIConclusionLive deliberately never folds Narvi's own check into
// CIConclusion, so this is not a CI-green failure either).
//
// hasNewerAttempt is therefore now a CALLER-SUPPLIED fact, never
// re-derived here (this package is I/O-free, §11) -- internal/app/
// reviewverdict.HasNewerReviewAttempt is the one function that computes
// it honestly, by querying turns directly for any is_review_attempt row
// in the SAME session strictly newer than the accepted attempt's own
// turns.created_at, regardless of whether that newer attempt ever posted
// a verdict. A caller that cannot resolve this (no store wired, a
// genuine lookup error) must pass true -- HasNewerReviewAttempt's own
// doc comment: the safe default denies the waiver, never grants one this
// deployment cannot actually confirm.
//
// A moved base or a changed ancestor chain, by themselves, do NOT change
// which verdict row is latest and do NOT by themselves make a newer
// attempt exist -- so hasNewerAttempt alone cannot detect those two
// triggers, and this function deliberately does not ask it to: they are
// instead caught downstream, by autoapproval.
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
// currentVerdictID or hasNewerAttempt.
func (a Acceptance) Applicable(currentVerdictID string, hasNewerAttempt bool) bool {
	if a.Revoked() {
		return false
	}
	if a.VerdictID == "" || a.VerdictID != currentVerdictID {
		return false
	}
	return !hasNewerAttempt
}
