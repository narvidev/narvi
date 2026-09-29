//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
)

// This file runs migration 000149 (session_spawn_source_mcp) through
// golang-migrate against real Postgres, in a database of its own migrated
// to 148 first. The up adds the 'mcp' value a session created over MCP
// records (technical plan §43.1). Postgres cannot drop an enum value, so
// the down recreates the type without it, and must refuse while a
// sessions row still holds 'mcp' rather than rewrite that row's source.

func TestMigrationSpawnSourceMcp_UpAndGuardedDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 148)

	labels := func() string {
		t.Helper()
		// Joined by name, never 'session_spawn_source'::regtype: that cast is
		// folded to an oid when pgx prepares and caches the statement, and
		// the down recreates the type under a new oid.
		rows, err := db.QueryContext(ctx, `SELECT e.enumlabel FROM pg_enum e JOIN pg_type ty ON ty.oid = e.enumtypid
			WHERE ty.typname = 'session_spawn_source' ORDER BY e.enumsortorder`)
		if err != nil {
			t.Fatalf("read session_spawn_source labels: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var got []string
		for rows.Next() {
			var label string
			if err := rows.Scan(&label); err != nil {
				t.Fatal(err)
			}
			got = append(got, label)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	sourceOf := func(id string) string {
		t.Helper()
		var source string
		if err := db.QueryRowContext(ctx, `SELECT spawn_source::text FROM sessions WHERE id = $1`, id).Scan(&source); err != nil {
			t.Fatalf("read session %s: %v", id, err)
		}
		return source
	}
	insert := func(source string) (string, error) {
		var id string
		err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ($1::session_spawn_source) RETURNING id`, source).Scan(&id)
		return id, err
	}
	const (
		without = "web,slack,linear,github"
		with    = "web,slack,linear,github,mcp"
	)

	if got := labels(); got != without {
		t.Fatalf("labels at 148 = %q, want %q", got, without)
	}
	if _, err := insert("mcp"); err == nil {
		t.Fatal("an 'mcp' session was accepted at 148, before the value exists")
	}
	webID, err := insert("web")
	if err != nil {
		t.Fatalf("insert a web session at 148: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()

	// Up: the value exists, last in sort order, and a session can record it.
	if err := m.Migrate(149); err != nil {
		t.Fatalf("up to 149: %v", err)
	}
	if got := labels(); got != with {
		t.Fatalf("labels at 149 = %q, want %q", got, with)
	}
	mcpID, err := insert("mcp")
	if err != nil {
		t.Fatalf("insert an mcp session at 149: %v", err)
	}

	// Down with an 'mcp' row: refused, and nothing changed. The file runs as
	// one implicit transaction, so the RAISE rolls the whole down back.
	err = m.Migrate(148)
	if err == nil {
		t.Fatal("down to 148 succeeded while a session records 'mcp', want a refusal")
	}
	if !strings.Contains(err.Error(), "cannot drop session_spawn_source value 'mcp'") {
		t.Errorf("down to 148 = %v, want the guard's refusal", err)
	}
	if got := labels(); got != with {
		t.Errorf("labels after the refused down = %q, want %q unchanged", got, with)
	}
	if got := sourceOf(mcpID); got != "mcp" {
		t.Errorf("the mcp session's source after the refused down = %q, want mcp", got)
	}
	if got := sourceOf(webID); got != "web" {
		t.Errorf("the web session's source after the refused down = %q, want web", got)
	}
	// golang-migrate marked 148 dirty before running the down; the schema is
	// still 149's. The recovery the down file documents is a force to 149.
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != 148 || !dirty {
		t.Errorf("version after the refused down = %d (dirty %v), want 148 dirty", version, dirty)
	}
	if err := m.Force(149); err != nil {
		t.Fatalf("force 149: %v", err)
	}
	assertCleanVersion(t, connStr, 149)

	// Down once no row uses the value: the type is recreated without it and
	// every other session keeps its source.
	if _, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, mcpID); err != nil {
		t.Fatalf("delete the mcp session: %v", err)
	}
	if err := m.Migrate(148); err != nil {
		t.Fatalf("down to 148 with no mcp session: %v", err)
	}
	if got := labels(); got != without {
		t.Errorf("labels after down = %q, want %q", got, without)
	}
	if got := sourceOf(webID); got != "web" {
		t.Errorf("the web session's source after down = %q, want web", got)
	}
	var leftover bool
	if err := db.QueryRowContext(ctx, `SELECT to_regtype('session_spawn_source_old') IS NOT NULL`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover {
		t.Error("session_spawn_source_old survived the down")
	}

	// Up again works on that state.
	if err := m.Migrate(149); err != nil {
		t.Fatalf("up to 149 again: %v", err)
	}
	if got := labels(); got != with {
		t.Errorf("labels after up again = %q, want %q", got, with)
	}
	assertCleanVersion(t, connStr, 149)
}
