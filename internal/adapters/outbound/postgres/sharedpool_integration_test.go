//go:build integration

// This package -- the repo's OWN base Postgres-adapter package, the one
// that defines the store types (narvipg.SessionStore etc.) every other
// package's integration tests import and use -- has ~43 integration test
// functions across 13 *_integration_test.go files (all package
// postgres_test), and before this file, every one of those spun up its
// OWN brand-new throwaway Postgres container via testcontainers-go
// (event_artifact_wstoken_integration_test.go's own newTestPool, reused
// by 11 of this package's own 13 integration files). Even at a healthy
// ~1-3s per container start, that is real container-churn overhead
// across every test run, before any actual test logic runs.
//
// This mirrors, package-for-package, internal/adapters/inbound/httpapi's
// own perf/httpapi-integration-container-reuse fix (see that package's
// own sharedpool_integration_test.go for the original, more heavily
// documented precedent this file follows): exactly one container/pool
// for the WHOLE test binary instead of one per test, exposed via
// IntegrationTestPool/IntegrationTestPoolAndConnStr (below), with
// per-test isolation preserved via a full TRUNCATE + seed-data restore
// registered as t.Cleanup rather than relying on unique IDs alone.
//
// One file is a DELIBERATE exception, and is NOT converted to the shared
// pool by this change: postgres_integration_test.go's own
// TestSchemaSqlcStoresPipeline. It already starts (and fully owns) its
// own dedicated, disposable container -- inline, not via newTestPool --
// and its own step (e) runs the embedded migrations' full `Down()` at
// the end, dropping every table in that container's schema entirely. If
// this test shared the container/pool every OTHER test in this binary
// depends on, its own teardown would drop every table out from under all
// ~42 other tests. postgres_integration_test.go is left completely
// untouched by this change specifically so this test keeps its own
// private, disposable container exactly as before.
//
// # Why no async-actor-drain-ordering concern here (unlike httpapi/sessionactor)
//
// Unlike internal/adapters/inbound/httpapi and internal/app/sessionactor
// itself, this package never imports internal/app/sessionactor at all
// (verified: this package sits BELOW sessionactor in the dependency
// graph -- sessionactor imports this package's store types, not the
// reverse) -- so there is no sessionactor.Registry/Actor async
// background work anywhere in this package's own tests to worry about
// draining before a truncate runs. This package also never calls
// t.Parallel() anywhere (verified), so shared-container reuse is
// materially simpler to reason about here than in httpapi/sessionactor.
//
// # Why a full TRUNCATE, not per-test unique IDs alone
//
// Same rationale as httpapi's own version: an audit of this package's
// own tests found at least one genuine whole-table/unscoped assertion
// that would break under any leftover cross-test state --
// githubprsession_store_integration_test.go's own TestGitHubPRSessionStore_
// ConcurrentClaim_ExactlyOneWinnerSeesNoSession asserts `SELECT count(*)
// FROM sessions WHERE spawn_source = 'github'` == 1, not scoped to its
// own rows alone. Reproducing exactly what a fresh container already
// guaranteed (a byte-for-byte-empty, freshly-migrated database at the
// start of every test) via TRUNCATE was judged the safer, smaller change
// here too, matching httpapi's own documented tradeoff exactly.
// prepareDatabaseReset (below) also restores any migration-seeded row
// (e.g. prompt_templates' one seed row, migrations/000033_intent_
// classifier.up.sql) generically, so this keeps working unchanged even
// if a future migration seeds a different/additional table. Every
// session/turn/artifact/timer ID in this package's own tests is a UUID,
// not a serial int, so `TRUNCATE ... RESTART IDENTITY CASCADE` carries
// no ID-collision surprise here either.
package postgres_test

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

// TestMain replaces this package's old per-test container lifecycle
// (except postgres_integration_test.go's own TestSchemaSqlcStoresPipeline
// -- see this file's own top doc comment for why that one stays on its
// own dedicated container) with a single one for the whole binary. This
// package had no existing TestMain before this change (verified), so
// there is no merge concern here, unlike internal/app/sessionactor's own
// version of this same file.
func TestMain(m *testing.M) {
	ctx := context.Background()

	container, connStr, err := startSharedTestContainer(ctx)
	if err != nil {
		log.Fatalf("postgres: start shared integration-test container: %v", err)
	}

	if err := runMigrations(connStr); err != nil {
		log.Fatalf("postgres: run migrations against shared integration-test container: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		log.Fatalf("postgres: open shared integration-test pool: %v", err)
	}

	if err := prepareDatabaseReset(ctx, pool); err != nil {
		log.Fatalf("postgres: prepare shared integration-test database reset: %v", err)
	}

	sharedPool = pool
	sharedConnStr = connStr

	code := m.Run()

	pool.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		log.Printf("postgres: terminate shared integration-test container: %v", err)
	}
	os.Exit(code)
}

// testPostgresImage is the Postgres image this package's Postgres-backed
// tests start: NARVI_TEST_POSTGRES_IMAGE when set, else postgres:17-alpine,
// the version every suite is tested on. `make
// test-integration-postgres-floor` sets it to the oldest server boot accepts
// (platform.MinPostgresServerVersionNum); the control plane's boot
// test and the session actor's tests read the same variable.
func testPostgresImage() string {
	if image := os.Getenv("NARVI_TEST_POSTGRES_IMAGE"); image != "" {
		return image
	}
	return "postgres:17-alpine"
}

// startSharedTestContainer is this package's own former newTestPool's
// identical container-start logic, run exactly once now instead of once
// per test (except TestSchemaSqlcStoresPipeline's own dedicated
// container, which keeps its own identical inline copy unchanged) --
// including the SAME hardening (an independent watchdog raced against
// the startup call itself, not relying solely on context cancellation)
// that copy already carried, for the exact reasons documented at its own
// original call site (CI runs 30831633470/30834918806: a stalled
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
		container, err := tcpostgres.Run(startCtx, testPostgresImage(),
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
// -- see this file's own top doc comment for why a full reset (rather
// than relying on unique IDs alone) is the right tradeoff for this
// specific package's own existing tests.
func IntegrationTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := IntegrationTestPoolAndConnStr(t)
	return pool
}

// IntegrationTestPoolAndConnStr is IntegrationTestPool plus the shared
// container's raw connection string, for the rare test that needs to
// open its OWN differently-configured pool against the same database
// rather than reuse the shared-default pool.
func IntegrationTestPoolAndConnStr(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	if sharedPool == nil {
		t.Fatal("postgres: IntegrationTestPoolAndConnStr called before TestMain finished setting up the shared pool")
	}
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
		t.Fatalf("postgres: reset shared integration-test database (truncate): %v", err)
	}
	for _, stmt := range restoreStatements {
		if _, err := sharedPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("postgres: reset shared integration-test database (restore seed data): %v", err)
		}
	}
}
