//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

const (
	// holdReadMaxBuffers bounds what the re-review hold's read
	// (ReviewRetriggerHeld) reads: a descent of turns_open_session_id_idx,
	// which holds the open turns only, and at most the heap page of the
	// one open turn it finds. Walking the session's history reads a buffer
	// for a few dozen turns; a scan of the table, hundreds.
	holdReadMaxBuffers = 6
	// holdWakeMaxBuffers bounds the turn end's wake-up of the debounce
	// (WakeReviewRetriggerDebounce): a probe of session_timers' (session_id,
	// name) unique index and the row's heap page, one of sessions_pkey and
	// the session's page, and -- fires_at being indexed, so no update is
	// heap-only -- the new row version and its entry in each of the
	// table's three indexes.
	holdWakeMaxBuffers = 12
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
}

// holdPlanProbe is one session a statement is measured for.
type holdPlanProbe struct {
	name      string
	sessionID pgtype.UUID
	turns     int
	open      bool
}

// holdPlanDatabase creates a database of its own in the shared container,
// migrated to version, and returns a pool of exactly one connection on it
// and its connection string, like pagePlanDatabase.
func holdPlanDatabase(ctx context.Context, t *testing.T, version uint) (*pgxpool.Pool, string) {
	t.Helper()
	admin, adminConnStr := IntegrationTestPoolAndConnStr(t)
	name := fmt.Sprintf("holdplan_%d", time.Now().UnixNano())
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
	if err := m.Migrate(version); err != nil {
		_ = mdb.Close()
		t.Fatalf("migrate %s to %d: %v", name, version, err)
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
		       (ARRAY['completed', 'failed', 'cancelled'])[1 + g % 3]::turn_status,
		       now() - make_interval(secs => $2::int - g)
		FROM generate_series(1, $2::int) g`, others, shape.others*shape.perOther)
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
			SELECT $1, (ARRAY['completed', 'failed', 'cancelled'])[1 + g % 3]::turn_status, now() - make_interval(secs => $2::int - g)
			FROM generate_series(1, $2::int) g`, long, shape.longEnded)
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
	name   string
	writes bool
	run    func(ctx context.Context, q pgx.Tx, probe holdPlanProbe) error
	args   func(probe holdPlanProbe) string
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
// times and then under EXPLAIN (ANALYZE, BUFFERS) EXECUTE in mode, all on
// the pool's one connection inside a transaction that is rolled back.
func measureHoldPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement holdPlanStatement, probe holdPlanProbe, mode string) holdPlanMeasurement {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i := 0; i < 8; i++ {
		if err := statement.run(ctx, tx, probe); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s for %s: %v", statement.name, probe.name, err)
		}
	}
	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
		t.Fatalf("set plan_cache_mode: %v", err)
	}
	plan := explainPlan(ctx, t, tx, statement.name, statement.args(probe))
	scans, buffers, err := planScans(plan, "")
	if err != nil {
		t.Fatalf("read %s's plan: %v\n%s", statement.name, err, plan)
	}
	return holdPlanMeasurement{buffers: buffers, scans: scans}
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
// Before migration 000158 and after it, the four statements that read a
// session's open or in-flight turns -- GetSessionActivityFacts,
// RequestStopOpenTurns, ListStopRequestedOpenTurns,
// GetProcessingTurnForSession -- must read no more after than before, and
// never more than holdPlanMaxBuffers of the session's own turns. After it,
// the hold's read must read at most holdReadMaxBuffers, its only scan of
// turns on turns_open_session_id_idx -- never a sequential or bitmap heap
// scan of turns -- and the wake-up at most holdWakeMaxBuffers, through
// index scans of session_timers' (session_id, name) key and sessions_pkey
// alone, whatever the table holds.
func TestReviewRetriggerHold_PlansReadTheSessionsOwnTurns(t *testing.T) {
	const budget = 10
	grace := (30 * time.Second).Seconds()
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
			name: "RequestStopOpenTurns", writes: true,
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
	wake := holdPlanStatement{
		name: "WakeReviewRetriggerDebounce", writes: true,
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			_, err := narvipg.NewTimerStore(nil).WithTx(tx).WakeReviewRetriggerDebounce(ctx, p.sessionID)
			return err
		},
		args: func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) },
	}
	modes := []string{"force_custom_plan", "force_generic_plan"}

	for _, shape := range []holdPlanShape{
		{name: "5,000 sessions of 6 turns, every 20th open, beside a review session of 4,000 ended turns and one processing", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longOpen: true},
		{name: "300 sessions of 200 ended turns beside one of 20,000, none open", others: 300, perOther: 200, longEnded: 20_000},
		{name: "the first, the review session stored after ANALYZE", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longOpen: true, longAfterAnalyze: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			pool, connStr := holdPlanDatabase(ctx, t, 157)
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

			m, mdb := newMigrate(t, connStr)
			err := m.Migrate(158)
			_ = mdb.Close()
			if err != nil {
				t.Fatalf("migrate to 158: %v", err)
			}

			for _, statement := range unchanged {
				for _, probe := range probes {
					for _, mode := range modes {
						key := statement.name + ", " + probe.name + ", " + mode
						after := measureHoldPlan(ctx, t, pool, statement, probe, mode)
						t.Logf("%s: before 000158 %v; after %v", key, before[key], after)
						if after.buffers > before[key].buffers {
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
					// or not: the long one has a debounce, the small ones none.
					got = measureHoldPlan(ctx, t, pool, wake, probe, mode)
					t.Logf("WakeReviewRetriggerDebounce, %s, %s: %v", probe.name, mode, got)
					if problem := holdWakeProblem(got); problem != "" {
						t.Errorf("WakeReviewRetriggerDebounce, %s, %s: %s", probe.name, mode, problem)
					}
				}
			}
		})
	}
}

// holdReadProblem returns why the hold's read is not one probe of the open
// turns' index, or "".
func holdReadProblem(m holdPlanMeasurement) string {
	if m.buffers > holdReadMaxBuffers {
		return fmt.Sprintf("reads %.0f buffers, over %d (%v)", m.buffers, holdReadMaxBuffers, m.scans)
	}
	onIndex := false
	for _, s := range m.scans {
		if s.Relation != "turns" {
			continue
		}
		if s.Index != "turns_open_session_id_idx" || strings.Contains(s.Node, "Seq Scan") || strings.Contains(s.Node, "Bitmap Heap Scan") {
			return fmt.Sprintf("scans turns other than through turns_open_session_id_idx: %s", s)
		}
		onIndex = true
	}
	if !onIndex {
		return fmt.Sprintf("never scans turns_open_session_id_idx (%v)", m.scans)
	}
	return ""
}

// holdWakeProblem returns why the wake-up is not two key probes, or "".
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
