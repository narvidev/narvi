package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// SessionStore is a thin, pass-through wrapper around the sqlc-generated
// session queries (§4.3 SessionStore). No caching, no retries, no business
// rules — that lives in app/sessionactor (§2).
type SessionStore struct {
	q *sqlcgen.Queries
}

// NewSessionStore builds a SessionStore backed by pool.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	return &SessionStore{q: sqlcgen.New(pool)}
}

// WithTx returns a SessionStore whose queries run on tx instead of the
// pool this store was built with — used by app/sessionactor's
// epoch-fenced transactional-write helper, which needs
// GetActorEpochForUpdate and UpdateStatus to run inside the SAME
// transaction (§2: "state transition + appended event + outbox entries
// commit in ONE Postgres transaction").
func (s *SessionStore) WithTx(tx pgx.Tx) *SessionStore {
	return &SessionStore{q: s.q.WithTx(tx)}
}

// Create inserts a new session row and returns it.
func (s *SessionStore) Create(ctx context.Context, arg sqlcgen.CreateSessionParams) (sqlcgen.Session, error) {
	return s.q.CreateSession(ctx, arg)
}

// Get fetches a session by id.
func (s *SessionStore) Get(ctx context.Context, id pgtype.UUID) (sqlcgen.Session, error) {
	return s.q.GetSession(ctx, id)
}

// BumpActorEpoch increments a session's actor_epoch and returns the new
// value. Called once, at acquisition time, when an actor takes ownership
// of the session (§2: "bumped on each acquisition").
func (s *SessionStore) BumpActorEpoch(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.BumpActorEpoch(ctx, id)
}

// GetActorEpochForUpdate locks and reads a session's current actor_epoch.
// Meaningful only when called on a store built via WithTx: it must run
// inside the same transaction as the write it is fencing (§2).
func (s *SessionStore) GetActorEpochForUpdate(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.GetSessionActorEpochForUpdate(ctx, id)
}

// UpdateStatus persists a session's derived status + failure_reason.
func (s *SessionStore) UpdateStatus(ctx context.Context, arg sqlcgen.UpdateSessionStatusParams) (sqlcgen.Session, error) {
	return s.q.UpdateSessionStatus(ctx, arg)
}

// UpdateConversationID persists the session-level OpenCode conversation id
// (§3.3; see migrations/000018_session_repos.up.sql's own doc comment for
// why this is session-scoped).
func (s *SessionStore) UpdateConversationID(ctx context.Context, arg sqlcgen.UpdateSessionConversationIDParams) (sqlcgen.Session, error) {
	return s.q.UpdateSessionConversationID(ctx, arg)
}

// UpdateIntentDecisionIfNull persists decisionJSON (a marshaled
// intent.IntentDecisionRecord) via the §18.4 write-once guarded update --
// "UPDATE sessions SET intent_decision = ... WHERE intent_decision IS
// NULL", never read-then-write. won reports whether THIS call actually
// set the column (true) or a concurrent/earlier caller already had (false,
// "first decision wins") -- both are success outcomes; only a genuine
// database error is returned as err.
func (s *SessionStore) UpdateIntentDecisionIfNull(ctx context.Context, id pgtype.UUID, decisionJSON []byte) (won bool, err error) {
	rows, err := s.q.UpdateSessionIntentDecisionIfNull(ctx, sqlcgen.UpdateSessionIntentDecisionIfNullParams{
		ID:             id,
		IntentDecision: decisionJSON,
	})
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// ListFailed returns up to limit currently-'failed', unarchived sessions,
// most-recently-failed first -- §16's own needs_attention row source
// (see ListFailedSessions' own generated doc comment for the full design:
// system-wide, no per-user filter, ADMIN-ONLY at the RBAC/httpapi layer).
func (s *SessionStore) ListFailed(ctx context.Context, limit int32) ([]sqlcgen.Session, error) {
	return s.q.ListFailedSessions(ctx, limit)
}

// List returns up to limit unarchived sessions (most-recently-updated
// first), each paired with its current sandbox status (nil when the
// session has no sandbox row yet) -- backs GET /api/sessions (§12.2 item
// 1). See ListSessions' own generated doc comment for the full
// mine_only/join design.
func (s *SessionStore) List(ctx context.Context, arg sqlcgen.ListSessionsParams) ([]sqlcgen.ListSessionsRow, error) {
	return s.q.ListSessions(ctx, arg)
}

// ListOutcomeCountsInWindow returns sinceTime's own (day, status,
// failure_reason) -> count breakdown -- the ONE Postgres read behind
// every session-derived rollup §12.2 item 6's platform-wide analytics view
// needs (internal/domain/platformanalytics). See
// ListSessionOutcomeCountsInWindow's own generated doc comment
// (sqlcgen/sessions.sql.go, sourced from queries/sessions.sql) for the
// full "why aggregate in SQL, not a bounded row fetch" reasoning.
func (s *SessionStore) ListOutcomeCountsInWindow(ctx context.Context, sinceTime pgtype.Timestamptz) ([]sqlcgen.ListSessionOutcomeCountsInWindowRow, error) {
	return s.q.ListSessionOutcomeCountsInWindow(ctx, sinceTime)
}
