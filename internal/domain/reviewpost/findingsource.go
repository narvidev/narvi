package reviewpost

import "github.com/narvidev/narvi/internal/domain/reviewtriage"

// This file implements §26.6's amendment: what the counter-reviewer adds
// is fact-checked, or published marked unverified. On the deep path the
// funnel is fact-check, then counter-review (§26.6), and the
// counter-reviewer may surface findings of its own. Those additions used
// to be exempt from the fact-check, on the ground that a tool-equipped
// pass is at least as rigorous; nothing measured that, and it exempted the
// one pass that adds findings from the one check that removes them. Now a
// second, diff-only fact-check run goes over what the counter-review
// added, before publication, and the server holds the rule: an addition
// counts as checked only when the turn's own sub-task trace shows a
// fact-check that started after the counter-review and completed
// (reviewverdict.AdditionsFactCheckInTrace). Any other addition is
// published marked unverified and counted apart.

// FindingSource is the pass of the review that produced a published
// finding: the primary reviewer, or (deep path only) the counter-reviewer.
// It is what the reviewer's payload says. The server cannot see which pass
// wrote a finding -- sub_task_finish records that a sub-task ended, never
// what it concluded (§26.4) -- so the source is recorded as self-reported
// (review_findings.reported_source), and precision is read per source on
// that basis (§26.5).
type FindingSource string

// The FindingSource values. A payload carries primary or counter_review;
// any other value is refused (ValidateVerdictInput's
// ErrInvalidFindingSource), and so is no source at all, except from a
// turn rendered before sources existed.
//
// # When an absent source is admitted
//
// The verdict body's shape comes from the review prompt, and the prompt is
// rendered once, when the turn is created (review.RenderTurnPrompt), then
// stored with the turn and re-sent as stored on every dispatch, a
// re-dispatch to a respawned sandbox included. A turn created by a binary
// that predates sources -- queued, running, or re-sent while the control
// plane is deployed -- was never told about the field, and posts its
// findings without one. Refusing them would refuse the verdict its own
// instructions shaped. The server can tell such a turn apart: its stored
// prompt carries no source instruction (review.PromptInstructsFindingSource,
// VerdictInput.SourceInstructed), and its payload reports no second
// fact-check run, a field only the current prompt names. For that turn
// alone an absent source is recorded as not recorded, the state
// migrations/000154 already gives a finding last published before sources
// existed, and every reader treats the two alike: neither primary nor an
// addition, never marked unverified, counted under "not_recorded". A turn
// whose prompt asked for a source and that leaves it off gets the same 400
// as a garbled value, so an addition it forgot to label is never published
// unmarked.
const (
	// FindingSourceNotRecorded is a finding whose source is not recorded:
	// posted with none by a turn rendered before sources existed, or last
	// published before sources existed. It is never a counter-review
	// addition.
	FindingSourceNotRecorded FindingSource = ""
	// FindingSourcePrimary is a finding the primary reviewer produced and
	// put through the first fact-check run.
	FindingSourcePrimary FindingSource = "primary"
	// FindingSourceCounterReview is a finding the counter-reviewer
	// surfaced on its own (§26.4) -- an addition, which the first
	// fact-check run, having run before the counter-review, never saw.
	FindingSourceCounterReview FindingSource = "counter_review"
)

// AdditionCheck is the server's resolution of whether a counter-review
// addition was fact-checked: ResolveAdditionCheck's result, recorded on
// each such finding (review_findings.addition_check) and on the verdict
// (review_verdicts.additions_check). Only AdditionChecked counts as
// checked; every other value -- including the zero value and any value
// this package does not know -- is published marked unverified and
// counted apart (Checked).
type AdditionCheck string

// The AdditionCheck values. Each names what the server found, not what
// it assumes: the trace is read from Postgres when the verdict is posted,
// and the verdict POST and the event carrying a sub-task's finish travel
// separately (§26.4's accepted race), so "not found" is a statement about
// that read, never a claim that no run happened.
const (
	// AdditionChecked: the reviewer reported the second fact-check run
	// done, and the turn's trace, read in full, holds a fact-check
	// sub-task that started after the counter-review's last event and
	// completed.
	AdditionChecked AdditionCheck = "checked"
	// AdditionNotRun: the reviewer did not report the second run done --
	// it reported it skipped (an error, or the review's cost budget,
	// §26.7) or reported nothing.
	AdditionNotRun AdditionCheck = "not_run"
	// AdditionNotFound: the reviewer reported the run done, but the
	// trace, read in full when the verdict was posted, held no fact-check
	// sub-task that started after the counter-review and completed: the
	// run was only reported, ran before the counter-review, did not
	// complete, or its finish had not landed yet.
	AdditionNotFound AdditionCheck = "not_found"
	// AdditionUnconfirmed: the reviewer reported the run done, and the
	// trace could not be read in full (a failed read, a row that did not
	// decode, a finish with no start, a turn with no dispatch scope to
	// read it in or whose events cannot be told from another turn's, or a
	// read cut at the next turn's dispatch that found no run inside its
	// window), so whether a run covered the additions could not be
	// confirmed.
	AdditionUnconfirmed AdditionCheck = "unconfirmed"
)

// Checked reports whether c is the one value that counts as checked.
// Fail-conservative: the zero value and an unknown value are unverified.
func (c AdditionCheck) Checked() bool {
	return c == AdditionChecked
}

// AdditionsTrace is what the server read from the posting turn's own
// sub-task trace about the second fact-check run -- computed by the
// caller (reviewverdict.AdditionsFactCheckInTrace) and handed in on
// VerdictInput, never accepted from the payload. The zero value is "not
// read in full", so a caller that never read the trace can only ever
// produce an unconfirmed addition, never a checked one.
type AdditionsTrace string

// The AdditionsTrace values.
const (
	// AdditionsTraceUnread: the trace was not read, or not read in full.
	AdditionsTraceUnread AdditionsTrace = ""
	// AdditionsTraceRunFound: read in full, and it holds a fact-check
	// sub-task that started after the counter-review's last event and
	// completed.
	AdditionsTraceRunFound AdditionsTrace = "run_found"
	// AdditionsTraceNoRunFound: read in full, and it holds no such
	// sub-task.
	AdditionsTraceNoRunFound AdditionsTrace = "no_run_found"
)

// ResolveAdditionCheck is the rule, server-side: a counter-review addition
// is checked only when the reviewer reported the second fact-check run
// done AND the trace shows it. The report alone never suffices (that is
// the payload's claim), and the trace alone does not either: a run the
// reviewer reports skipped is one whose kills it did not apply.
func ResolveAdditionCheck(reported FactCheckStatus, trace AdditionsTrace) AdditionCheck {
	if reported != FactCheckDone {
		return AdditionNotRun
	}
	switch trace {
	case AdditionsTraceRunFound:
		return AdditionChecked
	case AdditionsTraceNoRunFound:
		return AdditionNotFound
	default:
		return AdditionUnconfirmed
	}
}

// SecondFactCheck is the deep path's second fact-check run, over what the
// counter-review added, recorded apart from the first run
// (VerdictInput.FactCheck/FactCheckKilled): what the reviewer reported
// about it and what the server resolved. Its zero value means there was
// nothing to record, and is what review_verdicts stores as three NULLs.
type SecondFactCheck struct {
	// Reported is the reviewer's own report, "" when it reported none.
	Reported FactCheckStatus
	// ReportedKilled is the count the reviewer says the run removed --
	// self-reported, like FactCheckKilled.
	ReportedKilled int
	// Resolved is the server's resolution of the additions the verdict
	// published, "" when it published none: there is then no addition to
	// mark checked or unverified, even when a second run was reported
	// (one that removed every addition). So a non-empty Resolved always
	// describes published additions, and every value but AdditionChecked
	// means they were published marked unverified.
	Resolved AdditionCheck
}

// BuildSecondFactCheck resolves in's second fact-check run. Applicable
// only on the deep path (ValidateVerdictInput refuses additions and a
// second-run report anywhere else), and only when there is something to
// record: the payload published a counter-review addition, or reported a
// second run. A run that removed every addition still has an outcome
// worth recording -- its report and kill count -- but with no addition
// published there is nothing for the server to resolve, so Resolved stays
// "" ("no addition published") rather than claiming unverified additions
// that do not exist. Every counter-review finding BuildFindings returns
// carries this same Resolved value, so the verdict's record and its
// findings' marks come from one resolution and cannot disagree.
func BuildSecondFactCheck(in VerdictInput) SecondFactCheck {
	if in.ReviewDepth != reviewtriage.DepthDeep {
		return SecondFactCheck{}
	}
	published := hasCounterReviewAddition(in.Findings)
	if !published && in.AdditionsFactCheck == "" {
		return SecondFactCheck{}
	}
	out := SecondFactCheck{
		Reported:       in.AdditionsFactCheck,
		ReportedKilled: in.AdditionsFactCheckKilled,
	}
	if published {
		out.Resolved = ResolveAdditionCheck(in.AdditionsFactCheck, in.AdditionsTrace)
	}
	return out
}

// hasCounterReviewAddition reports whether any of findings says the
// counter-reviewer produced it.
func hasCounterReviewAddition(findings []FindingInput) bool {
	for _, f := range findings {
		if f.Source == FindingSourceCounterReview {
			return true
		}
	}
	return false
}
