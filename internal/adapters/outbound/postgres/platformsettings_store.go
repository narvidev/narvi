package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// PlatformSettingsStore is a thin, pass-through wrapper around the
// platform_settings queries (technical plan §40.2): the deployment's one
// row of platform-wide settings, whose one setting today is the autonomy
// freeze. Every automatic-action site reads the freeze through
// AutonomyFrozen, by way of internal/app/autonomy.Gate, per action and
// with no cache.
type PlatformSettingsStore struct {
	q *sqlcgen.Queries
}

// NewPlatformSettingsStore builds a PlatformSettingsStore backed by pool.
func NewPlatformSettingsStore(pool *pgxpool.Pool) *PlatformSettingsStore {
	return &PlatformSettingsStore{q: sqlcgen.New(pool)}
}

// WithTx returns a PlatformSettingsStore whose queries run on tx: a site
// that has a transaction of its own reads the freeze inside it.
func (s *PlatformSettingsStore) WithTx(tx pgx.Tx) *PlatformSettingsStore {
	return &PlatformSettingsStore{q: s.q.WithTx(tx)}
}

// AutonomyFrozen reports whether autonomy is frozen. A missing row reads as
// not frozen. An error is the read's own: the caller treats it as a skip,
// never as a pass.
func (s *PlatformSettingsStore) AutonomyFrozen(ctx context.Context) (bool, error) {
	return s.q.GetAutonomyFrozen(ctx)
}

// GetAutonomyFreeze returns the freeze in force with the freezing
// administrator's display name. pgx.ErrNoRows when the row is missing,
// which reads as not frozen.
func (s *PlatformSettingsStore) GetAutonomyFreeze(ctx context.Context) (sqlcgen.GetAutonomyFreezeRow, error) {
	return s.q.GetAutonomyFreeze(ctx)
}

// Freeze freezes autonomy, recording frozenBy (invalid for no user) and
// reason. pgx.ErrNoRows when autonomy is already frozen: the first
// freeze's who, when and why are kept as they are.
func (s *PlatformSettingsStore) Freeze(ctx context.Context, frozenBy pgtype.UUID, reason string) (sqlcgen.PlatformSetting, error) {
	return s.q.FreezeAutonomy(ctx, sqlcgen.FreezeAutonomyParams{FrozenBy: frozenBy, Reason: &reason})
}

// Unfreeze lifts the freeze in force and returns what was lifted.
// pgx.ErrNoRows when autonomy is not frozen.
func (s *PlatformSettingsStore) Unfreeze(ctx context.Context) (sqlcgen.UnfreezeAutonomyRow, error) {
	return s.q.UnfreezeAutonomy(ctx)
}
