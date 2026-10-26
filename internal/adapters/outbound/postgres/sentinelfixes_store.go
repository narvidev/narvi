package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// SentinelFixStore is a thin, pass-through wrapper around the
// sqlc-generated sentinel_fixes queries ("sentinels +
// suggestions", §17) -- see migrations/000047_sentinel_fixes.up.sql's own
// doc comment for the table's full design and its two-step claim idiom
// (mirroring github_pr_sessions' own established precedent).
type SentinelFixStore struct {
	q *sqlcgen.Queries
}

// NewSentinelFixStore builds a SentinelFixStore backed by pool.
func NewSentinelFixStore(pool *pgxpool.Pool) *SentinelFixStore {
	return &SentinelFixStore{q: sqlcgen.New(pool)}
}

// WithTx returns a SentinelFixStore whose queries run on tx instead of the
// pool this store was built with -- mirrors every other store's own
// identical WithTx convention. The Claim sequence below (InsertIfAbsent +
// GetForUpdate) is ALWAYS called on a WithTx-scoped store, exactly like
// github_pr_sessions' own identical two-step claim -- and so is
// LockForUpdate (below), the outbox worker's own later, SEPARATE atomic
// claim of the SAME row (see that method's own doc comment).
func (s *SentinelFixStore) WithTx(tx pgx.Tx) *SentinelFixStore {
	return &SentinelFixStore{q: s.q.WithTx(tx)}
}

// Claim performs the full two-step atomic claim (§17.1's own "a second
// qualifying finding on a PR that already has a fix in flight reuses this
// SAME row rather than racing a second child session") -- MUST be called
// on a WithTx-scoped store, inside the SAME transaction
// internal/adapters/inbound/httpapi/reviewverdict.go already holds open
// for its own findings-upsert/outbox-enqueue write. Returns the row
// whether this call created it or it already existed -- callers branch on
// the returned row's own FixChildSessionID.Valid to decide "is a fix
// already in flight for this PR" (see reviewverdict.go's own doc comment
// on that decision).
func (s *SentinelFixStore) Claim(ctx context.Context, repoFullName string, originPRNumber int32, originReviewSessionID pgtype.UUID, originHeadBranch string) (sqlcgen.SentinelFix, error) {
	if _, err := s.q.InsertSentinelFixIfAbsent(ctx, sqlcgen.InsertSentinelFixIfAbsentParams{
		RepoFullName:          repoFullName,
		OriginPrNumber:        originPRNumber,
		OriginReviewSessionID: originReviewSessionID,
		OriginHeadBranch:      originHeadBranch,
	}); err != nil {
		return sqlcgen.SentinelFix{}, err
	}
	return s.q.GetSentinelFixForUpdate(ctx, sqlcgen.GetSentinelFixForUpdateParams{
		RepoFullName:   repoFullName,
		OriginPrNumber: originPRNumber,
	})
}

// GetByID is the outbox worker's own idempotency check -- pgx.ErrNoRows
// (unwrapped) means this claim id no longer exists (should be
// unreachable in practice: ON DELETE CASCADE only fires if the origin
// session itself were deleted).
func (s *SentinelFixStore) GetByID(ctx context.Context, id pgtype.UUID) (sqlcgen.SentinelFix, error) {
	return s.q.GetSentinelFixByID(ctx, id)
}

// LockForUpdate locks the (repoFullName, originPRNumber) claim row for the
// rest of the caller's own transaction, returning its CURRENT row --
// callers branch on the returned row's own FixChildSessionID.Valid to
// decide whether a spawn is still needed. MUST be called on a
// WithTx-scoped store. Mirrors GitHubPRSessionStore.LockForUpdate's own
// identical shape (githubprsession_store.go) for the SAME class of "the
// claim row already exists (created via Claim, above, in a PRIOR,
// already-committed transaction), now serialize concurrent claimants
// against it" problem.
//
// internal/app/outboxworker's own sentinelAutoFixNotifier.Deliver
// (sentinelautofix.go) is this method's own one real caller, closing a
// confirmed audit finding: Deliver used to spawn the child session
// (httpapi.SpawnChildSession, its own separate transaction) and THEN
// write fix_child_session_id back onto this row as a SEPARATE,
// non-atomic write -- so a redelivered/retried Deliver call whose earlier
// attempt committed the child session but crashed/timed out (or hit a
// transient error) before that second write could spawn a SECOND child
// session for the SAME claim. Deliver now takes THIS lock, then calls
// httpapi.CreateSessionOnTx and the fix_child_session_id write both
// inline on the SAME transaction this lock is held on -- mirroring
// internal/adapters/inbound/github's own coalesce.go CreateOrJoin
// (EnsureRow+LockForUpdate, then CreateSessionOnTx inline, then the guard
// write, then commit) for the structurally identical "claim a row, then
// create a session under that SAME lock" problem. A concurrent or
// redelivered claimant blocks here until the winner's own transaction
// commits (and then observes FixChildSessionID already valid) or rolls
// back (leaving NO trace at all -- Postgres aborts the whole transaction
// -- so a following retry starts clean).
func (s *SentinelFixStore) LockForUpdate(ctx context.Context, repoFullName string, originPRNumber int32) (sqlcgen.SentinelFix, error) {
	return s.q.GetSentinelFixForUpdate(ctx, sqlcgen.GetSentinelFixForUpdateParams{
		RepoFullName:   repoFullName,
		OriginPrNumber: originPRNumber,
	})
}

// Get is a plain, unlocked read -- the merge-gating webhook's own lookup
// (§17.4). pgx.ErrNoRows (unwrapped) means this PR never had a sentinel
// auto-fix triggered on it.
func (s *SentinelFixStore) Get(ctx context.Context, repoFullName string, originPRNumber int32) (sqlcgen.SentinelFix, error) {
	return s.q.GetSentinelFix(ctx, sqlcgen.GetSentinelFixParams{
		RepoFullName:   repoFullName,
		OriginPrNumber: originPRNumber,
	})
}

// GetByFixSession is the reverse lookup pushpr.go's own
// createSentinelFixPRBestEffort needs (that code path only ever has the
// FIX session's own id in hand).
func (s *SentinelFixStore) GetByFixSession(ctx context.Context, fixChildSessionID pgtype.UUID) (sqlcgen.SentinelFix, error) {
	return s.q.GetSentinelFixByFixSession(ctx, fixChildSessionID)
}

// UpdateChildSession records that the outbox worker has spawned the child
// session -- status moves 'pending' -> 'spawned'.
func (s *SentinelFixStore) UpdateChildSession(ctx context.Context, id, fixChildSessionID pgtype.UUID) (sqlcgen.SentinelFix, error) {
	return s.q.UpdateSentinelFixChildSession(ctx, sqlcgen.UpdateSentinelFixChildSessionParams{
		ID:                id,
		FixChildSessionID: fixChildSessionID,
	})
}

// UpdateOpened records that the fix PR has actually been opened -- status
// moves 'spawned' -> 'fix_open'.
func (s *SentinelFixStore) UpdateOpened(ctx context.Context, id pgtype.UUID, fixPRNumber int32) (sqlcgen.SentinelFix, error) {
	return s.q.UpdateSentinelFixOpened(ctx, sqlcgen.UpdateSentinelFixOpenedParams{
		ID:          id,
		FixPrNumber: &fixPRNumber,
	})
}

// UpdateStackRegistered records the POST /repos/{owner}/{repo}/stacks
// call's own outcome -- observability only (see this table's own
// migration doc comment for why this is never the authority on whether
// registration actually stuck).
func (s *SentinelFixStore) UpdateStackRegistered(ctx context.Context, id pgtype.UUID, registered bool) (sqlcgen.SentinelFix, error) {
	return s.q.UpdateSentinelFixStackRegistered(ctx, sqlcgen.UpdateSentinelFixStackRegisteredParams{
		ID:              id,
		StackRegistered: registered,
	})
}

// MarkMerged is merge-gating's own terminal write (§17.4).
func (s *SentinelFixStore) MarkMerged(ctx context.Context, id pgtype.UUID) (sqlcgen.SentinelFix, error) {
	return s.q.MarkSentinelFixMerged(ctx, id)
}

// MarkAbandoned is set when the origin PR closes without merging (§17.5).
func (s *SentinelFixStore) MarkAbandoned(ctx context.Context, id pgtype.UUID) (sqlcgen.SentinelFix, error) {
	return s.q.MarkSentinelFixAbandoned(ctx, id)
}

// ReleaseUnspawned deletes claim id while it is still 'pending' with no
// child session (ReleaseUnspawnedSentinelFix's own doc comment), returning
// the rows deleted: 0 when a child was recorded on it meanwhile, or the
// row is already gone.
func (s *SentinelFixStore) ReleaseUnspawned(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.ReleaseUnspawnedSentinelFix(ctx, id)
}

// ExistsByFixPRNumber reports whether (repoFullName, fixPRNumber) is a
// registered sentinel-auto-fix follow-up PR -- §16's own §17
// structural exclusion (see ExistsSentinelFixByFixPRNumber's own generated
// doc comment for why this is the correct, specific marker rather than
// sessions.parent_session_id/spawn_depth).
func (s *SentinelFixStore) ExistsByFixPRNumber(ctx context.Context, repoFullName string, fixPRNumber int32) (bool, error) {
	return s.q.ExistsSentinelFixByFixPRNumber(ctx, sqlcgen.ExistsSentinelFixByFixPRNumberParams{
		RepoFullName: repoFullName,
		FixPrNumber:  &fixPRNumber,
	})
}
