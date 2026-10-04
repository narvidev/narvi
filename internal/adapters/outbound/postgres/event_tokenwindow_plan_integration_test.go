//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

const (
	// tokenWindowIndex is the index every read of a turn's window must
	// scan: its migration says why it is built as it is.
	tokenWindowIndex = "events_token_window_idx"
	// tokenWindowFrames is how many `token` frames each window of the long
	// session holds: twenty text parts of two frames, as the pinned
	// runtime stores a part.
	tokenWindowFrames = 40
	// tokenWindowSessionRows is how many rows of its own the long session
	// logs in each window: its frames, and four tool events a frame --
	// step_start, tool_call, tool_result and step_finish, as a real turn
	// stores them, each carrying 2 KB of tool output.
	tokenWindowSessionRows = 5 * tokenWindowFrames
	// tokenWindowIndexBuffers is what a read may read beyond one heap page a
	// frame: a descent of events_token_window_idx and the leaf pages the
	// window's entries span, up to five on these logs.
	tokenWindowIndexBuffers = 8
	// tokenWindowMaxRowsRemoved bounds the rows a read removes by a filter.
	// A range scan of events_token_window_idx removes none: both bounds
	// are index conditions. Reading the window's id range on events_pkey
	// removes every other session's event in it, and walking the session's
	// text on another index removes every frame outside the window.
	tokenWindowMaxRowsRemoved = 4
)

// tokenWindowMaxBuffers bounds what one read of a turn's window reads, the
// whole statement's buffers: one heap page a frame of the window, at most,
// and the few index pages tokenWindowIndexBuffers allows. It has no term
// for the session's other text, the window's other rows -- the session's
// own tool events and every other session's events logged inside its id
// range -- or the table, and never grows with any of them: whatever plan
// the planner settles on reads events_token_window_idx's range from the
// window's upper bound to its lower one and nothing else, as a backward
// index scan or, under a custom plan, a bitmap scan of the same range
// whose rows it then sorts. Before that index, a generic plan walked
// events_token_part_idx over every `token` frame of the session and a
// custom plan took that walk or events_pkey across the window's id range,
// so what a read cost grew with the session's text or with what every
// session logged during the turn: up to 3,225 buffers on this test's logs.
func tokenWindowMaxBuffers(windowFrames int) float64 {
	return float64(windowFrames + tokenWindowIndexBuffers)
}

// tokenWindowAnalyze is when a log's statistics are last gathered.
type tokenWindowAnalyze int

const (
	// analyzeLast analyzes the table once the whole log is stored.
	analyzeLast tokenWindowAnalyze = iota
	// analyzeBeforeWindows analyzes it after the long session's history,
	// before its windows: the statistics know nothing of the turns read.
	analyzeBeforeWindows
	// analyzeBeforeLong analyzes it before the long session's first event:
	// the statistics know nothing of the session read.
	analyzeBeforeLong
)

// tokenWindowShape is a log TestEventStore_ListTokenFramesInWindow_ReadsTheTurnsWindow
// reads. Other sessions -- others of them -- log before events first; then
// one long session logs below text frames of history, a turn whose window
// holds tokenWindowSessionRows rows of its own among inWindow events of the
// other sessions, above text frames of later turns, and a last turn like
// the first; then the other sessions log after events more. Each history
// frame sits beside a step event of the session's own, and one in five of
// the other sessions' events is a `token` frame.
//
// So each log holds two windows of the long session: the first turn's,
// bounded above, with the session's text on both sides of it, and the last
// turn's, open above, with the session's text below it. Other sessions'
// events lie inside both windows' id ranges.
type tokenWindowShape struct {
	name                   string
	others, before         int
	below, inWindow, above int
	after                  int
	analyze                tokenWindowAnalyze
}

// tokenWindowRead is one read of a window of the long session: its bounds
// and the ids of its frames, newest first -- what the read must return.
type tokenWindowRead struct {
	name  string
	lower int64
	upper *int64
	want  []int64
}

// storeTokenWindowShape stores shape, every `token` frame's text textLen
// characters long, with autovacuum off so the statistics are the ones the
// shape gathers, and returns the long session and its two windows' reads.
func storeTokenWindowShape(ctx context.Context, t *testing.T, pool *pgxpool.Pool, shape tokenWindowShape, textLen int) (long pgtype.UUID, reads []tokenWindowRead) {
	t.Helper()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	exec("autovacuum off", `ALTER TABLE events SET (autovacuum_enabled = false)`)
	var sessionIDs []string
	if err := pool.QueryRow(ctx, `
		WITH s AS (
			INSERT INTO sessions (spawn_source, repos, spawn_depth)
			SELECT 'web', '[]'::jsonb, 0 FROM generate_series(1, $1::int)
			RETURNING id
		)
		SELECT array_agg(id::text) FROM s`, shape.others+1).Scan(&sessionIDs); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if err := long.Scan(sessionIDs[0]); err != nil {
		t.Fatalf("read the long session's id: %v", err)
	}
	others := sessionIDs[1:]

	storeOthers := func(what, prefix string, n int) {
		if n == 0 {
			return
		}
		exec(what, `INSERT INTO events (session_id, type, message_id, payload)
			SELECT ($1::uuid[])[1 + g % cardinality($1::uuid[])],
			       (ARRAY['token', 'step_start', 'tool_call', 'tool_result', 'step_finish'])[1 + g % 5],
			       $2::text || g,
			       CASE WHEN g % 5 = 0
			            THEN jsonb_build_object('type', 'token', 'messageId', $2::text || 'prt_' || (g / 10), 'text', rpad($2::text || g, $3::int, 'x'))
			            ELSE '{"type":"step_start"}'::jsonb END
			FROM generate_series(1, $4::int) g`, others, prefix, textLen, n)
	}
	storeText := func(what, prefix string, frames int) {
		exec(what, `INSERT INTO events (session_id, type, message_id, payload)
			SELECT $1, CASE WHEN g % 2 = 0 THEN 'token' ELSE 'step_start' END, $2::text || g,
			       CASE WHEN g % 2 = 0 THEN jsonb_build_object('type', 'token', 'messageId', $2::text || 'prt_' || (g / 4), 'text', rpad($2::text || g, $3::int, 'x'))
			            ELSE '{"type":"step_start"}'::jsonb END
			FROM generate_series(1, 2 * $4::int) g`, long, prefix, textLen, frames)
	}
	watermark := func() int64 {
		var id int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1`, long).Scan(&id); err != nil {
			t.Fatalf("read the long session's watermark: %v", err)
		}
		return id
	}
	// storeWindow stores one turn of the long session, its rows spread
	// evenly among inWindow events of the other sessions, and returns its
	// frames' ids, newest first.
	storeWindow := func(what, prefix string) []int64 {
		total := shape.inWindow + tokenWindowSessionRows
		if total%tokenWindowSessionRows != 0 {
			t.Fatalf("%d events do not interleave %d of the long session's evenly", total, tokenWindowSessionRows)
		}
		every := total / tokenWindowSessionRows
		exec(what, `INSERT INTO events (session_id, type, message_id, payload)
			SELECT CASE WHEN g % $5::int = 0 THEN $6::uuid ELSE ($1::uuid[])[1 + g % cardinality($1::uuid[])] END,
			       CASE WHEN g % $5::int = 0 AND (g / $5::int) % 5 = 0 THEN 'token'
			            WHEN g % $5::int = 0 THEN (ARRAY['step_start', 'tool_call', 'tool_result', 'step_finish'])[1 + (g / $5::int) % 4]
			            ELSE (ARRAY['token', 'step_start', 'tool_call', 'tool_result', 'step_finish'])[1 + g % 5] END,
			       $2::text || g,
			       CASE WHEN g % $5::int = 0 AND (g / $5::int) % 5 = 0
			                THEN jsonb_build_object('type', 'token', 'messageId', $2::text || 'prt_' || (g / $5::int / 10), 'text', rpad($2::text || g, $3::int, 'x'))
			            WHEN g % $5::int = 0
			                THEN jsonb_build_object('type', 'tool_result', 'messageId', $2::text || 'msg_' || (g / $5::int / 5), 'callId', $2::text || 'call_' || g,
			                                        'output', jsonb_build_object('output', repeat('x', 2000)))
			            WHEN g % 5 = 0
			                THEN jsonb_build_object('type', 'token', 'messageId', $2::text || 'prt_o' || (g / 10), 'text', rpad($2::text || g, $3::int, 'x'))
			            ELSE '{"type":"step_start"}'::jsonb END
			FROM generate_series(1, $4::int) g`, others, prefix, textLen, total, every, long)
		var ids []int64
		if err := pool.QueryRow(ctx, `
			SELECT array_agg(id ORDER BY id DESC) FROM events
			WHERE session_id = $1 AND type = 'token' AND message_id LIKE $2::text || '%'`, long, prefix).Scan(&ids); err != nil {
			t.Fatalf("read %s's frames: %v", what, err)
		}
		if len(ids) != tokenWindowFrames {
			t.Fatalf("%s holds %d frames, want %d", what, len(ids), tokenWindowFrames)
		}
		return ids
	}
	analyze := func() { exec("analyze", `ANALYZE events`) }

	storeOthers("the other sessions' events before the long session", "before-", shape.before)
	if shape.analyze == analyzeBeforeLong {
		analyze()
	}
	storeText("the long session's history", "history-", shape.below)
	if shape.analyze == analyzeBeforeWindows {
		analyze()
	}
	first := tokenWindowRead{name: "a window bounded above", lower: watermark()}
	first.want = storeWindow("the first turn", "first-")
	upper := watermark()
	first.upper = &upper
	storeText("the later turns' text", "later-", shape.above)
	last := tokenWindowRead{name: "a window open above", lower: watermark()}
	last.want = storeWindow("the last turn", "last-")
	storeOthers("the other sessions' events after the last turn", "after-", shape.after)
	if shape.analyze == analyzeLast {
		analyze()
	}
	return long, []tokenWindowRead{first, last}
}

// tokenWindowDatabases migrates one database to the latest version, as a
// template, and returns a function that creates a database of its own for
// one log from it, with a pool of exactly one connection on it, as
// pagePlanDatabase does: each log's statistics are its own, and every
// statement shares one connection's prepared-statement cache. Copying the
// template takes a fraction of the time migrating each of the matrix's 24
// databases from scratch took. The template and every copy are dropped at
// cleanup.
func tokenWindowDatabases(ctx context.Context, t *testing.T) func(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin, adminConnStr := IntegrationTestPoolAndConnStr(t)
	u, err := url.Parse(adminConnStr)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	connStr := func(name string) string {
		c := *u
		c.Path = "/" + name
		return c.String()
	}
	template := fmt.Sprintf("tokenwindow_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{template}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", template, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{template}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", template, err)
		}
	})
	m, mdb := newMigrate(t, connStr(template))
	upErr := m.Up()
	// CREATE DATABASE ... TEMPLATE refuses a template anyone is connected
	// to, so the migrator's own connection, which closing mdb leaves open,
	// is closed too.
	srcErr, dbErr := m.Close()
	_ = mdb.Close()
	if upErr != nil {
		t.Fatalf("migrate %s up: %v", template, upErr)
	}
	if srcErr != nil || dbErr != nil {
		t.Fatalf("close the migrator of %s: %v, %v", template, srcErr, dbErr)
	}

	copies := 0
	return func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		copies++
		name := fmt.Sprintf("%s_%d", template, copies)
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{template}.Sanitize()); err != nil {
			t.Fatalf("create database %s from %s: %v", name, template, err)
		}
		pool, err := narvipg.NewPoolWithMaxConns(ctx, connStr(name), 1)
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
}

// tokenWindowReadPlan is how one read of a window ran: its scans -- of every
// relation, bitmap index scans included -- the buffers the whole statement
// read, and the rows its nodes removed by a filter or an index recheck.
type tokenWindowReadPlan struct {
	scans   []pagePlanScan
	buffers float64
	removed float64
}

// readTokenWindowPlan reads plan, EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
// of one read of a window.
func readTokenWindowPlan(plan string) (tokenWindowReadPlan, error) {
	scans, buffers, err := planScans(plan, "")
	if err != nil {
		return tokenWindowReadPlan{}, err
	}
	type node struct {
		Loops   float64 `json:"Actual Loops"`
		Filter  float64 `json:"Rows Removed by Filter"`
		Recheck float64 `json:"Rows Removed by Index Recheck"`
		Plans   []json.RawMessage
	}
	var top []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &top); err != nil {
		return tokenWindowReadPlan{}, err
	}
	// EXPLAIN gives the rows a node removed per loop.
	var removed float64
	var walk func(raw json.RawMessage) error
	walk = func(raw json.RawMessage) error {
		var n node
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		removed += (n.Filter + n.Recheck) * max(n.Loops, 1)
		for _, child := range n.Plans {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range top {
		if err := walk(p.Plan); err != nil {
			return tokenWindowReadPlan{}, err
		}
	}
	return tokenWindowReadPlan{scans: scans, buffers: buffers, removed: removed}, nil
}

// scansTheWindowIndex reports whether scans read events through
// events_token_window_idx alone: an Index Scan of it, which reads the
// window's entries in the read's order, or a bitmap scan of it, whose
// rows -- the window's frames, no others -- are sorted afterwards. A custom
// plan takes either, by the window's size.
func scansTheWindowIndex(scans []pagePlanScan) bool {
	switch len(scans) {
	case 1:
		return scans[0].Node == "Index Scan" && scans[0].Relation == "events" && scans[0].Index == tokenWindowIndex
	case 2:
		return scans[0].Node == "Bitmap Heap Scan" && scans[0].Relation == "events" &&
			scans[1].Node == "Bitmap Index Scan" && scans[1].Index == tokenWindowIndex
	}
	return false
}

// tokenWindowReadProblem returns why plan, one read of a window of
// windowFrames frames, is not a scan of events_token_window_idx reading at
// most tokenWindowMaxBuffers and removing at most tokenWindowMaxRowsRemoved
// rows, or ""; and how the read ran.
func tokenWindowReadProblem(plan string, windowFrames int) (string, tokenWindowReadPlan) {
	read, err := readTokenWindowPlan(plan)
	if err != nil {
		return fmt.Sprintf("has a plan that does not parse: %v", err), read
	}
	var problems []string
	if !scansTheWindowIndex(read.scans) {
		problems = append(problems, fmt.Sprintf("reads events by %s, not a scan of %s alone", scanShape(read.scans), tokenWindowIndex))
	}
	if limit := tokenWindowMaxBuffers(windowFrames); read.buffers > limit {
		problems = append(problems, fmt.Sprintf("reads %.0f buffers, over %.0f for a window of %d frames", read.buffers, limit, windowFrames))
	}
	if read.removed > tokenWindowMaxRowsRemoved {
		problems = append(problems, fmt.Sprintf("removes %.0f rows, over %d", read.removed, tokenWindowMaxRowsRemoved))
	}
	return strings.Join(problems, "; "), read
}

// TestEventStore_ListTokenFramesInWindow_ReadsTheTurnsWindow pins the read
// every reader of a turn's text makes (sessionactor.ReadWindowFinal): one
// turn's window of `token` frames. In every cell of a matrix it must return
// exactly the window's frames, newest first, and be a scan of
// events_token_window_idx alone (scansTheWindowIndex) that reads at most
// tokenWindowMaxBuffers -- a few buffers beyond the window's own frames --
// and removes at most tokenWindowMaxRowsRemoved rows.
//
// The planner's choice turns on the log's statistics and on how much the
// session and the other sessions logged, so the matrix reads eight log
// shapes: a window interleaved with 20,000 events of 5,000 other sessions,
// with none, 20,000 or 40,000 of their events stored before the session;
// 5,000 small sessions beside one long one; two sessions sharing the log;
// the windows' turns logged after the last ANALYZE; 30,000 other events
// appended after them as well; and the long session stored after ANALYZE.
// Each is stored at 8, 300 and 1,000 characters of token text, and each log
// is read for a window bounded above, with the session's text on both sides
// of it, and one open above, under plan_cache_mode = force_custom_plan and
// force_generic_plan: 96 cells, four on each log. The read is the
// statement as the store sends it, sqlc's text with bound parameters: the
// store runs it more than five times under each mode, so the plan a
// deployment's statement cache settles on is the one measured, and the
// connection's own prepared statement is then run under EXPLAIN (ANALYZE,
// BUFFERS, FORMAT JSON). Every cell logs one line, "cell | shape | text |
// window | mode | scans | buffers | rows removed", whatever its outcome.
func TestEventStore_ListTokenFramesInWindow_ReadsTheTurnsWindow(t *testing.T) {
	interleaved := func(stored string, before int) tokenWindowShape {
		return tokenWindowShape{
			name:   "the interleaved window, " + stored + " other events stored before the session",
			others: 5_000, before: before, below: 3_000, inWindow: 20_000, above: 6_000,
		}
	}
	newDatabase := tokenWindowDatabases(context.Background(), t)
	shapes := []tokenWindowShape{
		interleaved("no", 0),
		interleaved("20,000", 20_000),
		interleaved("40,000", 40_000),
		{name: "5,000 small sessions beside one long one", others: 5_000, before: 40_000, below: 3_000, inWindow: 1_000, above: 6_000},
		{name: "two sessions sharing the log", others: 1, before: 20_000, below: 3_000, inWindow: 20_000, above: 6_000},
		{name: "the windows' turns logged after the last ANALYZE", others: 5_000, before: 20_000, below: 3_000, inWindow: 20_000, above: 6_000, analyze: analyzeBeforeWindows},
		{name: "30,000 other events appended after them", others: 5_000, before: 20_000, below: 3_000, inWindow: 20_000, above: 6_000, after: 30_000, analyze: analyzeBeforeWindows},
		{name: "the long session stored after ANALYZE", others: 5_000, before: 40_000, below: 3_000, inWindow: 20_000, above: 6_000, analyze: analyzeBeforeLong},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			for _, textLen := range []int{8, 300, 1_000} {
				t.Run(fmt.Sprintf("%d characters of text", textLen), func(t *testing.T) {
					ctx := context.Background()
					pool := newDatabase(t)
					long, reads := storeTokenWindowShape(ctx, t, pool, shape, textLen)
					events := narvipg.NewEventStore(pool)
					for _, read := range reads {
						for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
							cell := fmt.Sprintf("%s, %d characters, %s, %s", shape.name, textLen, read.name, mode)
							if _, err := pool.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
								t.Fatalf("set plan_cache_mode: %v", err)
							}
							for run := 0; run < 8; run++ {
								frames, err := events.ListTokenFramesInWindow(ctx, long, read.lower, read.upper, 2000)
								if err != nil {
									t.Fatalf("%s: ListTokenFramesInWindow: %v", cell, err)
								}
								got := make([]int64, len(frames))
								for i, f := range frames {
									got[i] = f.ID
								}
								if !slices.Equal(got, read.want) {
									t.Fatalf("%s: run %d read frames %v, want the window's %v", cell, run, got, read.want)
								}
							}
							upper := "NULL::bigint"
							if read.upper != nil {
								upper = fmt.Sprint(*read.upper)
							}
							plan := explainPlan(ctx, t, pool, "ListTokenFramesInWindow", fmt.Sprintf("'%s'::uuid, %d, %s, 2000", long.String(), read.lower, upper))
							problem, ran := tokenWindowReadProblem(plan, len(read.want))
							t.Logf("cell | %s | %d | %s | %s | %s | %.0f | %.0f", shape.name, textLen, read.name, mode, scanShape(ran.scans), ran.buffers, ran.removed)
							if problem != "" {
								t.Errorf("%s: the read %s", cell, problem)
							}
						}
					}
					if _, err := pool.Exec(ctx, `RESET plan_cache_mode`); err != nil {
						t.Fatalf("reset plan_cache_mode: %v", err)
					}
				})
			}
		})
	}
}

// TestTokenWindowReadProblem pins how the plan test above judges a read:
// an Index Scan of events_token_window_idx, or a bitmap scan of it, within
// its buffers and rows removed passes; a read through another index, a
// bitmap scan of another index or of more than one, or a read over either
// bound fails, saying why.
func TestTokenWindowReadProblem(t *testing.T) {
	scan := func(node, index string, buffers, removed int) string {
		return fmt.Sprintf(`{"Node Type": "Limit", "Actual Loops": 1, "Shared Hit Blocks": %d, "Plans": [
			{"Node Type": %q, "Relation Name": "events", "Index Name": %q, "Actual Loops": 1,
			 "Rows Removed by Filter": %d, "Shared Hit Blocks": %d}]}`, buffers, node, index, removed, buffers)
	}
	bitmap := func(recheck int, indexes ...string) string {
		children := make([]string, len(indexes))
		for i, index := range indexes {
			children[i] = fmt.Sprintf(`{"Node Type": "Bitmap Index Scan", "Index Name": %q, "Actual Loops": 1, "Shared Hit Blocks": 2}`, index)
		}
		return fmt.Sprintf(`{"Node Type": "Limit", "Actual Loops": 1, "Shared Hit Blocks": 42, "Plans": [
			{"Node Type": "Sort", "Actual Loops": 1, "Shared Hit Blocks": 42, "Plans": [
				{"Node Type": "Bitmap Heap Scan", "Relation Name": "events", "Actual Loops": 1, "Rows Removed by Index Recheck": %d,
				 "Shared Hit Blocks": 42, "Plans": [%s]}]}]}`, recheck, strings.Join(children, ", "))
	}
	plan := func(top string) string { return `[{"Plan": ` + top + `}]` }
	tests := []struct {
		name string
		plan string
		want []string
	}{
		{name: "a range scan of the window's frames", plan: plan(scan("Index Scan", tokenWindowIndex, 44, 0))},
		{name: "a bitmap scan of the window's frames", plan: plan(bitmap(0, tokenWindowIndex))},
		{name: "a scan over the buffers", plan: plan(scan("Index Scan", tokenWindowIndex, 49, 0)), want: []string{"reads 49 buffers, over 48"}},
		{name: "a scan removing rows by a filter", plan: plan(scan("Index Scan", tokenWindowIndex, 20, 5)), want: []string{"removes 5 rows"}},
		{name: "a lossy bitmap rechecking rows away", plan: plan(bitmap(300, tokenWindowIndex)), want: []string{"removes 300 rows"}},
		{name: "a walk of the session's text", plan: plan(scan("Index Scan", "events_token_part_idx", 809, 6004)),
			want: []string{"not a scan of events_token_window_idx alone", "reads 809 buffers", "removes 6004 rows"}},
		{name: "events_pkey across the window", plan: plan(scan("Index Scan", "events_pkey", 443, 20000)),
			want: []string{"not a scan of events_token_window_idx alone", "removes 20000 rows"}},
		{name: "a bitmap scan of another index", plan: plan(bitmap(0, "events_session_id_id_idx")),
			want: []string{"not a scan of events_token_window_idx alone"}},
		{name: "a bitmap scan of two indexes", plan: plan(bitmap(0, tokenWindowIndex, "events_pkey")),
			want: []string{"not a scan of events_token_window_idx alone"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problem, _ := tokenWindowReadProblem(tt.plan, tokenWindowFrames)
			if len(tt.want) == 0 && problem != "" {
				t.Fatalf("tokenWindowReadProblem = %q, want none", problem)
			}
			if len(tt.want) > 0 && problem == "" {
				t.Fatalf("tokenWindowReadProblem found no problem, want %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(problem, w) {
					t.Errorf("tokenWindowReadProblem = %q, want it to say %q", problem, w)
				}
			}
		})
	}
}
