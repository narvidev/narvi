// This file (mergeoutcome.go) implements §31.7's own G4 arming write:
// capturing the GitHub `pull_request` webhook's own "closed" action onto
// github_pr_sessions.pr_merged/pr_closed_at (migrations/
// 000118_github_pr_sessions_merge_outcome.up.sql) -- a THIRD, independent
// reader of the SAME event type this package already parses twice:
// pullrequestevent.go's own sentinel-auto-fix merge gate (§17.4/§17.5,
// reacting to a DIFFERENT PR -- the sentinel fix's own ORIGIN -- closing)
// and pullrequestsynchronize.go's own "synchronize" re-review trigger.
//
// This capture never claims or consumes the request the way either of
// those two lanes does: it is a pure, best-effort side effect that runs
// ALONGSIDE whichever of them also fires (or neither) for the SAME
// delivery, never gating or replacing either. A PR this deployment
// reviewed can be, at the same moment, both a tracked review subject
// (this file's own concern) and a sentinel-fix's own origin PR
// (pullrequestevent.go's concern) -- the two facts are unrelated, and
// both deserve to be recorded independently.

package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// MergeOutcomeRecorder is the narrow slice of *postgres.GitHubPRSessionStore
// this file needs -- a small, locally-defined interface (mirroring
// FalsePositivePatternCapturer/ArchRecapContestCapturer's own established
// precedent, falsepositivecapture.go/archrecapcontest.go) so a unit test
// can inject a fake with no real Postgres connection.
type MergeOutcomeRecorder interface {
	RecordMergeOutcome(ctx context.Context, repoFullName string, prNumber int32, merged bool, closedAt time.Time) (sqlcgen.GithubPrSession, error)
}

// mergeOutcomePayload is the small subset of GitHub's real `pull_request`
// webhook event shape this capture needs
// (https://docs.github.com/webhooks/webhook-events-and-payloads#pull_request)
// -- mirrors pullRequestEventPayload's own identical Repository/
// PullRequest.Number/Merged fields (pullrequestevent.go) plus the one
// field that payload never needed, ClosedAt: GitHub's own RFC3339
// timestamp, which encoding/json parses directly into *time.Time.
type mergeOutcomePayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number   int32      `json:"number"`
		Merged   bool       `json:"merged"`
		ClosedAt *time.Time `json:"closed_at"`
	} `json:"pull_request"`
}

// captureMergeOutcome is dispatched from handler.go for EVERY
// `pull_request` event whose own action is "closed", regardless of
// whether the sentinel-auto-fix merge gate (pullrequestevent.go) also
// fires for the SAME delivery -- best-effort, logged, never blocking or
// altering the response the request otherwise gets. prSessions == nil
// (this package's own handler_test.go, or any minimal wiring that
// doesn't care about this Step) is a silent no-op.
//
// ClosedAt == nil (a malformed/absent payload field -- should not happen
// for a genuine GitHub "closed" action, defended against anyway) skips
// the capture entirely rather than fabricating "now" as a stand-in: the
// whole point of persisting a real closed_at is to measure a future
// quarantine-age condition (§31.7's own G4) from GitHub's own timestamp,
// never from whenever this webhook happened to be delivered or
// processed.
//
// No backfill: a PR that closed before this capture existed simply never
// runs through this function at all -- migrations/
// 000118_github_pr_sessions_merge_outcome.up.sql's own doc comment states
// the same fact from the schema side.
func captureMergeOutcome(ctx context.Context, logger *slog.Logger, prSessions MergeOutcomeRecorder, body []byte) {
	if prSessions == nil {
		return
	}

	var p mergeOutcomePayload
	if err := json.Unmarshal(body, &p); err != nil {
		logger.Warn("github: capture merge outcome: unmarshal pull_request payload failed", "error", err)
		return
	}
	if p.PullRequest.ClosedAt == nil {
		logger.Warn("github: capture merge outcome: pull_request.closed_at missing on a closed event, skipping capture",
			"repo_full_name", p.Repository.FullName, "pr_number", p.PullRequest.Number)
		return
	}

	if _, err := prSessions.RecordMergeOutcome(ctx, p.Repository.FullName, p.PullRequest.Number, p.PullRequest.Merged, *p.PullRequest.ClosedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No github_pr_sessions row, or one with session_id still
			// NULL -- this PR was never actually reviewed by this
			// deployment (RecordMergeOutcome's own generated doc
			// comment). Not an error; nothing to arm.
			return
		}
		logger.Error("github: capture merge outcome: record merge outcome failed", "error", err, "repo_full_name", p.Repository.FullName, "pr_number", p.PullRequest.Number)
	}
}
