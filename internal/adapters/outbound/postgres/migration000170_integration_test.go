//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs the snapshot runtime provenance migration
// (snapshot_runtime_provenance) through golang-migrate against real
// Postgres, in a database of its own migrated to the version before it
// first. The up adds what technical plan §35.5b records of a snapshot: the
// protocol and agent runtime version its minting agent reported
// (sandboxes.snapshot_agent_protocol, snapshot_runtime_version), when it
// was recorded (snapshot_minted_at), and the snapshot id they describe
// (snapshot_provenance_id); the down removes them.
//
// It also pins what the migration's "Rolling deploy" and "Rolling back"
// sections say of the previous binary: its sandbox statements -- copied
// below verbatim from the sqlc output it was built with -- run with the
// columns present and leave them alone, so a snapshot it records, or
// clears, no longer matches snapshot_provenance_id and reads "provenance
// unknown", never the provenance of the snapshot before it; it cannot boot
// on this version; and it can once the recorded version is forced back
// with the columns kept, after which this release's migration runs again
// and keeps their values. This release's own statement is read from the
// sqlc output (generatedQuery), so it cannot drift from what the store
// sends.

// snapshotProvenanceMigration is the migration's version.
const snapshotProvenanceMigration = 170

// The previous binary's sandbox statements, as its sqlc output sent them:
// the column list of every SELECT * and RETURNING * on sandboxes, and the
// two statements that write snapshot_id.
const (
	preProvenanceSandboxColumns = previous167SandboxColumns + ", review_checkout_gen"
	preProvenanceGetSandbox     = `SELECT ` + preProvenanceSandboxColumns + ` FROM sandboxes WHERE session_id = $1`
	preProvenanceUpdateSnapshot = `UPDATE sandboxes
SET snapshot_id = $2, snapshot_suppressed_in_shadow = $3, pending_snapshot_message_id = NULL, updated_at = now()
WHERE session_id = $1
RETURNING ` + preProvenanceSandboxColumns
	preProvenanceClearSnapshot = `UPDATE sandboxes
SET snapshot_id = NULL, snapshot_suppressed_in_shadow = false, updated_at = now()
WHERE session_id = $1 AND snapshot_id IS NOT NULL`
)

// provenanceColumns is the part of a sandbox row the migration is about,
// read by name, with the snapshot id it is keyed to.
type provenanceColumns struct {
	snapshotID, provenanceID      *string
	agentProtocol, runtimeVersion *string
	mintedAt                      sql.NullTime
}

func readProvenanceColumns(ctx context.Context, t *testing.T, db *sql.DB, sessionID string) provenanceColumns {
	t.Helper()
	var c provenanceColumns
	if err := db.QueryRowContext(ctx, `SELECT snapshot_id, snapshot_provenance_id, snapshot_agent_protocol, snapshot_runtime_version, snapshot_minted_at
		FROM sandboxes WHERE session_id = $1`, sessionID).Scan(&c.snapshotID, &c.provenanceID, &c.agentProtocol, &c.runtimeVersion, &c.mintedAt); err != nil {
		t.Fatalf("read the sandbox's provenance columns: %v", err)
	}
	return c
}

// recorded reports whether the columns describe the row's snapshot: the
// id key matches it.
func (c provenanceColumns) recorded() bool {
	return c.snapshotID != nil && c.provenanceID != nil && *c.snapshotID == *c.provenanceID
}

func (c provenanceColumns) String() string {
	str := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	minted := "NULL"
	if c.mintedAt.Valid {
		minted = c.mintedAt.Time.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("snapshot_id %s, snapshot_provenance_id %s, snapshot_agent_protocol %s, snapshot_runtime_version %s, snapshot_minted_at %s",
		str(c.snapshotID), str(c.provenanceID), str(c.agentProtocol), str(c.runtimeVersion), minted)
}

func TestMigrationSnapshotRuntimeProvenance_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previous := versionBefore(t, snapshotProvenanceMigration)
	connStr, db := migrationTestDatabase(ctx, t, previous)
	sandboxColumns := len(strings.Split(preProvenanceSandboxColumns, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE table_name = 'sandboxes' AND column_name IN ('snapshot_provenance_id', 'snapshot_agent_protocol', 'snapshot_runtime_version', 'snapshot_minted_at')`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for rows.Next() {
			var name, shape string
			if err := rows.Scan(&name, &shape); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			got[name] = shape
		}
		iterErr := rows.Err()
		_ = rows.Close()
		if iterErr != nil {
			t.Fatal(iterErr)
		}
		return got
	}

	if got := columns(); len(got) != 0 {
		t.Fatalf("provenance columns at %d: %v, want none", previous, got)
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	previousRow(ctx, t, db, sandboxColumns, `INSERT INTO sandboxes (session_id) VALUES ($1) RETURNING `+preProvenanceSandboxColumns, sessionID)
	// A snapshot recorded before the migration, by the previous binary.
	previousRow(ctx, t, db, sandboxColumns, preProvenanceUpdateSnapshot, sessionID, "snap-before", false)

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(snapshotProvenanceMigration); err != nil {
		t.Fatalf("up to %d: %v", snapshotProvenanceMigration, err)
	}
	want := map[string]string{
		"sandboxes.snapshot_provenance_id":   "YES  text",
		"sandboxes.snapshot_agent_protocol":  "YES  text",
		"sandboxes.snapshot_runtime_version": "YES  text",
		"sandboxes.snapshot_minted_at":       "YES  timestamp with time zone",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at %d = %v, want %v", snapshotProvenanceMigration, got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], shape)
			}
		}
	}

	// A snapshot that exists at the migration has no provenance: no
	// backfill, so it reads "provenance unknown".
	if c := readProvenanceColumns(ctx, t, db, sessionID); c.recorded() || c.agentProtocol != nil || c.runtimeVersion != nil || c.mintedAt.Valid {
		t.Fatalf("a snapshot that existed at the migration reads %v; want its provenance unrecorded", c)
	}

	// This release records a snapshot with what its agent reported.
	updateSnapshot := generatedQuery(t, "sandboxes.sql.go", "updateSandboxSnapshotID")
	if _, err := db.ExecContext(ctx, updateSnapshot, "snap-1", false, "1.24.0", "1.14.19", sessionID); err != nil {
		t.Fatalf("this release's UpdateSandboxSnapshotID: %v", err)
	}
	first := readProvenanceColumns(ctx, t, db, sessionID)
	if !first.recorded() || *first.snapshotID != "snap-1" || first.agentProtocol == nil || *first.agentProtocol != "1.24.0" ||
		first.runtimeVersion == nil || *first.runtimeVersion != "1.14.19" || !first.mintedAt.Valid {
		t.Fatalf("after this release's mint: %v; want snap-1 recorded with 1.24.0 and 1.14.19, and a mint time", first)
	}

	// The previous binary, still running during a rolling deploy: it reads
	// sandboxes with the columns present, and its mint moves snapshot_id
	// alone, so the id key no longer matches -- snap-2 reads "provenance
	// unknown", never snap-1's protocol.
	previousRow(ctx, t, db, sandboxColumns, preProvenanceGetSandbox, sessionID)
	previousRow(ctx, t, db, sandboxColumns, preProvenanceUpdateSnapshot, sessionID, "snap-2", true)
	if c := readProvenanceColumns(ctx, t, db, sessionID); c.recorded() || c.snapshotID == nil || *c.snapshotID != "snap-2" ||
		c.provenanceID == nil || *c.provenanceID != "snap-1" || c.agentProtocol == nil || *c.agentProtocol != "1.24.0" {
		t.Fatalf("after the previous binary's mint: %v; want snap-2 with snap-1's provenance left on snap-1 (not matching)", c)
	}
	// Its clear leaves the provenance on the snapshot it cleared, which no
	// longer matches either.
	if _, err := db.ExecContext(ctx, preProvenanceClearSnapshot, sessionID); err != nil {
		t.Fatalf("the previous binary's ClearSandboxSnapshot: %v", err)
	}
	if c := readProvenanceColumns(ctx, t, db, sessionID); c.recorded() || c.snapshotID != nil {
		t.Fatalf("after the previous binary's clear: %v; want no snapshot and nothing recorded for it", c)
	}
	// This release's next mint records again, over whatever was left.
	if _, err := db.ExecContext(ctx, updateSnapshot, "snap-3", false, nil, nil, sessionID); err != nil {
		t.Fatalf("this release's next mint: %v", err)
	}
	third := readProvenanceColumns(ctx, t, db, sessionID)
	if !third.recorded() || *third.snapshotID != "snap-3" || third.agentProtocol != nil || third.runtimeVersion != nil || !third.mintedAt.Valid {
		t.Fatalf("after this release's mint by an agent that reported nothing: %v; want snap-3 recorded with no protocol and no runtime", third)
	}

	// It cannot boot on this version: golang-migrate refuses a version it
	// has no file for.
	previousMigrate, pdb := previousBinaryMigrate(t, connStr, int(previous))
	if err := previousMigrate.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(snapshotProvenanceMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", snapshotProvenanceMigration, err, snapshotProvenanceMigration)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force the previous version, and
	// the previous binary boots and works. Deploying this release again
	// runs the migration again, which keeps the columns and their values.
	if err := m.Force(int(previous)); err != nil {
		t.Fatalf("force %d: %v", previous, err)
	}
	previousMigrate, pdb = previousBinaryMigrate(t, connStr, int(previous))
	if err := previousMigrate.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force %d = %v, want no change", previous, err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, sandboxColumns, preProvenanceGetSandbox, sessionID)
	// Pinned to this version, like every migration test here: Up would
	// also apply whatever later migrations exist by the time this runs.
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(snapshotProvenanceMigration); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want it applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, snapshotProvenanceMigration)
	if c := readProvenanceColumns(ctx, t, db, sessionID); c.String() != third.String() {
		t.Fatalf("after the migration ran again: %v; want %v, kept", c, third)
	}

	// Down: the columns go, every row stays. Run twice -- the second time
	// on a database a rollback already brought back to the previous
	// version with the columns dropped, forced forward again -- which the
	// IF EXISTS guard allows. Up again works on that state.
	countSandboxes := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sandboxes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countSandboxes()
	if err := m.Migrate(previous); err != nil {
		t.Fatalf("down to %d: %v", previous, err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after the down: %v, want none", got)
	}
	if after := countSandboxes(); after != before {
		t.Fatalf("sandboxes after the down: %d, want %d", after, before)
	}
	previousRow(ctx, t, db, sandboxColumns, preProvenanceGetSandbox, sessionID)
	if err := m.Force(snapshotProvenanceMigration); err != nil {
		t.Fatalf("force %d: %v", snapshotProvenanceMigration, err)
	}
	if err := m.Migrate(previous); err != nil {
		t.Fatalf("down to %d again, the columns already gone: %v", previous, err)
	}
	if err := m.Migrate(snapshotProvenanceMigration); err != nil {
		t.Fatalf("up to %d again: %v", snapshotProvenanceMigration, err)
	}
	assertCleanVersion(t, connStr, snapshotProvenanceMigration)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
