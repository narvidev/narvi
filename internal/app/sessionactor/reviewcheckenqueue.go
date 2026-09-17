// This file (reviewcheckenqueue.go) implements the review's own
// GitHub-native result surface (§8.2/§21.1/§21.1b)
// PhaseRunning/PhaseTerminalNotAssessed enqueues -- two call sites,
// dispatch.go's own tryPlanDispatch (right after a github-origin turn
// transitions Pending -> Dispatched -> Processing) and outboxenqueue.go's
// own github branch (a github-origin turn reaching ANY terminal state).
// Both share enqueueReviewCheck below, mirroring outboxenqueue.go's own
// established shape: a small, focused function this package's own
// machinery calls, never a business-logic decision inlined at either
// call site.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// enqueueReviewCheck enqueues exactly one
// ports.NotificationKindGitHubReviewCheck row for turn, carrying phase --
// the shared body both enqueueReviewCheckRunning and
// enqueueReviewCheckNotAssessed below call. Skipped, logged, never a
// hard failure, when turn carries no review_head_sha yet (its own
// context-fetch degraded, mirroring httpapi.PostReviewVerdict's own
// identical "no head sha, no row" precedent) or when this session's own
// github_pr_sessions claim row is unexpectedly missing (should be
// unreachable -- every github-origin session is created BY that same
// claim path -- but defended rather than assumed, mirroring
// outboxenqueue.go's own identical defensive convention for the
// Slack/Linear reverse lookups).
func (a *Actor) enqueueReviewCheck(ctx context.Context, tx pgx.Tx, turn sqlcgen.Turn, phase reviewcheck.Phase, logPrefix string) error {
	if turn.ReviewHeadSha == nil || *turn.ReviewHeadSha == "" {
		a.logger.Warn("sessionactor: "+logPrefix+": turn has no review head sha on record, skipping", "turn_id", turn.ID.String())
		return nil
	}

	prSession, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.logger.Warn("sessionactor: " + logPrefix + ": github-origin session has no github_pr_sessions row; skipping")
			return nil
		}
		return fmt.Errorf("sessionactor: %s: get github pr session: %w", logPrefix, err)
	}

	owner, repo, ok := reposource.SplitFullName(prSession.RepoFullName)
	if !ok {
		a.logger.Warn("sessionactor: "+logPrefix+": repo_full_name not in owner/repo shape, skipping", "repo_full_name", prSession.RepoFullName)
		return nil
	}

	var verdictContext reviewverdict.Context
	if len(turn.ReviewVerdictContext) > 0 {
		if unmarshalErr := json.Unmarshal(turn.ReviewVerdictContext, &verdictContext); unmarshalErr != nil {
			a.logger.Warn("sessionactor: "+logPrefix+": unmarshal review_verdict_context failed, publishing without base/policy context", "error", unmarshalErr, "turn_id", turn.ID.String())
			verdictContext = reviewverdict.Context{}
		}
	}

	payload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repo, PRNumber: int(prSession.PrNumber), HeadSHA: *turn.ReviewHeadSha,
		AttemptID:        turn.ID.String(),
		AttemptCreatedAt: turn.CreatedAt.Time,
		Phase:            string(phase),
		BaseRef:          verdictContext.BaseRef,
		BaseSHA:          verdictContext.BaseSHA,
		PolicyVersion:    verdictContext.PolicyVersion,
	})
	if err != nil {
		return fmt.Errorf("sessionactor: %s: marshal payload: %w", logPrefix, err)
	}

	if _, err := a.stores.outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: a.sessionID,
		Kind:      string(ports.NotificationKindGitHubReviewCheck),
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("sessionactor: %s: create outbox entry: %w", logPrefix, err)
	}
	return nil
}

// enqueueReviewCheckRunning is dispatch.go's own tryPlanDispatch call
// site -- called ONLY when a.sessionID's own SpawnSource is
// sqlcgen.SessionSpawnSourceGithub (that function's own caller-side
// gate), right after turn transitions Pending -> Dispatched -> Processing
// in that SAME transaction.
func (a *Actor) enqueueReviewCheckRunning(ctx context.Context, tx pgx.Tx, turn sqlcgen.Turn) error {
	return a.enqueueReviewCheck(ctx, tx, turn, reviewcheck.PhaseRunning, "enqueue review-check running")
}

// enqueueReviewCheckNotAssessed is outboxenqueue.go's own github-branch
// call site -- called for EVERY github-origin turn reaching a terminal
// state (complete/fail/cancel/timeout), regardless of trig: "a review
// that did not complete" (decision 1) is a fact about whether the
// verdict-posting tool was ever called for THIS attempt, not about which
// trigger ended the turn. Refuses to enqueue when a real verdict was
// already posted for processing.ID (reviewVerdict.ExistsForAttempt,
// scoped to THIS attempt specifically, never the PR as a whole) -- that
// case already got its own reviewcheck.PhaseTerminalAssessed emission,
// at the one place a verdict is actually posted
// (httpapi.PostReviewVerdict, same transaction as that verdict's own
// review_verdicts insert), and publishing NotAssessed on top of it here
// would be racing an accurate result with an inaccurate one for the
// exact same attempt.
func (a *Actor) enqueueReviewCheckNotAssessed(ctx context.Context, tx pgx.Tx, processing sqlcgen.Turn) error {
	assessed, err := a.stores.reviewVerdict.WithTx(tx).ExistsForAttempt(ctx, processing.ID)
	if err != nil {
		return fmt.Errorf("sessionactor: enqueue review-check not-assessed: check existing verdict: %w", err)
	}
	if assessed {
		return nil
	}
	return a.enqueueReviewCheck(ctx, tx, processing, reviewcheck.PhaseTerminalNotAssessed, "enqueue review-check not-assessed")
}
