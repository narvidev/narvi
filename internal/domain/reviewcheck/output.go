package reviewcheck

// CheckName is the fixed, single check name every narvi/review check run
// this publisher ever creates or updates carries -- GitHub's own "select
// by SHA and App" identity rule (§21.1b/the brief's own identity rules)
// filters candidate check runs by (head SHA, owning App), never by name
// alone, but the name is still what a human reads on the PR's Checks
// tab, and a fixed constant (never a per-repo/per-caller configurable
// string) is what makes that filter meaningful in the first place: a
// caller that could rename its own check would defeat the one piece of
// this identity a human actually recognizes.
const CheckName = "narvi/review"

// Status is GitHub's own check-run "status" vocabulary (the Checks API's
// documented enum: queued, in_progress, completed -- nothing else is a
// legal value GitHub will accept). Named here, not a bare string,
// exactly like this codebase's other closed external vocabularies
// (reviewpost.FormalReviewEvent, turn.State) -- so a caller can never
// pass GitHub a value this package has not itself decided is one of the
// three.
type Status string

// The three legal GitHub check-run status values -- see Status's own doc
// comment above for why this is a closed, named set rather than a bare
// string a caller could pass anything through.
const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Conclusion is GitHub's own check-run "conclusion" vocabulary --
// deliberately a NARROW subset of GitHub's real, larger enum (which also
// contains neutral/failure/cancelled/skipped/timed_out/stale as a
// GitHub-internal-only value never settable by an app -- see this
// package's own doc comment on PhaseStale for why THIS package's own
// "stale" is expressed as action_required, never by attempting to write
// GitHub's own reserved conclusion value of the same name). Two values
// only, because two are all this publisher's own state mapping
// (ComputeOutput below) ever needs to write -- decision 1 rejects
// neutral and failure outright, and nothing here computes a risk-based
// pass/fail (§21.2's eligibility engine, and the formal PR review event
// §8.2 already submits, are what carry that signal -- this check
// expresses PROCESS completion, never risk).
type Conclusion string

const (
	// ConclusionNone means "not completed yet" -- paired only with
	// StatusQueued/StatusInProgress, where GitHub's own API requires
	// conclusion to be entirely absent.
	ConclusionNone Conclusion = ""
	// ConclusionSuccess is published for PhaseTerminalAssessed only: a
	// real review.Verdict was posted for the current attempt. Never
	// gated on the verdict's OWN risk level -- see this file's own top
	// doc comment.
	ConclusionSuccess Conclusion = "success"
	// ConclusionActionRequired is decision 1's own fixed answer for "a
	// review that did not complete": the only GitHub conclusion whose
	// semantics match the truth (does not satisfy a required check,
	// tells a human an action is expected). Also reused, unchanged, for
	// PhaseStale (see PhaseStale's own doc comment for why reusing this
	// exact value -- rather than GitHub's own reserved "stale"
	// conclusion, which an App cannot set via this API at all -- is
	// deliberate). ConclusionNeutral does not exist as a constant in
	// this package at all: it must never be reachable from ComputeOutput,
	// by construction, not merely by convention -- see this package's own
	// test asserting no output value equals "neutral" for every Phase,
	// including the fail-closed default.
	ConclusionActionRequired Conclusion = "action_required"
)

// Output is what ComputeOutput resolves a Phase to: the GitHub-facing
// status/conclusion pair, plus the human-facing title/summary text a
// caller renders into the check run's own "output" object. Title/Summary
// are deliberately plain, static strings here -- never templated with
// verdict content (risk level, findings) -- this package computes ONLY
// the check's own process-state text; a caller that wants to enrich
// Summary with real verdict data (e.g. the risk level, for a human's
// convenience) appends to it, it never replaces what this function
// already decided.
type Output struct {
	Status     Status
	Conclusion Conclusion
	Title      string
	Summary    string
}

// ComputeOutput is the ENTIRE state mapping this publisher implements --
// "map each state to GitHub's status/conclusion pair and write down the
// mapping where a reader will find it" (the brief's own words). This
// doc comment is that mapping:
//
//	Phase                    | status      | conclusion       | required-check semantics
//	-------------------------|-------------|------------------|--------------------------
//	PhaseQueued               | queued      | (none)           | does not satisfy (not yet completed)
//	PhaseRunning               | in_progress | (none)           | does not satisfy (not yet completed)
//	PhaseStale                 | completed   | action_required  | does not satisfy (decision 1)
//	PhaseTerminalNotAssessed   | completed   | action_required  | does not satisfy (decision 1, FIXED)
//	PhaseTerminalAssessed      | completed   | success          | satisfies
//
// Only the PhaseTerminalNotAssessed row is fixed by decision 1 ("a
// review that did not complete publishes action_required... GitHub has
// no conclusion value meaning 'not assessed'"); every other row is this
// function's own choice, stated here:
//
//   - Queued/Running never carry a conclusion at all, because GitHub's
//     own API rejects one on an incomplete check run -- there is no
//     decision to make for these two beyond mapping Phase to the
//     matching GitHub status literally.
//   - PhaseStale reuses action_required rather than GitHub's own
//     conclusion value of the identical name, because that value is
//     documented as settable only by GitHub itself, never by an app
//     (this package's own doc comment on PhaseStale) -- and reuses it
//     rather than neutral for the SAME reason decision 1 forbids neutral
//     for not-assessed: neutral satisfies a required check, and a stale
//     result satisfying a required check is precisely "a PR nobody
//     re-reviewed after its base moved becomes mergeable by an outdated
//     green checkmark" -- the identical failure shape decision 2 exists
//     to close for "never reviewed at all". A human distinguishes the
//     two only by Title/Summary text, never by conclusion.
//   - PhaseTerminalAssessed maps to success, unconditionally, regardless
//     of the posted verdict's own risk level: this check expresses
//     REVIEW COMPLETION, not review OUTCOME. A completed, high-risk
//     verdict's own blocking behavior comes from the formal PR review
//     event (§8.2's REQUEST_CHANGES) and the review:*-risk labels
//     (already published, unchanged, by ports.NotificationKindGitHubVerdict)
//     -- duplicating that signal here, on a DIFFERENT GitHub primitive
//     with its own, coarser vocabulary, would let the two drift and
//     disagree about the same pull request, exactly what §21.1b's "every
//     consumer reads the same context" opens by naming.
//
// An unrecognized/invalid Phase (p.Valid() == false: the zero value, or
// any string this package's own five constants do not name) is a
// PROGRAMMER ERROR reaching this function -- never silently rendered as
// a pass. It fails closed to the SAME (completed, action_required) pair
// PhaseTerminalNotAssessed uses, with a title that says so plainly,
// mirroring this codebase's own "an unrecognized value is refused, never
// defaulted to the permissive case" convention (e.g.
// reviewpost.RiskLabel's own fail-conservative default).
func ComputeOutput(p Phase) Output {
	switch p {
	case PhaseQueued:
		return Output{
			Status:  StatusQueued,
			Title:   "Review queued",
			Summary: "Narvi has not yet started reviewing this pull request.",
		}
	case PhaseRunning:
		return Output{
			Status:  StatusInProgress,
			Title:   "Review in progress",
			Summary: "Narvi is reviewing this pull request.",
		}
	case PhaseStale:
		return Output{
			Status:     StatusCompleted,
			Conclusion: ConclusionActionRequired,
			Title:      "Review stale",
			Summary:    "The reviewed code has changed since Narvi's last result for this pull request. A fresh review is needed.",
		}
	case PhaseTerminalNotAssessed:
		return Output{
			Status:     StatusCompleted,
			Conclusion: ConclusionActionRequired,
			Title:      "Review not completed",
			Summary:    "Narvi did not produce a review result for this pull request. This does not satisfy a required check.",
		}
	case PhaseTerminalAssessed:
		return Output{
			Status:     StatusCompleted,
			Conclusion: ConclusionSuccess,
			Title:      "Review complete",
			Summary:    "Narvi has posted a review verdict for this pull request.",
		}
	default:
		return Output{
			Status:     StatusCompleted,
			Conclusion: ConclusionActionRequired,
			Title:      "Review state unknown",
			Summary:    "Narvi could not determine this pull request's review state. This does not satisfy a required check.",
		}
	}
}
