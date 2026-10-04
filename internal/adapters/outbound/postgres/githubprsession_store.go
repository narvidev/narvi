package postgres

import (
	"context"
	"errors"
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

// GetByRepoAndPRNumber is the FORWARD, non-locking, non-claiming read the
// decision inbox's own "open review" action needs: given (repoFullName,
// prNumber), does a review session already exist for this exact PR, and
// if so which one? Deliberately never EnsureRow/LockForUpdate -- this is
// a read-only display lookup with no claim to make, and must never
// contend with (or block behind) a real concurrent @mention claim for the
// same PR the way holding LockForUpdate's own row lock would. Returns
// pgx.ErrNoRows (unwrapped) when Narvi has never been mentioned on this
// PR -- a common, legitimate negative, mirroring GetBySessionID's own
// identical contract.
func (s *GitHubPRSessionStore) GetByRepoAndPRNumber(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.GithubPrSession, error) {
	return s.q.GetGitHubPRSessionByRepoAndPRNumber(ctx, sqlcgen.GetGitHubPRSessionByRepoAndPRNumberParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// SetHeadSHA is REMOVED as of migrations/000072_turns_review_head_sha.up.sql
// -- github_pr_sessions.
// pending_head_sha (and this method) is superseded by turns.
// review_head_sha, set once at turn-creation time
// (internal/adapters/inbound/httpapi's createTurnLocked/CreateSessionOnTx)
// and read back via turns.GetByDispatchedMessageID (finding F3, §21.1's
// amendment, migrations/000131_turns_dispatched_message_id.up.sql) -- see
// migration 000072's own doc comment for the full "why a shared, mutable
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

// IncrementMentionCount is §12.2 item 2's own "coalesced-mention/session-
// reuse info" gap -- MUST be called on the bare, pool-backed store
// (coalesce.go's own REUSE branch calls it on c.PRSessions, never
// txPRSessions), AFTER that branch's own claim tx has already committed
// AND its authorization check AND turn creation have both succeeded --
// audit fix: a denied or failed REUSE attempt must not touch this column
// at all, and by the time this runs, that has already been decided. See
// IncrementGitHubPRSessionMentionCount's own generated doc comment for
// why calling this with no transaction/lock held is still safe with no
// CAS guard.
func (s *GitHubPRSessionStore) IncrementMentionCount(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.GithubPrSession, error) {
	return s.q.IncrementGitHubPRSessionMentionCount(ctx, sqlcgen.IncrementGitHubPRSessionMentionCountParams{
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

// RequeueAutoRetrigger is technical plan §24.9's re-request of an
// automatic re-review whose attempt ended context_moved: the pending head
// becomes headSHA unless a push left one, and the count of moves in a row
// grows by one; the returned row carries both. pgx.ErrNoRows (unwrapped):
// no claim row with a session for this pull request. See
// RequeueAutoRetrigger's generated doc comment.
func (s *GitHubPRSessionStore) RequeueAutoRetrigger(ctx context.Context, repoFullName string, prNumber int32, headSHA string) (sqlcgen.GithubPrSession, error) {
	return s.q.RequeueAutoRetrigger(ctx, sqlcgen.RequeueAutoRetriggerParams{
		HeadSha:      headSHA,
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// DropAutoRetrigger is technical plan §24.9's bound: the automatic
// re-review gives up on headSHA, its pending head, recording when and on
// which head for the session's status. pgx.ErrNoRows (unwrapped): the
// pending head is no longer headSHA. See DropAutoRetrigger's generated doc
// comment.
func (s *GitHubPRSessionStore) DropAutoRetrigger(ctx context.Context, repoFullName string, prNumber int32, headSHA string) (sqlcgen.GithubPrSession, error) {
	return s.q.DropAutoRetrigger(ctx, sqlcgen.DropAutoRetriggerParams{
		HeadSha:      headSHA,
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// ResetAutoRetriggerContextMoves starts sessionID's count of automatic
// attempts in a row that met a moved context again, because one of them
// started (technical plan §24.9); it reports the rows it wrote, 0 when the
// count was already 0 or the session claims no pull request.
func (s *GitHubPRSessionStore) ResetAutoRetriggerContextMoves(ctx context.Context, sessionID pgtype.UUID) (int64, error) {
	return s.q.ResetAutoRetriggerContextMoves(ctx, sessionID)
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

// RepoEntitlementFacts is a repository's eligibility for session creation
// as one statement read it (§31.4): Known, a github_pr_sessions row names
// it (fix/repo-scoped-authorization's own entitlement signal -- see
// ReadRepoEntitlement's generated doc comment for why that is a sound,
// externally-verified proof this deployment is attached to the repository);
// Revoked, an administrator revoked it (repo_entitlement_revocations). The
// two are only ever read together, so no caller can hold Known without
// Revoked.
type RepoEntitlementFacts struct {
	Known   bool
	Revoked bool
}

// RepoEntitlement reads repoFullName's RepoEntitlementFacts in one
// statement (ReadRepoEntitlement). Its callers are the session-creation
// resolvers (httpapi's repoentitlementgate.go), which refuse on Revoked
// before Known, and httpapi's confirmRepoKnown (reposettings.go), which
// scopes the repository-scoped admin routes on Known alone so an
// administrator can still reach a revoked repository -- its restore route
// included. internal/ops' ScanRepoEntitlementReads pins that list.
func (s *GitHubPRSessionStore) RepoEntitlement(ctx context.Context, repoFullName string) (RepoEntitlementFacts, error) {
	row, err := s.q.ReadRepoEntitlement(ctx, repoFullName)
	if err != nil {
		return RepoEntitlementFacts{}, err
	}
	return RepoEntitlementFacts{Known: row.RepoKnown, Revoked: row.Revoked}, nil
}

// RevokedRepoForSession reports the first repository of sessionID an
// administrator revoked (§31.4), and whether there is one: one of
// repoFullNames (the session's clone URLs, already parsed by the caller),
// or the base repository of a pull-request claim keyed to the session (a
// review session's github_pr_sessions row, a sentinel auto-fix child's
// sentinel_fixes row) -- see FirstRevokedRepoForSession's generated doc
// comment. internal/app/sessionactor reads it before a spawn and before
// each dispatch.
func (s *GitHubPRSessionStore) RevokedRepoForSession(ctx context.Context, sessionID pgtype.UUID, repoFullNames []string) (string, bool, error) {
	if repoFullNames == nil {
		repoFullNames = []string{}
	}
	repo, err := s.q.FirstRevokedRepoForSession(ctx, sqlcgen.FirstRevokedRepoForSessionParams{
		RepoFullNames: repoFullNames,
		SessionID:     sessionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return repo, true, nil
}
