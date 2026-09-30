//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/narvidev/narvi/migrations"
)

// This file runs migration 000152 (outbox_consecutive_interruptions)
// through golang-migrate against real Postgres, in a database of its own
// migrated to 151 first. The up adds the run of shutdown interruptions
// technical plan §5.1's rule keeps on an outbox row; the down removes it.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling back"
// sections say of the previous binary: that binary's own outbox statements
// -- copied below verbatim from the sqlc output it was built with -- run
// with the column present and leave it alone; it cannot boot on 152; and
// it can once the recorded version is forced back to 151 with the column
// kept, after which this release's migration runs again cleanly.

// previousOutboxColumns is the column list the previous binary's sqlc
// output wrote out for every SELECT * and RETURNING * on outbox.
const previousOutboxColumns = "id, session_id, kind, payload, status, attempts, next_attempt_at, delivered_at, last_error, created_at, correlation_id, suppressed_in_shadow, delivered_to_ledger"

// The previous binary's outbox statements, as its sqlc output sent them.
const (
	previousCreateOutboxEntry = `INSERT INTO outbox (session_id, kind, payload, correlation_id, suppressed_in_shadow)
VALUES ($1, $2, $3, $4, $5)
RETURNING ` + previousOutboxColumns
	previousListDuePendingOutboxEntries = `SELECT ` + previousOutboxColumns + ` FROM outbox
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT $1
FOR UPDATE SKIP LOCKED`
	previousClaimOutboxEntry = `UPDATE outbox
SET attempts = attempts + 1, next_attempt_at = $2
WHERE id = $1 AND status = 'pending'
RETURNING ` + previousOutboxColumns
	previousRenewOutboxClaim = `UPDATE outbox
SET next_attempt_at = $2
WHERE id = $1 AND status = 'pending' AND next_attempt_at = $3
RETURNING ` + previousOutboxColumns
	previousRecordOutboxEntryFailure = `UPDATE outbox
SET next_attempt_at = $2, last_error = $3
WHERE id = $1 AND status = 'pending'
RETURNING ` + previousOutboxColumns
	previousMarkOutboxEntryDelivered = `UPDATE outbox
SET status = 'delivered', delivered_at = now()
WHERE id = $1 AND status = 'pending'
RETURNING ` + previousOutboxColumns
	previousMarkOutboxEntryDeadLetter = `UPDATE outbox
SET status = 'dead_letter', last_error = $2
WHERE id = $1 AND status = 'pending'
RETURNING ` + previousOutboxColumns
	previousGetOutboxEntry = `SELECT ` + previousOutboxColumns + ` FROM outbox
WHERE id = $1`
)

// previousOutboxRow is what the previous binary scans a whole outbox row
// into -- exactly its thirteen columns.
type previousOutboxRow struct {
	id            string
	attempts      int
	nextAttemptAt time.Time
}

func scanPreviousOutboxRow(t *testing.T, row *sql.Row) previousOutboxRow {
	t.Helper()
	var (
		out                                   previousOutboxRow
		sessionID, correlationID, lastError   sql.NullString
		kind, status                          string
		payload                               []byte
		deliveredAt                           sql.NullTime
		createdAt                             time.Time
		suppressedInShadow, deliveredToLedger bool
	)
	if err := row.Scan(&out.id, &sessionID, &kind, &payload, &status, &out.attempts, &out.nextAttemptAt, &deliveredAt, &lastError, &createdAt, &correlationID, &suppressedInShadow, &deliveredToLedger); err != nil {
		t.Fatalf("scan the previous binary's outbox row: %v", err)
	}
	return out
}

// runPreviousBinaryOutbox runs one row through every outbox statement the
// previous binary sends -- enqueue, claim, renew, record a failure, get,
// deliver -- and dead-letters a second, all against the schema as it now
// is. It returns the ids of the two rows it wrote.
func runPreviousBinaryOutbox(ctx context.Context, t *testing.T, db *sql.DB) (delivered, deadLettered string) {
	t.Helper()
	enqueue := func() previousOutboxRow {
		t.Helper()
		return scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousCreateOutboxEntry, nil, "blob_delete", []byte(`{"key":"k"}`), nil, false))
	}
	row := enqueue()
	other := enqueue()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, previousListDuePendingOutboxEntries, 20)
	if err != nil {
		t.Fatalf("the previous binary's list due: %v", err)
	}
	var due int
	for rows.Next() {
		due++
	}
	listErr := rows.Err()
	_ = rows.Close()
	if listErr != nil || due == 0 {
		t.Fatalf("the previous binary's list due = %d rows (%v), want the rows it enqueued", due, listErr)
	}
	claimed := scanPreviousOutboxRow(t, tx.QueryRowContext(ctx, previousClaimOutboxEntry, row.id, time.Now().Add(time.Minute)))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	renewed := scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousRenewOutboxClaim, row.id, time.Now().Add(2*time.Minute), claimed.nextAttemptAt))
	failed := scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousRecordOutboxEntryFailure, row.id, renewed.nextAttemptAt, "remote answered 500"))
	if failed.attempts != 1 {
		t.Fatalf("attempts after the previous binary's claim and failure = %d, want 1", failed.attempts)
	}
	scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousGetOutboxEntry, row.id))
	scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousMarkOutboxEntryDelivered, row.id))
	scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousMarkOutboxEntryDeadLetter, other.id, "gave up"))
	return row.id, other.id
}

// previousBinaryMigrate is golang-migrate as the previous binary runs it at
// boot: the same embedded migrations, without 000152.
func previousBinaryMigrate(t *testing.T, connStr string) (*migrate.Migrate, *sql.DB) {
	t.Helper()
	previous := fstest.MapFS{}
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		version, err := strconv.Atoi(path[:strings.Index(path, "_")])
		if err != nil {
			return err
		}
		if version > 151 {
			return nil
		}
		data, err := fs.ReadFile(migrations.FS, path)
		if err != nil {
			return err
		}
		previous[path] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatalf("assemble the previous binary's migrations: %v", err)
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatal(err)
	}
	dbDriver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := iofs.New(previous, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx", dbDriver)
	if err != nil {
		t.Fatal(err)
	}
	return m, db
}

func TestMigrationOutboxConsecutiveInterruptions_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 151)

	hasColumn := func() bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'outbox' AND column_name = 'consecutive_interruptions'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	run := func(id string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT consecutive_interruptions FROM outbox WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read consecutive_interruptions: %v", err)
		}
		return n
	}
	countRows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if hasColumn() {
		t.Fatal("consecutive_interruptions exists at 151")
	}
	// A row the previous binary enqueued before the migration.
	existing := scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousCreateOutboxEntry, nil, "slack", []byte(`{}`), nil, false))

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(152); err != nil {
		t.Fatalf("up to 152: %v", err)
	}
	if !hasColumn() {
		t.Fatal("consecutive_interruptions missing at 152")
	}
	if got := run(existing.id); got != 0 {
		t.Fatalf("an existing row's run = %d, want 0", got)
	}

	// The previous binary, still running during a rolling deploy, works
	// with the column present and leaves it alone: its rows start at zero,
	// and its failure write keeps whatever run a row already carries.
	delivered, deadLettered := runPreviousBinaryOutbox(ctx, t, db)
	if run(delivered) != 0 || run(deadLettered) != 0 {
		t.Fatalf("runs of the previous binary's rows = %d, %d; want 0, 0", run(delivered), run(deadLettered))
	}
	if _, err := db.ExecContext(ctx, `UPDATE outbox SET consecutive_interruptions = 2 WHERE id = $1`, existing.id); err != nil {
		t.Fatal(err)
	}
	scanPreviousOutboxRow(t, db.QueryRowContext(ctx, previousRecordOutboxEntryFailure, existing.id, time.Now(), "remote answered 500"))
	if got := run(existing.id); got != 2 {
		t.Fatalf("run after the previous binary's failure write = %d, want 2, untouched", got)
	}

	// It cannot boot on 152: golang-migrate refuses a version it has no
	// file for.
	previous, pdb := previousBinaryMigrate(t, connStr)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "152") {
		t.Fatalf("the previous binary's boot on 152 = %v, want a refusal naming 152", err)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force 151, and the previous binary
	// boots and works. Deploying this release again runs 000152 again,
	// which keeps the column and its runs.
	if err := m.Force(151); err != nil {
		t.Fatalf("force 151: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 151 = %v, want no change", err)
	}
	_ = pdb.Close()
	runPreviousBinaryOutbox(ctx, t, db)
	again, adb := newMigrate(t, connStr)
	if err := again.Up(); err != nil {
		t.Fatalf("this release's boot after the rollback = %v, want 000152 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 152)
	if got := run(existing.id); got != 2 {
		t.Fatalf("run after 000152 ran again = %d, want 2, kept", got)
	}

	// Down: the column goes, every row stays; up again works on that state.
	before := countRows()
	if err := m.Migrate(151); err != nil {
		t.Fatalf("down to 151: %v", err)
	}
	if hasColumn() {
		t.Fatal("consecutive_interruptions still exists after the down")
	}
	if after := countRows(); after != before {
		t.Fatalf("%d outbox rows after the down, want %d", after, before)
	}
	if err := m.Migrate(152); err != nil {
		t.Fatalf("up to 152 again: %v", err)
	}
	assertCleanVersion(t, connStr, 152)
}
