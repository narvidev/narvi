//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// tokenWindowLog is a log TestEventStore_ListTokenFramesInWindow_ReadsTheTurnsWindow
// reads: others sessions of a few events each, and one long session holding
// history, an earlier turn's text, and a later turn that logged toolRows
// tool events -- step_start, tool_call, tool_result, step_finish, as a real
// turn stores them -- and a little text of its own.
type tokenWindowLog struct {
	name             string
	others, otherPer int
	history          int
	earlierTokens    int
	toolRows         int
	laterTokens      int
	longAfterAnalyze bool
}

// storeTokenWindowLog stores log and returns the long session and the two
// turns' bounds: the earlier turn's window is (earlierLower, laterLower],
// the later turn's (laterLower, the log's end).
func storeTokenWindowLog(ctx context.Context, t *testing.T, pool *pgxpool.Pool, log tokenWindowLog) (long pgtype.UUID, earlierLower, laterLower int64) {
	t.Helper()
	var sessionIDs []string
	if err := pool.QueryRow(ctx, `
		WITH s AS (
			INSERT INTO sessions (spawn_source, repos, spawn_depth)
			SELECT 'web', '[]'::jsonb, 0 FROM generate_series(1, $1::int)
			RETURNING id
		)
		SELECT array_agg(id::text) FROM s`, log.others+1).Scan(&sessionIDs); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if err := long.Scan(sessionIDs[0]); err != nil {
		t.Fatalf("read the long session's id: %v", err)
	}
	others := sessionIDs[1:]
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT ($1::uuid[])[1 + g % cardinality($1::uuid[])], 'warning', 'log-' || g, '{"type":"warning","message":"log"}'::jsonb
		FROM generate_series(1, $2::int) g`, others, log.others*log.otherPer); err != nil {
		t.Fatalf("store the other sessions' events: %v", err)
	}
	storeLong := func() {
		exec := func(what, sql string, args ...any) {
			if _, err := pool.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("store %s: %v", what, err)
			}
		}
		// Earlier turns' history: their text parts, two frames each, among
		// their other events.
		exec("history", `INSERT INTO events (session_id, type, message_id, payload)
			SELECT $1, CASE WHEN g % 2 = 0 THEN 'token' ELSE 'step_start' END, 'history-' || g,
			       CASE WHEN g % 2 = 0 THEN jsonb_build_object('type', 'token', 'messageId', 'prt_history_' || (g / 4), 'text', 'history ' || g)
			            ELSE '{"type":"step_start"}'::jsonb END
			FROM generate_series(1, $2::int) g`, long, log.history)
		if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1`, long).Scan(&earlierLower); err != nil {
			t.Fatalf("read the earlier turn's watermark: %v", err)
		}
		exec("the earlier turn's text", `INSERT INTO events (session_id, type, message_id, payload)
			SELECT $1, 'token', 'prt_earlier#' || g, jsonb_build_object('type', 'token', 'messageId', 'prt_earlier', 'text', 'earlier ' || g)
			FROM generate_series(1, $2::int) g`, long, log.earlierTokens)
		if err := pool.QueryRow(ctx, `SELECT MAX(id) FROM events WHERE session_id = $1`, long).Scan(&laterLower); err != nil {
			t.Fatalf("read the later turn's watermark: %v", err)
		}
		exec("the later turn's tool events", `INSERT INTO events (session_id, type, message_id, payload)
			SELECT $1, (ARRAY['step_start', 'tool_call', 'tool_result', 'step_finish'])[1 + g % 4], 'msg_' || (g / 4) || '#' || g,
			       jsonb_build_object('type', 'tool_result', 'messageId', 'msg_' || (g / 4), 'callId', 'call_' || g, 'output', jsonb_build_object('output', repeat('x', 2000)))
			FROM generate_series(1, $2::int) g`, long, log.toolRows)
		exec("the later turn's text", `INSERT INTO events (session_id, type, message_id, payload)
			SELECT $1, 'token', 'prt_later#' || g, jsonb_build_object('type', 'token', 'messageId', 'prt_later', 'text', 'later ' || g)
			FROM generate_series(1, $2::int) g`, long, log.laterTokens)
	}
	if !log.longAfterAnalyze {
		storeLong()
	}
	if _, err := pool.Exec(ctx, `ANALYZE events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	if log.longAfterAnalyze {
		storeLong()
	}
	return long, earlierLower, laterLower
}

// tokenWindowMaxBuffers bounds what one read of a turn's window reads, as
// measured on the logs below. The planner reads the window one of two
// ways: on events_token_part_idx, the partial index of the session's
// `token` frames, whose id is no seek key behind the part id, so the read
// passes over the index entries of every frame the session holds -- about
// one buffer for 70 -- and reads the heap for the window's own; or on
// events_pkey across the window, passing over its other rows, tool events
// among them -- about one buffer for 30. Either way it grows with the
// session's text frames or the window's rows, never with the session's
// other events outside the window or the table, and reads no payload but
// the window's frames: the tail read it replaced read the session's newest
// 2000 events whole, tool payloads included, and lost an earlier turn's
// text once a later turn logged enough tool steps.
func tokenWindowMaxBuffers(sessionFrames, windowRows int) float64 {
	return float64(20 + sessionFrames/40 + windowRows/16)
}

// TestEventStore_ListTokenFramesInWindow_ReadsTheTurnsWindow pins the read
// every reader of a turn's text makes (sessionactor.ReadWindowFinal): a
// turn's window of `token` frames. It must return the window's frames and
// no others, and read no more than tokenWindowMaxBuffers says -- the
// session's text frames' index entries or the window's rows, never the
// session's other events outside the window -- under a custom plan and the
// generic plan pgx's statement cache lets Postgres settle on from a
// statement's sixth run, on logs whose shapes turn the planner's
// expectation of events a session: many small sessions beside one long
// one, and a long one stored after the table was analyzed.
func TestEventStore_ListTokenFramesInWindow_ReadsTheTurnsWindow(t *testing.T) {
	for _, log := range []tokenWindowLog{
		{name: "5,000 sessions of 8 events beside one with 3,000 frames of history and 2,400 tool rows after its earlier turn", others: 5_000, otherPer: 8, history: 6_000, earlierTokens: 40, toolRows: 2_400, laterTokens: 4},
		{name: "200 sessions of 500 events beside one with 10,000 frames of history and 8,000 tool rows", others: 200, otherPer: 500, history: 20_000, earlierTokens: 40, toolRows: 8_000, laterTokens: 4},
		{name: "4,000 sessions of 25 events, and the long one stored after ANALYZE", others: 4_000, otherPer: 25, history: 6_000, earlierTokens: 40, toolRows: 2_400, laterTokens: 4, longAfterAnalyze: true},
	} {
		t.Run(log.name, func(t *testing.T) {
			ctx := context.Background()
			pool := pagePlanDatabase(ctx, t)
			long, earlierLower, laterLower := storeTokenWindowLog(ctx, t, pool, log)
			events := narvipg.NewEventStore(pool)

			// For the record, the read this one replaced for the plan views'
			// fallback, the session result's summary and the plan notices:
			// the session's newest 2000 events, of every type, whole.
			for i := 0; i < 8; i++ {
				if _, err := events.ListRecentForSession(ctx, long, 2000); err != nil {
					t.Fatalf("ListRecentForSession: %v", err)
				}
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				if _, err := pool.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
					t.Fatalf("set plan_cache_mode: %v", err)
				}
				t.Logf("the tail read it replaced, %s: %s", mode, explainPageStatement(ctx, t, pool, "ListRecentEventsForSession", fmt.Sprintf("'%s'::uuid, 2000", long.String())))
			}
			if _, err := pool.Exec(ctx, `RESET plan_cache_mode`); err != nil {
				t.Fatalf("reset plan_cache_mode: %v", err)
			}

			for _, read := range []struct {
				name       string
				lower      int64
				upper      *int64
				wantFrames int
				windowRows int
			}{
				{name: "the earlier turn's window", lower: earlierLower, upper: &laterLower, wantFrames: log.earlierTokens, windowRows: log.earlierTokens},
				{name: "the later turn's window, open above", lower: laterLower, wantFrames: log.laterTokens, windowRows: log.toolRows + log.laterTokens},
			} {
				for i := 0; i < 8; i++ {
					frames, err := events.ListTokenFramesInWindow(ctx, long, read.lower, read.upper, 2000)
					if err != nil {
						t.Fatalf("%s: ListTokenFramesInWindow: %v", read.name, err)
					}
					if len(frames) != read.wantFrames {
						t.Fatalf("%s: read %d frames, want %d", read.name, len(frames), read.wantFrames)
					}
					for _, f := range frames {
						if f.Type != "token" || f.ID <= read.lower || (read.upper != nil && f.ID > *read.upper) {
							t.Fatalf("%s: read event %d (%s) outside the window or not a token frame", read.name, f.ID, f.Type)
						}
					}
				}
				upper := "NULL::bigint"
				if read.upper != nil {
					upper = fmt.Sprint(*read.upper)
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					if _, err := pool.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
						t.Fatalf("set plan_cache_mode: %v", err)
					}
					scans := explainPageStatement(ctx, t, pool, "ListTokenFramesInWindow", fmt.Sprintf("'%s'::uuid, %d, %s, 2000", long.String(), read.lower, upper))
					t.Logf("%s, %s: %s", read.name, mode, scans)
					var buffers float64
					for _, s := range scans {
						buffers += s.Buffers
					}
					sessionFrames := log.history/2 + log.earlierTokens + log.laterTokens
					if limit := tokenWindowMaxBuffers(sessionFrames, read.windowRows); len(scans) == 0 || buffers > limit {
						t.Errorf("%s, %s: read %.0f buffers in %v, want at most %.0f: the read is not held to the turn's window", read.name, mode, buffers, scans, limit)
					}
				}
				if _, err := pool.Exec(ctx, `RESET plan_cache_mode`); err != nil {
					t.Fatalf("reset plan_cache_mode: %v", err)
				}
			}
		})
	}
}
