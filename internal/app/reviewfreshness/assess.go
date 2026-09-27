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
	// missing from the record, or the live read failed or timed out.
	StateUnconfirmed State = "unconfirmed"
	// StateNotApplicable: there is no assessed verdict to be fresh, or the
	// pull request is merged or closed.
	StateNotApplicable State = "not_applicable"
)

// Assessment is Assess's answer: the state, and why -- the comparison's
// own autoapproval.Reason text for stale and unconfirmed-by-the-record,
// what could not be read for unconfirmed-by-the-live-read, why the verdict
// is moot for not_applicable. Reason is "" for current.
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
// (autoapproval.CheckFreshness) over the one live read (ReadLive), exactly
// as the merge path decides it.
//
//  1. The record alone first: CheckFreshness with the live side assumed
//     equal to the recorded one -- the merge path's own probe. A verdict
//     that predates context tracking, recorded an unknown commit or link,
//     never recorded its head, or was produced under an older policy
//     version fails on the record whatever the pull request looks like
//     now, so it is reported (stale or unconfirmed) with no live read.
//     This step can never answer current: a probe that passes only means
//     the live read decides.
//  2. The pull request, read live (GetOpenPR, bounded by
//     GitHubGetOpenPRTimeout, the auto-merge worker's own read): an error,
//     or a deadline that cut its composite read short, is unconfirmed; a
//     pull request no longer open is not applicable.
//  3. ReadLive: a fact it cannot establish is unconfirmed, naming it.
//  4. CheckFreshness over the recorded context and the live facts: current
//     only when it passes; otherwise stale or unconfirmed by
//     autoapproval.ClassifyFreshness, with the comparison's own reason.
//
// Never an error, never current without step 4 passing on live facts read
// in this call. The caller decides not_applicable for a verdict that is
// not assessed, or a pull request its own records show merged or closed,
// before calling.
func Assess(ctx context.Context, deps Deps, record reviewverdict.Record, pr PullRequest) Assessment {
	if reason := autoapproval.CheckFreshness(recordedAsLive(record)); reason != autoapproval.ReasonNone {
		return classified(reason)
	}
	if deps.SourceControl == nil {
		return Assessment{State: StateUnconfirmed, Reason: ReasonNoCodeHost}
	}

	logger := platform.Logger(ctx)
	readCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.GitHubGetOpenPRTimeout)
	target, found, err := deps.SourceControl.GetOpenPR(readCtx, pr.Owner, pr.Repo, pr.Number, deps.Token)
	cancel()
	if err != nil {
		logger.Warn("reviewfreshness: live pull request read failed, freshness unconfirmed", "error", err, "owner", pr.Owner, "repo", pr.Repo, "pr_number", pr.Number)
		return Assessment{State: StateUnconfirmed, Reason: ReasonPullRequestUnread}
	}
	// GetOpenPR is a composite of several reads: a deadline that fired
	// partway returns err == nil with whatever the later reads left blank
	// (the auto-merge worker's own H3 guard). Checked for DeadlineExceeded
	// specifically: cancel() above sets Canceled on the ordinary path.
	if errors.Is(readCtx.Err(), context.DeadlineExceeded) {
		logger.Warn("reviewfreshness: live pull request read timed out partway, freshness unconfirmed", "owner", pr.Owner, "repo", pr.Repo, "pr_number", pr.Number)
		return Assessment{State: StateUnconfirmed, Reason: ReasonPullRequestUnread}
	}
	if !found {
		return Assessment{State: StateNotApplicable, Reason: ReasonNoLongerOpen}
	}

	live, failure := ReadLive(ctx, deps.Timeouts, deps.SourceControl, deps.Token, target, record.Context)
	if failure != nil {
		logger.Warn("reviewfreshness: live freshness read failed, freshness unconfirmed", "step", string(failure.Step), "error", failure.Err, "owner", pr.Owner, "repo", pr.Repo, "pr_number", pr.Number)
		return Assessment{State: StateUnconfirmed, Reason: failure.Describe()}
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

// recordedAsLive is the probe's input: the record compared with itself,
// the live side assumed equal to what was recorded and every movement
// assumed forward -- so only what fails on the record alone can fail.
func recordedAsLive(record reviewverdict.Record) autoapproval.FreshnessInput {
	return FreshnessInput(record, LiveFacts{
		HeadSHA:                             record.HeadSHA,
		BaseRef:                             record.Context.BaseRef,
		BaseSHA:                             record.Context.BaseSHA,
		AncestorChain:                       record.Context.AncestorChain,
		BaseAdvancedWithoutRewrite:          true,
		AncestorChainAdvancedWithoutRewrite: true,
	})
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
