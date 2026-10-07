// This file (headresolve.go) closes the H5 audit finding this batch
// (fix/audit-github-pr-payload-correctness) fixes: a PR @mention arriving
// via GitHub's "issue_comment" webhook event (the PR "Conversation" tab,
// the most common way the bot gets mentioned) created the review session
// with HeadBranch left nil, meaning the session cloned the BASE repo's own
// DEFAULT branch -- never the PR's actual head branch -- even for a
// same-repo PR. Only "pull_request_review_comment" (a comment on a
// specific diff line) ever carried the real head branch, since that
// event's own payload embeds it directly (payload.go's own
// pullRequestReviewCommentPayload).
//
// resolveIssueCommentHead below closes that gap with one authenticated
// GitHub REST API call (githubapi.Adapter.GetPullRequest, GET
// /repos/{owner}/{repo}/pulls/{number}), resolving the PR's TRUE head
// branch, head commit and head repository (which may be a fork) BEFORE
// handler.go turns the mention into the session's own repo spec --
// mirroring exactly what pull_request_review_comment's own payload already
// carries for free. The session clones the base repository whatever the
// head repository is (mention.RepoName's doc comment); the head
// repository only says whether the head branch is in it.

package github

import (
	"context"
	"log/slog"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
)

// PullRequestResolver is the narrow slice of githubapi.Adapter's own real,
// authenticated GitHub REST API surface (GetPullRequest) this package
// needs -- a small, locally-defined interface (not one of
// internal/app/ports' general-purpose abstractions: this need is specific
// to this one ingress adapter resolving ITS OWN mention shape, not a
// general "source control" operation another adapter would ever implement
// independently, so it does not belong in internal/app/ports) so tests can
// inject a fake with no real HTTP round trip. githubapi.Adapter satisfies
// this exactly, with no adapter-side change needed.
type PullRequestResolver interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error)
}

// resolveIssueCommentHead resolves m's TRUE head branch/repo via
// resolver.GetPullRequest (H5 audit fix) -- called by handler.go ONLY for
// an issue_comment-sourced mention (the one event type whose own payload
// never embeds head.ref/head.repo directly; see issueCommentPayload's own
// doc comment). pull_request_review_comment's own mention already carries
// the real head branch/repo straight out of parsePullRequestReviewComment,
// so handler.go never calls this for that event type at all.
//
// Three distinct outcomes, all logged, none of them fatal to this mention:
//
//   - resolver == nil (no PullRequestResolver wired at all -- e.g. this
//     package's own handler_test.go, which never populates
//     Config.PullRequests): returns m unchanged, no log line (this is
//     expected, deliberate test/minimal wiring, not a failure).
//   - m.RepoFullName does not actually split into "<owner>/<repo>", or the
//     GetPullRequest call itself fails (network error, non-2xx, a
//     cancelled/expired ctx), or GitHub reports an empty head ref: logged
//     as a warning, then returns m EXACTLY as parseIssueComment already
//     produced it -- today's PRE-fix fallback (HeadBranch nil, base
//     repo's own default branch). Mirrors createPRBestEffort's own
//     log-and-continue convention for a failed outbound GitHub API call
//     (internal/app/sessionactor/pushpr.go) rather than dropping the
//     mention/failing the whole webhook delivery over a single
//     best-effort lookup.
//   - A successful call: m.HeadBranch is set to the PR's real head ref,
//     and m.HeadRepoFullName to its head repository's full name (a fork's
//     for a pull request from a fork; empty when GitHub's own head.repo
//     was null, the head repository deleted, mirroring L15's handling of
//     pull_request_review_comment's own sibling nullable field).
//     m.RepoName/RepoCloneURL are never changed: they stay the base
//     repository parseIssueComment set, the one every review session
//     clones (technical plan §21.1, §30.4). m.Stack is
//     ALSO set here ("review sessions", §17.6's amendment) when
//     GitHub reports one -- the exact "incremental addition to a call this
//     ingress already makes for every issue_comment mention, not a new
//     outbound call" §17.6 itself describes: this is the ONE call this
//     package makes for an issue_comment mention regardless, so capturing
//     stack context here means internal/app/reviewcontext.Fetch's own
//     knownStack short-circuit applies for THIS trigger path too, exactly
//     like the label-retrigger lane's own inline webhook payload already
//     gets for free -- no second, redundant GetPullRequest call for stack
//     alone.
func resolveIssueCommentHead(ctx context.Context, logger *slog.Logger, resolver PullRequestResolver, botToken string, m mention) mention {
	if resolver == nil {
		return m
	}

	owner, repo, ok := reposource.SplitFullName(m.RepoFullName)
	if !ok {
		logger.Warn("github: could not split repo_full_name into owner/repo, falling back to base repo's own default branch",
			"repo_full_name", m.RepoFullName, "pr_number", m.PRNumber)
		return m
	}

	pr, err := resolver.GetPullRequest(ctx, owner, repo, m.PRNumber, botToken)
	if err != nil {
		logger.Warn("github: resolve pull request head via GitHub API failed, falling back to base repo's own default branch",
			"error", err, "repo", m.RepoFullName, "pr_number", m.PRNumber)
		return m
	}
	if pr.HeadRef == "" {
		logger.Warn("github: GitHub API reported an empty head ref, falling back to base repo's own default branch",
			"repo", m.RepoFullName, "pr_number", m.PRNumber)
		return m
	}

	headBranch := pr.HeadRef
	m.HeadBranch = &headBranch
	// (§21.1): the SAME already-in-flight GetPullRequest call
	// also carries the PR's current head SHA -- captured here for free,
	// mirroring m.Stack's own identical "one call already made for head-
	// branch resolution, no second call just for this" reasoning
	// immediately above.
	if pr.HeadSHA != "" {
		headSHA := pr.HeadSHA
		m.HeadSHA = &headSHA
	}
	// The PR's head repository (may be a fork; identical to what
	// pull_request_review_comment's own payload carries directly for the
	// sibling event type) -- empty when GitHub's own head.repo was null.
	// It never replaces the repository the session clones.
	m.HeadRepoFullName = pr.HeadRepoFullName

	if pr.Stack != nil {
		m.Stack = &review.StackContext{
			Position:        pr.Stack.Position,
			Size:            pr.Stack.Size,
			UltimateBaseRef: pr.Stack.BaseRef,
			UltimateBaseSHA: pr.Stack.BaseSHA,
		}
	}

	return m
}
