//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// This file runs migration 000154 (review_findings_source) through
// golang-migrate against real Postgres, in a database of its own migrated
// to 153 first. The up adds review_findings.reported_source/addition_check
// and review_verdicts.additions_fact_check/additions_fact_check_killed/
// additions_check (technical plan §26.6's amendment); the down removes
// them.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling back"
// sections say of the previous binary: that binary's own review_findings
// and review_verdicts statements -- copied below verbatim from the sqlc
// output it was built with -- run with the columns present, insert NULL
// into them, and leave a re-reported finding's recorded source alone; a
// row written before the migration reads as "no source recorded"; this
// release's upsert overwrites both columns on a re-report; the previous
// binary cannot boot on 154; and it can once the recorded version is
// forced back to 153 with the columns kept, after which this release's
// migration runs again and keeps their values.

// previousFindingColumns/previousVerdictColumns are the column lists the
// previous binary's sqlc output wrote out for every SELECT * and
// RETURNING * on review_findings and review_verdicts.
const (
	previousFindingColumns = "id, repo_full_name, pr_number, identity_hash, sentinel_kind, severity, file_path, line, description, suggested_fix, status, rebuttal_text, rebutted_by, rebutted_at, fix_child_session_id, fix_pr_number, first_seen_at, last_seen_at"
	previousVerdictColumns = "id, repo_full_name, pr_number, head_sha, risk_level, premise, blast_radius, files_changed, tests_coverage, docs_drift, proposed_shippable, shippable, session_id, created_at, digest_summary, digest_arch_decisions, digest_stack_risks, digest_unverified_limits, digest_description_adequacy, digest_adequacy_explanation, digest_proposed_body, review_path, counter_review, fact_check, fact_check_killed, digest_contested_points, suppressed_in_shadow, arch_decision_tags, arch_decision_roots, knowledge_mode, knowledge_influenced, base_ref, base_sha, ancestor_chain, policy_version, attempt_id"
)

// The previous binary's statements, as its sqlc output sent them.
const (
	previousUpsertReviewFinding = `INSERT INTO review_findings (
    repo_full_name, pr_number, identity_hash, sentinel_kind, severity,
    file_path, line, description, suggested_fix
)
VALUES ($1, $2, $3, $7, $4, $5, $8, $6, $9)
ON CONFLICT (repo_full_name, pr_number, identity_hash)
DO UPDATE SET last_seen_at = now()
RETURNING ` + previousFindingColumns
	previousListAllReviewFindingsForPR = `SELECT ` + previousFindingColumns + ` FROM review_findings
WHERE repo_full_name = $1 AND pr_number = $2
ORDER BY first_seen_at ASC`
	previousListReviewFindingStatusesInWindow = `SELECT status FROM review_findings
WHERE repo_full_name = $1 AND first_seen_at > $2
ORDER BY first_seen_at ASC
LIMIT $3`
	previousInsertReviewVerdict = `INSERT INTO review_verdicts (
    repo_full_name, pr_number, head_sha,
    risk_level, premise, blast_radius, files_changed, tests_coverage, docs_drift,
    proposed_shippable, shippable, session_id,
    digest_summary, digest_arch_decisions, digest_stack_risks, digest_unverified_limits,
    digest_description_adequacy, digest_adequacy_explanation, digest_proposed_body,
    review_path,
    counter_review, fact_check, fact_check_killed, digest_contested_points,
    suppressed_in_shadow,
    arch_decision_tags, arch_decision_roots,
    knowledge_mode, knowledge_influenced,
    base_ref, base_sha, ancestor_chain, policy_version, attempt_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34)
RETURNING ` + previousVerdictColumns
)

const migration154Repo = "acme/m154"

// countColumns runs query and returns how many columns its one row has,
// failing unless it returns exactly one row.
func countColumns(ctx context.Context, t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("the previous binary's statement failed: %v\n%s", err, query)
	}
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil || n != 1 {
		t.Fatalf("the previous binary's statement returned %d rows (%v), want 1", n, rowsErr)
	}
	return len(cols)
}

// runPreviousBinaryReviewWrites sends the previous binary's finding upsert
// (for identityHash) and verdict insert, then its two finding reads, and
// checks each sees exactly the columns it was built with.
func runPreviousBinaryReviewWrites(ctx context.Context, t *testing.T, db *sql.DB, identityHash string) {
	t.Helper()
	if n := countColumns(ctx, t, db, previousUpsertReviewFinding,
		migration154Repo, 7, identityHash, "medium", "a.go", "desc "+identityHash, nil, nil, nil); n != 18 {
		t.Fatalf("the previous binary's upsert returned %d columns, want its 18", n)
	}
	if n := countColumns(ctx, t, db, previousInsertReviewVerdict,
		migration154Repo, 7, "sha-m154",
		"low", "ok", "[]", 1, "adequate", "none",
		"auto", "auto", nil,
		"summary", "[]", nil, nil,
		"ok", "accurate", nil,
		"deep",
		"done", "done", 0, nil,
		false,
		"[]", "[]",
		nil, false,
		nil, nil, "[]", 1, nil); n != 36 {
		t.Fatalf("the previous binary's verdict insert returned %d columns, want its 36", n)
	}
	rows, err := db.QueryContext(ctx, previousListAllReviewFindingsForPR, migration154Repo, 7)
	if err != nil {
		t.Fatalf("the previous binary's list all: %v", err)
	}
	cols, _ := rows.Columns()
	_ = rows.Close()
	if len(cols) != 18 {
		t.Fatalf("the previous binary's list all returned %d columns, want 18", len(cols))
	}
	rows, err = db.QueryContext(ctx, previousListReviewFindingStatusesInWindow, migration154Repo, "2000-01-01T00:00:00Z", 100)
	if err != nil {
		t.Fatalf("the previous binary's statuses-in-window: %v", err)
	}
	_ = rows.Close()
}

func TestMigrationReviewFindingsSource_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 153)

	columns := []struct{ table, column string }{
		{"review_findings", "reported_source"},
		{"review_findings", "addition_check"},
		{"review_verdicts", "additions_fact_check"},
		{"review_verdicts", "additions_fact_check_killed"},
		{"review_verdicts", "additions_check"},
	}
	presentColumns := func() int {
		t.Helper()
		n := 0
		for _, c := range columns {
			var nullable string
			err := db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, c.table, c.column).Scan(&nullable)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if nullable != "YES" {
				t.Fatalf("%s.%s is_nullable = %s, want YES", c.table, c.column, nullable)
			}
			n++
		}
		return n
	}
	findingSource := func(identityHash string) (source, check *string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT reported_source, addition_check FROM review_findings WHERE repo_full_name = $1 AND identity_hash = $2`,
			migration154Repo, identityHash).Scan(&source, &check); err != nil {
			t.Fatalf("read %s's source: %v", identityHash, err)
		}
		return source, check
	}
	countRows := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := presentColumns(); n != 0 {
		t.Fatalf("%d of the new columns exist at 153", n)
	}
	// A finding and a verdict the previous binary wrote before the migration.
	runPreviousBinaryReviewWrites(ctx, t, db, "before")

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(154); err != nil {
		t.Fatalf("up to 154: %v", err)
	}
	if n := presentColumns(); n != len(columns) {
		t.Fatalf("%d of %d new columns exist at 154", n, len(columns))
	}
	// A row written before the migration has no source recorded -- it
	// reads as neither primary nor an unverified addition.
	if source, check := findingSource("before"); source != nil || check != nil {
		t.Fatalf("a pre-migration finding reads source %v, check %v; want both NULL", source, check)
	}
	var additionsFactCheck, additionsCheck *string
	var additionsKilled *int32
	if err := db.QueryRowContext(ctx, `SELECT additions_fact_check, additions_fact_check_killed, additions_check FROM review_verdicts WHERE repo_full_name = $1`,
		migration154Repo).Scan(&additionsFactCheck, &additionsKilled, &additionsCheck); err != nil {
		t.Fatal(err)
	}
	if additionsFactCheck != nil || additionsKilled != nil || additionsCheck != nil {
		t.Fatal("a pre-migration verdict records a second fact-check run")
	}

	// This release's upsert records the source and check, and a re-report
	// overwrites them -- the finding's latest publication -- while keeping
	// its status.
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	q := sqlcgen.New(pool)
	upsert := func(source, check string) {
		t.Helper()
		params := sqlcgen.UpsertReviewFindingParams{RepoFullName: migration154Repo, PrNumber: 7, IdentityHash: "before", Severity: "medium", FilePath: "a.go", Description: "desc before", ReportedSource: &source}
		if check != "" {
			params.AdditionCheck = &check
		}
		if _, err := q.UpsertReviewFinding(ctx, params); err != nil {
			t.Fatalf("this release's upsert: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE review_findings SET status = 'rebutted' WHERE identity_hash = 'before'`); err != nil {
		t.Fatal(err)
	}
	upsert("counter_review", "not_found")
	if source, check := findingSource("before"); source == nil || *source != "counter_review" || check == nil || *check != "not_found" {
		t.Fatalf("after this release's upsert: source %v, check %v; want counter_review, not_found", source, check)
	}
	upsert("primary", "")
	if source, check := findingSource("before"); source == nil || *source != "primary" || check != nil {
		t.Fatalf("after a re-report as primary: source %v, check %v; want primary, NULL", source, check)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM review_findings WHERE identity_hash = 'before'`).Scan(&status); err != nil || status != "rebutted" {
		t.Fatalf("status after the re-reports = %q (%v), want rebutted, kept", status, err)
	}
	upsert("counter_review", "checked")
	pool.Close()

	// The previous binary, still running during a rolling deploy, works
	// with the columns present: it inserts NULLs, and its re-report keeps
	// the source and check this release last recorded.
	runPreviousBinaryReviewWrites(ctx, t, db, "during")
	if source, check := findingSource("during"); source != nil || check != nil {
		t.Fatalf("the previous binary's new finding reads source %v, check %v; want NULL", source, check)
	}
	runPreviousBinaryReviewWrites(ctx, t, db, "before")
	if source, check := findingSource("before"); source == nil || *source != "counter_review" || check == nil || *check != "checked" {
		t.Fatalf("after the previous binary's re-report: source %v, check %v; want counter_review, checked, kept", source, check)
	}

	// It cannot boot on 154: golang-migrate refuses a version it has no
	// file for.
	previous, pdb := previousBinaryMigrate(t, connStr, 153)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "154") {
		t.Fatalf("the previous binary's boot on 154 = %v, want a refusal naming 154", err)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force 153, and the previous
	// binary boots and works. Deploying this release again runs 000154
	// again, which keeps the columns and their values.
	if err := m.Force(153); err != nil {
		t.Fatalf("force 153: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, 153)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 153 = %v, want no change", err)
	}
	_ = pdb.Close()
	runPreviousBinaryReviewWrites(ctx, t, db, "rolled-back")
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(154); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want 000154 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 154)
	if source, check := findingSource("before"); source == nil || *source != "counter_review" || check == nil || *check != "checked" {
		t.Fatalf("after 000154 ran again: source %v, check %v; want counter_review, checked, kept", source, check)
	}

	// Down: the columns go, every row stays; up again works on that state.
	findingsBefore, verdictsBefore := countRows("review_findings"), countRows("review_verdicts")
	if err := m.Migrate(153); err != nil {
		t.Fatalf("down to 153: %v", err)
	}
	if n := presentColumns(); n != 0 {
		t.Fatalf("%d of the new columns still exist after the down", n)
	}
	if f, v := countRows("review_findings"), countRows("review_verdicts"); f != findingsBefore || v != verdictsBefore {
		t.Fatalf("after the down: %d findings, %d verdicts; want %d, %d", f, v, findingsBefore, verdictsBefore)
	}
	if err := m.Migrate(154); err != nil {
		t.Fatalf("up to 154 again: %v", err)
	}
	assertCleanVersion(t, connStr, 154)
	if source, check := findingSource("before"); source != nil || check != nil {
		t.Fatalf("after the down and up again: source %v, check %v; want NULL", source, check)
	}
}
