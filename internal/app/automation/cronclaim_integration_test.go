//go:build integration

package automation_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins that a cron occurrence fires once however many replicas
// run the trigger pump: every replica runs it, with no leader, so two
// ticks whose windows hold the same occurrence can both decide to fire it.
// ClaimCronFire is a compare-and-swap on the last fire each tick read, so
// only the first claim wins.

// secondEngine builds another Engine on f's pool and stores: a second
// replica's pump.
func (f *testFixture) secondEngine(t *testing.T) *automation.Engine {
	t.Helper()
	ctx := context.Background()
	registry, err := sessionactor.NewRegistry(ctx, f.pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	engine, err := automation.NewEngine(f.automations, f.invocations, f.runs, f.sessions, f.turns, f.environments, narvipg.NewAuditLogStore(f.pool), f.pool, registry,
		platform.DefaultTimeouts(), false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(f.pool), narvipg.NewGitHubPRSessionStore(f.pool))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

// waitForClaimWaiters waits until n backends of this database wait on a
// lock inside ClaimCronFire, and reports whether they did within ten
// seconds.
func waitForClaimWaiters(ctx context.Context, t *testing.T, pool *pgxpool.Pool, n int) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%name: ClaimCronFire %'`).Scan(&waiting); err != nil {
			t.Fatalf("read the waiting claims: %v", err)
		}
		if waiting >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestCronTrigger_TwoReplicasStraddlingAMinute_FireOnce: two replicas'
// ticks, one each side of a minute boundary, both read the automation
// before either claims, and both windows hold the same occurrence. A
// transaction holding the row lock makes that interleaving certain: A
// reads and blocks in its claim, then B reads and blocks behind it; the
// lock goes, A claims its minute, and B's claim -- of the next minute,
// from the value both read -- finds the row claimed since and creates
// nothing. Exactly one invocation, for an automation that has never
// fired, for one whose first occurrence a freeze held, and for one that
// has fired before.
func TestCronTrigger_TwoReplicasStraddlingAMinute_FireOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// firedBefore is the last fire's distance before the occurrence;
		// 0 is an automation that has never fired.
		firedBefore time.Duration
		// frozenFirst holds the occurrence with a frozen tick first.
		frozenFirst bool
		// a and b are the two replicas' ticks after the occurrence, either
		// side of a minute boundary.
		a, b time.Duration
	}{
		{name: "never fired", a: 59 * time.Second, b: 61 * time.Second},
		{name: "never fired, the occurrence a freeze held", frozenFirst: true, a: 3*time.Minute + 59*time.Second, b: 4*time.Minute + time.Second},
		{name: "fired before", firedBefore: 24 * time.Hour, a: 59 * time.Second, b: 61 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			occurrence := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
			auto := f.createCronAutomation(t, "daily, two replicas", dailyAt(occurrence), sqlcgen.AutomationStatusActive)
			f.setCreatedAt(t, auto.ID, occurrence.Add(-time.Hour))
			if tc.firedBefore > 0 {
				if _, err := f.pool.Exec(ctx, "UPDATE automations SET last_cron_fired_at = $1 WHERE id = $2", occurrence.Add(-tc.firedBefore), auto.ID); err != nil {
					t.Fatalf("record the earlier fire: %v", err)
				}
			}
			if tc.frozenFirst {
				f.freeze(t)
				if err := f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(10*time.Second)); err != nil {
					t.Fatalf("frozen tick: %v", err)
				}
				f.unfreeze(t)
			}
			replicaB := f.secondEngine(t)

			lock, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Rollback(ctx) }()
			if _, err := lock.Exec(ctx, "SELECT 1 FROM automations WHERE id = $1 FOR UPDATE", auto.ID); err != nil {
				t.Fatalf("lock the automation: %v", err)
			}

			var g errgroup.Group
			g.Go(func() error { return f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(tc.a)) })
			blocked := waitForClaimWaiters(ctx, t, f.pool, 1)
			if blocked {
				g.Go(func() error { return replicaB.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(tc.b)) })
				blocked = waitForClaimWaiters(ctx, t, f.pool, 2)
			}
			_ = lock.Rollback(ctx)
			if err := g.Wait(); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if !blocked {
				t.Fatal("the two ticks did not both reach their claim before either claimed: each must decide to fire this occurrence")
			}

			if got := f.countInvocationsForAutomation(t, auto.ID); got != 1 {
				t.Fatalf("invocations after two replicas' ticks = %d, want 1: one occurrence fires once", got)
			}
			fired := f.lastCronFiredAt(t, auto.ID)
			if want := occurrence.Add(tc.a).Truncate(time.Minute); !fired.Valid || !fired.Time.Equal(want) {
				t.Fatalf("last_cron_fired_at = %v, want %v: replica A's claim, the first", fired.Time, want)
			}
		})
	}
}
