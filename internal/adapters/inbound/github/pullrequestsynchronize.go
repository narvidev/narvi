// This file (pullrequestsynchronize.go) implements §24's ("review:
// automatic re-review on new commits", §24.1) own new GitHub ingress
// lane: `X-GitHub-Event: pull_request` with `action: "synchronize"` --
// GitHub's own name for "new commits landed on this PR's head". This is a
// genuinely new ingress lane, not a small extension of the existing
// mention pipeline (§24.1's own framing): this event carries no comment
// body and no commenting actor at all, so doc.go's own mention-detection
// step (step 5: "does the comment body actually mention Config.BotHandle")
// does not apply here -- routing is by (repo, PR) identity alone, never
// by text.
//
// # What this handler does, and does not, decide
//
// This lane's ONLY job is: (1) confirm a review session already exists
// for this PR (github_pr_sessions has a row with a non-NULL session_id --
// no row, or a NULL session_id, means "no session to re-trigger",
// acknowledged and ignored, exactly like today's "no mention" case for a
// comment event); (2) if one does, upsert
// github_pr_sessions.pending_retrigger_head_sha to this event's own
// pull_request.head.sha and re-arm the review_retrigger_debounce named
// timer (session_timers, §2), atomically, in ONE transaction. It NEVER
// decides whether to actually re-review (that is §24.3's own job, running
// later, in the review session's own actor, once the debounce timer
// fires) and it NEVER checks the per-repo opt-in (§24.5) -- that check
// also happens at debounce-fire time, in the actor, not here; this
// handler arms the SAME timer/pending-head-sha pair for a PR regardless
// of whether the repo has opted in, deliberately keeping this webhook
// handler thin (§24.3 step 1 is what actually gates on it).
//
// # Why a direct, actor-bypassing write
//
// command.go's own Command sum type has exactly three members
// (TimerFired, SandboxEvent, EnsureDispatched) -- none of which
// represents an inbound "new commits pushed" signal an HTTP-layer webhook
// handler could hand into a session actor's mailbox. This handler
// therefore writes directly, mirroring how coalesce.go already writes
// github_pr_sessions directly today, bypassing the actor entirely: via
// postgres.GitHubPRSessionStore.UpsertPendingRetriggerHeadSHA and the
// EXPORTED postgres.TimerStore.Upsert, in the SAME transaction. Both
// commit atomically as one unit or neither does, so a crash between them
// can never leave a pushed commit with no armed timer.

package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// handlePullRequestSynchronize implements this file's own top doc
// comment's full pipeline for ONE `pull_request` webhook delivery whose
// action is "synchronize" -- called from NewHandler (handler.go) BEFORE
// parseMention/the mention pipeline, mirroring handlePullRequestClosed's
// own identical "checked before parseMention" placement (pullrequestevent.go).
//
// Always writes an HTTP response itself. On a genuine processing failure
// (a malformed payload, or a Postgres error) it ALSO releases the webhook
// delivery claim (deliveries.Release) -- unlike handlePullRequestClosed's
// own decode-failure path, this mirrors parseMention's own established
// "claimed but never actually acted on -> release so a human-triggered
// GitHub redelivery can retry" precedent (handler.go), per §24.1's own
// explicit instruction to reuse the SAME claim/release handling the
// existing webhook toolkit already provides.
func handlePullRequestSynchronize(
	ctx context.Context,
	w http.ResponseWriter,
	body []byte,
	pool *pgxpool.Pool,
	prSessions *postgres.GitHubPRSessionStore,
	timers *postgres.TimerStore,
	timeouts platform.Timeouts,
	deliveries *postgres.WebhookDeliveryStore,
	deliveryID string,
) {
	logger := platform.Logger(ctx)

	releaseAndFail := func(status int) {
		if releaseErr := deliveries.Release(ctx, githubDeliveryProvider, deliveryID); releaseErr != nil {
			logger.Error("github: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
		}
		w.WriteHeader(status)
	}

	var payload pullRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Error("github: decode pull_request synchronize webhook payload failed", "error", err, "delivery_id", deliveryID)
		releaseAndFail(http.StatusBadRequest)
		return
	}
	// repository.full_name, pull_request.number, and pull_request.head.sha
	// are §24.1's own required fields -- none of which the existing
	// issueCommentPayload/pullRequestReviewCommentPayload structs need,
	// since neither carries a head SHA today. A genuine GitHub
	// `synchronize` event always carries all three; an empty one here
	// indicates a malformed/unexpected payload, treated the same as a
	// decode failure.
	if payload.Repository.FullName == "" || payload.PullRequest.Number == 0 || payload.PullRequest.Head.SHA == "" {
		logger.Error("github: pull_request synchronize webhook payload missing repository.full_name/pull_request.number/pull_request.head.sha",
			"delivery_id", deliveryID)
		releaseAndFail(http.StatusBadRequest)
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		logger.Error("github: begin pull_request synchronize transaction failed", "error", err, "delivery_id", deliveryID)
		releaseAndFail(http.StatusInternalServerError)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// §24.1: no github_pr_sessions row, or a row whose session_id is
	// still NULL, means there is no review session to re-trigger --
	// acknowledged, untouched, exactly like today's "no mention" no-op
	// for a comment event. See UpsertPendingRetriggerHeadSHA's own
	// generated doc comment for the guarded UPDATE this is.
	row, err := prSessions.WithTx(tx).UpsertPendingRetriggerHeadSHA(ctx, payload.Repository.FullName, payload.PullRequest.Number, payload.PullRequest.Head.SHA)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.WriteHeader(http.StatusOK)
			return
		}
		logger.Error("github: upsert pending retrigger head sha failed", "error", err, "delivery_id", deliveryID)
		releaseAndFail(http.StatusInternalServerError)
		return
	}

	// §24.2: re-arm (or arm, for this PR's own first push since its
	// review session was created) the trailing-edge debounce timer -- the
	// SAME upsert-on-UNIQUE(session_id, name) idiom the 5 pre-existing
	// named timers already use (session_timers.sql's own
	// UpsertSessionTimer), so a second push before the first debounce
	// fires simply pushes fires_at further out.
	if _, err := timers.WithTx(tx).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: row.SessionID,
		Name:      sessionactor.TimerReviewRetriggerDebounce,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(timeouts.ReviewRetriggerDebounce), Valid: true},
	}); err != nil {
		logger.Error("github: arm review_retrigger_debounce timer failed", "error", err, "delivery_id", deliveryID)
		releaseAndFail(http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("github: commit pull_request synchronize transaction failed", "error", err, "delivery_id", deliveryID)
		releaseAndFail(http.StatusInternalServerError)
		return
	}
	committed = true
	w.WriteHeader(http.StatusOK)
}
