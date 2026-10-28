package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// UserStore is a thin, pass-through wrapper around the sqlc-generated user
// queries (§13.2 identity graph anchor, §13.3 RBAC role,
// migrations/000002_users.up.sql). No caching, no retries, no business
// rules -- allowlist evaluation and initial-admin assignment are
// internal/adapters/inbound/auth's job (§13.1, "auth v1"); this store
// only ever persists what it's given.
type UserStore struct {
	q *sqlcgen.Queries
}

// NewUserStore builds a UserStore backed by pool.
func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{q: sqlcgen.New(pool)}
}

// WithTx returns a UserStore whose queries run on tx instead of the pool
// this store was built with -- used by internal/adapters/inbound/auth's
// OAuth callback handler, which needs UserStore.Create and
// IdentityStore.Create to run inside the SAME transaction on first
// sign-in (§13.1: "in ONE transaction").
func (s *UserStore) WithTx(tx pgx.Tx) *UserStore {
	return &UserStore{q: s.q.WithTx(tx)}
}

// Create inserts a new user row and returns it. Called exactly once per
// user, at first-sign-in time (or by seeding). UpdateRole and
// FollowPrimaryEmail are the only later writes to it.
func (s *UserStore) Create(ctx context.Context, arg sqlcgen.CreateUserParams) (sqlcgen.User, error) {
	return s.q.CreateUser(ctx, arg)
}

// GetByID fetches a user by id.
func (s *UserStore) GetByID(ctx context.Context, id pgtype.UUID) (sqlcgen.User, error) {
	return s.q.GetUserByID(ctx, id)
}

// GetByPrimaryEmail fetches a user by primary_email (case-insensitive --
// see queries/users.sql's own doc comment on GetUserByPrimaryEmail).
// pgx.ErrNoRows means no user has this email as their primary_email. Not
// evidence for linking (§13.2 step 2): internal/adapters/inbound/auth's
// first sign-in uses it only to refuse an email a member already holds.
func (s *UserStore) GetByPrimaryEmail(ctx context.Context, email string) (sqlcgen.User, error) {
	return s.q.GetUserByPrimaryEmail(ctx, email)
}

// List returns every user, oldest-first -- backs the members API's own
// GET /api/members ("identities + full RBAC", §13.3).
func (s *UserStore) List(ctx context.Context) ([]sqlcgen.User, error) {
	return s.q.ListUsersOrderedByCreatedAt(ctx)
}

// UpdateRole changes a user's role. Backs the admin-only role-change
// endpoint (§13.3's own "members & roles: admin only" row) -- callers gate
// this behind domain/authz.Authorize themselves; this store performs no
// authorization of its own.
func (s *UserStore) UpdateRole(ctx context.Context, id pgtype.UUID, role sqlcgen.UserRole) (sqlcgen.User, error) {
	return s.q.UpdateUserRole(ctx, sqlcgen.UpdateUserRoleParams{ID: id, Role: role})
}

// FollowPrimaryEmail moves user id's primary_email from previous to next
// (§13.2: a user's primary email follows the sign-in identity it came
// from), only while it still is previous, only when next really differs,
// and only when no other user holds next, all ignoring case. Returns the
// number of rows updated: 0 when it did not move. A concurrent exact-case
// insert of next can still raise a unique violation (23505); the caller,
// internal/app/identitylink.RefreshSignInEvidence, runs it in a savepoint
// and absorbs that.
func (s *UserStore) FollowPrimaryEmail(ctx context.Context, id pgtype.UUID, previous, next string) (int64, error) {
	return s.q.FollowUserPrimaryEmail(ctx, sqlcgen.FollowUserPrimaryEmailParams{
		ID:            id,
		PreviousEmail: previous,
		NewEmail:      next,
	})
}

// ListActiveAdminIDsForUpdate returns the id of every currently
// role=admin, disabled=false user, row-locked (FOR UPDATE) for the
// duration of the caller's own transaction -- backs UpdateMemberRole's
// own last-admin guard (an audit finding, H8: demoting the sole
// remaining admin must be refused, not silently allowed). Callers MUST
// run this WithTx, inside the SAME transaction as whatever UpdateRole
// call the guard's verdict decides whether to allow, so a concurrent
// demotion of a DIFFERENT admin blocks on the shared row lock rather
// than both transactions reading a stale headcount and both proceeding.
func (s *UserStore) ListActiveAdminIDsForUpdate(ctx context.Context) ([]pgtype.UUID, error) {
	return s.q.ListActiveAdminIDsForUpdate(ctx)
}
