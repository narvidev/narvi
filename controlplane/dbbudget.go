package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
)

// meterName is this package's own OTel meter name, following every other
// package's "narvi/<package>" convention.
const meterName = "narvi/controlplane"

// connectionBudgetQuery reads the three settings that bound how many
// connections a non-superuser role can open. reserved_connections exists
// from Postgres 16 on, the oldest server boot accepts
// (platform.MinPostgresServerVersionNum), and serve refuses an older one
// before this runs, so it is read like the other two.
const connectionBudgetQuery = `SELECT current_setting('max_connections')::int,
       current_setting('superuser_reserved_connections')::int,
       current_setting('reserved_connections')::int`

// connectionBudget is this replica's Postgres connection need against the
// server's limits (§5.1): its query pool plus the one lock connection that
// holds every session actor's advisory lock, whatever the replica hosts.
type connectionBudget struct {
	MaxConnections               int
	SuperuserReservedConnections int
	ReservedConnections          int
	PoolMaxConns                 int
}

// need is this replica's own connections: the pool, plus the lock
// connection.
func (b connectionBudget) need() int { return b.PoolMaxConns + 1 }

// available is what a role holding no reserved-slot privilege can open.
func (b connectionBudget) available() int {
	return b.MaxConnections - b.SuperuserReservedConnections - b.ReservedConnections
}

// fits reports whether this one replica's need fits at all. It cannot say
// whether the whole fleet's does -- one process cannot see how many
// replicas there are -- so it is a floor, not the real check (§5.1:
// replicas × (NARVI_DB_POOL_MAX_CONNS + 1) must fit, with headroom).
func (b connectionBudget) fits() bool { return b.need() <= b.available() }

// checkConnectionBudget logs this replica's connection need beside the
// server's limits at boot, and warns when even this one replica does not
// fit. It never refuses to boot: the fleet-wide sum it cannot see is the
// number that matters, and an operator sizing it needs the replica's own
// share in the log either way. The read is bounded by timeout; a failed
// read is logged and skipped -- boot has already read the server's version
// by then, and the migrations that follow surface a database lost since on
// their own.
func checkConnectionBudget(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, timeout time.Duration) {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	b := connectionBudget{PoolMaxConns: int(pool.Config().MaxConns)}
	if err := pool.QueryRow(qctx, connectionBudgetQuery).Scan(
		&b.MaxConnections, &b.SuperuserReservedConnections, &b.ReservedConnections,
	); err != nil {
		logger.Warn("narvi control-plane: could not read postgres connection limits; connection budget not checked", "error", err)
		return
	}

	attrs := []any{
		"replica_need", b.need(),
		"pool_max_conns", b.PoolMaxConns,
		"lock_connections", 1,
		"max_connections", b.MaxConnections,
		"superuser_reserved_connections", b.SuperuserReservedConnections,
		"reserved_connections", b.ReservedConnections,
		"available_connections", b.available(),
	}
	if !b.fits() {
		logger.Warn("narvi control-plane: this replica alone needs more postgres connections than the server allows; "+
			"lower NARVI_DB_POOL_MAX_CONNS or raise max_connections -- the whole fleet needs replicas × (NARVI_DB_POOL_MAX_CONNS + 1)",
			attrs...)
		return
	}
	logger.Info("narvi control-plane: postgres connection budget -- the whole fleet needs replicas × (NARVI_DB_POOL_MAX_CONNS + 1), with headroom",
		attrs...)
}

// registerPoolMetrics registers three observable instruments read from
// pool.Stat() at each collection: how many query-pool connections are out
// right now, the pool's size, and how many acquires ever found the pool
// empty and had to wait. Session actors hold none of the pool's
// connections (their advisory locks live on one separate lock connection,
// §2), so db_pool_acquired_conns tracks query load alone. The returned
// func unregisters the callback.
func registerPoolMetrics(meter metric.Meter, pool *pgxpool.Pool) (func(), error) {
	acquired, err := meter.Int64ObservableGauge(
		"db_pool_acquired_conns",
		metric.WithDescription("Query-pool connections checked out right now. Session actors hold none (their advisory locks live on a separate lock connection, §2), so this is query load alone; at db_pool_max_conns every further acquire waits."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("controlplane: construct db_pool_acquired_conns gauge: %w", err)
	}
	maxConns, err := meter.Int64ObservableGauge(
		"db_pool_max_conns",
		metric.WithDescription("The query pool's size (NARVI_DB_POOL_MAX_CONNS). This replica opens one more connection beside it, for session actors' advisory locks."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("controlplane: construct db_pool_max_conns gauge: %w", err)
	}
	emptyAcquires, err := meter.Int64ObservableCounter(
		"db_pool_empty_acquires",
		metric.WithDescription("Cumulative query-pool acquires that found no idle connection and had to wait for one to be released or opened."),
		metric.WithUnit("{acquire}"),
	)
	if err != nil {
		return nil, fmt.Errorf("controlplane: construct db_pool_empty_acquires counter: %w", err)
	}

	registration, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		stat := pool.Stat()
		o.ObserveInt64(acquired, int64(stat.AcquiredConns()))
		o.ObserveInt64(maxConns, int64(stat.MaxConns()))
		o.ObserveInt64(emptyAcquires, stat.EmptyAcquireCount())
		return nil
	}, acquired, maxConns, emptyAcquires)
	if err != nil {
		return nil, fmt.Errorf("controlplane: register postgres pool metrics callback: %w", err)
	}
	return func() { _ = registration.Unregister() }, nil
}
