//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// contextWriteMaxBuffers bounds each write technical plan §24.9's context
// check adds, measured as the row's first write in its transaction: a key
// probe of the row's table and its heap page, and -- for an update no
// index lets be heap-only, or an insert -- the new version and its index
// entries, plus a free space map read and a table extension on the
// densely loaded tables below. holdWakeMaxBuffers' allowance, for the same
// shape of write.
const contextWriteMaxBuffers = holdWakeMaxBuffers

// contextTurnWriteIndexEntryMaxBuffers is what a write of a turn that is
// never heap-only (contextWritesTurns) reads beyond contextWriteMaxBuffers
// since the plan tests migrate past 000166 (holdPlanLatestVersion): its new
// row version's entry in turns_session_dispatched_idx, an index this test's
// bound predates. It is the session guard's plan test's own measure of that
// entry (sessionDispatchedIndexEntryMaxBuffers) for an index two levels
// above its leaves, which it is on the tables below, of up to 316,000
// turns: the metapage and one page of each level down to the leaf. main's
// text of UpdateTurnStatus reads the same.
var contextTurnWriteIndexEntryMaxBuffers = sessionDispatchedIndexEntryMaxBuffers(2)

// contextWritesTurns names the measured writes of a turn: each new row
// version gets an entry in every index of turns, the one 000166 added
// included.
var contextWritesTurns = map[string]bool{"UpdateTurnStatus": true, "SetTurnContextUnconfirmed": true}

// contextWriteLimit is what the write named name may read.
func contextWriteLimit(name string) float64 {
	if contextWritesTurns[name] {
		return contextWriteMaxBuffers + contextTurnWriteIndexEntryMaxBuffers
	}
	return contextWriteMaxBuffers
}

// measureTurnWrites measures each of statements, writes of one turn, as
// measureHoldPlan does, on turns settled for it as measureGuardWrite
// settles them. Every measure writes nine row versions of the probe's
// newest turn, rolled back, and each takes an entry in every index of
// turns, in the leaves the turn's keys fall in -- the same leaves for
// every measure of the turn. Unsettled, those leaves fill with the dead
// entries of the measures before, and the measured write then deletes
// them to make room, reading their heap pages: the same write of the same
// turn read 24 buffers, then 29 in every measure after. So each statement
// runs once unmeasured first, and a leaf its entries do not fit splits
// then; and each measure runs on turns vacuumed with INDEX_CLEANUP ON,
// which removes every dead entry, so each leaf it reaches has room for
// the nine entries it took in the unmeasured run.
func measureTurnWrites(ctx context.Context, t *testing.T, pool *pgxpool.Pool, probe holdPlanProbe, mode string, statements ...holdPlanStatement) []holdPlanMeasurement {
	t.Helper()
	for _, s := range statements {
		measureHoldPlan(ctx, t, pool, s, probe, mode)
	}
	out := make([]holdPlanMeasurement, len(statements))
	for i, s := range statements {
		if _, err := pool.Exec(ctx, `VACUUM (INDEX_CLEANUP ON) turns`); err != nil {
			t.Fatalf("vacuum turns: %v", err)
		}
		out[i] = measureHoldPlan(ctx, t, pool, s, probe, mode)
	}
	return out
}

// contextPlanShape is a table TestReviewContextCheck_PlansReadTheSessionsOwnTurns
// reads: holdPlanShape's sessions, every one claiming a pull request, whose
// ended turns ended a second after they were created -- the last of them
// nine seconds before the shape is stored, so before any turn stored after
// it was created -- every fifth turn a
// review attempt, some of those ended context_moved; and a long review
// session whose last turn is longTail:
//
//   - "processing", a turn in flight;
//   - "attempt", a pending automatic review attempt that waited behind no
//     turn -- every earlier turn of its session ended before it was
//     created, so the context check's pre-read walks the whole of the
//     session's history to learn it;
//   - "queued", the same attempt created just before the session's last
//     ended turn ended, so it waited behind that one turn: the pre-read
//     walks until it finds it, which on a heap read in its physical order
//     is the last.
//
// With interleave set, the long session's ended turns are stored one in
// every interleave rows, the rest other sessions' turns stored in between
// -- a session whose turns span months of a busy table, each on a heap
// page of its own -- instead of one after another.
type contextPlanShape struct {
	name                        string
	others, perOther, openEvery int
	longEnded                   int
	longTail                    string
	longAfterAnalyze            bool
	interleave                  int
	// tailTrigger is the lane the long session's "attempt" or "queued"
	// tail recorded as asking for it: 'auto' when empty, a person's lane
	// ('button' or 'label') for the owed requests' plan test.
	tailTrigger string
}

// storeContextPlanShape stores shape in pool's database, migrated to 159,
// and analyzes it -- before the long session when shape.longAfterAnalyze --
// and returns the sessions to probe: the long one, a small one with an open
// turn when the shape has any, and a small one with none.
func storeContextPlanShape(ctx context.Context, t *testing.T, pool *pgxpool.Pool, shape contextPlanShape) []holdPlanProbe {
	t.Helper()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	for _, table := range []string{"sessions", "turns", "session_timers", "github_pr_sessions"} {
		exec("autovacuum off "+table, `ALTER TABLE `+table+` SET (autovacuum_enabled = false)`)
	}
	var others []string
	if err := pool.QueryRow(ctx, `
		WITH s AS (
			INSERT INTO sessions (spawn_source, repos, spawn_depth)
			SELECT 'github', '[]'::jsonb, 0 FROM generate_series(1, $1::int)
			RETURNING id
		)
		SELECT array_agg(id::text) FROM s`, shape.others).Scan(&others); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	// turns stores a session's ended turns: every fifth a review attempt,
	// every fifth of those the automatic lane's, and every seventh of
	// those ended context_moved.
	turns := func(what string, sessions any, n int, pick string) {
		exec(what, `
			INSERT INTO turns (session_id, status, created_at, completed_at, is_review_attempt, review_head_sha, request_trigger, end_reason)
			SELECT `+pick+`,
			       (ARRAY['completed', 'failed', 'cancelled'])[1 + g % 3]::turn_status,
			       now() - make_interval(secs => $2::int - g + 10),
			       now() - make_interval(secs => $2::int - g + 9),
			       g % 5 = 0,
			       CASE WHEN g % 5 = 0 THEN 'sha-' || g END,
			       CASE WHEN g % 25 = 0 THEN 'auto' END,
			       CASE WHEN g % 175 = 0 THEN 'context_moved' END
			FROM generate_series(1, $2::int) g`, sessions, n)
	}
	turns("the other sessions' ended turns", others, shape.others*shape.perOther, `($1::uuid[])[1 + g % cardinality($1::uuid[])]`)
	if shape.openEvery > 0 {
		exec("the other sessions' open turns", `
			INSERT INTO turns (session_id, status)
			SELECT o.id, 'pending' FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)
			WHERE o.n % $2::int = 0`, others, shape.openEvery)
	}
	claims := func(sessions []string, prefix string) {
		exec("pull request claims", `
			INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id, pending_retrigger_head_sha)
			SELECT $2::text || (o.n % 50)::text, o.n, o.id, CASE WHEN o.n % 3 = 0 THEN 'sha-pending' END
			FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)`, sessions, prefix)
	}
	claims(others, "acme/plan-")
	exec("timers", `
		INSERT INTO session_timers (session_id, name, fires_at)
		SELECT o.id, k.name, now() + interval '1 hour'
		FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)
		CROSS JOIN (VALUES ('liveness_check'), ('inactivity'), ('turn_deadline')) AS k(name)
		WHERE k.name <> 'turn_deadline' OR o.n % 2 = 0`, others)

	var long pgtype.UUID
	storeLong := func() {
		if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source, repos, spawn_depth) VALUES ('github', '[]'::jsonb, 0) RETURNING id`).Scan(&long); err != nil {
			t.Fatalf("create the long session: %v", err)
		}
		if shape.interleave > 0 {
			exec("the long session's ended turns, among others'", `
				INSERT INTO turns (session_id, status, created_at, completed_at, is_review_attempt, review_head_sha, request_trigger, end_reason)
				SELECT CASE WHEN g % $3::int = 0 THEN $1::uuid ELSE ($4::uuid[])[1 + g % cardinality($4::uuid[])] END,
				       (ARRAY['completed', 'failed', 'cancelled'])[1 + (g / $3::int) % 3]::turn_status,
				       now() - make_interval(secs => ($2::int * $3::int - g) / $3::int + 10),
				       now() - make_interval(secs => ($2::int * $3::int - g) / $3::int + 9),
				       (g / $3::int) % 5 = 0,
				       CASE WHEN (g / $3::int) % 5 = 0 THEN 'sha-' || g END,
				       CASE WHEN (g / $3::int) % 25 = 0 THEN 'auto' END,
				       CASE WHEN (g / $3::int) % 175 = 0 THEN 'context_moved' END
				FROM generate_series(1, $2::int * $3::int) g`, long, shape.longEnded, shape.interleave, others)
		} else {
			turns("the long session's ended turns", long, shape.longEnded, `$1::uuid`)
		}
		switch shape.longTail {
		case "processing":
			exec("the long session's processing turn", `INSERT INTO turns (session_id, status, dispatched_at) VALUES ($1, 'processing', now())`, long)
		case "attempt":
			exec("the long session's automatic attempt", `
				INSERT INTO turns (session_id, status, is_review_attempt, review_head_sha, review_verdict_context, request_trigger)
				VALUES ($1, 'pending', true, 'sha-queued', '{"baseRef":"main","baseSha":"b","policyVersion":1}', COALESCE(NULLIF($2::text, ''), 'auto'))`, long, shape.tailTrigger)
		case "queued":
			// Created half a second before the session's last ended turn
			// ended, after that turn was created.
			exec("the long session's queued automatic attempt", `
				INSERT INTO turns (session_id, status, created_at, is_review_attempt, review_head_sha, review_verdict_context, request_trigger)
				SELECT $1, 'pending', max(completed_at) - interval '0.5 seconds', true, 'sha-queued', '{"baseRef":"main","baseSha":"b","policyVersion":1}', COALESCE(NULLIF($2::text, ''), 'auto')
				FROM turns WHERE session_id = $1`, long, shape.tailTrigger)
		}
		exec("the long session's claim", `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id, pending_retrigger_head_sha) VALUES ('acme/long', 1, $1, 'sha-pending')`, long)
		exec("the long session's timers", `
			INSERT INTO session_timers (session_id, name, fires_at)
			VALUES ($1, 'liveness_check', now() + interval '1 hour'), ($1, 'review_retrigger_debounce', now() + interval '10 minutes')`, long)
	}
	if !shape.longAfterAnalyze {
		storeLong()
	}
	exec("analyze", `ANALYZE`)
	if shape.longAfterAnalyze {
		storeLong()
	}

	probes := []holdPlanProbe{{name: "the long session", sessionID: long, open: shape.longTail != ""}}
	if shape.openEvery > 0 {
		var open pgtype.UUID
		if err := open.Scan(others[shape.openEvery-1]); err != nil {
			t.Fatal(err)
		}
		probes = append(probes, holdPlanProbe{name: "a small session with an open turn", sessionID: open, open: true})
	}
	var closed pgtype.UUID
	if err := closed.Scan(others[0]); err != nil {
		t.Fatal(err)
	}
	probes = append(probes, holdPlanProbe{name: "a small session with none open", sessionID: closed})
	// Each probe's own turns, counted: an interleaved shape gives the other
	// sessions turns beyond perOther.
	for i := range probes {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, probes[i].sessionID).Scan(&probes[i].turns); err != nil {
			t.Fatalf("count the turns of %s: %v", probes[i].name, err)
		}
	}
	return probes
}

// generatedQuery returns the SQL sqlc generated for one query, read from
// its generated Go source: the text the store sends.
func generatedQuery(t *testing.T, file, constName string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("sqlcgen", file))
	if err != nil {
		t.Fatalf("read the generated %s: %v", file, err)
	}
	m := regexp.MustCompile("(?s)const " + constName + " = `(.*?)`").FindSubmatch(src)
	if m == nil {
		t.Fatalf("no generated query %s in %s", constName, file)
	}
	return string(m[1])
}

// before159 is query as main sent it before this release: the generated
// text with each of cuts removed -- each must occur exactly as many times
// as listed, so a change to the statement fails here instead of measuring
// a stranger -- and renamed, so its prepared statement is found apart from
// the store's.
func before159(t *testing.T, query, name string, cuts map[string]int) string {
	t.Helper()
	for cut, n := range cuts {
		if got := strings.Count(query, cut); got != n {
			t.Fatalf("%s: %q occurs %d times, want %d: the statement changed, update the cut", name, cut, got, n)
		}
		query = strings.ReplaceAll(query, cut, "")
	}
	if !strings.HasPrefix(query, "-- name: "+name+" ") {
		t.Fatalf("%s: the generated text does not open with its name", name)
	}
	return "-- name: Before159" + strings.TrimPrefix(query, "-- name: ")
}

// preparedStatement is a holdPlanStatement for a statement the test
// prepares itself on the pool's one connection -- main's text of a
// statement this release changed -- and runs by name.
func preparedStatement(name, sql string, args func(p holdPlanProbe) []any, explain func(p holdPlanProbe) string) holdPlanStatement {
	stmt := "before159_" + strings.ToLower(name)
	return holdPlanStatement{
		name: "Before159" + name,
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			if _, err := tx.Conn().Prepare(ctx, stmt, sql); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, stmt, args(p)...)
			if err != nil {
				return err
			}
			rows.Close()
			return rows.Err()
		},
		args: explain,
	}
}

// measureAgainstMain measures changed, this release's text of a read, and
// before, main's text of it, for probe in mode, each as measureHoldPlan
// does, on tables settled for both: each text runs through measureHoldPlan
// once, unmeasured, before either is measured.
//
// The writes measured before a read leave row versions their rolled-back
// transactions wrote -- CreateTurn's pending turns, an update's new
// version -- and those versions' index entries. The first plain index
// scan to meet such an entry reads the heap page its version lies on,
// learns it is dead and marks the entry so -- unless deduplication has
// merged it into a posting list with a live version, which both texts
// then pay alike -- and every later scan skips a marked entry without
// that read. A bitmap plan's heap scan reads the page as well, but
// nothing in a bitmap plan marks the entry. Measured one right after the
// other, the first text whose plan is a plain index scan pays for the
// marking -- which one turns on whether the eight runs before its EXPLAIN
// planned a bitmap or an index scan, which ANALYZE's sample decides -- so
// the second reads less for the same plan. Once each text has run its
// plans once, neither measured run can be the first to mark an entry:
// each plan it runs has run since the last write, and nothing writes in
// between.
func measureAgainstMain(ctx context.Context, t *testing.T, pool *pgxpool.Pool, changed, before holdPlanStatement, probe holdPlanProbe, mode string) (release, mainText holdPlanMeasurement) {
	t.Helper()
	for _, s := range []holdPlanStatement{changed, before} {
		measureHoldPlan(ctx, t, pool, s, probe, mode)
	}
	return measureHoldPlan(ctx, t, pool, changed, probe, mode), measureHoldPlan(ctx, t, pool, before, probe, mode)
}

// scanShape is a plan's scans without their counts: which node reads
// which relation through which index, in plan order.
func scanShape(scans []pagePlanScan) string {
	parts := make([]string, len(scans))
	for i, s := range scans {
		parts[i] = s.Node + " " + s.Relation + " " + s.Index
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// contextReadProblem returns why a read of a session's turns reads more
// than holdPlanMaxBuffers of its own turns -- a read of another session's
// -- or, when noSeqScan, scans turns sequentially; or "". A changed reader
// is held to no sequential scan only through "no worse than main's text":
// a custom plan of main's own text may scan a table whose long session
// holds a large share of it, and this release does not change that plan.
func contextReadProblem(m holdPlanMeasurement, ownTurns int, noSeqScan bool) string {
	if limit := holdPlanMaxBuffers(ownTurns); m.buffers > limit {
		return fmt.Sprintf("reads %.0f buffers, over %.0f for %d turns of its own: it reads other sessions' turns (%v)", m.buffers, limit, ownTurns, m.scans)
	}
	for _, s := range m.scans {
		if noSeqScan && s.Relation == "turns" && s.Node == "Seq Scan" {
			return fmt.Sprintf("scans turns sequentially: %s", s)
		}
	}
	return ""
}

// TestReviewContextCheck_PlansReadTheSessionsOwnTurns measures, by buffers
// read, every statement technical plan §24.9's context check adds or
// changes, under a custom plan and under the generic plan pgx's statement
// cache lets Postgres settle on from a statement's sixth run, on the hold's
// tables plus a fourth whose long review session ends with a queued
// automatic attempt and nothing in flight -- the pre-read's longest walk.
//
// The readers it changed -- GetNewestReviewAttempt and
// ExistsNewerReviewAttempt, which now skip a turn ended context_moved,
// GetSessionActivityFacts, whose last run and newest turn skip it and
// which reads the re-review's drop, and ListTurnsForSession, which breaks
// a created_at tie by id -- must read no more than main's text of the same
// statement on the same table, the two measured once the table is settled
// for both (measureAgainstMain), and, like the context check's own pre-read
// (GetReviewAttemptToCheck), never more than holdPlanMaxBuffers of the
// session's own turns; the pre-read never scans turns sequentially either.
// The writes --
// UpdateTurnStatus (which may now set end_reason) and
// UpsertPendingRetriggerHeadSHA (which now clears a drop) beside main's
// text of each, and the new SetTurnContextUnconfirmed,
// RequeueAutoRetrigger, DropAutoRetrigger, ResetAutoRetriggerContextMoves
// and RequeueReviewRetriggerDebounce -- must read at most
// contextWriteMaxBuffers each, whatever the table holds (a write of a turn
// contextTurnWriteIndexEntryMaxBuffers more, for its entry in an index
// added since: contextWriteLimit); the two changed
// ones on main's own access path too (a write's own buffers vary by one
// between two runs, as its new row version lands). The writes are
// measured after every read, a write of a turn on turns settled for it
// (measureTurnWrites).
func TestReviewContextCheck_PlansReadTheSessionsOwnTurns(t *testing.T) {
	const budget = 10
	uuidArg := func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) }
	// newestTurn is the probe's newest turn, the one its writes write.
	var newestTurn func(p holdPlanProbe) pgtype.UUID
	turnArg := func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", newestTurn(p).String()) }
	// claim is the probe's pull request.
	var claim func(p holdPlanProbe) (string, int32)
	// accepted is the created_at of the probe's newest review attempt: the
	// accepted attempt ExistsNewerReviewAttempt asks about, as the merge
	// path asks it -- is any attempt newer than the one a person accepted.
	var accepted func(p holdPlanProbe) pgtype.Timestamptz
	acceptedArgs := func(p holdPlanProbe) string {
		return fmt.Sprintf("%s, '%s'::timestamptz", uuidArg(p), accepted(p).Time.Format(time.RFC3339Nano))
	}
	claimArgs := func(p holdPlanProbe, rest string) string {
		repo, pr := claim(p)
		return fmt.Sprintf("'%s', %d%s", repo, pr, rest)
	}

	newestAttempt := generatedQuery(t, "turns.sql.go", "getNewestReviewAttempt")
	existsNewer := generatedQuery(t, "turns.sql.go", "existsNewerReviewAttempt")
	facts := generatedQuery(t, "sessions.sql.go", "getSessionActivityFacts")
	listTurns := generatedQuery(t, "turns.sql.go", "listTurnsForSession")
	updateStatus := generatedQuery(t, "turns.sql.go", "updateTurnStatus")
	upsertPending := generatedQuery(t, "githubprsessions.sql.go", "upsertPendingRetriggerHeadSHA")

	type pair struct {
		changed, before holdPlanStatement
		write           bool
	}
	pairs := []pair{
		{
			changed: holdPlanStatement{
				name: "GetNewestReviewAttempt",
				run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
					_, err := narvipg.NewTurnStore(nil).WithTx(tx).NewestReviewAttempt(ctx, p.sessionID)
					return err
				},
				args: uuidArg,
			},
			before: preparedStatement("GetNewestReviewAttempt",
				before159(t, newestAttempt, "GetNewestReviewAttempt", map[string]int{" AND end_reason IS NULL": 1}),
				func(p holdPlanProbe) []any { return []any{p.sessionID} }, uuidArg),
		},
		{
			changed: holdPlanStatement{
				name: "ExistsNewerReviewAttempt",
				run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
					_, err := narvipg.NewTurnStore(nil).WithTx(tx).ExistsNewerReviewAttempt(ctx, p.sessionID, accepted(p))
					return err
				},
				args: acceptedArgs,
			},
			before: preparedStatement("ExistsNewerReviewAttempt",
				before159(t, existsNewer, "ExistsNewerReviewAttempt", map[string]int{" AND end_reason IS NULL": 1}),
				func(p holdPlanProbe) []any { return []any{p.sessionID, accepted(p)} },
				acceptedArgs),
		},
		{
			changed: holdPlanStatement{
				name: "GetSessionActivityFacts",
				run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
					_, err := narvipg.NewSessionStore(nil).WithTx(tx).ActivityFacts(ctx, p.sessionID, budget)
					return err
				},
				args: func(p holdPlanProbe) string { return fmt.Sprintf("%d, %s", budget, uuidArg(p)) },
			},
			before: preparedStatement("GetSessionActivityFacts",
				before159(t, facts, "GetSessionActivityFacts", map[string]int{
					" AND t.end_reason IS NULL": 2,
					"    reretrigger.dropped_at::timestamptz AS review_retrigger_dropped_at,\n":                    1,
					"    COALESCE(reretrigger.dropped_head_sha, '')::text AS review_retrigger_dropped_head_sha,\n": 1,
					",\n    max(gps.auto_retrigger_dropped_at) AS dropped_at,\n    (array_agg(gps.auto_retrigger_dropped_head_sha ORDER BY gps.auto_retrigger_dropped_at DESC NULLS LAST, gps.pr_number DESC))[1] AS dropped_head_sha": 1,
				}),
				func(p holdPlanProbe) []any { return []any{budget, p.sessionID} },
				func(p holdPlanProbe) string { return fmt.Sprintf("%d, %s", budget, uuidArg(p)) }),
		},
		{
			changed: holdPlanStatement{
				name: "ListTurnsForSession",
				run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
					_, err := narvipg.NewTurnStore(nil).WithTx(tx).ListForSession(ctx, p.sessionID)
					return err
				},
				args: uuidArg,
			},
			before: preparedStatement("ListTurnsForSession",
				before159(t, listTurns, "ListTurnsForSession", map[string]int{", id ASC": 1}),
				func(p holdPlanProbe) []any { return []any{p.sessionID} }, uuidArg),
		},
		{
			write: true,
			changed: holdPlanStatement{
				name: "UpdateTurnStatus",
				// The context_moved end's status write, as the recorder
				// makes it.
				run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
					reason := "context_moved"
					_, err := narvipg.NewTurnStore(nil).WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
						ID: newestTurn(p), Status: sqlcgen.TurnStatusFailed,
						CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, EndReason: &reason,
					})
					return err
				},
				args: func(p holdPlanProbe) string {
					return turnArg(p) + ", 'failed', NULL, now(), NULL, NULL, NULL, 'context_moved'"
				},
			},
			before: preparedStatement("UpdateTurnStatus",
				before159(t, updateStatus, "UpdateTurnStatus", map[string]int{",\n    end_reason = COALESCE($8, end_reason)": 1}),
				func(p holdPlanProbe) []any {
					return []any{newestTurn(p), "failed", nil, pgtype.Timestamptz{Time: time.Now(), Valid: true}, nil, nil, nil}
				},
				func(p holdPlanProbe) string { return turnArg(p) + ", 'failed', NULL, now(), NULL, NULL, NULL" }),
		},
		{
			write: true,
			changed: holdPlanStatement{
				name: "UpsertPendingRetriggerHeadSHA",
				run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
					repo, pr := claim(p)
					_, err := narvipg.NewGitHubPRSessionStore(nil).WithTx(tx).UpsertPendingRetriggerHeadSHA(ctx, repo, pr, "sha-pushed")
					return err
				},
				args: func(p holdPlanProbe) string { return claimArgs(p, ", 'sha-pushed'") },
			},
			before: preparedStatement("UpsertPendingRetriggerHeadSHA",
				before159(t, upsertPending, "UpsertPendingRetriggerHeadSHA", map[string]int{",\n    auto_retrigger_dropped_at = NULL,\n    auto_retrigger_dropped_head_sha = NULL": 1}),
				func(p holdPlanProbe) []any {
					repo, pr := claim(p)
					return []any{repo, pr, "sha-pushed"}
				},
				func(p holdPlanProbe) string { return claimArgs(p, ", 'sha-pushed'") }),
		},
	}
	preRead := holdPlanStatement{
		name: "GetReviewAttemptToCheck",
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			_, err := narvipg.NewTurnStore(nil).WithTx(tx).ReviewAttemptToCheck(ctx, p.sessionID)
			return err
		},
		args: uuidArg,
	}
	writes := []holdPlanStatement{
		{
			name: "SetTurnContextUnconfirmed",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).SetContextUnconfirmed(ctx, newestTurn(p))
				return err
			},
			args: turnArg,
		},
		{
			name: "RequeueAutoRetrigger",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				repo, pr := claim(p)
				_, err := narvipg.NewGitHubPRSessionStore(nil).WithTx(tx).RequeueAutoRetrigger(ctx, repo, pr, "sha-live")
				return err
			},
			args: func(p holdPlanProbe) string { return "'sha-live', " + claimArgs(p, "") },
		},
		{
			name: "DropAutoRetrigger",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				repo, pr := claim(p)
				_, err := narvipg.NewGitHubPRSessionStore(nil).WithTx(tx).DropAutoRetrigger(ctx, repo, pr, "sha-pending")
				return err
			},
			args: func(p holdPlanProbe) string { return "'sha-pending', " + claimArgs(p, "") },
		},
		{
			name: "ResetAutoRetriggerContextMoves",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewGitHubPRSessionStore(nil).WithTx(tx).ResetAutoRetriggerContextMoves(ctx, p.sessionID)
				return err
			},
			args: uuidArg,
		},
		{
			name: "RequeueReviewRetriggerDebounce",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTimerStore(nil).WithTx(tx).RequeueReviewRetriggerDebounce(ctx, p.sessionID)
				return err
			},
			args: uuidArg,
		},
	}
	modes := []string{"force_custom_plan", "force_generic_plan"}

	for _, shape := range []contextPlanShape{
		{name: "5,000 sessions of 6 turns, every 20th open, beside a review session of 4,000 ended turns and one processing", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "processing"},
		{name: "300 sessions of 200 ended turns beside one of 20,000, none open", others: 300, perOther: 200, longEnded: 20_000},
		{name: "the first, the review session stored after ANALYZE", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "processing", longAfterAnalyze: true},
		{name: "the first, the review session's last turn an automatic attempt that waited behind no turn", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "attempt"},
		{name: "the first, the review session's 4,000 turns among 316,000 of the others', its last an automatic attempt that waited behind no turn", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "attempt", interleave: 80},
		{name: "the same, the attempt queued behind the session's last ended turn", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "queued", interleave: 80},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := holdPlanDatabase(ctx, t, holdPlanLatestVersion)
			probes := storeContextPlanShape(ctx, t, pool, shape)
			if shape.longTail == "attempt" || shape.longTail == "queued" {
				pick, err := narvipg.NewTurnStore(pool).ReviewAttemptToCheck(ctx, probes[0].sessionID)
				if err != nil {
					t.Fatalf("the long session's pick: %v", err)
				}
				if want := shape.longTail == "queued"; pick.Queued != want {
					t.Fatalf("the long session's attempt reads queued %v, want %v: the shape is not what it says", pick.Queued, want)
				}
			}
			newest := map[pgtype.UUID]pgtype.UUID{}
			claims := map[pgtype.UUID]struct {
				repo string
				pr   int32
			}{}
			for _, p := range probes {
				var id pgtype.UUID
				if err := pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, p.sessionID).Scan(&id); err != nil {
					t.Fatalf("the newest turn of %s: %v", p.name, err)
				}
				newest[p.sessionID] = id
				var c struct {
					repo string
					pr   int32
				}
				if err := pool.QueryRow(ctx, `SELECT repo_full_name, pr_number FROM github_pr_sessions WHERE session_id = $1`, p.sessionID).Scan(&c.repo, &c.pr); err != nil {
					t.Fatalf("the claim of %s: %v", p.name, err)
				}
				claims[p.sessionID] = c
			}
			attempts := map[pgtype.UUID]pgtype.Timestamptz{}
			for _, p := range probes {
				var at pgtype.Timestamptz
				// A session that ran no review attempt asks about its oldest turn.
				if err := pool.QueryRow(ctx, `SELECT COALESCE(max(created_at) FILTER (WHERE is_review_attempt), min(created_at)) FROM turns WHERE session_id = $1`, p.sessionID).Scan(&at); err != nil || !at.Valid {
					t.Fatalf("the newest review attempt of %s: %v (valid %v)", p.name, err, at.Valid)
				}
				attempts[p.sessionID] = at
			}
			accepted = func(p holdPlanProbe) pgtype.Timestamptz { return attempts[p.sessionID] }
			newestTurn = func(p holdPlanProbe) pgtype.UUID { return newest[p.sessionID] }
			claim = func(p holdPlanProbe) (string, int32) { c := claims[p.sessionID]; return c.repo, c.pr }

			// The reads first, on turns as the shape stored them: settling a
			// write of a turn vacuums turns, which would set the visibility
			// map the reads are measured without.
			for _, probe := range probes {
				for _, mode := range modes {
					for _, pr := range pairs {
						if pr.write {
							continue
						}
						key := pr.changed.name + ", " + probe.name + ", " + mode
						after, before := measureAgainstMain(ctx, t, pool, pr.changed, pr.before, probe, mode)
						t.Logf("%s: main's text %v; this release's %v", key, before, after)
						if after.buffers > before.buffers {
							t.Errorf("%s: reads %.0f buffers, main's text %.0f: the change made it worse", key, after.buffers, before.buffers)
						}
						if problem := contextReadProblem(after, probe.turns, false); problem != "" {
							t.Errorf("%s: %s", key, problem)
						}
					}
					got := measureHoldPlan(ctx, t, pool, preRead, probe, mode)
					t.Logf("GetReviewAttemptToCheck, %s, %s: %v", probe.name, mode, got)
					if problem := contextReadProblem(got, probe.turns, true); problem != "" {
						t.Errorf("GetReviewAttemptToCheck, %s, %s: %s", probe.name, mode, problem)
					}
				}
			}
			// measure measures statements, texts of the write named name, in
			// turn: on turns settled for them when it writes a turn
			// (measureTurnWrites).
			measure := func(name string, probe holdPlanProbe, mode string, statements ...holdPlanStatement) []holdPlanMeasurement {
				if contextWritesTurns[name] {
					return measureTurnWrites(ctx, t, pool, probe, mode, statements...)
				}
				out := make([]holdPlanMeasurement, len(statements))
				for i, s := range statements {
					out[i] = measureHoldPlan(ctx, t, pool, s, probe, mode)
				}
				return out
			}
			for _, probe := range probes {
				for _, mode := range modes {
					for _, pr := range pairs {
						if !pr.write {
							continue
						}
						key := pr.changed.name + ", " + probe.name + ", " + mode
						m := measure(pr.changed.name, probe, mode, pr.before, pr.changed)
						before, after := m[0], m[1]
						t.Logf("%s: main's text %v; this release's %v", key, before, after)
						// A write's own buffers vary by one between two
						// runs on a densely loaded table -- where its new
						// row version lands -- so its plan is held to
						// main's, its read to the bound.
						if got, want := scanShape(after.scans), scanShape(before.scans); got != want {
							t.Errorf("%s: plans %s, main's text %s: the change moved its access path", key, got, want)
						}
						if limit := contextWriteLimit(pr.changed.name); after.buffers > limit {
							t.Errorf("%s: reads %.0f buffers, over %.0f (%v)", key, after.buffers, limit, after.scans)
						}
					}
					for _, w := range writes {
						got := measure(w.name, probe, mode, w)[0]
						t.Logf("%s, %s, %s: %v", w.name, probe.name, mode, got)
						if limit := contextWriteLimit(w.name); got.buffers > limit {
							t.Errorf("%s, %s, %s: reads %.0f buffers, over %.0f (%v)", w.name, probe.name, mode, got.buffers, limit, got.scans)
						}
					}
				}
			}
		})
	}
}
