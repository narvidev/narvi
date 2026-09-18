// This file (mergedbetween.go) implements ports.SourceControl.
// ListMergedBetween ("release PR review", §15.2): "list PRs
// merged since the last release point, check review + CI-at-merge-SHA +
// reverts for each". For a release PR itself, spec.BaseRef/HeadRef are
// simply that PR's own base/head branches -- exactly what a real
// `git compare BaseRef...HeadRef` names -- so this answers "which
// already-individually-reviewed PRs does this release PR bundle".
//
// # Discovering constituent PRs
//
// GitHub's REST API has no single endpoint that lists "PRs merged in
// this ref range" directly. This adapter derives it from the SAME
// compare API GetPullRequestDiff's content-negotiated diff endpoint
// already targets (GET .../compare/{base}...{head}, here requested with
// GitHub's default JSON shape instead), which returns every commit
// reachable from HeadRef but not BaseRef. Each commit's own message is
// matched against GitHub's two well-known, default merge-commit-message
// shapes:
//
//   - "Merge pull request #N from ..." (the default "Create a merge
//     commit" strategy).
//   - "<title> (#N)" as the first line (the default "Squash and merge"
//     strategy).
//
// KNOWN, ACCEPTED LIMITATION: a PR merged via GitHub's third strategy,
// "Rebase and merge", leaves no reliable back-reference to its own PR
// number in any commit message at all (rebased commits keep their
// ORIGINAL messages verbatim) -- GitHub's own authoritative answer for
// "which PR(s) does this commit belong to" is a dedicated per-commit
// endpoint (GET /commits/{sha}/pulls), which this adapter does NOT call
// per commit here (a release PR can bundle hundreds of commits; calling
// an extra endpoint for every single one, on top of the several already
// made per DISCOVERED PR below, was judged too expensive for what this
// Step needs). A rebase-merged constituent PR is therefore silently
// absent from this manifest today -- a real, honestly-documented gap,
// not a correctness bug this file pretends not to have.
//
// # Per-PR fields, and how each is actually determined
//
// See buildMergedPR's own doc comment for the full per-field sourcing
// (reviews, CI-at-merge-SHA via combined status + check-runs, admin-
// override inference via branch protection, revert detection via one
// batch-wide search query, changed-path prefixes, and the manual-
// conflict-resolution heuristic) -- each documented at its own call site
// below rather than restated here.
//
// # Cost
//
// This is a genuinely expensive port method: one compare call, one
// branch-protection call (cached per unique base branch), one revert-
// search call (once for the whole batch), plus up to six further calls
// PER discovered constituent PR (detail, reviews, combined status,
// check-runs, files, commits). §15.2 itself names this cost's own
// justification: "fully mechanizable... a compliance check, not a code
// review" -- there is no cheaper way to answer these specific, per-PR
// factual questions through GitHub's REST API as it exists today.
// maxConstituentPRs below bounds the worst case, matching this
// codebase's own "bounded from day one" discipline (§21.1) rather than
// leaving an unbounded release PR free to make an unbounded number of
// outbound calls.

package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
)

// maxConstituentPRs bounds how many discovered constituent PRs
// ListMergedBetween ever builds full detail for -- a release PR
// bundling more than this many PRs still returns a (silently truncated)
// manifest over the FIRST maxConstituentPRs discovered, in the compare
// API's own commit order, rather than making an unbounded number of
// outbound calls. Chosen generously for a realistic release cut while
// keeping worst-case cost bounded (this file's own top doc comment: up
// to ~6 further calls per PR).
const maxConstituentPRs = 100

// maxRevertSearchResults bounds how many revert-titled merged PRs
// fetchReverts (below) considers per repo, per ListMergedBetween call --
// GitHub's Search API itself caps a single page at 100 results; this
// adapter never paginates beyond that one page (a repo with more than
// 100 revert-titled PRs total is a real, if unusual, case this file
// accepts as a known limitation rather than an unbounded search).
const maxRevertSearchResults = 100

var (
	// mergeCommitPRNumberRE matches GitHub's default "Create a merge
	// commit" strategy's own first-line message shape.
	mergeCommitPRNumberRE = regexp.MustCompile(`^Merge pull request #(\d+) from `)
	// squashMergePRNumberRE matches GitHub's default "Squash and merge"
	// strategy's own first-line message shape.
	squashMergePRNumberRE = regexp.MustCompile(`\(#(\d+)\)\s*$`)
)

// compareCommitResponse is the subset of one entry in GitHub's real GET
// /repos/{owner}/{repo}/compare/{base}...{head} response's own "commits"
// array this adapter needs (https://docs.github.com/rest/commits/commits#compare-two-commits).
type compareCommitResponse struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// compareResponse is the subset of GitHub's real compare-two-commits
// response shape this adapter needs. TotalCommits is GitHub's own
// reported total commit count for the whole range -- distinct from
// len(Commits), which GitHub caps at 250 regardless of TotalCommits;
// this adapter logs no warning on a mismatch (it has no logger) but
// documents it here as a KNOWN LIMITATION: a range spanning more than
// 250 commits may silently miss constituent PRs whose own merge/squash
// commit fell outside the first 250 returned.
type compareResponse struct {
	TotalCommits int                     `json:"total_commits"`
	Commits      []compareCommitResponse `json:"commits"`
}

// extractCandidatePRNumbers derives every candidate constituent PR
// number from commits, in the SAME order compare returned them
// (chronological, oldest first, per GitHub's own documented behavior) --
// see this file's own top doc comment for the two message shapes
// matched, and the known rebase-merge gap. Deduplicated (a real PR
// number never legitimately repeats in this list, but nothing prevents
// defending against it cheaply).
func extractCandidatePRNumbers(commits []compareCommitResponse) []int {
	seen := make(map[int]bool)
	var numbers []int
	for _, c := range commits {
		firstLine := c.Commit.Message
		if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
			firstLine = firstLine[:idx]
		}

		var numStr string
		if m := mergeCommitPRNumberRE.FindStringSubmatch(firstLine); m != nil {
			numStr = m[1]
		} else if m := squashMergePRNumberRE.FindStringSubmatch(firstLine); m != nil {
			numStr = m[1]
		} else {
			continue
		}

		n, err := strconv.Atoi(numStr)
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		numbers = append(numbers, n)
	}
	return numbers
}

// mergedPRDetailResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/pulls/{number} response shape buildMergedPR
// needs, beyond what pullRequestResponse (adapter.go) already decodes --
// a SEPARATE, narrower struct (rather than extending pullRequestResponse
// itself) since this file's own needs (Merged/MergedAt/MergeCommitSHA)
// are specific to an already-merged PR and irrelevant to every other
// caller of that shared type.
type mergedPRDetailResponse struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// fetchMergedPRDetail fetches number's own detail via GET
// /repos/{owner}/{repo}/pulls/{number}. Returns ok=false (nil error) when
// GitHub reports this PR as NOT actually merged (Merged == false) -- a
// real, expected outcome for a candidate number extractCandidatePRNumbers
// mis-extracted (e.g. a coincidental "(#123)" substring in an unrelated
// commit message) or for a PR later force-pushed/closed without merging;
// never treated as an error, simply excluded from the manifest.
func (a *Adapter) fetchMergedPRDetail(ctx context.Context, owner, repo string, number int, token string) (mergedPRDetailResponse, bool, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return mergedPRDetailResponse{}, false, fmt.Errorf("githubapi: fetch merged pr detail: %w", err)
	}
	var parsed mergedPRDetailResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return mergedPRDetailResponse{}, false, fmt.Errorf("githubapi: decode merged pr detail: %w", err)
	}
	if !parsed.Merged {
		return mergedPRDetailResponse{}, false, nil
	}
	return parsed, true, nil
}

// reviewItemResponse is the subset of one entry in GitHub's real GET
// /repos/{owner}/{repo}/pulls/{number}/reviews response this adapter
// needs (https://docs.github.com/rest/pulls/reviews#list-reviews-for-a-pull-request).
// User identifies WHICH reviewer submitted this review -- needed by
// listopenprs.go's own fetchReviewDecision (
// second round) to reduce GitHub's own append-only review list down to
// each reviewer's LATEST decision; fetchHasApprovingReview immediately
// below has no analogous need for it (a bare "does at least one APPROVED
// review exist anywhere in this PR's history" is that function's own,
// deliberately coarser, already-accepted question) and simply ignores it.
type reviewItemResponse struct {
	State string              `json:"state"`
	User  *simpleUserResponse `json:"user"`
}

// fetchHasApprovingReview reports whether number carries at least one
// review with state APPROVED -- GitHub's own review-state vocabulary
// (APPROVED/CHANGES_REQUESTED/COMMENTED/DISMISSED/PENDING), matched
// exactly as GitHub reports it.
func (a *Adapter) fetchHasApprovingReview(ctx context.Context, owner, repo string, number int, token string) (bool, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return false, fmt.Errorf("githubapi: fetch pr reviews: %w", err)
	}
	var reviews []reviewItemResponse
	if err := json.Unmarshal(body, &reviews); err != nil {
		return false, fmt.Errorf("githubapi: decode pr reviews: %w", err)
	}
	for _, r := range reviews {
		if r.State == "APPROVED" {
			return true, nil
		}
	}
	return false, nil
}

// branchProtectionResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/branches/{branch}/protection response this
// adapter needs -- just WHETHER an approving-review requirement is
// configured at all (https://docs.github.com/rest/branches/branch-protection).
type branchProtectionResponse struct {
	RequiredPullRequestReviews *struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
}

// branchRequiresApprovingReview reports whether branch's own protection
// rules require at least one approving review to merge -- the positive
// fact MergedViaAdminOverride (buildMergedPR below) needs before it will
// EVER assert "admin override": without this confirmation, a PR merging
// with no approving review is equally explained by "this branch simply
// has no review requirement configured", which is not an override of
// anything.
//
// A 404 (GitHub's own documented response for an UNPROTECTED branch --
// https://docs.github.com/rest/branches/branch-protection#get-branch-protection)
// is a definitive, legitimate "no" -- protection genuinely does not
// exist here. Any OTHER failure (403: this token lacks admin rights to
// view protection settings -- GitHub REQUIRES admin/maintain permission
// for this specific endpoint; a transport/timeout error; any other
// non-2xx) is INDETERMINATE, not a "no" -- also returned as false, per
// this function's own doc comment: MergedViaAdminOverride must never be
// asserted without a POSITIVE confirmation, so "could not determine"
// and "confirmed unprotected" collapse to the SAME safe, non-accusatory
// answer here (unlike ports.SourceControl.CheckRepoAccess, which must
// keep these two cases distinguishable for a DIFFERENT, access-control
// purpose -- there is no analogous caller-visible distinction this
// function's own one real caller needs).
func (a *Adapter) branchRequiresApprovingReview(ctx context.Context, owner, repo, branch, token string) bool {
	path := fmt.Sprintf("%s/repos/%s/%s/branches/%s/protection", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return false
	}
	var parsed branchProtectionResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	// Blocking-finding fix #3: RequiredPullRequestReviews != nil ALONE is
	// not enough -- GitHub sets that sub-object whenever "Require a pull
	// request before merging" is enabled, REGARDLESS of whether "Require
	// approvals" is also checked; a repo with the former on and the
	// latter off (a valid, common config) reports
	// RequiredApprovingReviewCount == 0, which means this branch does
	// NOT actually require an approving review at all. Reading only the
	// non-nil check misread that config as "requires review", falsely
	// flagging every PR merged there as an admin override -- exactly the
	// POSITIVE confirmation this function's own doc comment says
	// MergedViaAdminOverride must never be asserted without.
	return parsed.RequiredPullRequestReviews != nil && parsed.RequiredPullRequestReviews.RequiredApprovingReviewCount > 0
}

// combinedStatusResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/commits/{ref}/status response this adapter needs
// (https://docs.github.com/rest/commits/statuses#get-the-combined-status-for-a-specific-reference)
// -- GitHub's own LEGACY Status API surface (statuses created via POST
// .../statuses), state is one of "success"/"pending"/"failure"/"error".
// TotalCount is GitHub's own documented count of individual statuses this
// combined result rolls up: GitHub's
// own docs for this exact endpoint state the rule verbatim -- "pending if
// there are no statuses or a context is pending" -- so state=="pending"
// ALONE can never be read as "a status is genuinely still in flight": a
// repo with ZERO legacy commit statuses ever posted (e.g. one whose CI
// runs exclusively through GitHub Actions check-runs -- the SEPARATE
// surface checkRunsResponse below reports, and the dominant modern CI
// configuration) reports this SAME state=="pending" forever, for a
// completely different reason. See fetchCIConclusionLive's own doc
// comment (listopenprs.go) for where this distinction is load-bearing;
// fetchCIConclusion (this file, below) never inspects state=="pending" at
// all, so it has no analogous need for this field.
type combinedStatusResponse struct {
	State      string `json:"state"`
	TotalCount int    `json:"total_count"`
}

// checkRunsResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/commits/{ref}/check-runs response this adapter
// needs (https://docs.github.com/rest/checks/runs#list-check-runs-for-a-git-reference)
// -- GitHub's own NEWER Checks API surface (what GitHub Actions and most
// modern CI integrations report through); conclusion is nil while a
// check is still in progress.
//
// TotalCount is GitHub's own documented count of ALL check runs for this
// ref, independent of pagination -- CheckRuns carries only whatever page
// was actually served. Modeled (F1) because, before this field
// existed, a truncated first page was structurally indistinguishable from
// a complete one: with no per_page sent, GitHub's documented default page
// size is 30, so a ref carrying more than 30 check runs served only the
// first 30, silently, with no error and no decode failure -- exactly the
// "confident green from a composite that was only ever half read" defect
// fetchCIConclusionLive (listopenprs.go) already closed for a failed GET,
// but had not yet closed for a truncated-but-successful one. See that
// function's own use of this field for the live, fail-closed consumer;
// fetchCIConclusion below is the retrospective sibling and its own
// deliberate non-use of this field is addressed at that function's own
// doc comment.
//
// Name is the check run's own "name" field, decoded (previously absent)
// so both readers below can exclude reviewcheck.CheckName -- fourth
// review round, HIGH, reproduced in both directions against the real
// code: this publisher's own narvi/review check run sits at the exact
// SHA either function reads, so with no exclusion its own
// action_required (a review that timed out with no verdict) read as a
// genuine CI failure, its own success read as a genuine CI success (a
// repository with no CI at all became "green" purely on the strength of
// Narvi's own check -- the merge gate satisfied by Narvi grading its own
// homework), and its own queued/running (Conclusion == nil) read as an
// incomplete required check, making every in-flight review make its own
// PR read as CI-not-green. output.go's own doc comment states the
// intent this defeated: this check "expresses PROCESS completion, never
// risk" -- yet unfiltered, it fed straight into ciGreen's eligibility
// computation. Filtered by NAME alone, not by writer App id or
// external_id (both considered and rejected, this package's own two
// callers' doc comments have the full "why" for each).
type checkRunsResponse struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		Name       string  `json:"name"`
		Conclusion *string `json:"conclusion"`
	} `json:"check_runs"`
}

// ciFailureConclusions is the set of GitHub Checks API "conclusion"
// values this adapter treats as a genuine failure signal -- "neutral"
// and "success" are NOT failure signals; "skipped" is also not (a check
// that opted out of running says nothing about whether the code is
// broken).
var ciFailureConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"action_required": true,
}

// fetchCIConclusion determines a constituent PR's own CI conclusion AT
// mergeSHA -- THE COMMIT THAT ACTUALLY MERGED, never the PR's current/
// latest head (§15.2's own central requirement: "a force-push after
// approval can hide a run that was red when it actually merged"). Reads
// BOTH of GitHub's two independent CI surfaces -- the legacy combined-
// Status API and the newer Checks API -- since a repo may report through
// either or both depending on how its CI is configured (a repo using
// ONLY GitHub Actions reports EXCLUSIVELY through check-runs; the
// combined-status endpoint alone would silently show no signal at all
// for such a repo). Any confirmed failure signal from EITHER surface
// wins; otherwise any confirmed success signal from either wins;
// otherwise (no signal from either, or either call itself failed)
// review.CIConclusionUnknown -- see review.CIConclusionUnknown's own doc
// comment (internal/domain/review/manifestcheck.go) for why this is
// deliberately NOT treated as a failure.
//
// # §15.2 RETROSPECTIVE AUDIT ONLY -- never a live pre-merge gate
//
// This function's own LENIENCY is only correct for its one real caller
// below (buildMergedPR, auditing an ALREADY-MERGED PR at its own fixed,
// historical merge SHA): by the time anything is being audited, every
// check run that will ever report for that commit already has -- an
// in-progress/queued run simply cannot exist anymore, so silently
// skipping a nil Conclusion (`if r.Conclusion == nil { continue }` below)
// never actually discards a live, still-pending signal, only ever a
// stale placeholder GitHub itself no longer updates. That precondition
// does NOT hold for an OPEN PR's CURRENT head SHA -- there, "some check
// finished green" while a nil-Conclusion run is silently skipped can
// mean "several required checks are still queued", not "the suite
// passed". listopenprs.go's own fetchCIConclusionLive is the STRICT
// sibling built specifically for that live case (an incomplete or
// cancelled run there means NOT green, full stop) -- it is a SEPARATE
// function, not a parameter/flag on this one, precisely so this
// function's own behavior for the §15.2 audit path can never be
// accidentally tightened (or fetchCIConclusionLive's own live-gate
// strictness accidentally loosened) by a future edit that assumes one
// implementation can serve both callers. Do not reuse this function for
// any live/pre-merge purpose; do not loosen fetchCIConclusionLive to
// match this one's own leniency.
//
// F5: this function used to gate each GET's
// contribution on a bare `if err == nil`, mirroring fetchCIConclusionLive's
// own PRE-fix shape one file over -- a failed call contributed NOTHING,
// so a status GET confirming "success" beside a check-runs GET that
// itself failed (or a decoded check-runs page that was a confirmed
// truncated prefix, F1) fell through to CIConclusionSuccess: a confident
// green from a composite that was only ever half read, reported both to
// ComputeReleaseManifestFindings (which could then silently MISS a real
// ManifestFindingRedAtMerge the unread half would have produced -- a
// consequence this package's own doc comment on that function never
// weighed, since its "Unknown is not an accusation" reasoning is about
// Unknown-vs-Failure, not about Success masking a read that never
// happened) AND rendered directly to a human as this PR's own
// "ciConclusionAtMergeSha" in the release manifest readout
// (releasemanifestreadout.go) -- a specific, confident, false factual
// claim about a PR's own CI history, the exact "failure rendering as a
// confident normal state" shape this codebase has fixed everywhere else
// it was found.
//
// UNLIKE fetchCIConclusionLive, this function needs no second "degraded"
// return value to express the fix: ports.MergedPR.CIConclusionAtMergeSHA
// already has a value this exact situation should report --
// CIConclusionUnknown, this package's own pre-existing "no signal could
// be determined" state, already treated as non-accusatory by every
// consumer (ComputeReleaseManifestFindings' own doc comment, immediately
// above this function's own callers). No new field, no new port surface:
// a genuine, CONFIRMED failure from the GET that DID succeed still wins
// (mirrors fetchCIConclusionLive's own identical "failure wins over
// incomplete/degraded" precedent), but sawSuccess alone, with the other
// GET's own health unknown, is no longer sufficient to report Success --
// it now reports the SAME Unknown a fully-successful read reports when
// neither GET found any signal at all, which is exactly the honest
// answer: "could not be determined", never "confirmed green".
//
// per_page=100 (F1's own fix, applied here too -- the identical
// undocumented-default-page-size gap existed on this GET as much as on
// fetchCIConclusionLive's) and the TotalCount truncation guard mirror
// that function's own identical fix one file over -- see checkRunsResponse's
// own doc comment for what TotalCount answers and why it is checked
// regardless of the per_page bump.
//
// # Narvi's own check run is excluded from this read, by NAME
//
// This §15.2 retrospective audit and fetchCIConclusionLive's own live
// gate (listopenprs.go) are Narvi's only two readers of a ref's check
// runs, and both used to read the ENTIRE set with no exclusion for
// reviewcheck.CheckName -- so the publisher's own narvi/review check run,
// sitting at the exact SHA either function reads, fed straight back into
// the CI conclusion it is itself computed alongside. Three options were
// weighed for how to exclude it:
//
//   - By NAME (chosen). reviewcheck.CheckName is a fixed constant this
//     publisher alone writes (its own doc comment: "the one piece of this
//     identity a human actually recognizes"), needs no per-call lookup,
//     and is unconditionally correct for what this exclusion is FOR: any
//     check run named narvi/review is Narvi's own process signal, never a
//     CI result, regardless of which App wrote it or which pull request
//     it was created for. A same-named run from a genuinely different
//     App is not a real-world case this codebase has ever observed, and
//     even if one existed, excluding it from CI-green computation is the
//     SAFE direction (an extra Unknown, never a false Success/Failure) --
//     the same posture every other ambiguous-signal case in this file
//     already takes.
//   - By writer App id (rejected). The "select by SHA and GitHub App"
//     identity rule governs adoption on the WRITE side
//     (resolveOrCreateCheckRun, internal/app/outboxworker/reviewcheck.go)
//     specifically to avoid adopting a DIFFERENT app's same-named check
//     run -- a concern that does not apply to excluding a run from THIS
//     read, where the goal is "never let narvi/review count as CI",
//     full stop, regardless of who wrote it. Threading this
//     deployment's own observed writer App id in here would also add a
//     dependency this file does not otherwise have (that value is
//     process-local state owned by outboxworker's own notifier, not
//     configuration or a value either read path already has to hand) to
//     a GET called on every open-PR list and every release-PR audit --
//     disproportionate cost for a filter NAME already settles correctly.
//   - By external_id (rejected). reviewcheck.PRExternalID scopes a check
//     run to ONE pull request -- correct for the write side's own
//     adoption predicate (a check run is scoped to a commit, not a PR;
//     external_id is what tells two PRs sharing one head SHA apart), but
//     wrong for this exclusion: a narvi/review run created for a
//     DIFFERENT pull request sharing this exact head SHA would carry a
//     non-matching external_id and would NOT be excluded by that filter
//     alone, leaving the exact hazard this fix exists to close open for
//     that PR. The hazard here is head-SHA-scoped, not PR-scoped, and
//     NAME is what covers it regardless of which PR's external_id the
//     run carries.
//
// The Actions-only regression this project has fixed twice before (see
// checkRunsResponse's own doc comment and TestListOpenPRsForUser_
// PendingCombinedStatusRequiresARealStatus, listopenprs_test.go) is
// unaffected: a real CI check run's own Name is never
// reviewcheck.CheckName, so this exclusion never touches it.
func (a *Adapter) fetchCIConclusion(ctx context.Context, owner, repo, mergeSHA, token string) ports.CIConclusion {
	sawFailure := false
	sawSuccess := false
	degraded := false

	statusPath := fmt.Sprintf("%s/repos/%s/%s/commits/%s/status", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(mergeSHA))
	if body, err := a.doGet(ctx, statusPath, token); err != nil {
		degraded = true
	} else {
		var status combinedStatusResponse
		if json.Unmarshal(body, &status) != nil {
			degraded = true
		} else {
			switch status.State {
			case "failure", "error":
				sawFailure = true
			case "success":
				sawSuccess = true
			}
		}
	}

	checksPath := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(mergeSHA))
	if body, err := a.doGet(ctx, checksPath, token); err != nil {
		degraded = true
	} else {
		var runs checkRunsResponse
		if json.Unmarshal(body, &runs) != nil {
			degraded = true
		} else {
			if runs.TotalCount > len(runs.CheckRuns) {
				// F1: a genuine, successfully-decoded response that is
				// still only a PREFIX of the real total -- see
				// fetchCIConclusionLive's own identical guard
				// (listopenprs.go) for the full "why".
				degraded = true
			}
			for _, r := range runs.CheckRuns {
				if r.Name == reviewcheck.CheckName {
					// Narvi's own check run -- checkRunsResponse's own
					// doc comment has the full "why": this publisher's
					// own narvi/review run must never contribute to the
					// CI conclusion it is itself read alongside, no
					// matter what it concluded (or didn't).
					continue
				}
				if r.Conclusion == nil {
					continue
				}
				if ciFailureConclusions[*r.Conclusion] {
					sawFailure = true
				} else if *r.Conclusion == "success" || *r.Conclusion == "neutral" {
					sawSuccess = true
				}
			}
		}
	}

	switch {
	case sawFailure:
		// A genuine, confirmed failure from whichever GET succeeded is
		// real signal regardless of the other GET's own health --
		// mirrors fetchCIConclusionLive's own identical precedent.
		return ports.CIConclusionFailure
	case degraded:
		// F5: sawSuccess may well be true here (the surviving GET
		// reported green), but with the other GET unread (or a decoded
		// check-runs page confirmed truncated), that green is
		// unconfirmed -- report Unknown, this package's own pre-existing
		// "could not be determined" value, never a confident Success.
		return ports.CIConclusionUnknown
	case sawSuccess:
		return ports.CIConclusionSuccess
	default:
		return ports.CIConclusionUnknown
	}
}

// pullFileResponse is the subset of one entry in GitHub's real GET
// /repos/{owner}/{repo}/pulls/{number}/files response this adapter needs
// (https://docs.github.com/rest/pulls/pulls#list-pull-requests-files).
type pullFileResponse struct {
	Filename string `json:"filename"`
}

// fetchChangedPathPrefixes fetches number's own changed files (first
// page, per_page=100 -- see maxConstituentPRs' own sibling bound;
// unlike that constant this is not separately named, since a single
// PR's own file count beyond 100 is rare enough that a bounded first
// page is an acceptable, honestly-scoped approximation for §15.3's own
// "same subsystem" signal, which does not need exhaustive coverage to be
// useful) and reduces each to its own top-level path prefix
// (changedPathPrefix below). Deduplicated.
func (a *Adapter) fetchChangedPathPrefixes(ctx context.Context, owner, repo string, number int, token string) ([]string, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return nil, fmt.Errorf("githubapi: fetch pr files: %w", err)
	}
	var files []pullFileResponse
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("githubapi: decode pr files: %w", err)
	}

	seen := make(map[string]bool, len(files))
	var prefixes []string
	for _, f := range files {
		prefix := changedPathPrefix(f.Filename)
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// changedPathPrefix reduces filename to its own top-level path prefix --
// the first two "/"-delimited segments when at least two exist (e.g.
// "internal/domain/review/verdict.go" -> "internal/domain"), or the
// single segment/whole filename otherwise (e.g. "README.md" ->
// "README.md", "docs/x.md" -> "docs/x.md" since it has only two
// segments total... wait: "docs/x.md" splits into ["docs","x.md"], two
// segments, both taken -> "docs/x.md"). Depth 2 mirrors this codebase's
// own repo-layout granularity (§1: "internal/domain", "internal/app",
// "internal/adapters/inbound", ...) more usefully than depth 1 alone
// (which would collapse everything under "internal" into one prefix,
// far too coarse to mean "same subsystem").
func changedPathPrefix(filename string) string {
	if filename == "" {
		return ""
	}
	parts := strings.SplitN(filename, "/", 3)
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return parts[0] + "/" + parts[1]
	}
}

// pullCommitResponse is the subset of one entry in GitHub's real GET
// /repos/{owner}/{repo}/pulls/{number}/commits response this adapter
// needs (https://docs.github.com/rest/commits/commits#list-commits) --
// just each commit's own parent count.
type pullCommitResponse struct {
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// fetchHadManualConflictResolution infers whether landing number
// required manually resolving a conflict against its base -- §15.3's own
// third OR-condition. GitHub's REST API has NO direct "this merge
// required manual conflict resolution" flag; this is a HEURISTIC, not a
// confirmed fact, and documented as such: a merge commit appearing
// WITHIN a PR's own commit history (a commit with 2+ parents, listed by
// GET .../pulls/{number}/commits, first page, per_page=100) means a
// contributor or GitHub's own "Resolve conflicts" web UI merged the base
// branch into the PR's branch at some point -- the standard way a
// conflict against a moving base gets resolved without a full rebase.
// KNOWN IMPRECISION: a merge commit can also appear from a routine
// "merge main into my branch" that never actually conflicted, so this
// can over-report; conversely, a conflict resolved via `git rebase`
// (which produces no merge commit at all) is invisible to this check, so
// it can also under-report. Accepted as the best available mechanical
// signal per this file's own top doc comment on cost -- a true, provably
// correct signal is not obtainable through GitHub's REST API here.
func (a *Adapter) fetchHadManualConflictResolution(ctx context.Context, owner, repo string, number int, token string) (bool, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/commits?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return false, fmt.Errorf("githubapi: fetch pr commits: %w", err)
	}
	var commits []pullCommitResponse
	if err := json.Unmarshal(body, &commits); err != nil {
		return false, fmt.Errorf("githubapi: decode pr commits: %w", err)
	}
	for _, c := range commits {
		if len(c.Parents) >= 2 {
			return true, nil
		}
	}
	return false, nil
}

// searchIssueItemResponse is the subset of one entry in GitHub's real GET
// /search/issues response's own "items" array this adapter needs
// (https://docs.github.com/rest/search/search#search-issues-and-pull-requests).
// Number is this revert candidate's OWN pull request number -- needed to
// fetch ITS OWN review state (fetchHasApprovingReview), a second call
// this adapter only ever makes for an item that already matched the
// revert-title convention (extractRevertedTitle) or the revert-body
// reference (extractRevertedPRNumber), never for the whole search page.
// Body is the revert PR's own full description -- GitHub's Search API
// returns the SAME full issue/PR resource shape a direct GET would (not
// a stripped-down search-only shape), so this needs no second call; see
// extractRevertedPRNumber's own doc comment for what this adapter looks
// for in it.
type searchIssueItemResponse struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	ClosedAt *string `json:"closed_at"`
}

// searchIssuesResponse is the subset of GitHub's real search-issues
// response shape this adapter needs.
type searchIssuesResponse struct {
	Items []searchIssueItemResponse `json:"items"`
}

// revertInfo is what fetchReverts (below) reports finding for one revert
// candidate.
type revertInfo struct {
	revertedAt  time.Time
	reviewState ports.RevertReviewState
}

// revertsQualifiedRE/revertsBareRE match GitHub's own auto-generated
// "Reverts owner/repo#N" cross-reference, written verbatim into a revert
// PR's own body by both GitHub's web UI "Revert pull request" button flow
// and the `gh pr revert`-equivalent flow -- the SAME positive identity
// link GitHub's own UI renders as a "Reverts owner/repo#N" timeline
// cross-reference for the ORIGINAL PR, distinct from an ordinary
// "Fixes #N"/"Closes #N" issue-closing keyword. revertsQualifiedRE
// matches the fully-qualified form GitHub always writes
// ("Reverts acme/widgets#142"); revertsBareRE is a defensive fallback for
// a same-repo-only short form ("Reverts #142"), in case some GitHub
// surface/version ever emits that instead -- both anchored to the start
// of a line (multiline mode), since this is always GitHub's own
// auto-generated line, never buried mid-sentence in the rest of a
// hand-written body.
var (
	revertsQualifiedRE = regexp.MustCompile(`(?im)^Reverts\s+([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#(\d+)`)
	revertsBareRE      = regexp.MustCompile(`(?im)^Reverts\s+#(\d+)`)
)

// extractRevertedPRNumber extracts the ORIGINAL PR number a revert PR's
// own body positively links back to (blocking-finding fix #2's PRIMARY
// mechanism -- see this file's own top doc comment / fetchReverts' own
// doc comment for the false-positive this closes). ok=false when body
// carries no such reference at all (an older revert PR that predates
// this convention, one hand-authored through some other tool, or a body
// a human has since edited to remove it -- fetchReverts falls back to
// the WEAKER title-only match for exactly this case), or when a
// fully-qualified reference names a DIFFERENT repo than owner/repo
// (defensive: this function's one caller only ever wants a revert of
// THIS repo's own PR #N, never a cross-repo reference some unrelated
// body happens to contain).
func extractRevertedPRNumber(owner, repo, body string) (int, bool) {
	if m := revertsQualifiedRE.FindStringSubmatch(body); m != nil {
		if !strings.EqualFold(m[1], owner) || !strings.EqualFold(m[2], repo) {
			return 0, false
		}
		if n, err := strconv.Atoi(m[3]); err == nil && n > 0 {
			return n, true
		}
		return 0, false
	}
	if m := revertsBareRE.FindStringSubmatch(body); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// fetchReverts runs ONE Search API query for the whole batch --
// "repo:{owner}/{repo} type:pr is:merged in:title Revert" -- rather than
// one query per constituent PR, deliberately: GitHub's Search API is
// rate-limited far more aggressively than the rest of the REST API (30
// requests/minute, authenticated, vs. 5000/hour for ordinary endpoints
// -- https://docs.github.com/rest/search/search#rate-limit), and a
// release PR can easily bundle more constituent PRs than that per-minute
// budget would allow if this were one search per PR.
//
// Blocking-finding fix #2: returns TWO lookup maps, byNumber and byTitle.
// byNumber is the STRONG, unambiguous identity link -- keyed on the
// ORIGINAL PR number parsed from the revert PR's own body
// (extractRevertedPRNumber) -- and is what buildMergedPR (below) always
// prefers: a real PR number can never collide across two unrelated PRs,
// so a byNumber match is trusted on its own. byTitle is a WEAKER
// fallback, keyed on the extracted original TITLE alone (extractRevertedTitle,
// matched against GitHub's own default `Revert "<original title>"` title
// convention), consulted ONLY when no byNumber match exists (an older
// revert PR, or one whose body was hand-edited/authored by some other
// tool that never wrote the "Reverts owner/repo#N" reference at all) --
// title text alone proves nothing about WHICH PR it reverted (a repeated
// Conventional-Commits-style title like "chore: bump dependencies" can
// legitimately recur across many unrelated PRs over a repo's history), so
// buildMergedPR additionally requires byTitle's own match to postdate the
// candidate PR's own merge (revert.revertedAt.After(mergedAt)) before
// ever trusting it -- the cheap, immediate correctness guard this fix's
// own write-up names, closing the exact false-positive this file used to
// produce (an unrelated, much older same-titled revert silently clamped
// into a nonsensical "reverted before it merged" claim).
//
// Every item considered here still requires a title match
// (extractRevertedTitle) before this function spends the extra
// fetchHasApprovingReview call on it -- GitHub's own title convention
// remains the cheap, near-universal "is this even a revert PR at all"
// gate, regardless of whether its body also carries the number
// reference; this keeps the call count identical to before this fix.
func (a *Adapter) fetchReverts(ctx context.Context, owner, repo, token string) (byNumber map[int]revertInfo, byTitle map[string]revertInfo, err error) {
	query := fmt.Sprintf("repo:%s/%s type:pr is:merged in:title Revert", owner, repo)
	path := fmt.Sprintf("%s/search/issues?q=%s&per_page=%d", a.apiBaseURL, url.QueryEscape(query), maxRevertSearchResults)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return nil, nil, fmt.Errorf("githubapi: search revert prs: %w", err)
	}
	var parsed searchIssuesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("githubapi: decode revert search results: %w", err)
	}

	byNumber = make(map[int]revertInfo)
	byTitle = make(map[string]revertInfo)
	for _, item := range parsed.Items {
		originalTitle, titleMatched := extractRevertedTitle(item.Title)
		if !titleMatched {
			continue
		}
		originalNumber, numberMatched := extractRevertedPRNumber(owner, repo, item.Body)

		var closedAt time.Time
		if item.ClosedAt != nil {
			if t, err := time.Parse(time.RFC3339, *item.ClosedAt); err == nil {
				closedAt = t
			}
		}

		// Blocking-finding fix #4: the revert PR's own review state is a
		// genuine tri-state, never a plain bool defaulting to "not
		// reviewed" on a fetch failure -- see ports.RevertReviewState's
		// own doc comment. A failed fetchHasApprovingReview call here
		// must NEVER manufacture RevertReviewStateNotReviewed (the ONE
		// value that ever triggers review.ManifestFindingUnreviewedRevert):
		// "was this PR reverted at all" (the PRIMARY fact) is already
		// confirmed by the title/number match above; only the SECONDARY
		// "was the revert itself reviewed" fact degrades to Unknown on a
		// sub-fetch failure, mirroring buildMergedPR's own identical
		// CI/files/commits degrade-one-field discipline rather than
		// excluding the whole PR or asserting an unconfirmed fact.
		reviewState := ports.RevertReviewStateUnknown
		if reviewed, err := a.fetchHasApprovingReview(ctx, owner, repo, item.Number, token); err == nil {
			if reviewed {
				reviewState = ports.RevertReviewStateReviewed
			} else {
				reviewState = ports.RevertReviewStateNotReviewed
			}
		}
		info := revertInfo{revertedAt: closedAt, reviewState: reviewState}

		if numberMatched {
			byNumber[originalNumber] = info
		}
		byTitle[originalTitle] = info
	}
	return byNumber, byTitle, nil
}

// revertTitlePrefix and revertTitleSuffix bracket GitHub's own default
// revert-PR title shape: `Revert "<original title>"`.
const (
	revertTitlePrefix = `Revert "`
	revertTitleSuffix = `"`
)

// extractRevertedTitle extracts the original PR title from a revert
// PR's own title, per GitHub's own default `Revert "<original title>"`
// convention -- ok=false when title does not match that shape at all.
func extractRevertedTitle(title string) (string, bool) {
	if !strings.HasPrefix(title, revertTitlePrefix) || !strings.HasSuffix(title, revertTitleSuffix) {
		return "", false
	}
	inner := title[len(revertTitlePrefix) : len(title)-len(revertTitleSuffix)]
	if inner == "" {
		return "", false
	}
	return inner, true
}

// buildMergedPR assembles one ports.MergedPR for prNumber. requiredReviewsByBase
// is a per-call cache keyed on base branch name -- branchRequiresApprovingReview
// is fetched at most ONCE per distinct base branch across the whole
// ListMergedBetween call, never once per PR (every constituent PR of a
// real release almost always shares the SAME base branch).
// revertsByNumber/revertsByTitle are fetchReverts' own batch-wide result
// -- see that function's own doc comment for how a match is looked up
// (blocking-finding fix #2: byNumber preferred and trusted outright,
// byTitle a weaker fallback additionally gated on
// revert.revertedAt.After(mergedAt)).
//
// Three-way return, mirroring GetPullRequestDiff's own (content,
// truncated, err) shape: ok=true is a normal, successful inclusion.
// ok=false, fetchFailed=false is a CONFIRMED, correct exclusion --
// GitHub itself reports prNumber as not actually merged
// (fetchMergedPRDetail's own doc comment) -- never a coverage gap.
// ok=false, fetchFailed=true is blocking-finding fix #5's own signal: a
// genuine sub-fetch ERROR (this PR's own detail or review-state call
// failed) silently dropped a PR this manifest SHOULD have included --
// ListMergedBetween folds this into its own truncated return so a
// caller rendering "no issues found" never claims completeness it does
// not have. Only these two calls (detail, own review state) are central
// enough to what this check audits to exclude the PR outright on
// failure; a failure fetching CI/files/commits instead degrades that ONE
// field to its own honest "nothing confirmed" value (CIConclusionUnknown
// / no path prefixes / HadManualConflictResolution=false) without
// excluding the PR OR affecting truncated -- none of those three
// defaults assert a false fact the way guessing HasApprovingReview would,
// and a caller already knows CIConclusionUnknown is never treated as a
// finding on its own (review.CIConclusionUnknown's own doc comment).
func (a *Adapter) buildMergedPR(ctx context.Context, owner, repo string, prNumber int, token string, requiredReviewsByBase map[string]bool, revertsByNumber map[int]revertInfo, revertsByTitle map[string]revertInfo) (pr ports.MergedPR, ok bool, fetchFailed bool) {
	detail, wasMerged, err := a.fetchMergedPRDetail(ctx, owner, repo, prNumber, token)
	if err != nil {
		return ports.MergedPR{}, false, true
	}
	if !wasMerged {
		return ports.MergedPR{}, false, false
	}

	hasApproval, err := a.fetchHasApprovingReview(ctx, owner, repo, prNumber, token)
	if err != nil {
		return ports.MergedPR{}, false, true
	}

	adminOverride := false
	if !hasApproval {
		requiresReview, cached := requiredReviewsByBase[detail.Base.Ref]
		if !cached {
			requiresReview = a.branchRequiresApprovingReview(ctx, owner, repo, detail.Base.Ref, token)
			requiredReviewsByBase[detail.Base.Ref] = requiresReview
		}
		adminOverride = requiresReview
	}

	var mergedAt time.Time
	if detail.MergedAt != nil {
		if t, err := time.Parse(time.RFC3339, *detail.MergedAt); err == nil {
			mergedAt = t
		}
	}

	ciConclusion := ports.CIConclusionUnknown
	if detail.MergeCommitSHA != "" {
		ciConclusion = a.fetchCIConclusion(ctx, owner, repo, detail.MergeCommitSHA, token)
	}

	prefixes, err := a.fetchChangedPathPrefixes(ctx, owner, repo, prNumber, token)
	if err != nil {
		prefixes = nil
	}

	manualConflict, err := a.fetchHadManualConflictResolution(ctx, owner, repo, prNumber, token)
	if err != nil {
		manualConflict = false
	}

	labels := make([]string, len(detail.Labels))
	for i, l := range detail.Labels {
		labels[i] = l.Name
	}

	result := ports.MergedPR{
		Number:                      detail.Number,
		Title:                       detail.Title,
		HasApprovingReview:          hasApproval,
		MergedViaAdminOverride:      adminOverride,
		CIConclusionAtMergeSHA:      ciConclusion,
		MergedAt:                    mergedAt,
		HadManualConflictResolution: manualConflict,
		ChangedPathPrefixes:         prefixes,
		Labels:                      labels,
	}

	// Blocking-finding fix #2: byNumber (the STRONG, positively-linked
	// identity match -- see fetchReverts' own doc comment) is always
	// preferred and, once matched, trusted outright: a real PR number
	// cannot collide across unrelated PRs the way a repeated title can.
	// byTitle is only ever consulted as a fallback, and even then ONLY
	// when its own revertedAt genuinely postdates this PR's own mergedAt
	// -- the cheap, immediate correctness guard this fix's own write-up
	// names, closing the false-positive a same-titled-but-unrelated,
	// possibly much OLDER revert used to produce (a nonsensical negative
	// time delta that then got silently clamped to zero, destroying the
	// one signal that would have exposed the bad match). A zero/unparsed
	// mergedAt or revertedAt can never satisfy an After() comparison
	// against a real timestamp, so a fallback match degrades to "no
	// match" rather than an unguarded acceptance whenever either
	// timestamp could not be determined.
	revert, found := revertsByNumber[prNumber]
	if !found {
		var titleMatch revertInfo
		if titleMatch, found = revertsByTitle[detail.Title]; found {
			found = !mergedAt.IsZero() && !titleMatch.revertedAt.IsZero() && titleMatch.revertedAt.After(mergedAt)
		}
		revert = titleMatch
	}
	if found {
		result.WasReverted = true
		if !revert.revertedAt.IsZero() {
			revertedAt := revert.revertedAt
			result.RevertedAt = &revertedAt
		}
		// The revert PR's own review state was already resolved inside
		// fetchReverts itself (a second call, made only for a title that
		// already matched the revert convention) -- see that function's
		// own doc comment, and ports.RevertReviewState's own doc comment
		// for why this is a tri-state (blocking-finding fix #4).
		result.RevertReviewState = revert.reviewState
	}

	return result, true, false
}

// ListMergedBetween implements ports.SourceControl ("release PR
// review", §15.2) -- see this file's own top doc comment for the full
// design (constituent-PR discovery, per-field sourcing, known
// limitations, and cost).
//
// Blocking-finding fix #5: truncated (the second return, mirroring
// GetPullRequestDiff's own identical shape) is set whenever this adapter
// KNOWS the returned merged slice is an incomplete picture of everything
// actually merged in this range, from any of three independent sources:
// (1) more candidate PR numbers were discovered than maxConstituentPRs
// bounds this call to build full detail for; (2) GitHub's own compare API
// caps len(compare.Commits) at 250 regardless of the range's real size --
// compare.TotalCommits > len(compare.Commits) means commits (and
// therefore possibly whole constituent PRs) beyond that cap were never
// even considered; (3) buildMergedPR's own fetchFailed return -- a
// genuine sub-fetch error silently dropped an individual PR from the
// manifest (see that function's own doc comment for exactly which
// sub-fetches count). A confirmed "not actually merged" exclusion
// (buildMergedPR's ok=false, fetchFailed=false) is never one of these
// sources -- that is correct, complete filtering, not a gap.
func (a *Adapter) ListMergedBetween(ctx context.Context, spec ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	comparePath := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s",
		a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo),
		url.PathEscape(spec.BaseRef), url.PathEscape(spec.HeadRef))
	body, err := a.doGet(ctx, comparePath, spec.Token)
	if err != nil {
		return nil, false, fmt.Errorf("githubapi: list merged between: compare: %w", err)
	}
	var compare compareResponse
	if err := json.Unmarshal(body, &compare); err != nil {
		return nil, false, fmt.Errorf("githubapi: list merged between: decode compare response: %w", err)
	}

	truncated := compare.TotalCommits > len(compare.Commits)

	candidates := extractCandidatePRNumbers(compare.Commits)
	if len(candidates) > maxConstituentPRs {
		candidates = candidates[:maxConstituentPRs]
		truncated = true
	}

	revertsByNumber, revertsByTitle, err := a.fetchReverts(ctx, spec.Owner, spec.Repo, spec.Token)
	if err != nil {
		// Best-effort: a failed revert search never fails this whole
		// call -- every PR simply reports WasReverted=false, a known,
		// accepted limitation (this file's own top doc comment).
		// Deliberately NOT folded into truncated: it degrades one signal
		// (WasReverted) on already-included PRs, never drops a PR from
		// the manifest outright -- see this function's own doc comment
		// for the three sources truncated actually tracks.
		revertsByNumber = map[int]revertInfo{}
		revertsByTitle = map[string]revertInfo{}
	}

	requiredReviewsByBase := make(map[string]bool)
	merged := make([]ports.MergedPR, 0, len(candidates))
	for _, number := range candidates {
		pr, ok, fetchFailed := a.buildMergedPR(ctx, spec.Owner, spec.Repo, number, spec.Token, requiredReviewsByBase, revertsByNumber, revertsByTitle)
		if fetchFailed {
			truncated = true
		}
		if !ok {
			continue
		}
		merged = append(merged, pr)
	}
	return merged, truncated, nil
}
