package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// GitHubPRSessionStore is a thin, pass-through wrapper around the
// sqlc-generated github_pr_sessions queries (§8.2's "atomic claim
// coalescing of concurrent @mentions" "GitHub ingress"). No
// caching, no retries, no business rules -- the coalescing DECISION
// (create a new session vs. reuse an existing one) lives in
// internal/adapters/inbound/github/coalesce.go, which is the only caller
// today.
type GitHubPRSessionStore struct {
	q *sqlcgen.Queries
}

// NewGitHubPRSessionStore builds a GitHubPRSessionStore backed by pool.
func NewGitHubPRSessionStore(pool *pgxpool.Pool) *GitHubPRSessionStore {
	return &GitHubPRSessionStore{q: sqlcgen.New(pool)}
}

// WithTx returns a GitHubPRSessionStore whose queries run on tx instead of
// the pool this store was built with -- EnsureRow/LockForUpdate/
// SetSessionID must ALL run in the SAME transaction for the atomic claim
// to be sound (see migrations/000028_github_pr_sessions.up.sql's own doc
// comment), so every real caller uses WithTx, never the bare pool-backed
// store this constructor returns directly.
func (s *GitHubPRSessionStore) WithTx(tx pgx.Tx) *GitHubPRSessionStore {
	return &GitHubPRSessionStore{q: s.q.WithTx(tx)}
}

// EnsureRow idempotently ensures a (repoFullName, prNumber) claim row
// exists, with session_id left NULL on a fresh insert -- see
// EnsureGitHubPRSessionRow's own generated doc comment.
func (s *GitHubPRSessionStore) EnsureRow(ctx context.Context, repoFullName string, prNumber int32) error {
	return s.q.EnsureGitHubPRSessionRow(ctx, sqlcgen.EnsureGitHubPRSessionRowParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// LockForUpdate locks the (repoFullName, prNumber) claim row for the rest
// of the caller's own transaction, returning its CURRENT session_id
// (Valid == false means no session has claimed this PR yet -- the caller
// is the first mention and should create one). See
// LockGitHubPRSessionForUpdate's own generated doc comment for the
// concurrency-serialization this provides.
func (s *GitHubPRSessionStore) LockForUpdate(ctx context.Context, repoFullName string, prNumber int32) (pgtype.UUID, error) {
	return s.q.LockGitHubPRSessionForUpdate(ctx, sqlcgen.LockGitHubPRSessionForUpdateParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// SetSessionID fills in session_id for a (repoFullName, prNumber) claim
// row still holding LockForUpdate's own row lock -- called exactly once,
// by whichever caller observed session_id NULL under that lock.
func (s *GitHubPRSessionStore) SetSessionID(ctx context.Context, repoFullName string, prNumber int32, sessionID pgtype.UUID) error {
	return s.q.SetGitHubPRSessionID(ctx, sqlcgen.SetGitHubPRSessionIDParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		SessionID:    sessionID,
	})
}

// GetBySessionID is the REVERSE lookup §5.1 ("outbox delivery") needs:
// given a session_id, which (repoFullName, prNumber) PR does it back?
// Returns pgx.ErrNoRows (unwrapped) when sessionID was never created via a
// GitHub PR mention.
func (s *GitHubPRSessionStore) GetBySessionID(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.GithubPrSession, error) {
	return s.q.GetGitHubPRSessionBySessionID(ctx, sessionID)
}

// SetHeadSHA is REMOVED as of migrations/000072_turns_review_head_sha.up.sql
// -- github_pr_sessions.
// pending_head_sha (and this method) is superseded by turns.
// review_head_sha, set once at turn-creation time
// (internal/adapters/inbound/httpapi's createTurnLocked/CreateSessionOnTx)
// and read back via TurnStore.GetProcessingTurnForSession -- see that
// migration's own doc comment for the full "why a shared, mutable
// per-(repo,PR) column was the wrong place for this fact".

// UpsertPendingRetriggerHeadSHA is §24's own actor-bypassing write
// (§24.1's 4th cost item, internal/adapters/inbound/github/
// pullrequestsynchronize.go): guarded on session_id IS NOT NULL, so
// pgx.ErrNoRows (unwrapped) means exactly "no row, or a row with
// session_id still NULL -- no review session to re-trigger", the SAME
// acknowledge-and-ignore outcome as today's "no mention" case. See
// UpsertPendingRetriggerHeadSHA's own generated doc comment.
func (s *GitHubPRSessionStore) UpsertPendingRetriggerHeadSHA(ctx context.Context, repoFullName string, prNumber int32, headSHA string) (sqlcgen.GithubPrSession, error) {
	return s.q.UpsertPendingRetriggerHeadSHA(ctx, sqlcgen.UpsertPendingRetriggerHeadSHAParams{
		RepoFullName:            repoFullName,
		PrNumber:                prNumber,
		PendingRetriggerHeadSha: &headSHA,
	})
}

// ClearPendingRetriggerHeadSHA is the review_retrigger_debounce timer's
// own guarded clear (§24.3 steps 3-4) -- expectedHeadSHA must still equal
// the column's CURRENT value for the clear to apply; pgx.ErrNoRows
// (unwrapped) means a newer synchronize event already overwrote it (see
// ClearPendingRetriggerHeadSHA's own generated doc comment for the full
// race this guards against), which the caller treats as harmless -- the
// newer event's own timer re-arm already covers the newer push. Rereview
// fix (finding 2): the caller (sessionactor.clearPendingRetriggerHeadSHAGuarded)
// reports this outcome back to ITS OWN caller as guardMissed == true,
// which skips that caller's subsequent deleteTimer call -- session_timers
// has UNIQUE(session_id, name), so there is exactly ONE
// review_retrigger_debounce row per session, the SAME row the newer
// event's own re-arm just updated, never a separate row of this firing's
// own to delete safely.
func (s *GitHubPRSessionStore) ClearPendingRetriggerHeadSHA(ctx context.Context, repoFullName string, prNumber int32, expectedHeadSHA string) (sqlcgen.GithubPrSession, error) {
	return s.q.ClearPendingRetriggerHeadSHA(ctx, sqlcgen.ClearPendingRetriggerHeadSHAParams{
		RepoFullName:            repoFullName,
		PrNumber:                prNumber,
		PendingRetriggerHeadSha: &expectedHeadSHA,
	})
}

// IncrementAutoRetriggerCount is §24.6's own budget-counter increment --
// called exactly once per automatically-enqueued re-review turn, never
// for a manual label/button re-trigger.
func (s *GitHubPRSessionStore) IncrementAutoRetriggerCount(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.GithubPrSession, error) {
	return s.q.IncrementAutoRetriggerCount(ctx, sqlcgen.IncrementAutoRetriggerCountParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// MarkAutoRetriggerBudgetNoticeSent is §24.6's own "post the notice
// exactly once" claim -- guarded on auto_retrigger_budget_notice_sent_at
// IS NULL; pgx.ErrNoRows (unwrapped) means this PR was already notified,
// so the caller must not post a second notice.
func (s *GitHubPRSessionStore) MarkAutoRetriggerBudgetNoticeSent(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.GithubPrSession, error) {
	return s.q.MarkAutoRetriggerBudgetNoticeSent(ctx, sqlcgen.MarkAutoRetriggerBudgetNoticeSentParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// RecordMergeOutcome captures a `pull_request` "closed" webhook's own
// merged/closed_at facts onto (repoFullName, prNumber)'s claim row --
// §31.7's own G4 arming write. Guarded on session_id IS NOT NULL; a
// pgx.ErrNoRows (unwrapped) result means "no session to arm eligibility
// for" -- see RecordMergeOutcome's own generated doc comment for the full
// "why" -- callers acknowledge and ignore it, never treat it as a real
// failure.
func (s *GitHubPRSessionStore) RecordMergeOutcome(ctx context.Context, repoFullName string, prNumber int32, merged bool, closedAt time.Time) (sqlcgen.GithubPrSession, error) {
	return s.q.RecordMergeOutcome(ctx, sqlcgen.RecordMergeOutcomeParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		PrMerged:     &merged,
		PrClosedAt:   pgtype.Timestamptz{Time: closedAt, Valid: true},
	})
}

// RepoKnown is fix/repo-scoped-authorization's own entitlement signal:
// reports whether ANY github_pr_sessions row exists for repoFullName --
// see RepoKnownToDeployment's own generated doc comment for the full "why
// this is a sound, externally-verified proof this deployment is genuinely
// attached to repoFullName" reasoning. Used by httpapi's own
// resolveKnownRepo (reposettings.go) to reject a request whose URL names a
// repository this deployment has never actually seen GitHub webhook
// traffic for.
func (s *GitHubPRSessionStore) RepoKnown(ctx context.Context, repoFullName string) (bool, error) {
	return s.q.RepoKnownToDeployment(ctx, repoFullName)
}
