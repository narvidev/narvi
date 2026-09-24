//go:build integration

// This file pins runRoutesCommand's own output format to
// controlplane/testdata/routes.golden BYTE FOR BYTE -- §41.1 review round
// 1, finding P1/P3's own proof obligation: the image's "routes" subcommand
// must produce a route table IDENTICAL to the golden, not merely one
// where every golden line happens to resolve on a live HTTP probe (the
// Makefile-level probe this Step's own review replaced -- see that
// target's own doc comment). Deliberately a byte-exact comparison, not
// TestBuild_RouteTableMatchesGolden's own missing/extra-set comparison
// immediately above in build_integration_test.go: a route ADDED to the
// router without a matching golden update fails THIS test (extra line in
// runRoutesCommand's own output, absent from the golden), exactly as a
// route removed without updating the golden already fails
// TestBuild_RouteTableMatchesGolden -- together the two tests cover both
// directions doubly, at two different layers (Build's own live router
// object vs. this command's own stdout-shaped rendering), which is the
// whole point: this test is what a `docker run <image> routes | cmp -
// routes.golden` in CI is actually trusting.
package controlplane

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/narvidev/narvi/extension"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// TestRunRoutesCommand_MatchesGolden is this file's own top doc comment's
// proof obligation.
func TestRunRoutesCommand_MatchesGolden(t *testing.T) {
	setRequiredEnv(t)

	_, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	var buf bytes.Buffer
	if err := runRoutesCommand(t.Context(), &buf); err != nil {
		t.Fatalf("runRoutesCommand: %v", err)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "routes.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), golden) {
		t.Errorf("runRoutesCommand's own output does not match controlplane/testdata/routes.golden BYTE FOR BYTE.\ngot:\n%s\nwant:\n%s", buf.String(), string(golden))
	}
}

// newUnmigratedTestPool is newTestPool's own deliberate opposite (see that
// function's own doc comment in build_integration_test.go): it starts a
// bare postgres:17-alpine container and returns a ready pool plus its
// connection string, WITHOUT ever running a single migration against it.
// Used only by TestRunRoutesCommand_DoesNotMigrate, which needs a database
// runRoutesCommand cannot have secretly migrated out from under it.
func newUnmigratedTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("narvi_test"),
		tcpostgres.WithUsername("narvi"),
		tcpostgres.WithPassword("narvi"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool, connStr
}

// assertDatabaseHasNoTables is TestRunRoutesCommand_DoesNotMigrate's own
// mutation-proof half: it connects independently (not through the pool
// runRoutesCommand itself used) and confirms the public schema is still
// empty after that call returns, i.e. that runRoutesCommand performed no
// DDL of its own.
func assertDatabaseHasNoTables(ctx context.Context, connStr string) error {
	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		return fmt.Errorf("assertDatabaseHasNoTables: NewPool: %w", err)
	}
	defer pool.Close()

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&count); err != nil {
		return fmt.Errorf("assertDatabaseHasNoTables: query: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("database has %d public tables after runRoutesCommand, want 0 -- runRoutesCommand must not apply migrations (§41.1 review round 2, finding Q1/Q3)", count)
	}
	return nil
}

// TestRunRoutesCommand_DoesNotMigrate is §41.1 review round 2, finding
// Q1/Q3's own proof obligation: runRoutesCommand must NOT write to the
// database it inspects. newTestPool below already migrates the schema
// (mirroring every other test in this package), so this test instead
// asserts the read-only property directly, against a database this test
// deliberately leaves UNMIGRATED: runRoutesCommand must still succeed and
// still print the golden route table, proving Build's router construction
// needs no migrated schema and that this command performs no DDL of its
// own that would otherwise be required to make it succeed.
func TestRunRoutesCommand_DoesNotMigrate(t *testing.T) {
	setRequiredEnv(t)

	_, connStr := newUnmigratedTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	var buf bytes.Buffer
	if err := runRoutesCommand(t.Context(), &buf); err != nil {
		t.Fatalf("runRoutesCommand against an unmigrated database: %v (a read-only routes listing must not require a migrated schema)", err)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "routes.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), golden) {
		t.Errorf("runRoutesCommand against an unmigrated database does not match controlplane/testdata/routes.golden BYTE FOR BYTE -- the route table is a static property of the composed router, not of the schema underneath it.\ngot:\n%s\nwant:\n%s", buf.String(), string(golden))
	}

	if err := assertDatabaseHasNoTables(t.Context(), connStr); err != nil {
		t.Error(err)
	}
}

// TestRunRoutesCommand_IncludesModuleRoutes is §41.1 review round 2,
// finding Q6/Q12's own proof obligation: with a module registered,
// runRoutesCommand's printed output must include that module's own
// mounted routes -- exactly what serve() would expose for the same
// composed binary, per this command's own "SAME Build serve() uses"
// doc-comment claim.
func TestRunRoutesCommand_IncludesModuleRoutes(t *testing.T) {
	setRequiredEnv(t)

	_, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	fakeModule := extension.Module{
		Name: "acmetest",
		Mount: func(r chi.Router, _ extension.Runtime) {
			r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
		},
	}

	var buf bytes.Buffer
	if err := runRoutesCommand(t.Context(), &buf, fakeModule); err != nil {
		t.Fatalf("runRoutesCommand with a module: %v", err)
	}

	if !strings.Contains(buf.String(), "/api/ext/acmetest") {
		t.Errorf("runRoutesCommand's output with a registered module does not contain its /api/ext/acmetest route -- routes must print what serve() would serve for the same composed binary.\ngot:\n%s", buf.String())
	}
}
