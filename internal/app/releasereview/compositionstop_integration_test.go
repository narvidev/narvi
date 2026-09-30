//go:build integration

// Integration test for releasereview's composition dispatch against a REAL
// Postgres instance: the turn it inserts goes through the production turn
// store, and the test reads what that leaves on the session. The
// testcontainers-Postgres-plus-embedded-migrations newTestPool below is this
// package's own copy of the small helper every DB-touching package keeps
// (internal/app/workflowengine's integration_helpers_test.go says why it is
// not shared). Run via `make test-integration`.
package releasereview_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/migrations"
)

// newTestPool spins up a throwaway Postgres container, runs every embedded
// migration up, and returns a ready *pgxpool.Pool.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

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
			t.Fatalf("start postgres container: %v", err)
		}
	case <-time.After(containerStartWatchdog):
		t.Fatalf("start postgres container: tcpostgres.Run did not return within %s -- Docker daemon likely "+
			"stalled without honoring context cancellation", containerStartWatchdog)
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

	migrateDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = migrateDB.Close() })

	dbDriver, err := migratepg.WithInstance(migrateDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migratepg.WithInstance: %v", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// TestRun_CompositionTurnLeavesAPersonsStopStanding: the release composition
// review turn is one releasereview.Worker inserts with no person behind it,
// through the production turn store, so it leaves a person's stop request
// on the release PR's review session standing (technical plan §3.3: only a
// person's own act clears it, SessionStore.ClearStopRequest's callers). The
// turn is inserted, and sessions.stop_requested_at keeps its instant.
func TestRun_CompositionTurnLeavesAPersonsStopStanding(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var requestedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `UPDATE sessions SET stop_requested_at = now() WHERE id = $1 RETURNING stop_requested_at`, session.ID).Scan(&requestedAt); err != nil {
		t.Fatalf("a person's stop: %v", err)
	}

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	templates := &fakeCompositionTemplateFetcher{template: "Review this release's composition."}
	diffFetcher := &fakeCompositionDiffFetcher{
		pr:               githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"},
		resolveBranchSHA: "main-tip-sha",
		diff:             "diff --git a/x b/x\n+hello\n",
	}
	deps := fullCompositionDeps(lister, &fakeOutboxEnqueuer{}, templates, diffFetcher, &fakeCompositionTurnInserter{}, &fakeCompositionDispatcher{})
	deps.CompositionTurns = narvipg.NewLockedTurnCreator(pool)

	releasereview.Run(ctx, discardLogger(), deps, releasereview.Input{
		SessionID: session.ID,
		Owner:     "acme", Repo: "widgets", PRNumber: 99, BaseRef: "main", HeadRef: "release/1.0", Token: "gho_bottoken",
	})

	all, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("turns = %d, want the one composition review turn", len(all))
	}
	var after pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT stop_requested_at FROM sessions WHERE id = $1`, session.ID).Scan(&after); err != nil {
		t.Fatalf("read stop request: %v", err)
	}
	if !after.Valid || !after.Time.Equal(requestedAt.Time) {
		t.Fatalf("stop_requested_at = %v after the composition turn, want %v: a turn the release review inserts never lifts a person's stop", after, requestedAt)
	}
}
