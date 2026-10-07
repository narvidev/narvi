//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/migrations"
)

// holdPlanLatestVersion is the migration the hold's plan test migrates
// to: the latest whose columns the statements it measures name.
const holdPlanLatestVersion = reviewCheckoutGenMigration

const (
	// holdReadMaxBuffers bounds what the re-review hold's read
	// (ReviewRetriggerHeld) reads: a descent of turns_open_session_id_idx,
	// which holds the open turns only, and at most the heap page of the
	// one open turn it finds. Walking the session's history reads a buffer
	// for a few dozen turns; a scan of the table, hundreds.
	holdReadMaxBuffers = 6
	// holdWakeMaxBuffers bounds the hold's re-arm and the turn end's
	// wake-up of the debounce (HoldReviewRetriggerDebounce,
	// WakeReviewRetriggerDebounce), each the row's first write in its
	// transaction: a probe of session_timers' (session_id, name) unique
	// index and the row's heap page, one of sessions_pkey and the session's
	// page, and -- fires_at being indexed, so no update is heap-only -- the
	// new row version and its entry in each of the table's three indexes.
	// On the densely loaded tables below the row's page has no room for the
	// new version, so the update also reads the free space map and extends
	// the table: 19 or 20 buffers measured, whatever the table holds. A row
	// updated again in the same transaction finds room on the page its last
	// version went to, and reads 9.
	holdWakeMaxBuffers = 24
	// openTurnIndexEntryMaxBuffers bounds what adding a row version's entry
	// to turns_open_session_id_idx reads: a descent of an index that holds
	// the open turns only, one or two levels.
	openTurnIndexEntryMaxBuffers = 2
)

// holdPlanMaxBuffers bounds what the statements reading a session's open
// or in-flight turns read once migration 000158 lets them plan on
// turns_open_session_id_idx (GetSessionActivityFacts, RequestStopOpenTurns,
// ListStopRequestedOpenTurns, GetProcessingTurnForSession), as measured on
// the tables below: a fixed number of index descents across the
// statements' other relations, plus up to a few buffers for each of the
// session's own turns -- GetSessionActivityFacts reads them all in three
// of its laterals (the histogram, the last run and the newest turn), a
// buffer a turn each where the session's turns lie one to a heap page --
// and never one for another session's. The open-turn statements read a
// buffer or two once the index serves them; a scan of the table reads
// every page of it.
func holdPlanMaxBuffers(ownTurns int) float64 {
	return float64(60 + 4*ownTurns)
}

// holdPlanShape is a turns table TestReviewRetriggerHold_PlansReadTheSessionsOwnTurns
// reads: others sessions of perOther ended turns each, every openEvery-th of
// them with one pending turn more (none when 0), and a long review session
// of longEnded ended turns, plus one processing when longOpen -- stored
// after the tables were analyzed when longAfterAnalyze, so the statistics
// know nothing of it. Every session has two or three timers; the long one
// has the re-review debounce.
type holdPlanShape struct {
	name                        string
	others, perOther, openEvery int
	longEnded                   int
	longOpen, longAfterAnalyze  bool
	// splitOverOne gives 80,000 ended turns 26,600 completed, 26,600
	// failed and 26,800 cancelled, the long session's all cancelled, and
	// has ANALYZE read every row: the three frequencies the statistics
	// store, each rounded to a float4, then sum to a hair over 1.0. A
	// selectivity estimate of "status is none of the three" that sums them
	// as disjoint finds the result out of range and falls back to treating
	// them as independent -- about 30% of the table instead of none.
	splitOverOne bool
}

// holdPlanProbe is one session a statement is measured for.
type holdPlanProbe struct {
	name      string
	sessionID pgtype.UUID
	turns     int
	open      bool
}

// holdPlanDatabase creates a database of its own in the shared container,
// migrated to version (migratedDatabase), and returns a pool of exactly one
// connection on it and its connection string, like pagePlanDatabase.
func holdPlanDatabase(ctx context.Context, t *testing.T, version uint) (*pgxpool.Pool, string) {
	t.Helper()
	name, connStr := migratedDatabase(ctx, t, "holdplan", version)
	pool, err := narvipg.NewPoolWithMaxConns(ctx, connStr, 1)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool, connStr
}

// storeHoldPlanShape stores shape and analyzes the tables -- before the
// long session when shape.longAfterAnalyze -- and returns the sessions to
// probe: the long one, a small one with an open turn when the shape has
// any, and a small one with none.
func storeHoldPlanShape(ctx context.Context, t *testing.T, pool *pgxpool.Pool, shape holdPlanShape) []holdPlanProbe {
	t.Helper()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	// Autovacuum stays off the measured tables, so every run reads its
	// plans in one state -- the one a busy table is in between two vacuums,
	// its visibility map unset -- rather than in whichever state a worker
	// that happened by left it.
	for _, table := range []string{"sessions", "turns", "session_timers"} {
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
	exec("the other sessions' ended turns", `
		INSERT INTO turns (session_id, status, created_at)
		SELECT ($1::uuid[])[1 + g % cardinality($1::uuid[])],
		       CASE WHEN NOT $3::boolean THEN (ARRAY['completed', 'failed', 'cancelled'])[1 + g % 3]
		            WHEN g <= 26600 THEN 'completed' WHEN g <= 53200 THEN 'failed' ELSE 'cancelled' END::turn_status,
		       now() - make_interval(secs => $2::int - g)
		FROM generate_series(1, $2::int) g`, others, shape.others*shape.perOther, shape.splitOverOne)
	if shape.openEvery > 0 {
		exec("the other sessions' open turns", `
			INSERT INTO turns (session_id, status)
			SELECT o.id, 'pending' FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)
			WHERE o.n % $2::int = 0`, others, shape.openEvery)
	}
	timers := func(sessions []string) {
		exec("timers", `
			INSERT INTO session_timers (session_id, name, fires_at)
			SELECT o.id, k.name, now() + interval '1 hour'
			FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)
			CROSS JOIN (VALUES ('liveness_check'), ('inactivity'), ('turn_deadline')) AS k(name)
			WHERE k.name <> 'turn_deadline' OR o.n % 2 = 0`, sessions)
	}
	timers(others)

	var long pgtype.UUID
	storeLong := func() {
		if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source, repos, spawn_depth) VALUES ('github', '[]'::jsonb, 0) RETURNING id`).Scan(&long); err != nil {
			t.Fatalf("create the long session: %v", err)
		}
		exec("the long session's ended turns", `
			INSERT INTO turns (session_id, status, created_at)
			SELECT $1, CASE WHEN $3::boolean THEN 'cancelled' ELSE (ARRAY['completed', 'failed', 'cancelled'])[1 + g % 3] END::turn_status,
			       now() - make_interval(secs => $2::int - g)
			FROM generate_series(1, $2::int) g`, long, shape.longEnded, shape.splitOverOne)
		if shape.longOpen {
			exec("the long session's processing turn", `INSERT INTO turns (session_id, status, dispatched_at) VALUES ($1, 'processing', now())`, long)
		}
		exec("the long session's timers", `
			INSERT INTO session_timers (session_id, name, fires_at)
			VALUES ($1, 'liveness_check', now() + interval '1 hour'), ($1, 'review_retrigger_debounce', now() + interval '10 minutes')`, long)
	}
	if !shape.longAfterAnalyze {
		storeLong()
	}
	if shape.splitOverOne {
		exec("full statistics of status", `ALTER TABLE turns ALTER COLUMN status SET STATISTICS 1000`)
	}
	exec("analyze", `ANALYZE`)
	if shape.longAfterAnalyze {
		storeLong()
	}

	probes := []holdPlanProbe{{name: "the long session", sessionID: long, turns: shape.longEnded + boolInt(shape.longOpen), open: shape.longOpen}}
	if shape.openEvery > 0 {
		var open pgtype.UUID
		if err := open.Scan(others[shape.openEvery-1]); err != nil {
			t.Fatal(err)
		}
		probes = append(probes, holdPlanProbe{name: "a small session with an open turn", sessionID: open, turns: shape.perOther + 1, open: true})
	}
	var closed pgtype.UUID
	if err := closed.Scan(others[0]); err != nil {
		t.Fatal(err)
	}
	return append(probes, holdPlanProbe{name: "a small session with none open", sessionID: closed, turns: shape.perOther})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// holdPlanStatement is one statement the test measures: run makes the
// store call (eight times, so pgx's statement cache crosses Postgres's
// five custom plans), args is what EXPLAIN EXECUTE passes it. A statement
// that writes is measured inside a transaction rolled back.
type holdPlanStatement struct {
	name string
	run  func(ctx context.Context, q pgx.Tx, probe holdPlanProbe) error
	args func(probe holdPlanProbe) string
	// writesOpenTurn is a write of an open turn that is never heap-only:
	// once 000158 is in, its new row version also gets an entry in
	// turns_open_session_id_idx, which it may read up to
	// openTurnIndexEntryMaxBuffers more for.
	writesOpenTurn bool
}

// holdPlanMeasurement is one statement's read for one probe and mode.
type holdPlanMeasurement struct {
	buffers float64
	scans   []pagePlanScan
}

func (m holdPlanMeasurement) String() string {
	return fmt.Sprintf("%.0f buffers: %v", m.buffers, m.scans)
}

// measureHoldPlan runs statement for probe through its store call eight
// times in a transaction that is rolled back, and then once under EXPLAIN
// (ANALYZE, BUFFERS) EXECUTE in mode in another, also rolled back, all on
// the pool's one connection: the measured run finds the rows as the last
// commit left them, as every production run of a write does, rather than
// as eight writes of its own transaction did.
func measureHoldPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement holdPlanStatement, probe holdPlanProbe, mode string) holdPlanMeasurement {
	t.Helper()
	inTx := func(fn func(tx pgx.Tx)) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		fn(tx)
	}
	inTx(func(tx pgx.Tx) {
		for i := 0; i < 8; i++ {
			if err := statement.run(ctx, tx, probe); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s for %s: %v", statement.name, probe.name, err)
			}
		}
	})
	var m holdPlanMeasurement
	inTx(func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
			t.Fatalf("set plan_cache_mode: %v", err)
		}
		plan := explainPlan(ctx, t, tx, statement.name, statement.args(probe))
		scans, buffers, err := planScans(plan, "")
		if err != nil {
			t.Fatalf("read %s's plan: %v\n%s", statement.name, err, plan)
		}
		m = holdPlanMeasurement{buffers: buffers, scans: scans}
	})
	return m
}

// TestReviewRetriggerHold_PlansReadTheSessionsOwnTurns measures, by buffers
// read, the statements technical plan §24.9's hold adds and the ones its
// index enters the plan space of, under a custom plan and under the generic
// plan pgx's statement cache lets Postgres settle on from a statement's
// sixth run, on three tables: many small sessions beside a long review
// session with one turn open; fewer, longer sessions beside a longer one
// with none open; and the first with the long session stored after the
// tables were analyzed, which a custom plan expects few turns of.
//
// The statements measured are this release's, which name the columns of
// every migration since 000158, so the database is migrated to the latest
// of them (holdPlanLatestVersion); the open-turn index 000158 builds is
// taken away before the tables are stored, for the measure before it, and
// built again by 000158's own up migration for the measure after.
//
// Without that index and with it, the five statements that read a
// session's open or in-flight turns -- GetSessionActivityFacts,
// RequestStopOpenTurns, ListStopRequestedOpenTurns,
// GetProcessingTurnForSession, RecordTurnStepCost -- must read no more
// after than before, and never more than holdPlanMaxBuffers of the
// session's own turns. RecordTurnStepCost alone may read up to
// openTurnIndexEntryMaxBuffers more: its update of a processing turn's
// cost is never heap-only (cost_usd is in turns_cost_created_at_idx's
// predicate), so the new version also gets an entry in the new index --
// a write, not a read the index changed. After 000158, the hold's read
// must read at most holdReadMaxBuffers, its only scan of turns on
// turns_open_session_id_idx -- never a sequential or bitmap heap scan of
// turns -- and the hold's re-arm and the wake-up at most
// holdWakeMaxBuffers each, through index scans of session_timers'
// (session_id, name) key and sessions_pkey alone, whatever the table
// holds.
func TestReviewRetriggerHold_PlansReadTheSessionsOwnTurns(t *testing.T) {
	const budget = 10
	grace := (30 * time.Second).Seconds()
	timeouts := platform.DefaultTimeouts()
	backstop := timeouts.ReviewRetriggerHoldBackstop
	// sessionactor's heldDebounceLead.
	heldLead := timeouts.ReviewRetriggerDebounce + platform.MinTimeoutMargin
	unchanged := []holdPlanStatement{
		{
			name: "GetSessionActivityFacts",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewSessionStore(nil).WithTx(tx).ActivityFacts(ctx, p.sessionID, budget)
				return err
			},
			args: func(p holdPlanProbe) string { return fmt.Sprintf("%d, '%s'::uuid", budget, p.sessionID.String()) },
		},
		{
			name: "RequestStopOpenTurns",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).RequestStopOpen(ctx, p.sessionID)
				return err
			},
			args: func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) },
		},
		{
			name: "ListStopRequestedOpenTurns",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).ListStopRequestedOpen(ctx, p.sessionID, 30*time.Second)
				return err
			},
			args: func(p holdPlanProbe) string { return fmt.Sprintf("%v, '%s'::uuid", grace, p.sessionID.String()) },
		},
		{
			name: "GetProcessingTurnForSession",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).GetProcessingTurnForSession(ctx, p.sessionID)
				return err
			},
			args: func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) },
		},
		{
			// Every step_finish of a processing turn: a step id never
			// counted before, so the cost is added.
			name: "RecordTurnStepCost", writesOpenTurn: true,
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).RecordStepCostUSD(ctx, p.sessionID, freshStepID(), 0.01)
				return err
			},
			args: func(p holdPlanProbe) string {
				return fmt.Sprintf("'%s'::uuid, '%s', 0.01", p.sessionID.String(), freshStepID())
			},
		},
	}
	held := holdPlanStatement{
		name: "ReviewRetriggerHeld",
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			got, err := narvipg.NewTurnStore(nil).WithTx(tx).ReviewRetriggerHeld(ctx, p.sessionID)
			if err == nil && got != p.open {
				return fmt.Errorf("held = %v, want %v", got, p.open)
			}
			return err
		},
		args: func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) },
	}
	// The long session's debounce sits ten minutes past its arm: held.
	wake := holdPlanStatement{
		name: "WakeReviewRetriggerDebounce",
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			_, err := narvipg.NewTimerStore(nil).WithTx(tx).WakeReviewRetriggerDebounce(ctx, p.sessionID, heldLead)
			return err
		},

		args: func(p holdPlanProbe) string {
			return fmt.Sprintf("'%s'::uuid, %v", p.sessionID.String(), heldLead.Seconds())
		},
	}
	rearm := holdPlanStatement{
		name: "HoldReviewRetriggerDebounce",
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			_, err := narvipg.NewTimerStore(nil).WithTx(tx).HoldReviewRetriggerDebounce(ctx, p.sessionID, backstop)
			return err
		},
		args: func(p holdPlanProbe) string {
			return fmt.Sprintf("%v, '%s'::uuid", backstop.Seconds(), p.sessionID.String())
		},
	}
	modes := []string{"force_custom_plan", "force_generic_plan"}

	for _, shape := range []holdPlanShape{
		{name: "5,000 sessions of 6 turns, every 20th open, beside a review session of 4,000 ended turns and one processing", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longOpen: true},
		{name: "300 sessions of 200 ended turns beside one of 20,000, none open", others: 300, perOther: 200, longEnded: 20_000},
		{name: "the first, the review session stored after ANALYZE", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longOpen: true, longAfterAnalyze: true},
		{name: "the second, its ended states' statistics summing a hair over the whole table", others: 300, perOther: 200, longEnded: 20_000, splitOverOne: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := holdPlanDatabase(ctx, t, holdPlanLatestVersion)
			if _, err := pool.Exec(ctx, `DROP INDEX turns_open_session_id_idx`); err != nil {
				t.Fatalf("take away 000158's index: %v", err)
			}
			probes := storeHoldPlanShape(ctx, t, pool, shape)

			before := map[string]holdPlanMeasurement{}
			for _, statement := range unchanged {
				for _, probe := range probes {
					for _, mode := range modes {
						key := statement.name + ", " + probe.name + ", " + mode
						before[key] = measureHoldPlan(ctx, t, pool, statement, probe, mode)
					}
				}
			}

			up, err := fs.ReadFile(migrations.FS, "000158_turns_open_session_id_idx.up.sql")
			if err != nil {
				t.Fatalf("read 000158: %v", err)
			}
			if _, err := pool.Exec(ctx, string(up)); err != nil {
				t.Fatalf("build 000158's index again: %v", err)
			}

			for _, statement := range unchanged {
				for _, probe := range probes {
					for _, mode := range modes {
						key := statement.name + ", " + probe.name + ", " + mode
						after := measureHoldPlan(ctx, t, pool, statement, probe, mode)
						t.Logf("%s: before 000158 %v; after %v", key, before[key], after)
						allowance := 0.0
						if statement.writesOpenTurn {
							allowance = openTurnIndexEntryMaxBuffers
						}
						if after.buffers > before[key].buffers+allowance {
							t.Errorf("%s: read %.0f buffers after 000158, %.0f before: the open-turn index made it worse", key, after.buffers, before[key].buffers)
						}
						if limit := holdPlanMaxBuffers(probe.turns); after.buffers > limit {
							t.Errorf("%s: read %.0f buffers, over %.0f for %d turns of its own: it reads other sessions' turns", key, after.buffers, limit, probe.turns)
						}
					}
				}
			}

			for _, probe := range probes {
				for _, mode := range modes {
					got := measureHoldPlan(ctx, t, pool, held, probe, mode)
					t.Logf("ReviewRetriggerHeld, %s, %s: %v", probe.name, mode, got)
					if problem := holdReadProblem(got); problem != "" {
						t.Errorf("ReviewRetriggerHeld, %s, %s: %s", probe.name, mode, problem)
					}
					// Every turn's end runs the wake-up, a review session's
					// or not: the long one has a held debounce, the small
					// ones none. The re-arm runs only for a review session
					// with a turn open, and is measured on every probe too.
					for _, statement := range []holdPlanStatement{wake, rearm} {
						got = measureHoldPlan(ctx, t, pool, statement, probe, mode)
						t.Logf("%s, %s, %s: %v", statement.name, probe.name, mode, got)
						if problem := holdWakeProblem(got); problem != "" {
							t.Errorf("%s, %s, %s: %s", statement.name, probe.name, mode, problem)
						}
					}
				}
			}
		})
	}
}

// holdReadProblem returns why the hold's read is not one probe of the open
// turns' index, or "": every scan of turns is an index scan of
// turns_open_session_id_idx, or a bitmap heap scan whose bitmap comes from
// that index alone -- never a sequential scan, nor a bitmap read through
// another index of the session's turns.
func holdReadProblem(m holdPlanMeasurement) string {
	if m.buffers > holdReadMaxBuffers {
		return fmt.Sprintf("reads %.0f buffers, over %d (%v)", m.buffers, holdReadMaxBuffers, m.scans)
	}
	const openTurns = "turns_open_session_id_idx"
	onIndex := false
	for _, s := range m.scans {
		switch {
		case s.Node == "Bitmap Index Scan":
			if s.Index != openTurns {
				return fmt.Sprintf("reads a bitmap of turns through another index: %s", s)
			}
			onIndex = true
		case s.Relation != "turns":
		case s.Node == "Bitmap Heap Scan":
			// Its Bitmap Index Scans are checked above.
		case (s.Node == "Index Scan" || s.Node == "Index Only Scan") && s.Index == openTurns:
			onIndex = true
		default:
			return fmt.Sprintf("scans turns other than through %s: %s", openTurns, s)
		}
	}
	if !onIndex {
		return fmt.Sprintf("never scans %s (%v)", openTurns, m.scans)
	}
	return ""
}

// stepIDs numbers the step ids freshStepID hands out.
var stepIDs atomic.Int64

// freshStepID is a step id no step_finish has counted, so
// RecordTurnStepCost adds the cost instead of skipping a redelivery.
func freshStepID() string {
	return fmt.Sprintf("step-%d", stepIDs.Add(1))
}

// holdWakeProblem returns why the hold's re-arm or the wake-up is not two
// key probes, or "".
func holdWakeProblem(m holdPlanMeasurement) string {
	if m.buffers > holdWakeMaxBuffers {
		return fmt.Sprintf("reads %.0f buffers, over %d (%v)", m.buffers, holdWakeMaxBuffers, m.scans)
	}
	want := map[string]string{"session_timers": "session_timers_session_id_name_key", "sessions": "sessions_pkey"}
	seen := map[string]bool{}
	for _, s := range m.scans {
		if s.Node == "ModifyTable" {
			// The row the UPDATE writes, not a scan.
			continue
		}
		if index, ok := want[s.Relation]; !ok || s.Index != index || !strings.HasPrefix(s.Node, "Index") {
			return fmt.Sprintf("scans %s, want index scans of %v only", s, want)
		}
		seen[s.Relation] = true
	}
	if len(seen) != len(want) {
		return fmt.Sprintf("scans %v, want both of %v", m.scans, want)
	}
	return ""
}
