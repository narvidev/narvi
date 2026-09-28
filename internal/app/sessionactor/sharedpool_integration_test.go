//go:build integration

// This package -- the actor registry's own home package -- is, after
// internal/adapters/inbound/httpapi, the SECOND largest integration
// suite in the repo (~126 test functions across 22 *_integration_test.go
// files, vs. a handful in most sibling packages), and before this file,
// every one of those ~126 tests spun up its OWN brand-new throwaway
// Postgres container via testcontainers-go (integration_helpers_test.go's
// own newTestPool), ran every embedded migration, opened a pool, and tore
// the whole thing down again. Even at a healthy ~1-3s per container start,
// that is 126-378s of pure container-churn overhead alone, before any
// actual test logic runs.
//
// This mirrors, package-for-package, httpapi's own perf/httpapi-
// integration-container-reuse fix (see internal/adapters/inbound/httpapi/
// sharedpool_integration_test.go for the original, more heavily
// documented precedent this file follows): exactly one container/pool for
// the WHOLE test binary instead of one per test, exposed via
// IntegrationTestPool/IntegrationTestPoolAndConnStr (below), with
// per-test isolation preserved via a full TRUNCATE + seed-data restore
// registered as t.Cleanup rather than relying on unique IDs alone.
//
// Unlike httpapi, this package has no white-box/black-box test-file split
// to route around (every *_integration_test.go file here already uses
// `package sessionactor` -- see integration_helpers_test.go's own doc
// comment for why), so IntegrationTestPool does not strictly need to be
// EXPORTED for any other file in this package to reach it. It is exported
// anyway, matching httpapi's own naming exactly, so this pattern reads
// identically wherever it appears in this repo.
//
// # Why this is safe against this package's own async Actor background work
//
// This package IS sessionactor.Registry/Actor's own home -- if shared-
// container reuse were unsafe against async actor work anywhere, this is
// where it would show up first. It is safe here for the exact same
// structural reason httpapi's own version is: every rig this package's
// tests build (NewRegistry, directly or via a handful of small per-file
// wrapper constructors) obtains its pool -- this file's own
// IntegrationTestPool/IntegrationTestPoolAndConnStr, which registers THIS
// file's own truncate-and-restore cleanup -- strictly BEFORE it
// constructs a Registry (which registers ITS OWN t.Cleanup(func() {
// _ = registry.Shutdown() }) afterward). Go's t.Cleanup contract runs
// cleanups in LIFO order, so for every ordinary test in this package,
// without exception: registry.Shutdown() (a synchronous, fully-draining
// wait -- cancels every live actor's own context, then blocks on
// errgroup.Wait until each has actually returned) always finishes BEFORE
// this file's own truncate-and-restore cleanup below ever runs. This
// package also never calls t.Parallel() in any integration-tagged file
// (verified directly), so at most one test's rig -- and therefore at most
// one test's worth of actor goroutines -- is ever alive at a time
// regardless.
//
// Three tests are the deliberate exception, and are NOT converted to the
// shared pool by this change: TestResilience_KillPodMidTurn_
// TurnFailsWithReason_NoStuckProcessing (resilience_killpod_integration_
// test.go), TestResilience_ConcurrentResumeAcrossActors_
// ResumeSandboxCalledAtMostOnce, and TestResilience_
// ConcurrentPlainSpawnAcrossActors_CreateSandboxCalledAtMostOnce (both
// dispatch_integration_test.go). All three call newTestPoolPair
// (resilience_killpod_integration_test.go), which deliberately spins up
// its OWN dedicated, disposable container and hands back TWO independent
// pools simulating two real pods sharing one database -- one of those
// pools (poolA) is by design never gracefully closed, and
// killAdvisoryLockHolder runs an unscoped `pg_terminate_backend` against
// whatever backend currently holds a granted advisory lock, to simulate a
// real `kill -9`. Neither behavior is safe to run against a container
// OTHER tests are still relying on: an unscoped pg_terminate_backend
// could kill an unrelated concurrently-alive test's own connection, and
// the deliberately-never-closed pool would leak a permanently-connected
// client into every later test's shared database for the rest of the
// binary's life. newTestPoolPair is left completely untouched by this
// change specifically so these three tests keep their own private,
// killable container exactly as before.
//
// # Why a full TRUNCATE, not per-test unique IDs alone
//
// Same rationale as httpapi's own version: an audit of this package's own
// ~126 tests found genuine whole-table/unscoped assertions that would
// break under any leftover cross-test state -- e.g. contractdrift_
// integration_test.go's own `SELECT count(*) FROM contract_drift_
// snapshots` expecting an exact count. Reproducing exactly what a fresh
// container already guaranteed (a byte-for-byte-empty, freshly-migrated
// database at the start of every test) via TRUNCATE was judged the
// safer, smaller change here too. prepareDatabaseReset (below) also
// restores any migration-seeded row (e.g. prompt_templates' one seed row,
// migrations/000033_intent_classifier.up.sql) generically, the same way
// httpapi's own version does, so this keeps working unchanged even if a
// future migration seeds a different/additional table.
package sessionactor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/migrations"
)

// sharedPool/sharedConnStr are set exactly once, by TestMain below,
// before any test in this binary runs, and never mutated again.
// truncateStatement/restoreStatements are computed once in the same
// place -- see prepareDatabaseReset's own doc comment.
var (
	sharedPool        *pgxpool.Pool
	sharedConnStr     string
	truncateStatement string
	restoreStatements []string
)

// TestMain replaces this package's old per-test container lifecycle with
// a single one for the whole binary -- see this file's own top doc
// comment for the full container-reuse story. Only one TestMain is
// allowed per test binary, so this is now the ONE place in this package
// that starts/stops Postgres AND wires the global OTel MeterProvider
// contractdrift_integration_test.go's own otelReader needs (that file
// used to own this TestMain itself, before this package also needed a
// shared container -- see its own doc comment).
func TestMain(m *testing.M) {
	ctx := context.Background()

	container, connStr, err := startSharedTestContainer(ctx)
	if err != nil {
		log.Fatalf("sessionactor: start shared integration-test container: %v", err)
	}

	if err := runMigrations(connStr); err != nil {
		log.Fatalf("sessionactor: run migrations against shared integration-test container: %v", err)
	}

	// MaxConns=20, pinned so this package's tests never depend on the
	// host's core count (pgxpool's own default is as few as 4 on a 2-4
	// vCPU GitHub Actions runner). It was chosen when a real CI hang was
	// traced to that default being smaller than this package's own
	// worst-case concurrent-Actor count (6, see TestRepoAccessGate_
	// RepeatedIndeterminateFailures_CircuitBreakerSkipsFurtherNetworkCalls,
	// repoaccessgate_integration_test.go): each live Actor then pinned one
	// pool connection for its whole life, acquired with no bound. Actors
	// pin none now -- every lock lives on the Registry's own lock
	// connection (lockholder.go), and hydration is bounded by
	// ActorHydrateTimeout -- so nothing needs 20 any more, and nothing
	// gains from shrinking it. lockholder_integration_test.go builds its
	// own small pools where a saturated pool is the point.
	pool, err := narvipg.NewPoolWithMaxConns(ctx, connStr, 20)
	if err != nil {
		log.Fatalf("sessionactor: open shared integration-test pool: %v", err)
	}

	if err := prepareDatabaseReset(ctx, pool); err != nil {
		log.Fatalf("sessionactor: prepare shared integration-test database reset: %v", err)
	}

	sharedPool = pool
	sharedConnStr = connStr

	otelReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelReader))
	otel.SetMeterProvider(mp)

	code := m.Run()

	_ = mp.Shutdown(context.Background())
	pool.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		log.Printf("sessionactor: terminate shared integration-test container: %v", err)
	}
	os.Exit(code)
}

// startSharedTestContainer is this package's own former newTestPool's
// (and newTestPoolPair's -- resilience_killpod_integration_test.go, kept
// separately for its own dedicated container) identical container-start
// logic, run exactly once now instead of once per test -- including the
// SAME hardening (an independent watchdog raced against the startup call
// itself, not relying solely on context cancellation) those copies each
// already carried, for the exact reasons documented at each of their own
// original call sites (CI runs 30831633470/30834918806: a stalled
// CI-runner Docker daemon that did not honor context cancellation even
// one layer inside testcontainers-go's own wait strategy).
func startSharedTestContainer(ctx context.Context) (*tcpostgres.PostgresContainer, string, error) {
	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	const containerStartWatchdog = 2*time.Minute + 15*time.Second
	type containerStartResult struct {
		container *tcpostgres.PostgresContainer
		err       error
	}
	startCh := make(chan containerStartResult, 1)
	var startGroup errgroup.Group
	startGroup.Go(func() error {
		container, err := tcpostgres.Run(startCtx, "postgres:17-alpine",
			tcpostgres.WithDatabase("narvi_test"),
			tcpostgres.WithUsername("narvi"),
			tcpostgres.WithPassword("narvi"),
			tcpostgres.BasicWaitStrategies(),
		)
		startCh <- containerStartResult{container: container, err: err}
		return nil
	})

	var container *tcpostgres.PostgresContainer
	var err error
	select {
	case res := <-startCh:
		container, err = res.container, res.err
		if err != nil {
			return nil, "", fmt.Errorf("start postgres container: %w", err)
		}
	case <-time.After(containerStartWatchdog):
		return nil, "", fmt.Errorf("start postgres container: tcpostgres.Run did not return within %s -- Docker daemon likely "+
			"stalled without honoring context cancellation (see this function's own doc comment)", containerStartWatchdog)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, "", fmt.Errorf("connection string: %w", err)
	}
	return container, connStr, nil
}

// runMigrations is this package's former per-test migration-running
// logic, now run exactly once against the shared container above.
func runMigrations(connStr string) error {
	migrateDB, err := sql.Open("pgx", connStr)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer func() { _ = migrateDB.Close() }()

	dbDriver, err := migratepg.WithInstance(migrateDB, &migratepg.Config{})
	if err != nil {
		return fmt.Errorf("migratepg.WithInstance: %w", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("iofs.New: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		return fmt.Errorf("migrate.NewWithInstance: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// prepareDatabaseReset runs once, immediately after migrating, and
// computes the two pieces every per-test reset (resetSharedTestDatabase,
// below) needs: truncateStatement, a single `TRUNCATE TABLE ...
// RESTART IDENTITY CASCADE` naming every real table migrations created
// (discovered from pg_tables, not hand-maintained, so this never drifts
// out of sync with the schema -- CASCADE lets one statement handle every
// FK relationship among them regardless of order); and restoreStatements,
// one `INSERT INTO t SELECT * FROM __seed_snapshot_t` for every table
// that already had at least one row at this point (prompt_templates,
// migrations/000033, plus §25.4's FK-dependent workflow seed rows,
// migrations/000057), restored in FK-dependency order
// (orderTablesForSeedRestore, below) -- each
// backed by a literal `CREATE TABLE __seed_snapshot_t AS TABLE t` copy
// taken here, once, of that pristine post-migration data. schema_
// migrations (golang-migrate's own bookkeeping table) is deliberately
// excluded from both: it must survive untouched, and is never re-applied
// within this same test binary's lifetime anyway.
func prepareDatabaseReset(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		ORDER BY tablename`)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scan tablename: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate tables: %w", err)
	}
	if len(tables) == 0 {
		return fmt.Errorf("no tables found after migrating -- pg_tables query is likely wrong")
	}

	quoted := make([]string, len(tables))
	for i, name := range tables {
		quoted[i] = pgx.Identifier{name}.Sanitize()
	}
	truncateStatement = fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", strings.Join(quoted, ", "))

	restoreOrder, err := orderTablesForSeedRestore(ctx, pool, tables)
	if err != nil {
		return err
	}

	restoreStatements = nil
	for _, name := range restoreOrder {
		quotedName := pgx.Identifier{name}.Sanitize()
		var hasRows bool
		if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s LIMIT 1)", quotedName)).Scan(&hasRows); err != nil {
			return fmt.Errorf("check %s for seed rows: %w", name, err)
		}
		if !hasRows {
			continue
		}
		snapshotName := pgx.Identifier{"__seed_snapshot_" + name}.Sanitize()
		if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s AS TABLE %s", snapshotName, quotedName)); err != nil {
			return fmt.Errorf("snapshot seed data for %s: %w", name, err)
		}
		restoreStatements = append(restoreStatements, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", quotedName, snapshotName))
	}
	return nil
}

// orderTablesForSeedRestore returns tables reordered parents-first over
// pg_constraint's FOREIGN KEY edges (Kahn's algorithm, with the incoming
// pg_tables name order as the deterministic tie-break), so the
// seed-restore INSERTs above only ever insert a child row after the
// parent rows it references already exist. The plain alphabetical order
// this replaces was only ever correct while prompt_templates (FK-free)
// was the sole migration-seeded table -- migrations/000057_workflows.
// up.sql is the first to seed FK-DEPENDENT rows, and workflow_bindings
// sorts alphabetically BEFORE the workflow_definitions rows it
// references, so an unordered restore would violate that FK on every
// reset. Self-referencing FKs are ignored (a table's own rows restore
// in one statement, whose FK checks run at statement end, so
// intra-table parents are always visible); a genuine cross-table FK
// cycle (none exists in this schema) falls back to name order for
// whatever remains rather than failing the whole suite.
func orderTablesForSeedRestore(ctx context.Context, pool *pgxpool.Pool, tables []string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT con.conrelid::regclass::text, con.confrelid::regclass::text
		FROM pg_constraint con
		WHERE con.contype = 'f'
		  AND con.connamespace = 'public'::regnamespace
		  AND con.conrelid <> con.confrelid`)
	if err != nil {
		return nil, fmt.Errorf("list foreign-key edges: %w", err)
	}
	defer rows.Close()

	inSet := make(map[string]bool, len(tables))
	for _, name := range tables {
		inSet[name] = true
	}
	parents := make(map[string]map[string]bool, len(tables))
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, fmt.Errorf("scan foreign-key edge: %w", err)
		}
		if !inSet[child] || !inSet[parent] {
			continue
		}
		if parents[child] == nil {
			parents[child] = make(map[string]bool)
		}
		parents[child][parent] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate foreign-key edges: %w", err)
	}

	ordered := make([]string, 0, len(tables))
	placed := make(map[string]bool, len(tables))
	remaining := append([]string(nil), tables...)
	for len(remaining) > 0 {
		var deferred []string
		progressed := false
		for _, name := range remaining {
			ready := true
			for parent := range parents[name] {
				if !placed[parent] {
					ready = false
					break
				}
			}
			if ready {
				ordered = append(ordered, name)
				placed[name] = true
				progressed = true
			} else {
				deferred = append(deferred, name)
			}
		}
		if !progressed {
			ordered = append(ordered, deferred...)
			break
		}
		remaining = deferred
	}
	return ordered, nil
}

// IntegrationTestPool returns this whole test binary's ONE shared
// Postgres pool (started once by TestMain above) and registers a
// t.Cleanup that resets the database back to the exact same pristine,
// freshly-migrated state a brand-new per-test container used to provide
// -- see this file's own top doc comment for why this is safe against
// this package's own async sessionactor.Actor background work (this
// package's own home turf), and why a full reset (rather than relying on
// unique IDs alone) is the right tradeoff for this specific package's own
// existing tests.
func IntegrationTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := IntegrationTestPoolAndConnStr(t)
	return pool
}

// IntegrationTestPoolAndConnStr is IntegrationTestPool plus the shared
// container's raw connection string (MaxConns unset, unlike sharedPool
// itself), for the rare test that needs to open its OWN
// differently-configured pool against the same database rather than
// reuse the shared-default pool.
func IntegrationTestPoolAndConnStr(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	if sharedPool == nil {
		t.Fatal("sessionactor: IntegrationTestPoolAndConnStr called before TestMain finished setting up the shared pool")
	}
	// Registered before any caller can possibly construct its own
	// sessionactor.Registry (every call site in this package obtains its
	// pool first) -- see this file's own top doc comment for why that
	// ordering, combined with Go's own LIFO t.Cleanup semantics, is what
	// makes this safe.
	t.Cleanup(func() { resetSharedTestDatabase(t) })
	return sharedPool, sharedConnStr
}

// resetSharedTestDatabase truncates every real table and restores
// whatever pristine seed data prepareDatabaseReset snapshotted -- see
// that function's own doc comment.
func resetSharedTestDatabase(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := sharedPool.Exec(ctx, truncateStatement); err != nil {
		t.Fatalf("sessionactor: reset shared integration-test database (truncate): %v", err)
	}
	for _, stmt := range restoreStatements {
		if _, err := sharedPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("sessionactor: reset shared integration-test database (restore seed data): %v", err)
		}
	}
}
