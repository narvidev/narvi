package reviewcheck

import "time"

// Emission is one candidate write to a pull request's own narvi/review
// check run -- "an emission carries the attempt and context it was
// produced for" (§21.1b). Every field is already-resolved data (no I/O,
// no clock, mirroring this package's own no-I/O contract); a caller
// builds one from facts it already has (a turn row's own id/created_at,
// a review_verdict_context it already unmarshaled), never from a fresh
// computation inside this package.
type Emission struct {
	// RepoFullName/PRNumber identify the pull request -- the SAME
	// natural key github_pr_sessions already claims per PR (§8.2).
	RepoFullName string
	PRNumber     int32
	// HeadSHA is the commit this emission targets -- a GitHub check run
	// is scoped to exactly one SHA (this package's own doc.go), so a
	// caller publishing for a NEW head (a fresh commit landed) is always
	// opening a new external identity, never updating one minted for an
	// older SHA. Required: an emission with no HeadSHA cannot be
	// published at all (nothing to create or update against) -- callers
	// are expected to skip enqueuing entirely rather than construct one,
	// mirroring httpapi.PostReviewVerdict's own "no review head sha on
	// record, skipping" precedent.
	HeadSHA string
	// AttemptID is the turn that produced this emission (turns.id,
	// forwarded verbatim, plain string -- mirrors reviewverdict.Record.
	// AttemptID's own identical "adapter-independent, caller converts at
	// the boundary" contract, §11). Empty for PhaseQueued only: no
	// attempt exists yet when a pull request merely enters scope.
	AttemptID string
	// AttemptCreatedAt is that SAME turn's own creation time (turns.
	// created_at) -- the ordering key Supersedes compares two DIFFERENT
	// attempts by. The zero time for a PhaseQueued emission (AttemptID
	// == "" too) is deliberate: a zero time can never be After() any
	// real attempt's own created_at, so a queued-for-nothing-yet
	// emission can never supersede a real attempt already in progress or
	// concluded, with no special-casing needed in Supersedes itself.
	AttemptCreatedAt time.Time
	// Phase is the review-process state this emission asserts.
	Phase Phase
	// BaseRef/BaseSHA/PolicyVersion mirror internal/domain/reviewverdict.
	// Context's own identically-named fields (that package's own doc
	// comment) -- carried here, not by importing that type, for the
	// reason this package's own doc.go states: a check run's identity
	// and this package's own supersession rule are not review_verdicts'
	// analytics/eligibility concern, even though the underlying facts
	// are the same ones a review turn's context-fetch already resolved.
	// Not consulted by Supersedes below (attempt recency is the ordering
	// key that matters for the write-conflict this package resolves);
	// carried for display/audit parity with the verdict this emission
	// may accompany, and for a future caller that needs to compare a
	// PUBLISHED emission's own recorded base against the pull request's
	// LIVE base to decide whether to enqueue a PhaseStale emission at all
	// (not built by this Step's own publisher -- see this package's own
	// doc comment on PhaseStale, and the implementing PR's own report,
	// for why that live-base-watching trigger is named as an explicit,
	// separate follow-up rather than assumed here).
	BaseRef       string
	BaseSHA       string
	PolicyVersion int
}

// Supersedes reports whether candidate should overwrite current --
// "refused when either has been superseded" (§21.1b), with "either"
// resolved here to a single, total ordering: attempt recency first, then
// same-attempt phase progression. current is the zero Emission when
// nothing has ever been published for this pull request (AttemptID ==
// "", the zero time) -- Supersedes always reports true in that case
// (nothing to lose to).
//
// Two comparisons, applied in order:
//
//  1. Different attempts (candidate.AttemptID != current.AttemptID):
//     whichever attempt is NEWER wins, by AttemptCreatedAt --
//     "an older emission with a matching head must still lose to a newer
//     attempt" (§21.1b), and the reverse: a newer attempt always wins
//     over an older one regardless of which one's own emission reaches
//     this comparison first (outbox delivery order is not attempt
//     order -- retries and backoff can reorder two attempts' own
//     deliveries). A tie (equal AttemptCreatedAt for two DIFFERENT
//     attempt ids -- theoretically possible, never observed: turns.id is
//     a fresh UUID per row, but two turns could share a created_at down
//     to whatever precision Postgres's now() returns) is NOT superseded
//     (After() is strict) -- the FIRST such candidate this function ever
//     sees wins and holds the row until a genuinely later attempt
//     arrives; this is an accepted, harmless residual (this codebase's
//     own "id DESC" tie-break precedent, review_verdicts.sql's own
//     GetLatestReviewVerdict, exists for an identical read-side tie and
//     is not reproduced here because a write-side tie has no reader
//     depending on reproducibility the way a read result does).
//  2. The SAME attempt (candidate.AttemptID == current.AttemptID):
//     candidate wins iff its Phase's rank is not a REGRESSION
//     (candidate.Phase.rank() >= current.Phase.rank()) -- Running never
//     overwrites an already-terminal result for the identical attempt
//     (a redelivered, stale outbox row arriving after the real terminal
//     emission already published must not resurrect
//     "in progress" over a concluded result), while a same-rank update
//     (e.g. PhaseTerminalAssessed republished with corrected display
//     text, or a plain redelivery of the identical emission) is
//     ORDINARILY allowed -- idempotent, never refused. finding A8's own
//     fix narrows this ONE further: when current is ALREADY
//     PhaseTerminalAssessed (a real review.Verdict was posted for this
//     exact attempt), a same-attempt candidate may only be ANOTHER
//     PhaseTerminalAssessed (the idempotent-redelivery/corrected-text
//     case above) -- never a same-rank Stale/TerminalNotAssessed
//     "correction", which is never a legitimate same-attempt transition
//     out of a real, already-posted success (supersedesSameAttemptTerminalAssessed,
//     below).
//
// An invalid candidate.Phase (Valid() == false) is refused unconditionally,
// regardless of attempt recency -- this package's own caller must never
// publish a Phase it cannot itself compute an Output for (ComputeOutput's
// own fail-closed default exists as defense in depth for exactly this
// case, never as license to reach it deliberately).
func Supersedes(current, candidate Emission) bool {
	if !candidate.Phase.Valid() {
		return false
	}
	if candidate.AttemptID != current.AttemptID {
		return candidate.AttemptCreatedAt.After(current.AttemptCreatedAt)
	}
	if supersedesSameAttemptTerminalAssessed(current, candidate) {
		return false
	}
	return candidate.Phase.rank() >= current.Phase.rank()
}

// supersedesSameAttemptTerminalAssessed (finding A8) reports whether
// current/candidate fall into the ONE same-rank, same-attempt shape
// Supersedes above refuses rather than allows: rank 2 is shared by three
// MUTUALLY EXCLUSIVE alternate outcomes (Stale/TerminalAssessed/
// TerminalNotAssessed, Phase.rank's own doc comment), and Supersedes'
// general same-attempt rule otherwise allows any one of them to replace
// any other for the identical attempt -- a legitimate "correction" for
// the Stale/TerminalNotAssessed pair (redisplaying with corrected text,
// or a re-evaluation flipping stale status), but never safe for
// PhaseTerminalAssessed: that phase means "a real review.Verdict was
// posted for this attempt", a fact that does not become UN-true. The
// ONLY same-attempt candidate ever allowed to replace an already-
// terminal-assessed current is another PhaseTerminalAssessed itself (an
// idempotent redelivery, or republished display text) -- never a
// same-attempt Stale/TerminalNotAssessed masquerading as a "correction".
func supersedesSameAttemptTerminalAssessed(current, candidate Emission) bool {
	return current.Phase == PhaseTerminalAssessed && candidate.Phase != PhaseTerminalAssessed
}
