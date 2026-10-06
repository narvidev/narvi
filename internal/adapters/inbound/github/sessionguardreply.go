// This file (sessionguardreply.go) is the GitHub mention lane's answer to
// a refusal of the session guard (technical plan §40.1): a pull request's
// review session that has spent its cap takes no new turn, a mention or a
// label re-trigger on it included, since the cap applies to every turn
// regardless of who asked. httpapi.CreateTurnForBot wraps the refusal --
// the *sessionguard.Refusal the 409 CreateTurnError carries as its sentinel
// -- with %w, so handler.go's error switch recognizes it
// (sessionguard.AsRefusal) as a deterministic state, not a transient
// failure: it acknowledges 200 WITHOUT releasing the delivery claim, since
// a redelivery would only meet the same refusal until an administrator
// raises the cap, and posts the refusal's own text on the pull request,
// as planawaitingreply.go does for a plan awaiting approval.

package github

import (
	"context"
	"log/slog"

	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// postSessionGuardReply posts refusal's text (sessionguard.Text) on
// repoFullName's pull request prNumber -- best effort, like
// postPlanAwaitingReply: no poster wired, or a failed post, is only logged.
func postSessionGuardReply(ctx context.Context, logger *slog.Logger, poster CommentPoster, botToken, repoFullName string, prNumber int32, refusal *sessionguard.Refusal) {
	if poster == nil || refusal == nil {
		return
	}
	owner, repo, ok := reposource.SplitFullName(repoFullName)
	if !ok {
		logger.Warn("github: could not split repo_full_name into owner/repo, skipping the session guard's reply",
			"repo_full_name", repoFullName, "pr_number", prNumber)
		return
	}
	if err := poster.PostIssueComment(ctx, owner, repo, int(prNumber), botToken, refusal.Error()); err != nil {
		logger.Warn("github: post the session guard's reply failed", "error", err, "repo", repoFullName, "pr_number", prNumber)
	}
}
