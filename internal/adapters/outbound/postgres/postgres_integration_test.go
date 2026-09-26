//go:build integration

// Integration test proving the PR-04 pipeline (embedded migrations +
// sqlc-generated queries + store skeletons) actually works against a real
// Postgres instance. Gated behind the "integration" build tag (needs
// Docker) so it does not run as part of the fast `make test` used by
// PR-01-03 — run via `make test-integration`.
package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/migrations"
)

// newMigrate builds a *migrate.Migrate wired to the embedded migrations
// FS (via the iofs source driver) and a *sql.DB opened against connStr
// (via golang-migrate's postgres database driver), proving the
// migrations.FS embed (§1) actually works — no reading files off disk by
// path.
func newMigrate(t *testing.T, connStr string) (*migrate.Migrate, *sql.DB) {
	t.Helper()

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	dbDriver, err := migratepg.WithInstance(db, &migratepg.Config{})
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

	return m, db
}

func TestSchemaSqlcStoresPipeline(t *testing.T) {
	ctx := context.Background()

	// startCtx bounds the container-startup call below via the ambient
	// context (image pull + Docker daemon round trip + Postgres's own
	// internal ready-wait) -- kept as defense in depth, but NOT solely
	// relied upon any more: CI run 30834918806 showed this exact bound
	// (added after CI run 30831633470's own ContainerStart hang) itself
	// fail to actually cut the call off when the hang recurred one layer
	// deeper, inside testcontainers-go's own wait.(*LogStrategy).
	// WaitUntilReady -- the goroutine dump showed it looping on a 100ms
	// poll for the FULL 10-minute panic window, never once observing
	// ctx.Done(), despite this same context chain being correctly wired
	// all the way through (confirmed directly: reproducing an
	// impossible-to-satisfy wait condition locally against this exact
	// call DOES correctly time out via this same context mechanism, at
	// testcontainers' own hardcoded 60s deadline -- so the mechanism is
	// sound in isolation, but evidently not dependable against whatever a
	// genuinely stalled CI-runner Docker daemon does to it in practice).
	//
	// Rather than keep chasing exactly why context cancellation isn't
	// always honored deep inside a third-party library under conditions
	// this dev machine cannot reproduce, the startup call now ALSO runs on
	// its own goroutine (via errgroup.Group.Go -- no naked `go` statement,
	// §11) raced against an independent, plain time.After watchdog:
	// whichever of "the call returned" or "the watchdog fired" happens
	// first decides the outcome, with no dependency on any context
	// cancellation actually being honored by anything downstream. If the
	// watchdog wins, the goroutine is deliberately abandoned (leaked, not
	// joined) rather than blocking this test's own cleanup on a call that
	// has already demonstrated it can ignore its own cancellation signal.
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
			"stalled without honoring context cancellation (see this function's own doc comment)", containerStartWatchdog)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	}()

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// (a) run the embedded migrations up.
	m, migrateDB := newMigrate(t, connStr)
	defer migrateDB.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// (b) exercise each of the 5 stores' Create/Get (or Upsert/Get)
	// round-trip.

	sessionStore := narvipg.NewSessionStore(pool)
	createdSession, err := sessionStore.Create(ctx, sqlcgen.CreateSessionParams{
		Title:       nil,
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   pgtype.UUID{}, // NULL: no seeded user in this test
	})
	if err != nil {
		t.Fatalf("SessionStore.Create: %v", err)
	}
	if createdSession.SpawnSource != sqlcgen.SessionSpawnSourceWeb {
		t.Fatalf("created session spawn_source = %q, want %q", createdSession.SpawnSource, sqlcgen.SessionSpawnSourceWeb)
	}

	gotSession, err := sessionStore.Get(ctx, createdSession.ID)
	if err != nil {
		t.Fatalf("SessionStore.Get: %v", err)
	}
	if gotSession.ID != createdSession.ID {
		t.Fatalf("SessionStore.Get returned id %v, want %v", gotSession.ID, createdSession.ID)
	}
	if gotSession.Status != sqlcgen.SessionStatusCreated {
		t.Fatalf("session status = %q, want %q", gotSession.Status, sqlcgen.SessionStatusCreated)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	createdSandbox, err := sandboxStore.Create(ctx, createdSession.ID)
	if err != nil {
		t.Fatalf("SandboxStore.Create: %v", err)
	}
	if createdSandbox.Gen != 1 {
		t.Fatalf("sandbox gen = %d, want 1", createdSandbox.Gen)
	}

	gotSandbox, err := sandboxStore.Get(ctx, createdSession.ID)
	if err != nil {
		t.Fatalf("SandboxStore.Get: %v", err)
	}
	if gotSandbox.ID != createdSandbox.ID {
		t.Fatalf("SandboxStore.Get returned id %v, want %v", gotSandbox.ID, createdSandbox.ID)
	}

	outboxStore := narvipg.NewOutboxStore(pool, false)
	createdEntry, err := outboxStore.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: createdSession.ID,
		Kind:      "slack_notify",
		Payload:   []byte(`{"channel":"C123"}`),
	})
	if err != nil {
		t.Fatalf("OutboxStore.Create: %v", err)
	}
	gotEntry, err := outboxStore.Get(ctx, createdEntry.ID)
	if err != nil {
		t.Fatalf("OutboxStore.Get: %v", err)
	}
	if gotEntry.Kind != "slack_notify" {
		t.Fatalf("outbox entry kind = %q, want %q", gotEntry.Kind, "slack_notify")
	}
	if gotEntry.Status != sqlcgen.OutboxStatusPending {
		t.Fatalf("outbox entry status = %q, want %q", gotEntry.Status, sqlcgen.OutboxStatusPending)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createdTurn, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: createdSession.ID,
		Status:    sqlcgen.TurnStatusProcessing,
	})
	if err != nil {
		t.Fatalf("TurnStore.Create (first processing turn): %v", err)
	}
	gotTurn, err := turnStore.Get(ctx, createdTurn.ID)
	if err != nil {
		t.Fatalf("TurnStore.Get: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %q, want %q", gotTurn.Status, sqlcgen.TurnStatusProcessing)
	}

	// (c) exercise turns_one_processing_per_session: a second 'processing'
	// turn for the same session must be rejected by Postgres.
	_, err = turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: createdSession.ID,
		Status:    sqlcgen.TurnStatusProcessing,
	})
	if err == nil {
		t.Fatal("TurnStore.Create (second processing turn) succeeded, want unique_violation error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("second processing turn error = %v (%T), want a *pgconn.PgError", err, err)
	}
	const uniqueViolation = "23505"
	if pgErr.Code != uniqueViolation {
		t.Fatalf("second processing turn error code = %q, want %q (unique_violation); constraint=%q", pgErr.Code, uniqueViolation, pgErr.ConstraintName)
	}
	if pgErr.ConstraintName != "turns_one_processing_per_session" {
		t.Fatalf("second processing turn violated constraint %q, want turns_one_processing_per_session", pgErr.ConstraintName)
	}

	// (d) exercise session_timers UNIQUE(session_id, name) + the
	// ON CONFLICT upsert: upserting the same (session_id, name) twice with
	// different fires_at values must update the row in place, not
	// duplicate it.
	timerStore := narvipg.NewTimerStore(pool)
	firstFiresAt := time.Now().Add(1 * time.Hour).Truncate(time.Microsecond)
	firstTimer, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: createdSession.ID,
		Name:      "turn_deadline",
		FiresAt:   pgtype.Timestamptz{Time: firstFiresAt, Valid: true},
	})
	if err != nil {
		t.Fatalf("TimerStore.Upsert (first arm): %v", err)
	}

	secondFiresAt := firstFiresAt.Add(30 * time.Minute)
	secondTimer, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: createdSession.ID,
		Name:      "turn_deadline",
		FiresAt:   pgtype.Timestamptz{Time: secondFiresAt, Valid: true},
	})
	if err != nil {
		t.Fatalf("TimerStore.Upsert (re-arm): %v", err)
	}

	if secondTimer.ID != firstTimer.ID {
		t.Fatalf("re-arm produced a different row (id %v != %v) — expected an update in place, not a duplicate insert", secondTimer.ID, firstTimer.ID)
	}
	if !secondTimer.FiresAt.Time.Equal(secondFiresAt) {
		t.Fatalf("re-armed fires_at = %v, want %v", secondTimer.FiresAt.Time, secondFiresAt)
	}

	gotTimer, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{
		SessionID: createdSession.ID,
		Name:      "turn_deadline",
	})
	if err != nil {
		t.Fatalf("TimerStore.Get: %v", err)
	}
	if !gotTimer.FiresAt.Time.Equal(secondFiresAt) {
		t.Fatalf("stored fires_at = %v, want %v (the re-armed value, proving update-in-place)", gotTimer.FiresAt.Time, secondFiresAt)
	}

	// (d2) migration 000143 round-trips with data it describes stored: a
	// metadata-document client, its re-fetch failing. The down keeps the
	// client (and whatever was issued under it) while dropping its cache
	// stamps; re-applying the up must stamp it stale -- fetched when it was
	// registered, re-fetched on its next use -- rather than refuse it and
	// leave the schema dirty, which would block every boot after a rollback
	// followed by a roll-forward (technical plan §43.15).
	const clientMetadataVersion = 143
	const docClientID = "https://client.example/mcp/client.json"
	clientStore := narvipg.NewMCPOAuthClientStore(pool)
	fetchedAt := time.Now().Add(-2 * time.Hour)
	docClient, err := clientStore.UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
		ClientID: docClientID, ClientName: "Editor Plugin", RedirectUris: []string{"http://127.0.0.1/callback"},
		MetadataFetchedAt: pgtype.Timestamptz{Time: fetchedAt, Valid: true},
		MetadataStaleAt:   pgtype.Timestamptz{Time: fetchedAt.Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertMetadataDocument: %v", err)
	}
	if _, err := clientStore.MarkMetadataRefetchFailed(ctx, docClient.ID, fetchedAt, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkMetadataRefetchFailed: %v", err)
	}
	if err := m.Migrate(clientMetadataVersion - 1); err != nil {
		t.Fatalf("migrate down past %d with a metadata-document client stored: %v", clientMetadataVersion, err)
	}
	if err := m.Migrate(clientMetadataVersion); err != nil {
		t.Fatalf("re-apply migration %d with a metadata-document client stored: %v", clientMetadataVersion, err)
	}
	if v, dirty, err := m.Version(); err != nil || dirty || v != clientMetadataVersion {
		t.Fatalf("after the round trip: version %d dirty %v err %v, want %d clean", v, dirty, err, clientMetadataVersion)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up after the round trip: %v", err)
	}
	// Read through the migration's own connection: the pool's cached
	// statements describe the table as it was before the round trip.
	var (
		sameClient, stampedAtRegistration, noFailure bool
	)
	if err := migrateDB.QueryRowContext(ctx, `
		SELECT id = $2::uuid, metadata_fetched_at = created_at AND metadata_stale_at = created_at, metadata_refetch_failed_at IS NULL
		FROM mcp_oauth_clients WHERE client_id = $1`, docClientID, docClient.ID.String()).Scan(&sameClient, &stampedAtRegistration, &noFailure); err != nil {
		t.Fatalf("read the metadata-document client after the round trip: %v", err)
	}
	if !sameClient || !stampedAtRegistration || !noFailure {
		t.Fatalf("after the round trip: same client %v, stamped stale at registration %v, no failure recorded %v; want all three", sameClient, stampedAtRegistration, noFailure)
	}

	// (e) run migrations Down() all the way and assert no error.
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down: %v", err)
	}
}
