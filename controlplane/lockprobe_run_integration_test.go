//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// actorLockConnApplicationNamePrefix begins every sessionactor lock
// connection's application_name (lockholder.go: each dial adds a nonce of
// its own); such a backend is the one this test terminates.
const actorLockConnApplicationNamePrefix = "narvi-actor-locks-"

// TestRun_StartsTheSessionActorLockProbe proves App.Run starts the session
// actors' lock probe (Registry.RunLockProbe) -- nothing else does, and no
// other test runs App.Run. With a 2 s probe interval, on a database of
// its own (so every lock backend on it is this App's): the probe loop dials
// the lock connection before anything hydrates; an actor takes its lock on
// it; the backend is then terminated and nothing but the probe touches the
// connection; within a few intervals the loss is counted
// (session_actor_lock_conn_lost), the actor refuses commands, and a new
// lock connection is dialled. Run then shuts down cleanly. Without the
// probe goroutine, or with a probe that returns at once, no lock
// connection is dialled until something hydrates, and nothing finds the
// loss.
func TestRun_StartsTheSessionActorLockProbe(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)
	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	cfg.Timeouts.ActorLockStatementTimeout = 100 * time.Millisecond
	cfg.Timeouts.ActorLockProbeInterval = 2 * time.Second
	cfg.Timeouts.ActorLockConnectAttemptTimeout = time.Second
	if err := cfg.Timeouts.Validate(); err != nil {
		t.Fatalf("adjusted timeouts: %v", err)
	}

	// The Registry's instruments come from the global meter provider when
	// Build constructs it; read them through a provider of this test's own.
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	var running errgroup.Group
	running.Go(func() error { return app.Run(runCtx, "127.0.0.1:0") })
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		stopRun()
		return running.Wait()
	}
	t.Cleanup(func() { _ = stop() })

	ctx := context.Background()
	lockBackends := func() []int32 {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE starts_with(application_name, $1) AND datname = current_database() ORDER BY pid`, actorLockConnApplicationNamePrefix)
		if err != nil {
			t.Fatalf("list lock backends: %v", err)
		}
		pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		if err != nil {
			t.Fatalf("collect lock backends: %v", err)
		}
		return pids
	}

	var first int32
	waitFor(t, 10*time.Second, "the probe loop to dial the lock connection before anything hydrates", func() bool {
		pids := lockBackends()
		if len(pids) == 1 {
			first = pids[0]
		}
		return len(pids) == 1
	})

	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create a session: %v", err)
	}
	actor, err := app.registry.GetOrSpawn(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if pids := lockBackends(); len(pids) != 1 || pids[0] != first {
		t.Fatalf("lock backends after a hydration = %v, want still only %d", pids, first)
	}

	terminateActorLockBackend(ctx, t, pool, first)

	waitFor(t, 10*time.Second, "the probe to count the lost lock connection", func() bool {
		return sumInt64Counter(ctx, t, reader, "session_actor_lock_conn_lost") == 1
	})
	if err := actor.Send(ctx, sessionactor.TimerFired{Name: "integration_test_timer"}); !errors.Is(err, sessionactor.ErrActorStopped) {
		t.Errorf("Send to the actor whose lock connection the probe found lost = %v, want ErrActorStopped", err)
	}
	waitFor(t, 10*time.Second, "a new lock connection to be dialled", func() bool {
		pids := lockBackends()
		return len(pids) == 1 && pids[0] != first
	})

	if err := stop(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("App.Run after its context was cancelled = %v, want a clean shutdown", err)
	}
}

// terminateActorLockBackend terminates pid, only if it is a session-actor
// lock connection on this test's own database, and waits until it is gone.
func terminateActorLockBackend(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	var terminated bool
	err := pool.QueryRow(ctx, `
		SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
		WHERE pid = $1 AND starts_with(application_name, $2) AND datname = current_database()`,
		pid, actorLockConnApplicationNamePrefix).Scan(&terminated)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pid %d is not a %s* backend on this test's database; refusing to terminate it", pid, actorLockConnApplicationNamePrefix)
	}
	if err != nil || !terminated {
		t.Fatalf("pg_terminate_backend(%d) = (%v, %v), want (true, nil)", pid, terminated, err)
	}
}

// waitFor polls cond every 20 ms until it holds, or fails the test naming
// what it waited for.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sumInt64Counter sums every data point of the int64 counter name that
// reader has collected.
func sumInt64Counter(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}
