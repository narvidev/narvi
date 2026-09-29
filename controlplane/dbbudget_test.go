package controlplane

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestConnectionBudget_Fits proves a replica's connection need is its pool
// plus the one lock connection (§5.1), set against what a role without a
// reserved-slot privilege can open.
func TestConnectionBudget_Fits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		b             connectionBudget
		wantNeed      int
		wantAvailable int
		wantFits      bool
	}{
		{"the defaults fit", connectionBudget{MaxConnections: 100, SuperuserReservedConnections: 3, PoolMaxConns: 20}, 21, 97, true},
		{"exactly full fits", connectionBudget{MaxConnections: 24, SuperuserReservedConnections: 3, PoolMaxConns: 20}, 21, 21, true},
		{"the lock connection is the one that does not fit", connectionBudget{MaxConnections: 23, SuperuserReservedConnections: 3, PoolMaxConns: 20}, 21, 20, false},
		{"reserved_connections counts too", connectionBudget{MaxConnections: 30, SuperuserReservedConnections: 3, ReservedConnections: 7, PoolMaxConns: 20}, 21, 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.b.need(); got != tc.wantNeed {
				t.Errorf("need() = %d, want %d", got, tc.wantNeed)
			}
			if got := tc.b.available(); got != tc.wantAvailable {
				t.Errorf("available() = %d, want %d", got, tc.wantAvailable)
			}
			if got := tc.b.fits(); got != tc.wantFits {
				t.Errorf("fits() = %v, want %v", got, tc.wantFits)
			}
		})
	}
}

// TestRegisterPoolMetrics_ObservesPoolStat proves the three db_pool_*
// instruments are read from the pool's own Stat() at collection, and stop
// being reported once unregistered. The pool is never dialled: pgxpool
// connects lazily, and Stat() needs no connection.
func TestRegisterPoolMetrics_ObservesPoolStat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, "postgres://narvi@127.0.0.1:1/narvi?pool_max_conns=7&sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })

	unregister, err := registerPoolMetrics(provider.Meter(meterName), pool)
	if err != nil {
		t.Fatalf("registerPoolMetrics: %v", err)
	}

	got := collectInt64(ctx, t, reader)
	for name, want := range map[string]int64{
		"db_pool_acquired_conns": 0,
		"db_pool_max_conns":      7,
		"db_pool_empty_acquires": 0,
	} {
		v, ok := got[name]
		if !ok {
			t.Errorf("%s not reported; got %v", name, got)
			continue
		}
		if v != want {
			t.Errorf("%s = %d, want %d", name, v, want)
		}
	}

	unregister()
	if after := collectInt64(ctx, t, reader); len(after) != 0 {
		t.Errorf("after unregister, still reported %v", after)
	}
}

// collectInt64 reads every int64 gauge and sum the reader holds, by name.
func collectInt64(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					out[m.Name] += dp.Value
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					out[m.Name] += dp.Value
				}
			}
		}
	}
	return out
}
