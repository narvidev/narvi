package reviewcheck

// Phase is the review-process state a narvi/review check run must be
// able to express -- the brief's own vocabulary: "queued, running, stale
// and terminal". Terminal splits into the two mutually-exclusive
// outcomes a completed review can have: PhaseTerminalAssessed (a real
// review.Verdict was posted) and PhaseTerminalNotAssessed (the review
// ended -- succeeded, failed, timed out, or was cancelled -- with no
// verdict ever posted for this attempt). PhaseStale is its own, FIFTH
// value, not a variant of terminal spelled differently: see its own doc
// comment below for why it is not "PhaseTerminalAssessed but old".
type Phase string

const (
	// PhaseQueued is published as soon as a pull request enters scope,
	// before any review has started (decision 2: "otherwise a PR nobody
	// triggered a review on carries no check, and a required check that
	// is never published makes the PR mergeable by absence"). No
	// attempt exists yet -- an Emission in this phase always carries
	// AttemptID == "".
	PhaseQueued Phase = "queued"
	// PhaseRunning is published once a review turn has been dispatched
	// for the current attempt -- an assessment is in flight, but no
	// result exists yet.
	PhaseRunning Phase = "running"
	// PhaseStale marks an EXISTING, previously-terminal check run as no
	// longer trustworthy: its recorded attempt/context has been
	// superseded (a newer attempt exists, or the base moved out from
	// under it) but nothing has yet produced a fresh terminal result for
	// the CURRENT head. Deliberately distinct from PhaseTerminalNotAssessed
	// even though both map to the identical GitHub (status, conclusion)
	// pair (ComputeOutput's own doc comment) -- "not_assessed" (this PR
	// has never been reviewed) and "stale" (this PR WAS reviewed, but not
	// at this code) read identically to GitHub's own required-check
	// machinery, and MUST, per decision 1, but they are not the same fact
	// for a human, and Output.Title/Summary say so.
	PhaseStale Phase = "stale"
	// PhaseTerminalAssessed is published once a real review.Verdict has
	// been posted for the current attempt -- an assessment exists.
	PhaseTerminalAssessed Phase = "terminal_assessed"
	// PhaseTerminalNotAssessed is published once a review turn reaches
	// ANY terminal state (complete, fail, cancel, or timeout) with no
	// review.Verdict ever posted for that same attempt -- "a review that
	// did not complete" (decision 1), regardless of why it didn't.
	PhaseTerminalNotAssessed Phase = "terminal_not_assessed"
)

// rank orders phases for Supersedes' own same-attempt comparison
// (emission.go): Queued(0) < Running(1) < every terminal-shaped phase
// (2). Stale/TerminalAssessed/TerminalNotAssessed share rank 2
// deliberately -- they are mutually-exclusive ALTERNATE outcomes for the
// SAME attempt, not a further ordering among themselves. Sharing a rank
// is NOT the same claim as "always applied" -- rank only says neither is
// a REGRESSION by rank; Supersedes' own
// supersedesSameAttemptTerminalAssessed additionally refuses the one
// same-attempt transition that shares this rank but is never a
// legitimate correction (PhaseTerminalAssessed -> PhaseTerminalNotAssessed,
// "no verdict was posted" arriving after one demonstrably was) while
// still allowing PhaseTerminalAssessed -> PhaseStale (the base moving out
// from under an already-posted verdict, same attempt) -- see that
// function's own doc comment for the full rule this rank alone does not
// express.
// An unrecognized Phase ranks -1, which Valid reports as false --
// Supersedes' own caller (internal/app/outboxworker's review-check
// notifier) must never publish an unrecognized phase to GitHub, and this
// is the fail-closed signal that lets it refuse before it does.
func (p Phase) rank() int {
	switch p {
	case PhaseQueued:
		return 0
	case PhaseRunning:
		return 1
	case PhaseStale, PhaseTerminalAssessed, PhaseTerminalNotAssessed:
		return 2
	default:
		return -1
	}
}

// Valid reports whether p is one of this package's five named phases --
// the zero value (Phase("")) and any other unrecognized string are both
// invalid.
func (p Phase) Valid() bool {
	return p.rank() >= 0
}

// Terminal reports whether p is one of the three phases that end an
// attempt (Stale/TerminalAssessed/TerminalNotAssessed) -- i.e. rank 2.
// PhaseStale counts as terminal here even though it is never itself the
// phase a completing turn publishes (only a later re-evaluation of an
// already-terminal row produces it): once published, it ends that
// attempt's own story exactly as the other two do.
func (p Phase) Terminal() bool {
	return p.rank() == 2
}
