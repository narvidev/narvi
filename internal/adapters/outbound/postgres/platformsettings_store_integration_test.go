//go:build integration

// Integration tests for PlatformSettingsStore (technical plan §40.2): the
// autonomy freeze's one read, which every automatic-action site makes per
// action, and its freeze and unfreeze.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestPlatformSettingsStore_FreezeAndUnfreeze pins the freeze's writes and
// its read: not frozen as migrated; a freeze records who, when and why; a
// second freeze matches nothing and keeps the first; the read sees each
// committed change; an unfreeze returns what it lifted, and a second one
// matches nothing.
func TestPlatformSettingsStore_FreezeAndUnfreeze(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewPlatformSettingsStore(pool)
	t.Cleanup(func() { _, _ = store.Unfreeze(context.Background()) })

	if frozen, err := store.AutonomyFrozen(ctx); err != nil || frozen {
		t.Fatalf("AutonomyFrozen as migrated = (%v, %v), want not frozen", frozen, err)
	}
	admin, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "freezer@example.com", DisplayName: "The Freezer", Role: sqlcgen.UserRoleAdmin})
	if err != nil {
		t.Fatalf("create the admin: %v", err)
	}

	first, err := store.Freeze(ctx, admin.ID, "a bad deploy is merging itself")
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !first.AutonomyFrozen || !first.AutonomyFrozenAt.Valid || first.AutonomyFrozenBy != admin.ID || first.AutonomyFreezeReason == nil || *first.AutonomyFreezeReason != "a bad deploy is merging itself" {
		t.Fatalf("Freeze returned %+v, want frozen by the admin, now, with the reason", first)
	}
	if frozen, err := store.AutonomyFrozen(ctx); err != nil || !frozen {
		t.Fatalf("AutonomyFrozen after the freeze = (%v, %v), want frozen", frozen, err)
	}

	if _, err := store.Freeze(ctx, pgtype.UUID{}, "a second freeze"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a second Freeze = %v, want pgx.ErrNoRows", err)
	}
	got, err := store.GetAutonomyFreeze(ctx)
	if err != nil {
		t.Fatalf("GetAutonomyFreeze: %v", err)
	}
	if !got.AutonomyFrozen || got.AutonomyFrozenBy != admin.ID || got.AutonomyFreezeReason == nil || *got.AutonomyFreezeReason != "a bad deploy is merging itself" ||
		!got.AutonomyFrozenAt.Time.Equal(first.AutonomyFrozenAt.Time) || got.AutonomyFrozenByDisplayName == nil || *got.AutonomyFrozenByDisplayName != "The Freezer" {
		t.Fatalf("the freeze in force after a second freeze = %+v, want the first one kept, with its freezer's name", got)
	}

	lifted, err := store.Unfreeze(ctx)
	if err != nil {
		t.Fatalf("Unfreeze: %v", err)
	}
	if lifted.FrozenBy != admin.ID || lifted.Reason == nil || *lifted.Reason != "a bad deploy is merging itself" || !lifted.FrozenAt.Time.Equal(first.AutonomyFrozenAt.Time) {
		t.Fatalf("Unfreeze returned %+v, want the freeze it lifted", lifted)
	}
	if frozen, err := store.AutonomyFrozen(ctx); err != nil || frozen {
		t.Fatalf("AutonomyFrozen after the unfreeze = (%v, %v), want not frozen", frozen, err)
	}
	if after, err := store.GetAutonomyFreeze(ctx); err != nil || after.AutonomyFrozen || after.AutonomyFrozenAt.Valid || after.AutonomyFrozenBy.Valid || after.AutonomyFreezeReason != nil {
		t.Fatalf("the row after the unfreeze = %+v (err %v), want no freeze and no details", after, err)
	}
	if _, err := store.Unfreeze(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an idle Unfreeze = %v, want pgx.ErrNoRows", err)
	}
}

// TestPlatformSettingsStore_AMissingRowReadsNotFrozen pins the read's
// default: with the row gone, autonomy reads as not frozen -- never an
// error a site would turn into a skip -- and a freeze inserts the row.
func TestPlatformSettingsStore_AMissingRowReadsNotFrozen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewPlatformSettingsStore(pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `INSERT INTO platform_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
		_, _ = store.Unfreeze(context.Background())
	})
	if _, err := pool.Exec(ctx, `DELETE FROM platform_settings`); err != nil {
		t.Fatalf("delete the row: %v", err)
	}

	if frozen, err := store.AutonomyFrozen(ctx); err != nil || frozen {
		t.Fatalf("AutonomyFrozen with no row = (%v, %v), want not frozen", frozen, err)
	}
	if _, err := store.GetAutonomyFreeze(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetAutonomyFreeze with no row = %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.Freeze(ctx, pgtype.UUID{}, "frozen with no row"); err != nil {
		t.Fatalf("Freeze with no row: %v", err)
	}
	if frozen, err := store.AutonomyFrozen(ctx); err != nil || !frozen {
		t.Fatalf("AutonomyFrozen after a freeze inserted the row = (%v, %v), want frozen", frozen, err)
	}
}

// TestPlatformSettingsStore_ReadInsideATransaction pins WithTx: a site's
// read inside its own transaction sees a freeze committed before the
// statement started (READ COMMITTED), and the freeze in its own
// transaction.
func TestPlatformSettingsStore_ReadInsideATransaction(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewPlatformSettingsStore(pool)
	t.Cleanup(func() { _, _ = store.Unfreeze(context.Background()) })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if frozen, err := store.WithTx(tx).AutonomyFrozen(ctx); err != nil || frozen {
		t.Fatalf("the first read in the transaction = (%v, %v), want not frozen", frozen, err)
	}
	if _, err := store.Freeze(ctx, pgtype.UUID{}, "frozen while a site's transaction is open"); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if frozen, err := store.WithTx(tx).AutonomyFrozen(ctx); err != nil || !frozen {
		t.Fatalf("a later read in the same transaction = (%v, %v), want frozen: each statement sees what committed before it", frozen, err)
	}
}

// TestPlatformSettingsStore_ConcurrentUnfreezesLiftOnce pins the
// unfreeze's lock: of two unfreezes of the same freeze at once, exactly
// one lifts it and the other matches nothing, so its lifting is audited
// once.
func TestPlatformSettingsStore_ConcurrentUnfreezesLiftOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewPlatformSettingsStore(pool)
	t.Cleanup(func() { _, _ = store.Unfreeze(context.Background()) })
	for round := 0; round < 5; round++ {
		if _, err := store.Freeze(ctx, pgtype.UUID{}, "frozen for a race"); err != nil {
			t.Fatalf("round %d: Freeze: %v", round, err)
		}
		results := make([]error, 2)
		var g errgroup.Group
		for i := range results {
			g.Go(func() error {
				_, results[i] = store.Unfreeze(ctx)
				return nil
			})
		}
		_ = g.Wait()
		lifted, missed := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				lifted++
			case errors.Is(err, pgx.ErrNoRows):
				missed++
			default:
				t.Fatalf("round %d: Unfreeze: %v", round, err)
			}
		}
		if lifted != 1 || missed != 1 {
			t.Fatalf("round %d: %d unfreezes lifted the freeze and %d matched nothing, want 1 and 1", round, lifted, missed)
		}
	}
}
