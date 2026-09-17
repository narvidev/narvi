package outboxworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/platform"
)

// This file implements the review's own GitHub-native result surface
// (§8.2/§21.1/§21.1b) publisher: ports.
// NotificationKindGitHubReviewCheck's own real Deliver. The full identity
// design lives in three places, each with its own doc comment this one
// only summarizes:
//
//   - internal/domain/reviewcheck: the pure state mapping (Phase ->
//     GitHub status/conclusion, ComputeOutput) and the write-conflict
//     ordering (Supersedes).
//   - migrations/000132_review_check_runs.up.sql / internal/adapters/
//     outbound/postgres.ReviewCheckRunStore: the per-PR claim table and
//     its atomic Ensure+Lock+Update sequencing.
//   - internal/adapters/outbound/githubapi's own checkruns.go: the raw
//     GitHub Checks API calls.
//
// Deliver below is the ONE place all three compose: claim (Postgres
// only, lock held briefly) -> release the lock -> call GitHub (no
// transaction open, mirroring ports.Notifier.Deliver's own "always
// outside any tx" contract) -> record the result with a guarded,
// optimistic-concurrency write (no lock re-acquired -- see
// SetReviewCheckRunExternalID's own generated doc comment for the
// accepted residual this trades for never holding a Postgres transaction
// across a network call).
type reviewCheckNotifier struct {
	pool     *pgxpool.Pool
	store    *postgres.ReviewCheckRunStore
	adapter  *githubapi.Adapter
	botToken string
	// appID is this deployment's own configured GitHub App id
	// (platform.Config.GitHubAppID) -- the second half of "select by SHA
	// and GitHub App": a recovery list-check-runs read (below) adopts
	// only a check run whose own reported app.id matches this value,
	// never merely one sharing reviewcheck.CheckName.
	appID int64
}

// NewReviewCheckNotifier builds a ports.Notifier for
// ports.NotificationKindGitHubReviewCheck.
func NewReviewCheckNotifier(pool *pgxpool.Pool, store *postgres.ReviewCheckRunStore, adapter *githubapi.Adapter, botToken string, appID int64) ports.Notifier {
	return &reviewCheckNotifier{pool: pool, store: store, adapter: adapter, botToken: botToken, appID: appID}
}

var _ ports.Notifier = (*reviewCheckNotifier)(nil)

// toEmission converts a (possibly placeholder, possibly real)
// review_check_runs row into a reviewcheck.Emission -- a placeholder row
// (EnsureReviewCheckRunRow's own fresh insert: head_sha == "", phase ==
// "") converts to the Emission{} zero value, which reviewcheck.
// Supersedes already treats as "nothing published yet, always lose".
func toEmission(repoFullName string, prNumber int32, headSHA string, attemptID pgtype.UUID, attemptCreatedAt pgtype.Timestamptz, phase string, baseRef, baseSHA *string, policyVersion int32) reviewcheck.Emission {
	e := reviewcheck.Emission{
		RepoFullName:  repoFullName,
		PRNumber:      prNumber,
		HeadSHA:       headSHA,
		Phase:         reviewcheck.Phase(phase),
		PolicyVersion: int(policyVersion),
	}
	if attemptID.Valid {
		e.AttemptID = attemptID.String()
	}
	if attemptCreatedAt.Valid {
		e.AttemptCreatedAt = attemptCreatedAt.Time
	}
	if baseRef != nil {
		e.BaseRef = *baseRef
	}
	if baseSHA != nil {
		e.BaseSHA = *baseSHA
	}
	return e
}

// parseAttemptID converts payload's own plain-string AttemptID back to
// pgtype.UUID -- "" (a PhaseQueued emission, internal/domain/reviewcheck.
// Emission's own doc comment on AttemptID) converts to the zero value
// (Valid == false), a genuine SQL NULL.
func parseAttemptID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if s == "" {
		return u, nil
	}
	if err := u.Scan(s); err != nil {
		return u, fmt.Errorf("outboxworker: reviewCheckNotifier: parse attempt id %q: %w", s, err)
	}
	return u, nil
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Deliver implements ports.Notifier. See this file's own top doc comment
// for the full claim -> call -> record sequence.
func (n *reviewCheckNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	logger := platform.Logger(ctx)

	var payload ports.ReviewCheckPayload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: decode payload: %w", err)
	}
	if payload.HeadSHA == "" {
		// Mirrors httpapi.PostReviewVerdict's own "no review head sha on
		// record, skipping" precedent: nothing to create or update
		// against without a commit SHA. Logged, never retried -- a
		// redelivery of the IDENTICAL payload would reach the identical
		// conclusion every time.
		logger.Warn("outboxworker: reviewCheckNotifier: emission carries no head sha, skipping", "repo", payload.Owner+"/"+payload.Repo, "pr_number", payload.PRNumber)
		return nil
	}
	candidatePhase := reviewcheck.Phase(payload.Phase)
	if !candidatePhase.Valid() {
		logger.Error("outboxworker: reviewCheckNotifier: emission carries an unrecognized phase, refusing to publish", "phase", payload.Phase, "repo", payload.Owner+"/"+payload.Repo, "pr_number", payload.PRNumber)
		return nil
	}

	attemptID, err := parseAttemptID(payload.AttemptID)
	if err != nil {
		return err
	}
	candidate := reviewcheck.Emission{
		RepoFullName:     payload.Owner + "/" + payload.Repo,
		PRNumber:         int32(payload.PRNumber),
		HeadSHA:          payload.HeadSHA,
		AttemptID:        payload.AttemptID,
		AttemptCreatedAt: payload.AttemptCreatedAt,
		Phase:            candidatePhase,
		BaseRef:          payload.BaseRef,
		BaseSHA:          payload.BaseSHA,
		PolicyVersion:    payload.PolicyVersion,
	}

	// --- Claim: Postgres only, lock held briefly, never across the
	// GitHub call below (migrations/000132's own doc comment). ---
	tx, err := n.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: begin claim tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	txStore := n.store.WithTx(tx)
	if err := txStore.EnsureRow(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber)); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: ensure claim row: %w", err)
	}
	row, err := txStore.LockForUpdate(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber))
	if err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: lock claim row: %w", err)
	}
	current := toEmission(row.RepoFullName, row.PrNumber, row.HeadSha, row.AttemptID, row.AttemptCreatedAt, row.Phase, row.BaseRef, row.BaseSha, row.PolicyVersion)

	if !reviewcheck.Supersedes(current, candidate) {
		// Refused, not an error -- §21.1b: "an emission carries the
		// attempt and context it was produced for, and is REFUSED when
		// either has been superseded." Rolling back is safe: this
		// transaction has made no durable change (EnsureRow is
		// idempotent; the lock is released on rollback).
		logger.Info("outboxworker: reviewCheckNotifier: emission superseded, refusing to publish",
			"repo", candidate.RepoFullName, "pr_number", candidate.PRNumber,
			"candidate_attempt", candidate.AttemptID, "current_attempt", current.AttemptID,
			"candidate_phase", candidate.Phase, "current_phase", current.Phase)
		return nil
	}

	// newIdentityNeeded decides whether the EXISTING external check run
	// (if any) may be updated in place, or whether this emission must
	// open a fresh one. Two, and only two, reasons ever force a fresh
	// identity:
	//
	//  1. The head sha itself changed -- a check run is scoped to one
	//     commit (this package's own doc comment on why); the old
	//     external_id belongs to a SHA this row no longer targets.
	//  2. The CURRENT row is already terminal-shaped (Stale/
	//     TerminalAssessed/TerminalNotAssessed) AND candidate belongs to
	//     a DIFFERENT attempt -- "open a NEW check when a review
	//     restarts after a terminal result... never reopen a concluded
	//     one" (the brief's own identity rule). Reaching this branch
	//     already means Supersedes accepted candidate (above), so
	//     candidate is a genuinely newer attempt, not a stray/stale one.
	//
	// Neither condition fires for an ordinary same-attempt phase
	// progression (Queued -> Running -> Terminal all share one
	// AttemptID) or for a same-attempt PhaseStale flip: those correctly
	// keep updating the SAME external check run.
	newIdentityNeeded := row.HeadSha != "" && (row.HeadSha != candidate.HeadSHA ||
		(current.Phase.Terminal() && candidate.AttemptID != current.AttemptID))
	var existingExternalID *int64
	if newIdentityNeeded {
		if err := txStore.ClearExternalID(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber)); err != nil {
			return fmt.Errorf("outboxworker: reviewCheckNotifier: clear stale external id: %w", err)
		}
	} else {
		existingExternalID = row.ExternalID
	}

	attemptCreatedAt := pgtype.Timestamptz{Time: candidate.AttemptCreatedAt, Valid: !candidate.AttemptCreatedAt.IsZero()}
	if _, err := txStore.UpdatePublished(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber), candidate.HeadSHA, attemptID, attemptCreatedAt, string(candidate.Phase), nonEmptyPtr(candidate.BaseRef), nonEmptyPtr(candidate.BaseSHA), int32(candidate.PolicyVersion)); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: update published emission: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: commit claim tx: %w", err)
	}
	committed = true

	// --- Call: GitHub, with no Postgres transaction open. ---
	output := reviewcheck.ComputeOutput(candidate.Phase)

	if existingExternalID != nil {
		if err := n.adapter.UpdateCheckRun(ctx, payload.Owner, payload.Repo, n.botToken, *existingExternalID, string(output.Status), string(output.Conclusion), output.Title, output.Summary); err != nil {
			return n.classifyAndWrap(logger, "update", err)
		}
		return nil
	}

	// No existing external id recorded locally: either this is genuinely
	// the first emission for this head sha, or an earlier delivery
	// attempt created the check run on GitHub but crashed OR is still
	// in flight (its own GitHub call has not yet reached SetExternalID).
	// The claim-row lock (LockForUpdate, above) serializes the two
	// attempts' own CLAIM decisions -- it cannot also serialize their
	// GitHub calls, which must run outside any transaction
	// (ports.Notifier.Deliver's own contract) -- so two attempts sharing
	// one head sha, racing closely enough, can each independently reach
	// this branch and each call CreateCheckRun below. Confirmed
	// empirically (reviewcheck_integration_test.go's own
	// TestReviewCheckNotifier_ConcurrentAttempts_ResolveToOneIdentity,
	// run repeated): the LOSING attempt's own check run becomes an
	// orphan nothing ever references again once its own SetExternalID
	// call (below) loses its guard -- Postgres's own claim row still
	// converges to exactly ONE identity regardless, which is what
	// §21.1b actually promises ("two ACTIVE identities... worse than
	// none" -- active meaning ones this system still treats as
	// current). Recover via "select by SHA
	// and GitHub App" (the brief's own identity rule) before creating a
	// duplicate.
	externalID, err := n.resolveOrCreateCheckRun(ctx, payload.Owner, payload.Repo, candidate.HeadSHA, output)
	if err != nil {
		return n.classifyAndWrap(logger, "create", err)
	}

	if _, err := n.store.SetExternalID(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber), externalID, attemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Guard miss: a newer candidate reserved this row while the
			// GitHub call above was in flight. See
			// SetReviewCheckRunExternalID's own generated doc comment for
			// the accepted residual this represents -- this delivery is
			// done regardless; the newer candidate's own Deliver call
			// already has, or will get, its own correct external id.
			logger.Warn("outboxworker: reviewCheckNotifier: external id write lost the guard (superseded during the GitHub call); the just-created/updated check run is now unrecorded locally",
				"repo", candidate.RepoFullName, "pr_number", candidate.PRNumber, "external_id", externalID)
			return nil
		}
		return fmt.Errorf("outboxworker: reviewCheckNotifier: record external id: %w", err)
	}
	return nil
}

// resolveOrCreateCheckRun implements "select by SHA and GitHub App": it
// first lists GitHub's own check runs for headSHA and adopts (PATCHes)
// the one matching BOTH reviewcheck.CheckName and this deployment's own
// GitHub App id, if any exists -- never merely the first name match,
// which could belong to a different app entirely. Only when no match is
// found does it create a brand-new check run.
func (n *reviewCheckNotifier) resolveOrCreateCheckRun(ctx context.Context, owner, repo, headSHA string, output reviewcheck.Output) (int64, error) {
	existing, err := n.adapter.ListCheckRunsForRef(ctx, owner, repo, headSHA, n.botToken)
	if err != nil {
		return 0, fmt.Errorf("list check runs for ref: %w", err)
	}
	for _, run := range existing {
		if run.Name == reviewcheck.CheckName && run.AppID == n.appID && run.HeadSHA == headSHA {
			if err := n.adapter.UpdateCheckRun(ctx, owner, repo, n.botToken, run.ID, string(output.Status), string(output.Conclusion), output.Title, output.Summary); err != nil {
				return 0, fmt.Errorf("adopt existing check run %d: %w", run.ID, err)
			}
			return run.ID, nil
		}
	}
	id, err := n.adapter.CreateCheckRun(ctx, owner, repo, n.botToken, headSHA, reviewcheck.CheckName, string(output.Status), string(output.Conclusion), output.Title, output.Summary)
	if err != nil {
		return 0, fmt.Errorf("create check run: %w", err)
	}
	return id, nil
}

// classifyAndWrap surfaces "permission, rate-limit and transient
// failures distinctly" (the brief's own identity-rules requirement),
// logging which of the three this attempt's error is BEFORE wrapping and
// returning it -- the outbox's own existing retry/backoff/dead-letter
// path (§5.1) applies uniformly to whatever this returns, but an operator
// reading logs (or a future dead-letter reason column) can already tell
// a missing `checks:write` permission apart from a flaky network apart
// from GitHub's own rate limiting, without this classification
// collapsing all three into one indistinguishable "delivery failed".
func (n *reviewCheckNotifier) classifyAndWrap(logger *slog.Logger, op string, err error) error {
	var apiErr *githubapi.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.RateLimited:
		logger.Warn("outboxworker: reviewCheckNotifier: rate-limited by GitHub", "op", op, "error", err)
	case errors.Is(err, ports.ErrPermissionDenied):
		logger.Error("outboxworker: reviewCheckNotifier: permission denied -- this installation likely lacks checks:write", "op", op, "error", err)
	case errors.Is(err, ports.ErrAuthenticationFailed):
		logger.Error("outboxworker: reviewCheckNotifier: authentication failed -- the configured bot credential was rejected", "op", op, "error", err)
	default:
		logger.Warn("outboxworker: reviewCheckNotifier: transient delivery failure", "op", op, "error", err)
	}
	return fmt.Errorf("outboxworker: reviewCheckNotifier: %s check run: %w", op, err)
}
