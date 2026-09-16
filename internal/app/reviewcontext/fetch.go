package reviewcontext

import (
	"context"
	"log/slog"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// Fetcher is the narrow slice of *githubapi.Adapter's own real,
// authenticated GitHub REST API surface Fetch needs -- a small, locally-
// defined interface (mirroring internal/adapters/inbound/github's own
// PullRequestResolver precedent, headresolve.go) so a unit test can inject
// a fake with no real HTTP round trip. *githubapi.Adapter satisfies this
// directly, with no adapter-side change beyond what this Step already adds
// to it.
//
// GetCompareDiff replaces the
// PREVIOUS GetPullRequestDiff here -- see Fetch's own doc comment for the
// full "why": GetPullRequestDiff always reflects a PR's CURRENT, moving
// head, which is exactly the property that let Diff and HeadSHA
// (independently fetched, before this fix) disagree about which commit
// either one actually reflected. GetPullRequestDiff itself is UNCHANGED
// and still exists on *githubapi.Adapter -- it simply has no caller
// through this narrower interface anymore.
// ResolveBranchSHA replays ports.SourceControl.ResolveBranchSHA's own
// exact signature (finding F1 (§21.1's amendment)) -- *githubapi.Adapter already
// implements this method (adapter.go, built for §8.5's image builds), so
// this interface reuses it as-is rather than inventing a second "resolve
// a branch ref to a commit" mechanism. See Fetch's own doc comment for
// why this call exists: GitHub's own `pull_request.base.sha` field is a
// per-PR CACHED snapshot of the base branch's tip -- verified against
// real GitHub PRs to lag the branch's actual current commit by an
// unknown, sometimes month-scale margin, refreshed on GitHub's own
// schedule, never on push. GetPullRequest's own response (adapter.go's
// pullRequestResponse) deliberately never decodes it at all (D11,
// internal/adapters/outbound/githubapi/adapter.go's own
// pullRequestResponse.Base doc comment has the full "why removed, not
// merely undecoded"). ResolveBranchSHA instead issues a real,
// synchronous GET .../commits/{branch}, so its result is the base
// branch's LIVE tip at the moment this review turn's context is
// assembled -- the one value BaseSHA below is pinned to.
type Fetcher interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error)
	GetCompareDiff(ctx context.Context, owner, repo, base, head, token string) (diff string, truncated bool, err error)
	ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (sha string, resolvedBranch string, err error)
}

// Fetch builds review.PreFetchedContext for owner/repo#number -- the ONE
// shared assembly point every review-session trigger path (a PR @mention,
// a label retrigger, or the manual re-review REST button, §8.2)
// calls before building its own review turn's prompt via
// review.RenderTurnPrompt.
//
// # Diff and HeadSHA now come
// # from one dependency chain, never two independently-raceable reads
//
// BEFORE this fix, Diff (via GetPullRequestDiff, always reflecting a PR's
// CURRENT, moving head) and HeadSHA (via a SEPARATE GetPullRequest call,
// sometimes skipped when already known) were two independent network
// reads with no ordering guarantee between them -- a commit landing on
// the PR in the gap between the two could make the returned HeadSHA name
// a DIFFERENT commit than the one Diff's own content actually reflects,
// silently defeating §21.2's stale-verdict guard once that HeadSHA is
// later persisted as review_verdicts.head_sha and compared against the
// PR's real current head (which, after this exact race, would already
// equal the WRONG, too-new SHA this function reported).
//
// The fix: resolve pr.HeadSHA (and pr.BaseRef, and Stack) via ONE
// GetPullRequest call FIRST, unconditionally, then fetch the diff via
// GetCompareDiff PINNED to that exact (pr.BaseRef, pr.HeadSHA) pair (the
// compare API, unlike the PR-resource diff endpoint, never re-resolves
// "head" to whatever is current at request time -- see GetCompareDiff's
// own doc comment, githubapi/adapter.go). Diff, when successfully
// fetched, is therefore GUARANTEED -- by construction of the call itself,
// not by two reads happening to agree -- to be the diff AT pr.HeadSHA:
// the SAME value this function returns as HeadSHA and the SAME value its
// caller goes on to persist. There is no longer a "two independent
// sources" question to ask.
//
// This deliberately gives up the PREVIOUS optimization of skipping the
// GetPullRequest call entirely when a caller's own webhook payload
// already supplied stack/head-sha inline (the label-retrigger path,
// historically this function's only such caller) -- correctness now
// requires this call unconditionally, since pr.BaseRef (needed to pin
// the compare call) was never available from that shortcut anyway, and a
// SHA sourced from an earlier webhook delivery is no longer trustworthy
// as "the commit the diff fetched moments later actually reflects" once
// Diff itself must be provably anchored to whatever SHA this function
// reports. One extra GitHub API call on that one ingress path is an
// acceptable, bounded cost for closing a hazard that could otherwise let
// an unattended worker merge code nobody's verdict actually examined.
//
// knownStack (still accepted) is used AS-IS when supplied, in preference
// to re-deriving it from this call's own pr.Stack -- the label-retrigger
// webhook path already has GitHub's own stack object inline in its OWN
// payload (a native pull_request event, §17.6), and preferring data a
// caller already handed over is this function's own established
// convention; both sources report the identical fact for the same PR
// either way, so this is a preference, never a correctness difference.
func Fetch(ctx context.Context, logger *slog.Logger, fetcher Fetcher, timeouts platform.Timeouts, owner, repo string, number int32, token string, knownStack *review.StackContext) review.PreFetchedContext {
	prCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubGetPRTimeout)
	pr, err := fetcher.GetPullRequest(prCtx, owner, repo, number, token)
	cancel()
	if err != nil {
		logger.Warn("reviewcontext: fetch pull request (for head sha/base ref, needed to pin the diff fetch) failed, review turn will carry no pre-fetched diff and no head sha to record",
			"error", err, "owner", owner, "repo", repo, "pr_number", number)
		// No confirmed CURRENT head: nothing safe to pin a diff fetch to,
		// and nothing safe to persist as review_verdicts.head_sha either
		// -- Diff/HeadSHA both stay at their own honest zero value,
		// mirroring this function's own pre-existing "a failed fetch
		// degrades gracefully, never fails the review turn's own
		// creation" precedent. knownStack, if the caller already had it
		// from its own webhook payload, is still worth keeping -- it cost
		// this call nothing and remains genuine, valid context.
		return review.PreFetchedContext{Stack: knownStack}
	}

	stack := knownStack
	if stack == nil && pr.Stack != nil {
		stack = &review.StackContext{
			Position:        pr.Stack.Position,
			Size:            pr.Stack.Size,
			UltimateBaseRef: pr.Stack.BaseRef,
			UltimateBaseSHA: pr.Stack.BaseSHA,
		}
	}
	// pr.Stack == nil, knownStack == nil: an ordinary, non-stacked PR --
	// stack stays nil.

	// Finding F1: GitHub's own per-PR "base.sha" field is a CACHED
	// snapshot of the base branch's tip -- verified against real GitHub
	// PRs to lag the branch's actual current commit by an unknown,
	// sometimes month-scale margin, refreshed on GitHub's own schedule
	// rather than on every push to the base branch. pr (githubapi.
	// PullRequest, above) never carries it at all (D11: removed, not
	// merely undecoded -- see Fetcher's own doc comment above for the
	// full "why"). Comparing a cached snapshot against itself later would
	// detect nothing: the SHA half of the freshness gate would pass
	// whether or not the base branch had actually moved -- exactly the
	// "or whose parent moved beneath it" hazard §21.1 names as the whole
	// reason this field was added, and the one half base_ref alone can
	// never see.
	//
	// The fix mirrors HeadSHA's own discipline one section up: resolve the
	// base branch's LIVE tip via ONE real call (ResolveBranchSHA, already
	// shipped for §8.5's image builds -- reused as-is, never a second
	// "resolve a ref" mechanism), then pin the diff fetch to THAT exact
	// commit, never to the branch name alone (which GitHub would otherwise
	// re-resolve to whatever is current at request time, one more
	// independently-raceable read). baseSHA is therefore the ONE value
	// both the diff and the persisted base_sha are anchored to -- an
	// anchor by construction, not two reads that merely tend to agree.
	//
	// A resolution failure degrades exactly like a GetPullRequest/
	// GetCompareDiff failure elsewhere in this function: logged, baseSHA
	// stays "", and the diff fetch below falls back to pinning on
	// pr.BaseRef (GitHub re-resolves the ref itself) rather than failing
	// the whole turn -- never a reason to refuse creating the review turn.
	// An empty persisted BaseSHA is not silently read as a match later:
	// autoapproval.ComputeEligible's own empty-base-sha guard (finding F2)
	// fails closed on either side being unresolved, rather than treating
	// "" == "" as fresh.
	baseCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubResolveBaseBranchSHATimeout)
	baseSHA, _, baseErr := fetcher.ResolveBranchSHA(baseCtx, ports.ResolveBranchSHASpec{Owner: owner, Repo: repo, Branch: pr.BaseRef, Token: token})
	cancel()
	if baseErr != nil {
		logger.Warn("reviewcontext: resolve base branch's live tip failed, review turn's persisted base sha will be empty (context reads as unknown, never a stale match) and the diff fetch will pin to the base ref name instead of a resolved commit",
			"error", baseErr, "owner", owner, "repo", repo, "pr_number", number, "base_ref", pr.BaseRef)
		baseSHA = ""
	}

	diffBase := pr.BaseRef
	if baseSHA != "" {
		diffBase = baseSHA
	}
	diffCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubPRDiffTimeout)
	diff, truncated, err := fetcher.GetCompareDiff(diffCtx, owner, repo, diffBase, pr.HeadSHA, token)
	cancel()
	if err != nil {
		logger.Warn("reviewcontext: fetch compare diff failed, review turn will carry no pre-fetched diff",
			"error", err, "owner", owner, "repo", repo, "pr_number", number, "head_sha", pr.HeadSHA)
		diff, truncated = "", false
	}

	// HeadSHA is reported here regardless of whether the diff fetch above
	// itself succeeded -- pr.HeadSHA is still an honest fact (the PR's
	// real head at THIS moment) even on a diff-fetch failure, mirroring
	// this function's own pre-existing behavior (HeadSHA and Diff have
	// always degraded independently on their own respective failure,
	// never coupled into a single all-or-nothing outcome) -- only now,
	// whenever Diff IS non-empty, it is provably anchored to this exact
	// value (this function's own top doc comment).
	//
	// Title/Body (adversarial-review fix, §26.2's own follow-up,
	// review.PreFetchedContext.Title's own doc comment): forwarded verbatim
	// from the SAME GetPullRequest call HeadSHA itself came from, above --
	// no separate fetch. Reported even when the diff fetch below failed,
	// exactly like HeadSHA, since pr itself was already successfully
	// resolved by this point regardless of what happens to the diff.
	//
	// Additions/Deletions/ChangedFilesCount/Labels (§26.3) are
	// likewise forwarded verbatim from the SAME GetPullRequest call --
	// reported even when the diff fetch below failed, exactly like Title/
	// Body. ChangedPaths is parsed from diff itself (reviewtriage.
	// ExtractChangedPaths), so it is empty exactly when diff is (a failed
	// or never-attempted diff fetch) -- reviewtriage's own fail-open-to-
	// light posture makes that degradation safe (this file's own doc
	// comment on review.PreFetchedContext.Additions).
	return review.PreFetchedContext{
		Diff:          diff,
		DiffTruncated: truncated,
		Stack:         stack,
		HeadSHA:       pr.HeadSHA,
		// BaseRef/AncestorChain/PolicyVersion (§21.1's amendment) are
		// resolved from the SAME GetPullRequest call HeadSHA/Stack
		// themselves already come from -- no separate fetch. AncestorChain
		// is derived from `stack` (already resolved above, preferring
		// knownStack exactly like Stack itself), never re-derived from
		// pr.Stack directly, mirroring `stack`'s own "knownStack takes
		// precedence" convention. PolicyVersion is stamped from this
		// package's own imported autoapproval.CurrentPolicyVersion --
		// internal/domain/review cannot import that package itself
		// (§11: "zero external imports"), so a caller that already can
		// sets it here.
		//
		// BaseSHA (finding F1) is deliberately baseSHA -- the LIVE
		// resolution above -- never GitHub's own possibly-stale
		// `base.sha` snapshot: githubapi.PullRequest carries no such
		// field to read here at all (D11; see the doc comment on the
		// ResolveBranchSHA call above for the full "why").
		BaseRef:           pr.BaseRef,
		BaseSHA:           baseSHA,
		AncestorChain:     review.AncestorChainFromStack(stack),
		PolicyVersion:     autoapproval.CurrentPolicyVersion,
		Title:             pr.Title,
		Body:              pr.Body,
		Additions:         pr.Additions,
		Deletions:         pr.Deletions,
		ChangedFilesCount: pr.ChangedFiles,
		ChangedPaths:      reviewtriage.ExtractChangedPaths(diff),
		Labels:            pr.Labels,
	}
}
