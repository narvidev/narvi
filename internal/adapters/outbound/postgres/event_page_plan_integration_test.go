//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

const (
	// pageLookupMaxBuffers bounds the buffers one lookup of the page walk
	// reads, on average over its loops: a descent of
	// events_session_id_id_idx to the cursor and the event's heap page --
	// four on these logs -- with room to spare. A lookup that reads the
	// session's entries up to the cursor, or the rest of the session to
	// sort them, reads hundreds.
	pageLookupMaxBuffers = 10
	// pageMaxRowsRemoved bounds the rows a scan of events filters out, per
	// loop: a positioned lookup filters none, and a scan of events_pkey
	// from the table's first id filters out every other session's events.
	pageMaxRowsRemoved = 100
)

// TestEventPage_WalkIsPositionedOnTheSessionIndex pins how far a page read
// scans (technical plan §6.2, §6.3). Each lookup of the walk
// (ListEventPageExtentForSession, its first event and each next one) must
// start at the cursor on events_session_id_id_idx and read a few buffers,
// however deep the cursor, under a custom plan and under the generic plan
// pgx's statement cache lets Postgres settle on from a statement's sixth
// run; and the read of the page's events by their ids must read a few
// buffers an id. Shapes that each passed an index-name check read far
// more: a plain `id > $2` scanned events_pkey from the cursor across every
// session under a generic plan; a plain `session_id = $1` beside the row
// comparisons became the index scan's start key, so every lookup read the
// session from its first event to the cursor, and paging a long session
// was quadratic; and ORDER BY id alone, with no equality on session_id,
// does not match the index's order, so a lookup sorts the rest of the
// session or scans events_pkey from the table's first id.
//
// It reads two logs, since the planner's choice turns on how many
// sessions it sees: few sessions sharing many events, interleaved, and
// many sessions beside one long one; each followed by a new session of
// 300 events. In each it reads the long session from its middle and the
// new session from its start, with EXPLAIN (ANALYZE, BUFFERS) under
// plan_cache_mode = force_custom_plan and force_generic_plan, and compares
// the page's time with the plain read it replaced (ListForSession) on the
// same events.
func TestEventPage_WalkIsPositionedOnTheSessionIndex(t *testing.T) {
	for _, log := range []struct {
		name string
		// others sessions share total events with the long session, which
		// takes every longEvery-th.
		others, total, longEvery int
	}{
		{name: "two sessions share 200,000 events", others: 1, total: 200_000, longEvery: 2},
		{name: "200 sessions beside one of 50,000 events", others: 200, total: 150_000, longEvery: 3},
	} {
		t.Run(log.name, func(t *testing.T) {
			ctx := context.Background()
			pool := pagePlanDatabase(ctx, t)
			long, middle, fresh := storePageWalkLog(ctx, t, pool, log.others, log.total, log.longEvery)
			events := narvipg.NewEventStore(pool)

			for _, read := range []struct {
				name      string
				sessionID pgtype.UUID
				cursor    int64
			}{
				{name: "the long session from its middle", sessionID: long, cursor: middle},
				{name: "a new session from its start", sessionID: fresh, cursor: 0},
			} {
				// Under the plan_cache_mode a deployment runs, once every
				// statement has run more than five times.
				if _, err := pool.Exec(ctx, `RESET plan_cache_mode`); err != nil {
					t.Fatalf("reset plan_cache_mode: %v", err)
				}
				comparePageReadTime(ctx, t, events, read.name, read.sessionID, read.cursor)

				page, err := events.ListPageForSession(ctx, read.sessionID, read.cursor, 100, platform.FetchHistoryMaxReplyBytes)
				if err != nil {
					t.Fatalf("ListPageForSession: %v", err)
				}
				if len(page.Events) != 100 {
					t.Fatalf("%s: read %d events, want 100", read.name, len(page.Events))
				}
				ids := make([]string, len(page.Events))
				for i, e := range page.Events {
					if e.SessionID != read.sessionID || e.ID <= read.cursor {
						t.Fatalf("%s: read event %d of another session or before the cursor", read.name, e.ID)
					}
					ids[i] = fmt.Sprint(e.ID)
				}

				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					if _, err := pool.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
						t.Fatalf("set plan_cache_mode: %v", err)
					}
					extent := explainPageStatement(ctx, t, pool, "ListEventPageExtentForSession",
						fmt.Sprintf("'%s'::uuid, %d, 100, %d", read.sessionID.String(), read.cursor, platform.FetchHistoryMaxReplyBytes))
					t.Logf("%s, %s: the walk's lookups: %s", read.name, mode, extent)
					if problem := pageWalkProblem(extent); problem != "" {
						t.Errorf("%s, %s: the walk %s", read.name, mode, problem)
					}
					byIDs := explainPageStatement(ctx, t, pool, "ListEventsForSessionByIDs",
						fmt.Sprintf("ARRAY[%s]::bigint[], '%s'::uuid", strings.Join(ids, ", "), read.sessionID.String()))
					t.Logf("%s, %s: the read by ids: %s", read.name, mode, byIDs)
					if problem := pageReadByIDsProblem(byIDs, len(ids)); problem != "" {
						t.Errorf("%s, %s: the read by ids %s", read.name, mode, problem)
					}
				}
			}
		})
	}
}

// storePageWalkLog stores total events across a long session and others
// other sessions, interleaved -- the long session takes every
// longEvery-th, the others the rest in turn -- then a new session's 300
// events, and analyzes the table. It returns the long session, the id of
// its event at the middle, and the new session.
func storePageWalkLog(ctx context.Context, t *testing.T, pool *pgxpool.Pool, others, total, longEvery int) (long pgtype.UUID, middle int64, fresh pgtype.UUID) {
	t.Helper()
	sessions := narvipg.NewSessionStore(pool)
	create := func() pgtype.UUID {
		created, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return created.ID
	}
	long = create()
	otherIDs := make([]string, others)
	for i := range otherIDs {
		otherIDs[i] = create().String()
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT CASE WHEN g % $3 = 0 THEN $2::uuid ELSE ($1::uuid[])[1 + g % cardinality($1::uuid[])] END,
		       'warning', 'log-' || g, '{"type":"warning","message":"log"}'::jsonb
		FROM generate_series(1, $4::int) g`, otherIDs, long, longEvery, total); err != nil {
		t.Fatalf("store the log: %v", err)
	}
	fresh = create()
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT $1, 'warning', 'fresh-' || g, '{"type":"warning","message":"fresh"}'::jsonb
		FROM generate_series(1, 300) g`, fresh); err != nil {
		t.Fatalf("store the new session's events: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM events WHERE session_id = $1 ORDER BY id OFFSET $2 LIMIT 1`,
		long, total/longEvery/2).Scan(&middle); err != nil {
		t.Fatalf("find the long session's middle: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	return long, middle, fresh
}

// comparePageReadTime fails when a page read takes more than ten times the
// plain read it replaced, ListForSession, of the same 100 events, plus
// 10 ms: the fastest of five runs each, after eight runs each, so both
// have settled on the plan their statement cache gives them. A lookup
// that scans the session up to its cursor, or every session from the
// table's first id, takes seconds a page here.
func comparePageReadTime(ctx context.Context, t *testing.T, events *narvipg.EventStore, name string, sessionID pgtype.UUID, cursor int64) {
	t.Helper()
	page := func() {
		if _, err := events.ListPageForSession(ctx, sessionID, cursor, 100, platform.FetchHistoryMaxReplyBytes); err != nil {
			t.Fatalf("ListPageForSession: %v", err)
		}
	}
	plain := func() {
		if _, err := events.ListForSession(ctx, sessionID, cursor, 100); err != nil {
			t.Fatalf("ListForSession: %v", err)
		}
	}
	fastest := func(read func()) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 5; i++ {
			start := time.Now()
			read()
			if took := time.Since(start); took < best {
				best = took
			}
		}
		return best
	}
	for i := 0; i < 8; i++ {
		page()
		plain()
	}
	pageTook, plainTook := fastest(page), fastest(plain)
	t.Logf("%s: a page read took %v, the plain read %v", name, pageTook, plainTook)
	if pageTook > 10*plainTook+10*time.Millisecond {
		t.Errorf("%s: a page read took %v, over ten times the plain read's %v plus 10 ms", name, pageTook, plainTook)
	}
}

// pagePlanScan is one scan of events in an executed plan.
type pagePlanScan struct {
	Node, Index    string
	Loops, Buffers float64
	// RowsRemoved is per loop, as EXPLAIN gives it.
	RowsRemoved float64
}

func (s pagePlanScan) String() string {
	on := s.Node
	if s.Index != "" {
		on += " using " + s.Index
	}
	return fmt.Sprintf("%s: %.0f loops, %.0f buffers, %.0f rows removed a loop", on, s.Loops, s.Buffers, s.RowsRemoved)
}

// explainPageStatement runs the connection's prepared statement for the
// named query, found by its sqlc name, with args, under EXPLAIN (ANALYZE,
// BUFFERS), and returns every scan of events the plan ran.
func explainPageStatement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement, args string) []pagePlanScan {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`,
		"-- name: "+statement+" %").Scan(&name); err != nil {
		t.Fatalf("find %s among the connection's prepared statements: %v", statement, err)
	}
	var plan string
	if err := pool.QueryRow(ctx,
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+pgx.Identifier{name}.Sanitize()+"("+args+")",
		pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	scans, err := pagePlanScans(plan)
	if err != nil {
		t.Fatalf("read %s's plan: %v\n%s", statement, err, plan)
	}
	return scans
}

// pagePlanScans returns every scan of events in plan, EXPLAIN's JSON.
func pagePlanScans(plan string) ([]pagePlanScan, error) {
	type node struct {
		NodeType    string  `json:"Node Type"`
		Relation    string  `json:"Relation Name"`
		Index       string  `json:"Index Name"`
		Loops       float64 `json:"Actual Loops"`
		RowsRemoved float64 `json:"Rows Removed by Filter"`
		SharedHit   float64 `json:"Shared Hit Blocks"`
		SharedRead  float64 `json:"Shared Read Blocks"`
		Plans       []json.RawMessage
	}
	var top []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &top); err != nil {
		return nil, err
	}
	if len(top) != 1 {
		return nil, fmt.Errorf("%d plans, want one", len(top))
	}
	var scans []pagePlanScan
	var walk func(raw json.RawMessage) error
	walk = func(raw json.RawMessage) error {
		var n node
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		if n.Relation == "events" {
			scans = append(scans, pagePlanScan{
				Node: n.NodeType, Index: n.Index, Loops: n.Loops,
				Buffers: n.SharedHit + n.SharedRead, RowsRemoved: n.RowsRemoved,
			})
		}
		for _, child := range n.Plans {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(top[0].Plan); err != nil {
		return nil, err
	}
	return scans, nil
}

// pageWalkProblem returns why the walk's scans, its first event's lookup
// and each next one's, are not positioned at the cursor, or "".
func pageWalkProblem(scans []pagePlanScan) string {
	ran := 0
	for _, s := range scans {
		if s.Loops == 0 {
			continue
		}
		ran++
		if perLoop := s.Buffers / s.Loops; perLoop > pageLookupMaxBuffers {
			return fmt.Sprintf("reads %.0f buffers a lookup, over %d (%s)", perLoop, pageLookupMaxBuffers, s)
		}
		if s.RowsRemoved > pageMaxRowsRemoved {
			return fmt.Sprintf("filters out %.0f rows a lookup, over %d (%s)", s.RowsRemoved, pageMaxRowsRemoved, s)
		}
	}
	if ran < 2 {
		return fmt.Sprintf("ran %d scans of events, want its first event's lookup and the next one's", ran)
	}
	return ""
}

// pageReadByIDsProblem returns why the read of a page's ids events reads
// more than a few buffers an id, or filters rows out, or "".
func pageReadByIDsProblem(scans []pagePlanScan, ids int) string {
	buffers := 0.0
	for _, s := range scans {
		buffers += s.Buffers
		if s.RowsRemoved > pageMaxRowsRemoved {
			return fmt.Sprintf("filters out %.0f rows a loop, over %d (%s)", s.RowsRemoved, pageMaxRowsRemoved, s)
		}
	}
	if len(scans) == 0 {
		return "ran no scan of events"
	}
	if limit := float64(pageLookupMaxBuffers * ids); buffers > limit {
		return fmt.Sprintf("reads %.0f buffers for %d ids, over %.0f", buffers, ids, limit)
	}
	return ""
}

// TestPagePlanScans pins how the plan test above reads a plan: every scan
// of events, at any depth, its buffers summed over its loops and its rows
// removed per loop, as EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) gives them.
func TestPagePlanScans(t *testing.T) {
	const plan = `[{"Plan": {"Node Type": "Sort", "Actual Loops": 1, "Shared Hit Blocks": 520, "Plans": [
		{"Node Type": "CTE Scan", "Actual Loops": 1, "Shared Hit Blocks": 520, "Plans": [
			{"Node Type": "Recursive Union", "Actual Loops": 1, "Shared Hit Blocks": 520, "Plans": [
				{"Node Type": "Limit", "Actual Loops": 1, "Shared Hit Blocks": 4, "Plans": [
					{"Node Type": "Index Scan", "Relation Name": "events", "Index Name": "events_session_id_id_idx",
					 "Actual Loops": 1, "Shared Hit Blocks": 3, "Shared Read Blocks": 1}]},
				{"Node Type": "Limit", "Actual Loops": 99, "Shared Hit Blocks": 516, "Plans": [
					{"Node Type": "Index Scan", "Relation Name": "events", "Index Name": "events_pkey",
					 "Actual Loops": 99, "Rows Removed by Filter": 400101, "Shared Hit Blocks": 500, "Shared Read Blocks": 16}]}]}]}]},
		"Planning Time": 0.1, "Execution Time": 2.5}]`
	scans, err := pagePlanScans(plan)
	if err != nil {
		t.Fatalf("pagePlanScans: %v", err)
	}
	want := []pagePlanScan{
		{Node: "Index Scan", Index: "events_session_id_id_idx", Loops: 1, Buffers: 4},
		{Node: "Index Scan", Index: "events_pkey", Loops: 99, Buffers: 516, RowsRemoved: 400101},
	}
	if fmt.Sprint(scans) != fmt.Sprint(want) {
		t.Fatalf("pagePlanScans = %v, want %v", scans, want)
	}
	if problem := pageWalkProblem(want[:1]); problem == "" {
		t.Error("pageWalkProblem passes a walk of one lookup")
	}
	if problem := pageWalkProblem(want); !strings.Contains(problem, "filters out 400101 rows") {
		t.Errorf("pageWalkProblem = %q, want the rows the second lookup filters out", problem)
	}
	deep := []pagePlanScan{want[0], {Node: "Index Scan", Index: "events_session_id_id_idx", Loops: 99, Buffers: 99 * 380}}
	if problem := pageWalkProblem(deep); !strings.Contains(problem, "reads 380 buffers a lookup") {
		t.Errorf("pageWalkProblem = %q, want the buffers the second lookup reads", problem)
	}
	positioned := []pagePlanScan{want[0], {Node: "Index Scan", Index: "events_session_id_id_idx", Loops: 99, Buffers: 99 * 4}}
	if problem := pageWalkProblem(positioned); problem != "" {
		t.Errorf("pageWalkProblem = %q for a positioned walk, want none", problem)
	}
}
