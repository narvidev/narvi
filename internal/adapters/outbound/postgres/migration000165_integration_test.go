//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// This file runs the session spend cap migration (session_spend_cap)
// through golang-migrate against real Postgres, in a database of its own
// migrated to the version before it first. The up adds technical plan
// §40.1's caps -- repo_settings.session_spend_cap_usd and
// automations.session_spend_cap_usd, NUMERIC(10, 2), NULL for no cap --
// their CHECK constraints, which refuse a cap of zero or less and one that
// is no number, and
// automation_runs_session_id_idx, which the session guard reads a session's
// automation through; the down removes them.
//
// It also pins what the migration's "Rolling deploy" and "Rolling back"
// sections say of the previous binary: its statements on repo_settings and
// automations -- copied below from the sqlc output it was built with -- run
// with the columns present and leave them NULL; and once the recorded
// version is forced back with the columns kept, this release's migration
// runs again and keeps their values.

// spendCapMigration is the migration's version; the previous binary's last
// is the one before it.
const spendCapMigration = 165

// The previous binary's statements on the two tables, as its sqlc output
// sent them: the column list of every SELECT * and RETURNING *.
const (
	preSpendCapRepoSettingsColumns = "repo_full_name, block_on_high_risk, created_at, updated_at, sentinel_autofix_enabled, rwx_preview_dispatch_key, rwx_preview_endpoint_template, rwx_preview_org_slug, auto_merge_enabled, max_auto_approve_files_changed, sensitive_blast_radius_tags, auto_retrigger_review_enabled, description_autofix_enabled, review_depth_mode, review_depth_deep_paths, review_cost_budget_light_usd, review_cost_budget_deep_usd, sessions_enabled, live_egress_enabled, live_egress_promoted_at, demotion_sweep_pending_at"
	preSpendCapUpsertRepoSettings  = `INSERT INTO repo_settings (repo_full_name, max_auto_approve_files_changed, sensitive_blast_radius_tags, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (repo_full_name)
DO UPDATE SET max_auto_approve_files_changed = EXCLUDED.max_auto_approve_files_changed, sensitive_blast_radius_tags = EXCLUDED.sensitive_blast_radius_tags, updated_at = now()
RETURNING ` + preSpendCapRepoSettingsColumns
	preSpendCapGetRepoSettings   = `SELECT ` + preSpendCapRepoSettingsColumns + ` FROM repo_settings WHERE repo_full_name = $1`
	preSpendCapAutomationColumns = "id, name, prompt, repos, status, consecutive_failures, created_by, created_at, updated_at, trigger_type, trigger_config, webhook_token_hash, last_cron_fired_at, sandbox_path_scope, sandbox_mock_configured, sandbox_contracts_path, env_vars, last_run_at, last_run_status, artifact_summary, creator_unauthorized_since"
	preSpendCapCreateAutomation  = `INSERT INTO automations (
    name, prompt, repos, created_by,
    trigger_type, trigger_config, webhook_token_hash,
    sandbox_path_scope, sandbox_mock_configured, sandbox_contracts_path,
    env_vars
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING ` + preSpendCapAutomationColumns
	preSpendCapGetAutomation = `SELECT ` + preSpendCapAutomationColumns + ` FROM automations WHERE id = $1`
)

// wantAutomationRunsSessionIndexDef is pg_get_indexdef of the index the
// migration builds.
const wantAutomationRunsSessionIndexDef = "CREATE INDEX automation_runs_session_id_idx ON public.automation_runs USING btree (session_id) WHERE (session_id IS NOT NULL)"

// spendCapSchema is what the migration adds, as the catalog reports it.
type spendCapSchema struct {
	columns     map[string]string // table.column -> "nullable type(precision,scale)"
	constraints map[string]string // name -> definition
	index       string            // pg_get_indexdef, "" when absent
	indexValid  bool
}

func readSpendCapSchema(ctx context.Context, t *testing.T, db *sql.DB) spendCapSchema {
	t.Helper()
	out := spendCapSchema{columns: map[string]string{}, constraints: map[string]string{}}
	rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name,
			is_nullable || ' ' || data_type || '(' || numeric_precision || ',' || numeric_scale || ')'
		FROM information_schema.columns
		WHERE column_name = 'session_spend_cap_usd' AND table_name IN ('repo_settings', 'automations')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, shape string
		if err := rows.Scan(&name, &shape); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		out.columns[name] = shape
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = db.QueryContext(ctx, `SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname IN ('repo_settings_session_spend_cap_usd_positive', 'automations_session_spend_cap_usd_positive')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		out.constraints[name] = def
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	err = db.QueryRowContext(ctx, `SELECT pg_get_indexdef(indexrelid), indisvalid FROM pg_index
		WHERE indexrelid = to_regclass('automation_runs_session_id_idx')`).Scan(&out.index, &out.indexValid)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return out
}

// assertSpendCapSchema fails unless the migration's columns, constraints
// and index are all there, in the shape it builds.
func assertSpendCapSchema(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	got := readSpendCapSchema(ctx, t, db)
	for _, table := range []string{"repo_settings", "automations"} {
		if shape := got.columns[table+".session_spend_cap_usd"]; shape != "YES numeric(10,2)" {
			t.Errorf("%s.session_spend_cap_usd = %q, want a nullable NUMERIC(10, 2)", table, shape)
		}
		if def := got.constraints[table+"_session_spend_cap_usd_positive"]; def != "CHECK (((session_spend_cap_usd > (0)::numeric) AND (session_spend_cap_usd < 'Infinity'::numeric)))" {
			t.Errorf("%s's positive-cap constraint = %q", table, def)
		}
	}
	if got.index != wantAutomationRunsSessionIndexDef || !got.indexValid {
		t.Errorf("automation_runs_session_id_idx = %q (valid %v), want %q", got.index, got.indexValid, wantAutomationRunsSessionIndexDef)
	}
}

// assertNoSpendCapSchema fails if any of it is there.
func assertNoSpendCapSchema(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	got := readSpendCapSchema(ctx, t, db)
	if len(got.columns) != 0 || len(got.constraints) != 0 || got.index != "" {
		t.Errorf("the spend cap schema is present: %+v", got)
	}
}

// TestMigration000165_UpDownUp: the up adds the columns, the constraints
// and the index; a cap of zero or less is refused by the database itself,
// and so is one that is no number -- NaN, which "> 0" alone would admit
// since NUMERIC orders it above every number, refused by the CHECK, and the
// infinities -- and NULL is accepted; the down removes all of it; the up
// runs again.
func TestMigration000165_UpDownUp(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, spendCapMigration-1)
	assertNoSpendCapSchema(ctx, t, db)

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(spendCapMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertSpendCapSchema(ctx, t, db)

	for i, tc := range []struct {
		value   any
		refused bool
		byCheck bool
	}{
		{value: nil},
		{value: "0.01"},
		{value: "99999999.99"},
		{value: "0", refused: true},
		{value: "0.00", refused: true},
		{value: "-1.00", refused: true},
		{value: "NaN", refused: true, byCheck: true},
		{value: "Infinity", refused: true},
		{value: "-Infinity", refused: true},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)`,
			fmt.Sprintf("acme/cap-check-%d", i), tc.value)
		if (err != nil) != tc.refused || (tc.byCheck && !strings.Contains(fmt.Sprint(err), "23514")) {
			t.Errorf("repo_settings cap %v: error %v, want refused %v (by the CHECK: %v)", tc.value, err, tc.refused, tc.byCheck)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO automations (name, repos, session_spend_cap_usd) VALUES ('a', '[]'::jsonb, $1::numeric)`, tc.value)
		if (err != nil) != tc.refused || (tc.byCheck && !strings.Contains(fmt.Sprint(err), "23514")) {
			t.Errorf("automations cap %v: error %v, want refused %v (by the CHECK: %v)", tc.value, err, tc.refused, tc.byCheck)
		}
	}

	if err := m.Migrate(spendCapMigration - 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertNoSpendCapSchema(ctx, t, db)

	if err := m.Migrate(spendCapMigration); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertSpendCapSchema(ctx, t, db)
	assertCleanVersion(t, connStr, spendCapMigration)
}

// TestMigration000165_PreviousBinaryAndTheForcePath: the previous binary's
// statements on repo_settings and automations run with the columns present
// and leave them NULL -- every session it serves has no cap -- and, after
// `migrate force` back to the previous version with the columns kept, the
// up runs again and keeps a cap this release wrote.
func TestMigration000165_PreviousBinaryAndTheForcePath(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, spendCapMigration)

	repoColumns := len(strings.Split(preSpendCapRepoSettingsColumns, ","))
	scanAll := func(row *sql.Row, n int) {
		t.Helper()
		dest := make([]any, n)
		for i := range dest {
			var v any
			dest[i] = &v
		}
		if err := row.Scan(dest...); err != nil {
			t.Fatalf("the previous binary's statement: %v", err)
		}
	}
	scanAll(db.QueryRowContext(ctx, preSpendCapUpsertRepoSettings, "acme/previous", 10, []string{}), repoColumns)
	scanAll(db.QueryRowContext(ctx, preSpendCapGetRepoSettings, "acme/previous"), repoColumns)

	var automationID string
	automationColumns := len(strings.Split(preSpendCapAutomationColumns, ","))
	dest := make([]any, automationColumns)
	for i := range dest {
		var v any
		dest[i] = &v
	}
	dest[0] = &automationID
	if err := db.QueryRowContext(ctx, preSpendCapCreateAutomation, "nightly", nil, "[]", nil, "manual", "{}", nil, nil, false, nil, "[]").Scan(dest...); err != nil {
		t.Fatalf("the previous binary's CreateAutomation: %v", err)
	}
	scanAll(db.QueryRowContext(ctx, preSpendCapGetAutomation, automationID), automationColumns)

	var repoCap, automationCap sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT session_spend_cap_usd::text FROM repo_settings WHERE repo_full_name = 'acme/previous'`).Scan(&repoCap); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT session_spend_cap_usd::text FROM automations WHERE id = $1`, automationID).Scan(&automationCap); err != nil {
		t.Fatal(err)
	}
	if repoCap.Valid || automationCap.Valid {
		t.Fatalf("caps after the previous binary's writes = %v, %v; want NULL, no cap", repoCap, automationCap)
	}

	// This release writes a cap; a rollback forces the version back with
	// the columns kept; the release deployed again runs the up again.
	if _, err := db.ExecContext(ctx, `UPDATE repo_settings SET session_spend_cap_usd = 12.50 WHERE repo_full_name = 'acme/previous'`); err != nil {
		t.Fatal(err)
	}
	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Force(spendCapMigration - 1); err != nil {
		t.Fatalf("force back: %v", err)
	}
	scanAll(db.QueryRowContext(ctx, preSpendCapGetRepoSettings, "acme/previous"), repoColumns)
	if err := m.Migrate(spendCapMigration); err != nil {
		t.Fatalf("up after the force: %v", err)
	}
	assertSpendCapSchema(ctx, t, db)
	if err := db.QueryRowContext(ctx, `SELECT session_spend_cap_usd::text FROM repo_settings WHERE repo_full_name = 'acme/previous'`).Scan(&repoCap); err != nil {
		t.Fatal(err)
	}
	if repoCap.String != "12.50" {
		t.Fatalf("cap after the up ran again = %v, want 12.50 kept", repoCap)
	}
	assertCleanVersion(t, connStr, spendCapMigration)
}

// TestMigration000165_PrebuiltAndInvalidIndexes: the operator's own
// concurrent build of automation_runs_session_id_idx, valid, is kept as it
// is; an INVALID leftover of a failed one is dropped and built again.
func TestMigration000165_PrebuiltAndInvalidIndexes(t *testing.T) {
	ctx := context.Background()
	indexOID := func(db *sql.DB) (int64, bool) {
		t.Helper()
		var oid int64
		var valid bool
		err := db.QueryRowContext(ctx, `SELECT indexrelid::bigint, indisvalid FROM pg_index WHERE indexrelid = to_regclass('automation_runs_session_id_idx')`).Scan(&oid, &valid)
		if err != nil {
			t.Fatalf("read the index: %v", err)
		}
		return oid, valid
	}

	t.Run("a valid pre-built index is kept", func(t *testing.T) {
		connStr, db := migrationTestDatabase(ctx, t, spendCapMigration-1)
		if _, err := db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS automation_runs_session_id_idx
			ON automation_runs (session_id) WHERE session_id IS NOT NULL`); err != nil {
			t.Fatalf("pre-build: %v", err)
		}
		prebuilt, _ := indexOID(db)
		m, mdb := newMigrate(t, connStr)
		defer func() { _ = mdb.Close() }()
		if err := m.Migrate(spendCapMigration); err != nil {
			t.Fatalf("up: %v", err)
		}
		if got, valid := indexOID(db); got != prebuilt || !valid {
			t.Errorf("index oid %d (valid %v) after the up, want the pre-built %d", got, valid, prebuilt)
		}
		assertSpendCapSchema(ctx, t, db)
	})

	t.Run("an INVALID leftover is rebuilt", func(t *testing.T) {
		connStr, db := migrationTestDatabase(ctx, t, spendCapMigration-1)
		// Two runs of one session make a concurrent UNIQUE build of the
		// name fail, leaving an INVALID index behind.
		if _, err := db.ExecContext(ctx, `
			WITH s AS (INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id),
			     a AS (INSERT INTO automations (name, repos) VALUES ('a', '[]'::jsonb) RETURNING id),
			     i AS (INSERT INTO automation_invocations (automation_id, targets, total_runs) SELECT a.id, '[]'::jsonb, 2 FROM a RETURNING id, automation_id)
			INSERT INTO automation_runs (invocation_id, automation_id, target, session_id)
			SELECT i.id, i.automation_id, t.target, s.id FROM i, s, (VALUES ('{"n":1}'::jsonb), ('{"n":2}'::jsonb)) AS t(target)`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX CONCURRENTLY automation_runs_session_id_idx ON automation_runs (session_id) WHERE session_id IS NOT NULL`); err == nil {
			t.Fatal("the failing concurrent build succeeded; the test needs it to fail")
		}
		leftover, valid := indexOID(db)
		if valid {
			t.Fatal("the leftover is valid; the test needs an INVALID one")
		}
		m, mdb := newMigrate(t, connStr)
		defer func() { _ = mdb.Close() }()
		if err := m.Migrate(spendCapMigration); err != nil {
			t.Fatalf("up: %v", err)
		}
		if got, _ := indexOID(db); got == leftover {
			t.Errorf("the index is still the INVALID leftover (oid %d)", got)
		}
		assertSpendCapSchema(ctx, t, db)
		assertCleanVersion(t, connStr, spendCapMigration)
	})
}

// TestMigration000165_ConcurrentMigrators: two control planes migrating at
// once, one waiting on golang-migrate's advisory lock while the other
// applies the migration, both succeed: the index is a plain build.
func TestMigration000165_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, spendCapMigration-1)
	concurrentMigratorsUp(ctx, t, connStr, db, spendCapMigration)
	assertSpendCapSchema(ctx, t, db)
}
