//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// pagePlanDatabase creates a database of its own in the shared container,
// migrated to the latest version, and returns a pool of exactly one
// connection on it, so every statement a test runs shares one session's
// prepared-statement cache, as a pool connection's does. The planner's
// statistics are then this database's own: what other tests store leaves
// them alone. Both are dropped at cleanup.
func pagePlanDatabase(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin, adminConnStr := IntegrationTestPoolAndConnStr(t)
	name := fmt.Sprintf("pageplan_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	u, err := url.Parse(adminConnStr)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	u.Path = "/" + name
	connStr := u.String()

	m, mdb := newMigrate(t, connStr)
	if err := m.Up(); err != nil {
		_ = mdb.Close()
		t.Fatalf("migrate %s up: %v", name, err)
	}
	_ = mdb.Close()

	pool, err := narvipg.NewPoolWithMaxConns(ctx, connStr, 1)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	return pool
}

// TestEventPage_GenericPlanReadsTheSessionIndex pins the plan a page read
// settles on (technical plan §6.2, §6.3). pgx caches each statement it
// prepares on a connection, and Postgres, by default
// (plan_cache_mode = auto), may run a cached statement on a generic plan
// from its sixth run on -- a plan made without the parameters' values. On
// a log of few sessions and many events, a young deployment's, the generic
// plan of a page statement written with a plain `id > $2` bound scanned
// the primary key from the cursor and filtered every other session's
// events out: a page read went from about 2 ms to over 100 ms on the same
// connection, growing with the whole table. Written as row comparisons on
// (session_id, id), bounded on both sides, every lookup the walk makes --
// its first event and each next one -- is a range of
// events_session_id_id_idx under any plan, and the page's events are then
// read by their ids.
//
// The log here is 200,000 events of 20 other sessions, interleaved, then
// the read session's 300, analyzed. The test reads a page on one
// connection more times than the planner's custom-plan threshold, so
// every statement the read prepares is in that connection's cache, then
// forces generic plans (plan_cache_mode = force_generic_plan) and reads
// each statement's plan (EXPLAIN EXECUTE): every scan of events must be a
// range of events_session_id_id_idx, or, for the read of the page's own
// events, a lookup of events_pkey by their ids.
func TestEventPage_GenericPlanReadsTheSessionIndex(t *testing.T) {
	ctx := context.Background()
	pool := pagePlanDatabase(ctx, t)
	sessions := narvipg.NewSessionStore(pool)

	others := make([]string, 20)
	for i := range others {
		created, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		others[i] = created.ID.String()
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT ($1::uuid[])[1 + g % 20], 'warning', 'other-' || g, '{"type":"warning","message":"other"}'::jsonb
		FROM generate_series(1, 200000) g`, others); err != nil {
		t.Fatalf("store other sessions' events: %v", err)
	}
	read, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT $1, 'warning', 'read-' || g, '{"type":"warning","message":"read"}'::jsonb
		FROM generate_series(1, 300) g`, read.ID); err != nil {
		t.Fatalf("store the read session's events: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}

	events := narvipg.NewEventStore(pool)
	for i := 0; i < 8; i++ {
		page, err := events.ListPageForSession(ctx, read.ID, 0, 100, platform.FetchHistoryMaxReplyBytes)
		if err != nil {
			t.Fatalf("ListPageForSession: %v", err)
		}
		if len(page.Events) != 100 {
			t.Fatalf("read %d events, want the session's first 100", len(page.Events))
		}
	}

	// Every plan read from here on is the generic one.
	if _, err := pool.Exec(ctx, `SET plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatalf("force generic plans: %v", err)
	}
	for _, tc := range []struct {
		statement string
		args      string
	}{
		{statement: "ListEventPageExtentForSession", args: fmt.Sprintf("'%s'::uuid, 0, 100, %d", read.ID.String(), platform.FetchHistoryMaxReplyBytes)},
		{statement: "ListEventsForSessionByIDs", args: fmt.Sprintf("ARRAY[1, 2, 3]::bigint[], '%s'::uuid", read.ID.String())},
	} {
		var name string
		if err := pool.QueryRow(ctx,
			`SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`,
			"-- name: "+tc.statement+" %").Scan(&name); err != nil {
			t.Fatalf("find %s among the connection's prepared statements: %v", tc.statement, err)
		}
		rows, err := pool.Query(ctx, "EXPLAIN EXECUTE "+pgx.Identifier{name}.Sanitize()+"("+tc.args+")", pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			t.Fatalf("explain %s: %v", tc.statement, err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("read %s's plan: %v", tc.statement, err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read %s's plan: %v", tc.statement, err)
		}
		joined := strings.Join(plan, "\n")
		t.Logf("%s's generic plan:\n%s", tc.statement, joined)
		if reason := pagePlanScansOtherSessions(joined); reason != "" {
			t.Fatalf("%s's generic plan %s", tc.statement, reason)
		}
	}
}

// pagePlanScansOtherSessions returns why plan, the generic plan of a page
// statement, may read events of other sessions than the one it pages, or
// "" when it may not: every scan of events must be a range of
// events_session_id_id_idx bounded by the session, or a lookup of
// events_pkey by exact ids -- never a range of events_pkey, a bitmap or
// sequential scan, which read whatever lies in an id range or the table.
func pagePlanScansOtherSessions(plan string) string {
	lines := strings.Split(plan, "\n")
	scans := 0
	for i, line := range lines {
		switch {
		case strings.Contains(line, "Seq Scan on events"), strings.Contains(line, "Bitmap Heap Scan on events"):
			return "scans events by a sequential or bitmap scan"
		case strings.Contains(line, "Index Scan using events_session_id_id_idx"), strings.Contains(line, "Index Only Scan using events_session_id_id_idx"):
			scans++
		case strings.Contains(line, "using events_pkey"):
			cond := ""
			if i+1 < len(lines) {
				cond = lines[i+1]
			}
			if !strings.Contains(cond, "Index Cond: (id = ANY (") {
				return "scans a range of events_pkey"
			}
			scans++
		case strings.Contains(line, "Scan") && strings.Contains(line, " on events"):
			return "scans events by another index: " + strings.TrimSpace(line)
		}
	}
	if scans == 0 {
		return "reads events through no index this test knows"
	}
	return ""
}

// TestPagePlanScansOtherSessions pins pagePlanScansOtherSessions against
// plans in the shapes Postgres 17 prints for page statements, those of the
// page statements before and after they kept to the session index
// included, so the plan test above fails on a scan that may read other
// sessions' events, and only on one.
func TestPagePlanScansOtherSessions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plan     string
		rejected bool
	}{
		{name: "a range of the session index", plan: `Limit
  ->  Index Scan using events_session_id_id_idx on events e
        Index Cond: ((ROW(session_id, id) > ROW($1, $2)) AND (ROW(session_id, id) <= ROW($1, '9223372036854775807'::bigint)) AND (session_id = $1))`},
		{name: "exact ids on the session index", plan: `Index Scan using events_session_id_id_idx on events
  Index Cond: ((session_id = $2) AND (id = ANY ($1)))`},
		{name: "exact ids on the primary key", plan: `Index Scan using events_pkey on events
  Index Cond: (id = ANY ($1))
  Filter: (session_id = $2)`},
		{name: "a range of the primary key", rejected: true, plan: `Limit
  ->  Index Scan using events_pkey on events e
        Index Cond: (id > $2)
        Filter: (session_id = $1)`},
		{name: "a range of the primary key between two ids", rejected: true, plan: `Index Scan using events_pkey on events
  Index Cond: ((id >= ($1)[1]) AND (id <= ($1)[cardinality($1)]))
  Filter: (session_id = $2)`},
		{name: "a bitmap scan", rejected: true, plan: `Sort
  ->  Bitmap Heap Scan on events
        Recheck Cond: (session_id = $1)
        ->  Bitmap Index Scan on events_session_id_message_id_idx
              Index Cond: (session_id = $1)`},
		{name: "a sequential scan", rejected: true, plan: `Seq Scan on events
  Filter: (session_id = $1)`},
		{name: "another index", rejected: true, plan: `Index Scan using events_session_id_message_id_idx on events
  Index Cond: (session_id = $1)`},
		{name: "no scan of events", rejected: true, plan: `Result`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := pagePlanScansOtherSessions(tc.plan)
			if (reason != "") != tc.rejected {
				t.Fatalf("pagePlanScansOtherSessions = %q, want rejected = %v", reason, tc.rejected)
			}
		})
	}
}
