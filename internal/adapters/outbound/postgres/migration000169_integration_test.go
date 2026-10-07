//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs the workflow_advance_holds migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up creates the table the workflow engine
// holds an automatic advance in while autonomy is frozen (technical plan
// §40.2, §25.9), with its two indexes and its cascades; the down cancels
// every run whose advance is held -- the one kind of candidate a rollback
// loses -- and leaves every other run as it is, then drops the table. It
// also pins what the migration's "Rolling back" section says: the previous
// binary cannot boot once it is applied; forcing the version back keeps the
// table and its rows, and this release's migration runs again over them;
// and the down runs again once the table is gone.

// workflowAdvanceHoldsMigration is this migration's version.
const workflowAdvanceHoldsMigration = 169

func TestMigrationWorkflowAdvanceHolds_DownCancelsHeldRuns(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, workflowAdvanceHoldsMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	hasTable := func() bool {
		t.Helper()
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('workflow_advance_holds') IS NOT NULL`).Scan(&exists); err != nil {
			t.Fatalf("look up the table: %v", err)
		}
		return exists
	}
	if hasTable() {
		t.Fatalf("workflow_advance_holds exists at %d, want it absent", previousVersion)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(workflowAdvanceHoldsMigration); err != nil {
		t.Fatalf("up to %d: %v", workflowAdvanceHoldsMigration, err)
	}
	if !hasTable() {
		t.Fatal("workflow_advance_holds missing after the up")
	}
	for _, index := range []string{"workflow_advance_holds_session_idx", "workflow_advance_holds_held_at_idx"} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil || !exists {
			t.Fatalf("index %s after the up: present %v (err %v), want it", index, exists, err)
		}
	}

	// Four runs of one custom definition, each on a session of its own
	// (workflow_runs_one_running_per_session): held and running, running
	// with no hold, held but completed, and one whose session is deleted.
	var defID, stepID string
	if err := db.QueryRowContext(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'test-holds-migration', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("insert the definition: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert the step: %v", err)
	}
	type seeded struct{ session, run, stepRun string }
	seed := func(runStatus string, held bool) seeded {
		t.Helper()
		var s seeded
		if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&s.session); err != nil {
			t.Fatalf("insert a session: %v", err)
		}
		if err := db.QueryRowContext(ctx, `INSERT INTO workflow_runs (session_id, lane, workflow_definition_id, definition_version, status) VALUES ($1, 'request', $2, 1, $3::workflow_run_status) RETURNING id::text`,
			s.session, defID, runStatus).Scan(&s.run); err != nil {
			t.Fatalf("insert a %s run: %v", runStatus, err)
		}
		if err := db.QueryRowContext(ctx, `INSERT INTO workflow_step_runs (workflow_run_id, step_definition_id, status, outcome_status, finished_at) VALUES ($1, $2, 'completed', 'ok', now()) RETURNING id::text`,
			s.run, stepID).Scan(&s.stepRun); err != nil {
			t.Fatalf("insert a finished attempt: %v", err)
		}
		if held {
			if _, err := db.ExecContext(ctx, `INSERT INTO workflow_advance_holds (workflow_run_id, step_run_id, session_id) VALUES ($1, $2, $3)`, s.run, s.stepRun, s.session); err != nil {
				t.Fatalf("hold the advance: %v", err)
			}
		}
		return s
	}
	heldRunning := seed("running", true)
	notHeld := seed("running", false)
	heldCompleted := seed("completed", true)
	deleted := seed("running", true)

	// One hold per run.
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_advance_holds (workflow_run_id, step_run_id, session_id) VALUES ($1, $2, $3)`,
		heldRunning.run, heldRunning.stepRun, heldRunning.session); err == nil || !strings.Contains(err.Error(), "workflow_advance_holds_pkey") {
		t.Fatalf("a second hold of one run = %v, want the primary key to refuse it", err)
	}
	// The hold goes with its session.
	if _, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, deleted.session); err != nil {
		t.Fatalf("delete a held run's session: %v", err)
	}
	holds := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_advance_holds`).Scan(&n); err != nil {
			t.Fatalf("count the holds: %v", err)
		}
		return n
	}
	if n := holds(); n != 2 {
		t.Fatalf("holds after a held run's session was deleted = %d, want 2: the hold cascades with it", n)
	}

	// The previous binary cannot boot: golang-migrate refuses a version it
	// has no file for.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(workflowAdvanceHoldsMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", workflowAdvanceHoldsMigration, err, workflowAdvanceHoldsMigration)
	}
	_ = pdb.Close()

	// Rolling back with the table kept: force the previous version, and the
	// previous binary boots. Deploying this release again runs the
	// migration again, which keeps the table and its rows.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after the force = %v, want no change", err)
	}
	_ = pdb.Close()
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(workflowAdvanceHoldsMigration); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want it applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, workflowAdvanceHoldsMigration)
	if n := holds(); n != 2 {
		t.Fatalf("holds after the migration ran again = %d, want the 2 kept", n)
	}

	// Down: the held running run is cancelled, every other run is left as
	// it is, and the table goes.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	if hasTable() {
		t.Fatal("workflow_advance_holds still exists after the down")
	}
	for _, tc := range []struct {
		name     string
		run      string
		want     string
		finished bool
	}{
		{name: "the held running run", run: heldRunning.run, want: "cancelled", finished: true},
		{name: "the running run with no hold", run: notHeld.run, want: "running", finished: false},
		{name: "the held completed run", run: heldCompleted.run, want: "completed", finished: false},
	} {
		var status string
		var finished sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT status::text, finished_at FROM workflow_runs WHERE id = $1`, tc.run).Scan(&status, &finished); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if status != tc.want || finished.Valid != tc.finished {
			t.Errorf("%s after the down: %s, finished %v; want %s, finished %v", tc.name, status, finished.Valid, tc.want, tc.finished)
		}
	}
	var attemptStatus string
	if err := db.QueryRowContext(ctx, `SELECT status::text FROM workflow_step_runs WHERE id = $1`, heldRunning.stepRun).Scan(&attemptStatus); err != nil || attemptStatus != "completed" {
		t.Fatalf("the held run's attempt after the down: %s (%v), want kept completed", attemptStatus, err)
	}

	// The down runs again once the table is gone: forced back up with no
	// table, it cancels nothing and drops nothing.
	if err := m.Force(workflowAdvanceHoldsMigration); err != nil {
		t.Fatalf("force %d: %v", workflowAdvanceHoldsMigration, err)
	}
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("the down again with the table gone = %v, want it to run", err)
	}
	var notHeldStatus string
	if err := db.QueryRowContext(ctx, `SELECT status::text FROM workflow_runs WHERE id = $1`, notHeld.run).Scan(&notHeldStatus); err != nil || notHeldStatus != "running" {
		t.Fatalf("the running run with no hold after the down ran again: %s (%v), want running", notHeldStatus, err)
	}

	// Up again: the table is back, empty.
	if err := m.Migrate(workflowAdvanceHoldsMigration); err != nil {
		t.Fatalf("up to %d again: %v", workflowAdvanceHoldsMigration, err)
	}
	assertCleanVersion(t, connStr, workflowAdvanceHoldsMigration)
	if n := holds(); n != 0 {
		t.Fatalf("holds after down and up = %d, want none", n)
	}
}
