// This file (releasemanifest.go) wires §15's own ("release PR
// review", §15) release-PR detection (§15.1) and manifest-check
// enqueueing (§15.2) into this package's own webhook handler --
// handler.go's own top-level call site, right after CreateOrJoin
// succeeds, mirrors this Step's own scoping decision: ONLY the WINNER
// (brand-new session) path ever triggers this, never the REUSE path --
// see internal/app/releasereview's own top doc comment for the full
// "why".
//
// Blocking-finding fix #1: this function used to call releasereview.Run
// directly, inline, on the bare webhook request context -- Run's own
// real work (ListMergedBetween) can take up to platform.Timeouts.
// GitHubListMergedBetweenTimeout (2 minutes, ~80+ sequential GitHub API
// calls for a large release cut), far longer than GitHub's own ~10s
// webhook delivery timeout, so that work silently died mid-flight,
// permanently, whenever a webhook delivery timed out. This function now
// calls releasereview.Enqueue instead -- a single, fast, durable INSERT
// -- and returns; internal/app/releasereview.Worker (a SEPARATE
// background loop, started alongside every other background loop in
// cmd/control-plane/main.go's own errgroup) claims that row and runs the
// actual check later, entirely decoupled from this request's own
// context/lifetime. See migrations/000050_release_manifest_pending.up.sql's
// own doc comment for the full "why".

package github

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/app/releasereview"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// triggerReleaseManifestCheckBestEffort detects whether repoFullName#
// prNumber is a release PR (§15.1) and, if so, enqueues the manifest
// check (§15.2, releasereview.Enqueue) for a SEPARATE background worker
// to actually run -- entirely best-effort: every failure here is logged
// and this function simply returns, exactly like this file's own sibling
// handoff-readiness/contract-drift best-effort helpers elsewhere in this
// codebase (internal/app/sessionactor/handoffsentinel.go's own identical
// "never block a spawn/push" discipline, applied here to "never block a
// webhook ack"). Deliberately fast throughout (one GetPullRequest call,
// bounded by Timeouts.GitHubGetPRTimeout, plus one Enqueue call, a single
// INSERT) -- see this file's own top doc comment for why the actual
// GitHub-API-heavy check work no longer runs anywhere on this call path.
//
// cfg.PendingChecks being nil (this package's own handler_test.go, or
// any other minimal wiring that doesn't care about this Step) simply
// skips this entirely -- mirrors cfg.DiffFetcher's own identical nil-safe
// precedent.
//
// Detection re-fetches the PR's own CURRENT head/base branch and labels
// via ONE GetPullRequest call, rather than reusing whatever headBranch
// this mention's own payload happened to carry: labels are never present
// on any of this package's own parsed mention payloads today (payload.go
// parses only what §17.6's own already-shipped concerns needed), and
// a fresh call here -- paid only on the WINNER path, i.e. once per
// brand-new review session, never once per ordinary mention -- is a
// small, bounded cost for a session-creation-time-only signal.
func triggerReleaseManifestCheckBestEffort(
	ctx context.Context,
	logger *slog.Logger,
	cfg Config,
	repoFullName string,
	prNumber int32,
	sessionID pgtype.UUID,
) {
	if cfg.PendingChecks == nil {
		return
	}

	owner, repo, ok := reposource.SplitFullName(repoFullName)
	if !ok {
		logger.Warn("github: release manifest: could not split repo_full_name into owner/repo, skipping",
			"repo_full_name", repoFullName, "pr_number", prNumber)
		return
	}

	prCtx, cancel := context.WithTimeout(ctx, cfg.Timeouts.GitHubGetPRTimeout)
	pr, err := cfg.PullRequests.GetPullRequest(prCtx, owner, repo, prNumber, cfg.Outbound.BotToken())
	cancel()
	if err != nil {
		logger.Warn("github: release manifest: fetch pull request (for release detection) failed, skipping",
			"error", err, "repo", repoFullName, "pr_number", prNumber)
		return
	}

	if !intentdomain.DetectRelease(pr.HeadRef, pr.BaseRef, pr.Labels, cfg.ReleaseBranchPattern, cfg.ReleaseLabel) {
		return
	}

	logger.Info("github: release PR detected, enqueueing manifest check",
		"repo", repoFullName, "pr_number", prNumber, "head", pr.HeadRef, "base", pr.BaseRef)

	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}

	releasereview.Enqueue(ctx, logger, cfg.PendingChecks, releasereview.Input{
		SessionID:     sessionID,
		Owner:         owner,
		Repo:          repo,
		PRNumber:      prNumber,
		BaseRef:       pr.BaseRef,
		HeadRef:       pr.HeadRef,
		Token:         cfg.Outbound.BotToken(),
		CorrelationID: correlationID,
	})
}
