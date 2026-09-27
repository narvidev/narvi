// Package reviewfreshness reads the live facts a review verdict's recorded
// context is compared against (§21.1's amendment, §21.1b), in ONE place:
// the pull request's current head, base ref and base commit, its ancestor
// chain, and whether each moved only forward since the verdict. The merge
// path (internal/app/decisioninbox's revalidateCore, behind both the
// human-clicked Merge and the auto-merge worker) and a session's result
// (row 182, technical plan §43.20) both call ReadLive, and both compare
// through autoapproval.CheckFreshness -- so no consumer builds a context of
// its own, and the two can never disagree about whether a verdict is still
// about the code as it stands ("no consumer re-derives a context of its
// own", §21.1b).
//
// Every live call is bounded by the platform.Timeouts constants the merge
// path has always used (DecisionInboxResolveBranchSHATimeout,
// DecisionInboxIsAncestorTimeout); no literal duration lives here (§5.4).
package reviewfreshness

import (
	"context"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// LiveFacts is the pull request's live side of the freshness comparison --
// autoapproval.FreshnessInput's Current* fields and its two fast-forward
// confirmations, as ReadLive established them.
type LiveFacts struct {
	// HeadSHA and BaseRef are the already-read pull request's own
	// (ports.OpenPR.HeadSHA/BaseRef), read by whichever live call produced
	// it -- trusted as read, exactly as the merge path always has.
	HeadSHA string
	BaseRef string
	// BaseSHA is the base branch's LIVE tip, resolved by name -- never
	// ports.OpenPR.BaseSHA, GitHub's own per-PR CACHED `base.sha` snapshot,
	// verified against real pull requests to lag the branch's real tip by
	// an unknown, sometimes month-scale margin (finding F1, §21.1's
	// amendment). This is the SAME ResolveBranchSHA a verdict's own
	// context was anchored to (internal/app/reviewcontext.Fetch), so a
	// genuine advance of the base branch between the verdict and now
	// surfaces as a real SHA mismatch -- the "whose parent moved beneath
	// it" hazard base_ref alone could never see.
	BaseSHA string
	// AncestorChain is the pull request's current stacked ancestry: nil
	// when it reports no link at all (a CONFIRMED fact -- not stacked, or
	// at its own bottom), else its one link (§17.6 bounds the chain to at
	// most one today) with the link's ref as read and its SHA resolved
	// LIVE -- never ports.OpenPR.AncestorChain's own cached SHA, the same
	// shape finding F1 proved stale-by-design for the base (round-10
	// finding B).
	AncestorChain []review.AncestorLink
	// BaseAdvancedWithoutRewrite and AncestorChainAdvancedWithoutRewrite
	// are true only when a live IsAncestor check positively confirmed the
	// recorded commit is an ancestor of (or identical to) the live one --
	// see autoapproval.EligibilityInput's fields of the same names for
	// what that confirmation establishes and the residual it carries.
	// False whenever the check did not apply (nothing moved, or a ref
	// changed, which refuses regardless).
	BaseAdvancedWithoutRewrite          bool
	AncestorChainAdvancedWithoutRewrite bool
}

// Step names the live call a Failure came from, so a caller can say which
// fact could not be established.
type Step string

// The live calls ReadLive makes, in order.
const (
	// StepResolveBase: resolving the base branch's live tip failed.
	StepResolveBase Step = "resolve_base"
	// StepBaseAncestry: the base commit moved under an unchanged ref, and
	// checking whether it moved only forward failed.
	StepBaseAncestry Step = "base_ancestry"
	// StepAncestorRefUnreadable: the pull request reports an ancestor link
	// whose ref could not be read at all (a degraded stack read -- the
	// adapter's position > 1 with no decodable base ref, round-12 D1):
	// there is no ref to resolve.
	StepAncestorRefUnreadable Step = "ancestor_ref_unreadable"
	// StepResolveAncestor: resolving the ancestor link's live tip failed,
	// or answered an empty commit.
	StepResolveAncestor Step = "resolve_ancestor"
	// StepAncestorAncestry: the ancestor link's commit moved under an
	// unchanged ref, and checking whether it moved only forward failed.
	StepAncestorAncestry Step = "ancestor_ancestry"
)

// Failure is a live fact ReadLive could not establish: the step, and the
// error the call returned (nil for StepAncestorRefUnreadable, and for a
// StepResolveAncestor that answered no error and an empty commit).
type Failure struct {
	Step Step
	Err  error
}

// ReadLive establishes the live facts for target, the pull request as
// the caller just read it, against recorded, the verdict's own recorded
// context: the base branch's live tip; when the base ref is unchanged and
// both commits are known and differ, whether the recorded one is an
// ancestor of the live one; the ancestor link's live tip, when the pull
// request reports a link; and when that link's ref is unchanged and both
// commits are known and differ, whether it moved only forward. The two
// ancestry checks run only where they could change the comparison's
// answer, so a pull request whose base did not move costs one live call.
//
// It stops at the first call that fails and reports it; it never degrades
// a failed read into a value (an empty base commit, a nil chain) that the
// comparison would read as "confirmed": a nil chain means "not stacked", a
// fact, and an unresolved link must never be mistaken for it (round-11
// finding A1). What a caller does with a Failure is the caller's: the
// merge path refuses with a reason naming the check that failed, and a
// session's result reports the verdict's freshness as unconfirmed.
//
// token is the credential the caller read target with. Each call is
// bounded: resolutions by DecisionInboxResolveBranchSHATimeout, ancestry
// checks by DecisionInboxIsAncestorTimeout.
func ReadLive(ctx context.Context, timeouts platform.Timeouts, sourceControl ports.SourceControl, token string, target ports.OpenPR, recorded reviewverdict.Context) (LiveFacts, *Failure) {
	live := LiveFacts{HeadSHA: target.HeadSHA, BaseRef: target.BaseRef}

	resolveCtx, cancel := context.WithTimeout(ctx, timeouts.DecisionInboxResolveBranchSHATimeout)
	baseSHA, _, err := sourceControl.ResolveBranchSHA(resolveCtx, ports.ResolveBranchSHASpec{
		Owner:  target.Owner,
		Repo:   target.Repo,
		Branch: target.BaseRef,
		Token:  token,
	})
	cancel()
	if err != nil {
		return LiveFacts{}, &Failure{Step: StepResolveBase, Err: err}
	}
	live.BaseSHA = baseSHA

	// Only attempted when it could matter: the base ref is unchanged (a
	// retarget refuses regardless, autoapproval.CheckFreshness) and the
	// base commit genuinely differs on both sides (an empty side already
	// fails as unknown, regardless of this confirmation).
	if recorded.BaseRef == target.BaseRef && recorded.BaseSHA != "" && baseSHA != "" && recorded.BaseSHA != baseSHA {
		ancestorCtx, cancel := context.WithTimeout(ctx, timeouts.DecisionInboxIsAncestorTimeout)
		confirmed, err := sourceControl.IsAncestor(ancestorCtx, ports.IsAncestorSpec{
			Owner:      target.Owner,
			Repo:       target.Repo,
			Ancestor:   recorded.BaseSHA,
			Descendant: baseSHA,
			Token:      token,
		})
		cancel()
		if err != nil {
			return LiveFacts{}, &Failure{Step: StepBaseAncestry, Err: err}
		}
		live.BaseAdvancedWithoutRewrite = confirmed
	}

	// A link reported with no ref at all is neither "no link" (nil) nor a
	// link this can resolve (round-12 sweep D1): not established.
	if len(target.AncestorChain) > 0 && target.AncestorChain[0].Ref == "" {
		return LiveFacts{}, &Failure{Step: StepAncestorRefUnreadable}
	}
	if len(target.AncestorChain) > 0 {
		ancestorSHACtx, cancel := context.WithTimeout(ctx, timeouts.DecisionInboxResolveBranchSHATimeout)
		liveAncestorSHA, _, err := sourceControl.ResolveBranchSHA(ancestorSHACtx, ports.ResolveBranchSHASpec{
			Owner:  target.Owner,
			Repo:   target.Repo,
			Branch: target.AncestorChain[0].Ref,
			Token:  token,
		})
		cancel()
		// An empty commit with no error is refused too: the production
		// adapter never answers it (ports.SourceControl's own doc comment),
		// and a defensive symmetric check costs nothing.
		if err != nil || liveAncestorSHA == "" {
			return LiveFacts{}, &Failure{Step: StepResolveAncestor, Err: err}
		}
		live.AncestorChain = []review.AncestorLink{{Ref: target.AncestorChain[0].Ref, SHA: liveAncestorSHA}}
	}

	// Only attempted when it could matter: both sides report a link, its
	// ref is unchanged (a restructure refuses regardless), and its commit
	// genuinely differs on both sides.
	if len(recorded.AncestorChain) > 0 && len(live.AncestorChain) > 0 &&
		recorded.AncestorChain[0].Ref == live.AncestorChain[0].Ref &&
		recorded.AncestorChain[0].SHA != "" && live.AncestorChain[0].SHA != "" &&
		recorded.AncestorChain[0].SHA != live.AncestorChain[0].SHA {
		chainAncestorCtx, cancel := context.WithTimeout(ctx, timeouts.DecisionInboxIsAncestorTimeout)
		confirmed, err := sourceControl.IsAncestor(chainAncestorCtx, ports.IsAncestorSpec{
			Owner:      target.Owner,
			Repo:       target.Repo,
			Ancestor:   recorded.AncestorChain[0].SHA,
			Descendant: live.AncestorChain[0].SHA,
			Token:      token,
		})
		cancel()
		if err != nil {
			return LiveFacts{}, &Failure{Step: StepAncestorAncestry, Err: err}
		}
		live.AncestorChainAdvancedWithoutRewrite = confirmed
	}

	return live, nil
}
