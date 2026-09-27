package reviewfreshness

import (
	"context"
	"errors"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// State is a verdict's freshness as a session's result reports it (row
// 182, technical plan §43.20).
type State string

// The four freshness states.
const (
	// StateCurrent: the verdict's recorded context matched the pull
	// request's live facts, read during this call. Never from the record
	// alone.
	StateCurrent State = "current"
	// StateStale: the comparison proved the verdict describes other code,
	// or was produced under other rules.
	StateStale State = "stale"
	// StateUnconfirmed: freshness could not be established -- a fact is
	// missing from the record, or the live read failed or ran out of time.
	StateUnconfirmed State = "unconfirmed"
	// StateNotApplicable: there is no assessed verdict to be fresh, or the
	// pull request is merged or no longer open.
	StateNotApplicable State = "not_applicable"
)

// Assessment is Assess's answer: the state, and why -- the comparison's
// own autoapproval.Reason text for stale and for unconfirmed-by-the-
// comparison, what could not be read for unconfirmed-by-the-live-read, why
// the verdict is moot for not_applicable. Reason is "" for current.
type Assessment struct {
	State  State
	Reason string
}

// The reasons Assess gives when it cannot read what it needs, or finds
// the pull request gone. Every one is a normal answer, never an error.
const (
	ReasonNoCodeHost        = "no code host connection is configured to read the pull request's live state"
	ReasonPullRequestUnread = "the pull request's live state could not be read from the code host"
	ReasonNoLongerOpen      = "the pull request is no longer open"
	// ReasonOutOfTime: the caller's own deadline -- for a session's
	// result, platform.Timeouts.SessionResultLiveReadBudget, all of its
	// pull requests together -- passed before the live read established
	// the verdict's freshness.
	ReasonOutOfTime = "the code host did not answer within the time this read allows for live checks"
)

// Describe says which live fact a Failure could not establish, for a
// reader of a session's result.
func (f *Failure) Describe() string {
	switch f.Step {
	case StepResolveBase:
		return "the base branch's current commit could not be read from the code host"
	case StepBaseAncestry:
		return "the base branch moved, and whether it only moved forward could not be confirmed with the code host"
	case StepAncestorRefUnreadable:
		return "the pull request's ancestor chain could not be read from the code host"
	case StepResolveAncestor:
		return "the ancestor branch's current commit could not be read from the code host"
	case StepAncestorAncestry:
		return "the ancestor branch moved, and whether it only moved forward could not be confirmed with the code host"
	default:
		return ReasonPullRequestUnread
	}
}

// Deps is what Assess reads the code host with: the deployment's source
// control port (nil when none is configured), the credential to read it
// with (the deployment's bot token, as the code-review view and the
// auto-merge worker use), and the bounds.
type Deps struct {
	SourceControl ports.SourceControl
	Token         string
	Timeouts      platform.Timeouts
}

// PullRequest names the pull request a verdict is about.
type PullRequest struct {
	Owner  string
	Repo   string
	Number int
}

// Assess reports whether record, an assessed verdict for pr, still
// describes pr as it stands -- by the one comparison
// (autoapproval.CheckFreshness), in the order the merge path
// (decisioninbox.revalidateCore) runs it, so the two give the same answer
// and the same reason for the same pull request:
//
//  1. The pull request, read live (GetOpenPR, bounded by
//     GitHubGetOpenPRTimeout, the auto-merge worker's own read): an error,
//     or a deadline that cut its composite read short, is unconfirmed; a
//     pull request no longer open is not applicable.
//  2. The merge path's own probe (ProbeInput): CheckFreshness with the
//     pull request's live head and base ref, and its base commit and
//     ancestor chain assumed equal to the recorded ones. A verdict whose
//     head moved is stale here, whatever else it recorded; one that
//     predates context tracking, recorded an unknown commit or link, or
//     was produced under an older policy version fails here too, with no
//     further live read. A probe that passes only means the live facts
//     decide: this step never answers current.
//  3. ReadLive: a fact it cannot establish is unconfirmed, naming it.
//  4. CheckFreshness over the recorded context and the live facts: current
//     only when it passes; otherwise stale or unconfirmed by
//     autoapproval.ClassifyFreshness, with the comparison's own reason.
//
// A live read that fails because ctx's own deadline passed -- a session
// result's budget for all of its live reads -- is unconfirmed with
// ReasonOutOfTime rather than the failed call's own reason.
//
// Never an error, never current without step 4 passing on live facts read
// in this call. The caller decides not_applicable for a verdict that is
// not assessed, or a pull request its own records show merged, before
// calling.
func Assess(ctx context.Context, deps Deps, record reviewverdict.Record, pr PullRequest) Assessment {
	if deps.SourceControl == nil {
		return Assessment{State: StateUnconfirmed, Reason: ReasonNoCodeHost}
	}

	logger := platform.Logger(ctx)
	readCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.GitHubGetOpenPRTimeout)
	target, found, err := deps.SourceControl.GetOpenPR(readCtx, pr.Owner, pr.Repo, pr.Number, deps.Token)
	cancel()
	if err != nil {
		logger.Warn("reviewfreshness: live pull request read failed, freshness unconfirmed", "error", err, "owner", pr.Owner, "repo", pr.Repo, "pr_number", pr.Number)
		return unconfirmed(ctx, ReasonPullRequestUnread)
	}
	// GetOpenPR is a composite of several reads: a deadline that fired
	// partway returns err == nil with whatever the later reads left blank
	// (the auto-merge worker's own H3 guard). Checked for DeadlineExceeded
	// specifically: cancel() above sets Canceled on the ordinary path.
	if errors.Is(readCtx.Err(), context.DeadlineExceeded) {
		logger.Warn("reviewfreshness: live pull request read timed out partway, freshness unconfirmed", "owner", pr.Owner, "repo", pr.Repo, "pr_number", pr.Number)
		return unconfirmed(ctx, ReasonPullRequestUnread)
	}
	if !found {
		return Assessment{State: StateNotApplicable, Reason: ReasonNoLongerOpen}
	}

	if reason := autoapproval.CheckFreshness(ProbeInput(record, target)); reason != autoapproval.ReasonNone {
		return classified(reason)
	}

	live, failure := ReadLive(ctx, deps.Timeouts, deps.SourceControl, deps.Token, target, record.Context)
	if failure != nil {
		logger.Warn("reviewfreshness: live freshness read failed, freshness unconfirmed", "step", string(failure.Step), "error", failure.Err, "owner", pr.Owner, "repo", pr.Repo, "pr_number", pr.Number)
		return unconfirmed(ctx, failure.Describe())
	}
	return classified(autoapproval.CheckFreshness(FreshnessInput(record, live)))
}

// FreshnessInput pairs record's recorded context with live, the pull
// request's live facts -- the comparison's input, assembled the way the
// merge path assembles its own (decisioninbox.revalidateCore's final
// eligibility input): the verdict is assessed, its head and context are
// the record's, and the current side is ReadLive's.
func FreshnessInput(record reviewverdict.Record, live LiveFacts) autoapproval.FreshnessInput {
	return autoapproval.FreshnessInput{
		VerdictAssessed:                     true,
		VerdictHeadSHA:                      record.HeadSHA,
		VerdictBaseRef:                      record.Context.BaseRef,
		VerdictBaseSHA:                      record.Context.BaseSHA,
		VerdictAncestorChain:                record.Context.AncestorChain,
		VerdictPolicyVersion:                record.Context.PolicyVersion,
		CurrentHeadSHA:                      live.HeadSHA,
		CurrentBaseRef:                      live.BaseRef,
		CurrentBaseSHA:                      live.BaseSHA,
		CurrentAncestorChain:                live.AncestorChain,
		BaseAdvancedWithoutRewrite:          live.BaseAdvancedWithoutRewrite,
		AncestorChainAdvancedWithoutRewrite: live.AncestorChainAdvancedWithoutRewrite,
	}
}

// ProbeInput is the merge path's own probe (decisioninbox.revalidateCore's
// probeInput, its freshness half): the pull request's live head and base
// ref, read with target, against the record -- and the base commit and
// ancestor chain, which cost further live calls, assumed equal to the
// recorded ones, every movement assumed forward. So what fails here fails
// whatever those calls would answer, and a moved head is stale before
// anything else is looked at, exactly as it is for the merge path
// (TestAssess_AgreesWithTheMergePathOnRecordDecidedVerdicts).
func ProbeInput(record reviewverdict.Record, target ports.OpenPR) autoapproval.FreshnessInput {
	return FreshnessInput(record, LiveFacts{
		HeadSHA:                             target.HeadSHA,
		BaseRef:                             target.BaseRef,
		BaseSHA:                             record.Context.BaseSHA,
		AncestorChain:                       record.Context.AncestorChain,
		BaseAdvancedWithoutRewrite:          true,
		AncestorChainAdvancedWithoutRewrite: true,
	})
}

// unconfirmed reports a live read that failed: with ReasonOutOfTime when
// the caller's own deadline is what ended it, else with reason.
func unconfirmed(ctx context.Context, reason string) Assessment {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Assessment{State: StateUnconfirmed, Reason: ReasonOutOfTime}
	}
	return Assessment{State: StateUnconfirmed, Reason: reason}
}

// classified renders a comparison's answer as an Assessment.
func classified(reason autoapproval.Reason) Assessment {
	switch autoapproval.ClassifyFreshness(reason) {
	case autoapproval.FreshnessCurrent:
		return Assessment{State: StateCurrent}
	case autoapproval.FreshnessStale:
		return Assessment{State: StateStale, Reason: string(reason)}
	default:
		return Assessment{State: StateUnconfirmed, Reason: string(reason)}
	}
}
