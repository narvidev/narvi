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
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/migrations"
)

// combinedCheckoutMigrate is golang-migrate with 000167 and 000168 as they
// would be in one file -- turns altered, then sandboxes, in one implicit
// transaction, as 000167 first shipped -- and every migration before them:
// the control TestMigration000167_168_NeverDeadlockWithAnActorsTransaction
// runs the same transactions against.
func combinedCheckoutMigrate(t *testing.T, connStr string) (*migrate.Migrate, *sql.DB) {
	t.Helper()
	files := fstest.MapFS{}
	read := func(name string) []byte {
		t.Helper()
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		prefix, _, found := strings.Cut(path, "_")
		if !found {
			return nil
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return err
		}
		if version < reviewCheckoutMigration {
			files[path] = &fstest.MapFile{Data: read(path)}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	up := append(read("000167_review_turn_checkout.up.sql"), read("000168_sandbox_review_checkout_gen.up.sql")...)
	down := append(read("000168_sandbox_review_checkout_gen.down.sql"), read("000167_review_turn_checkout.down.sql")...)
	files["000167_combined.up.sql"] = &fstest.MapFile{Data: up}
	files["000167_combined.down.sql"] = &fstest.MapFile{Data: down}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatal(err)
	}
	dbDriver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := iofs.New(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx", dbDriver)
	if err != nil {
		t.Fatal(err)
	}
	return m, db
}

// TestMigration000167_168_NeverDeadlockWithAnActorsTransaction runs the
// boot's migrations beside a transaction shaped like the session actor's
// dispatch evaluation -- its session row locked FOR UPDATE, then its
// sandbox read, then its turns -- in the two orders that make a cycle with
// a file that holds turns while it asks for sandboxes:
//
//   - a long read of turns holds the migration's first lock, the actor
//     reads sandboxes and then queues behind the migration on turns, and
//     the long read ends;
//   - the actor has read sandboxes when the migration starts, and reads
//     turns while the migration waits.
//
// 000167 alters turns alone and 000168 sandboxes alone, each in a
// transaction of its own, so neither holds a lock while it asks for
// another: the migrations wait their turn and apply, clean, and the actor
// commits. The control runs the same transactions against one file that
// alters turns then sandboxes -- 000167 as it first shipped -- and
// Postgres finds a deadlock and aborts whichever of the two waited first:
// the actor, when the long read held the migration before it (its
// evaluation fails), or the migration, when the actor had read sandboxes
// before the migration started (the version is left dirty, and every boot
// is refused until `migrate force`). Either way the test reproduces the
// cycle it guards against. deadlock_timeout is set to 200ms for the
// database so a check fires within the test's waits.
func TestMigration000167_168_NeverDeadlockWithAnActorsTransaction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		combined   bool
		longReader bool
	}{
		{name: "a long read of turns first", longReader: true},
		{name: "the actor's read of sandboxes first"},
		{name: "control, one file: a long read of turns first", combined: true, longReader: true},
		{name: "control, one file: the actor's read of sandboxes first", combined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			connStr, db := migrationTestDatabase(ctx, t, reviewCheckoutMigration-1,
				`DO $$ BEGIN EXECUTE format('ALTER DATABASE %I SET deadlock_timeout = %L', current_database(), '200ms'); END $$`)
			var sessionID string
			if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('github') RETURNING id::text`).Scan(&sessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO sandboxes (session_id, status) VALUES ($1, 'ready')`, sessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO turns (session_id, status) VALUES ($1, 'pending')`, sessionID); err != nil {
				t.Fatal(err)
			}

			m, mdb := newMigrate(t, connStr)
			target := uint(reviewCheckoutGenMigration)
			if tc.combined {
				m, mdb = combinedCheckoutMigrate(t, connStr)
				target = reviewCheckoutMigration
			}
			defer func() { _ = mdb.Close() }()

			waiting := func(table, mode string) func() bool {
				return func() bool {
					var n int
					if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
						WHERE c.relname = $1 AND l.mode = $2 AND NOT l.granted`, table, mode).Scan(&n); err != nil {
						t.Errorf("read pg_locks: %v", err)
						return true
					}
					return n > 0
				}
			}
			wait := func(what string, cond func() bool) {
				t.Helper()
				deadline := time.Now().Add(20 * time.Second)
				for !cond() {
					if time.Now().After(deadline) {
						t.Fatalf("never saw %s", what)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}

			actorConn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = actorConn.Close() }()
			actor, err := actorConn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = actor.Rollback() }()

			var readerConn *sql.Conn
			var reader *sql.Tx
			if tc.longReader {
				if readerConn, err = db.Conn(ctx); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = readerConn.Close() }()
				if reader, err = readerConn.BeginTx(ctx, nil); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = reader.Rollback() }()
				if _, err := reader.ExecContext(ctx, `SELECT count(*) FROM turns`); err != nil {
					t.Fatal(err)
				}
			} else {
				// The actor's evaluation is under way: session, then sandbox.
				if _, err := actor.ExecContext(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
					t.Fatal(err)
				}
				if _, err := actor.ExecContext(ctx, `SELECT gen FROM sandboxes WHERE session_id = $1`, sessionID); err != nil {
					t.Fatal(err)
				}
			}

			var migrations errgroup.Group
			migrations.Go(func() error { return m.Migrate(target) })

			var actorRead errgroup.Group
			if tc.longReader {
				wait("the migration queued on turns", waiting("turns", "AccessExclusiveLock"))
				if _, err := actor.ExecContext(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
					t.Fatal(err)
				}
				if _, err := actor.ExecContext(ctx, `SELECT gen FROM sandboxes WHERE session_id = $1`, sessionID); err != nil {
					t.Fatal(err)
				}
				actorRead.Go(func() error {
					_, err := actor.ExecContext(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID)
					return err
				})
				wait("the actor queued on turns", waiting("turns", "AccessShareLock"))
				if err := reader.Commit(); err != nil {
					t.Fatal(err)
				}
			} else {
				wait("the migration waiting on sandboxes", waiting("sandboxes", "AccessExclusiveLock"))
				actorRead.Go(func() error {
					_, err := actor.ExecContext(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID)
					return err
				})
			}
			actorErr := actorRead.Wait()
			if actorErr == nil {
				actorErr = actor.Commit()
			}
			migErr := migrations.Wait()

			version, dirty, verr := m.Version()
			if verr != nil {
				t.Fatalf("read the version: %v", verr)
			}
			deadlocked := func(err error) bool { return err != nil && strings.Contains(err.Error(), "deadlock detected") }
			if tc.combined {
				switch {
				case deadlocked(actorErr) && migErr == nil && version == uint(reviewCheckoutMigration) && !dirty:
					// The actor waited first, so it was aborted; the
					// migration then applied.
				case deadlocked(migErr) && actorErr == nil && dirty:
					// The migration waited first: aborted, its version dirty.
				default:
					t.Fatalf("one file altering turns then sandboxes: actor = %v, migrate = %v, version %d dirty %v; want one of them aborted as a deadlock -- the control did not reproduce the cycle", actorErr, migErr, version, dirty)
				}
				return
			}
			if actorErr != nil {
				t.Fatalf("the actor's transaction: %v, want it to read turns and commit", actorErr)
			}
			if migErr != nil && !errors.Is(migErr, migrate.ErrNoChange) {
				t.Fatalf("migrate = %v, want 000167 and 000168 applied", migErr)
			}
			if version != uint(reviewCheckoutGenMigration) || dirty {
				t.Fatalf("version %d dirty %v, want %d clean", version, dirty, reviewCheckoutGenMigration)
			}
		})
	}
}
