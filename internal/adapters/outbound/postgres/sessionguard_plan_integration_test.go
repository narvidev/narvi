//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/migrations"
)

// guardPlanLatestVersion is the migration the session guard's plan test
// migrates to: the latest whose columns the statements it measures name,
// after the one that builds turns_session_dispatched_idx
// (sessionDispatchedMigration).
const guardPlanLatestVersion = reviewCheckoutMigration

// guardReadFixedBuffers and guardReadBuffersPerTurn bound what the session
// guard's read (GetSessionGuardFacts) reads: a fixed number of key and claim
// index probes -- the session's row, its automation's first run and the
// automation, its pull-request claims, the repository caps -- and, for the
// spend, a range of turns_session_dispatched_idx over the session's own
// dispatched turns and, for the turn count, a range of (session_id,
// dispatched_message_id) over all its turns, with each one's heap page
// while the visibility map is unset, as on a table between two vacuums. Never a term in another
// session's turns: a scan of the table reads every page of it.
const (
	guardReadFixedBuffers   = 60
	guardReadBuffersPerTurn = 2
)

func guardReadMaxBuffers(ownTurns int) float64 {
	return float64(guardReadFixedBuffers + guardReadBuffersPerTurn*ownTurns)
}

// sessionDispatchedIndexEntryMaxBuffers is what adding one entry to
// turns_session_dispatched_idx can read, for an index levels levels above
// its leaves (bt_metap's level of its root): its metapage, when the
// relation's cached copy of it is gone -- turns' description rebuilt after
// a DROP INDEX rolled back drops it -- and one page of each level from the
// root down to the leaf the entry goes to. Nothing more, once the measure
// is settled (measureGuardWrite): no leaf fills while it is measured, so
// none splits, and no dead entry is left to delete on the way.
func sessionDispatchedIndexEntryMaxBuffers(levels int) float64 {
	return float64(1 + levels + 1)
}

// guardPlanShape is a table TestSessionGuardFacts_ReadsTheSessionsOwnTurns
// reads: others sessions of perOther dispatched, completed, costed turns
// each, every pendingEvery-th with one pending turn more, beside a long
// session of longDispatched such turns, plus one processing when
// longProcessing, and longUndispatched turns never dispatched -- half
// pending, half ended context_moved -- stored after the tables were
// analyzed when longAfterAnalyze. Every shape also holds the claims the
// read reaches a session's repositories through, at the size a deployment
// has them: a pull-request claim for every session and 5,000 claim-only
// sessions more, a sentinel auto-fix child's claim for every tenth, 2,000
// repositories' settings; and automationRuns runs (5,000 when 0) of
// automations automations (50 when 0), each naming a session, the long
// one's its own automation's with a cap.
type guardPlanShape struct {
	name                             string
	others, perOther, pendingEvery   int
	longDispatched, longUndispatched int
	longProcessing, longAfterAnalyze bool
	automations, automationRuns      int
}

// guardPlanProbe is one session the statements are measured for.
type guardPlanProbe struct {
	name           string
	sessionID      pgtype.UUID
	repos          []string
	turns          int
	dispatched     int
	pendingTurn    pgtype.UUID
	processingTurn pgtype.UUID
	automation     bool
}

// storeGuardPlanShape stores shape, analyzes the tables, and returns the
// probes: the long session, a small one with a pending turn, a small one
// with none, and a session with no turn at all.
func storeGuardPlanShape(ctx context.Context, t *testing.T, pool *pgxpool.Pool, shape guardPlanShape) []guardPlanProbe {
	t.Helper()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	// Autovacuum stays off the measured tables, so every run reads its
	// plans in one state -- the one a busy table is in between two vacuums,
	// its visibility map unset.
	for _, table := range []string{"sessions", "turns", "automation_runs", "repo_settings", "github_pr_sessions", "sentinel_fixes"} {
		exec("autovacuum off "+table, `ALTER TABLE `+table+` SET (autovacuum_enabled = false)`)
	}
	var others []string
	if err := pool.QueryRow(ctx, `
		WITH s AS (
			INSERT INTO sessions (spawn_source, repos, spawn_depth)
			SELECT 'github', jsonb_build_array(jsonb_build_object('url', 'https://github.com/acme/plan-' || (g % 2000) || '.git')), 0
			FROM generate_series(1, $1::int) g
			RETURNING id
		)
		SELECT array_agg(id::text) FROM s`, shape.others).Scan(&others); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	// A dispatched turn carries what its dispatch stamped -- the prompt's
	// message id, the gen and the event watermark -- as every production
	// one does: (session_id, dispatched_message_id) (000131) is then as
	// wide as it is in production, which its plans' costs depend on.
	exec("the other sessions' dispatched turns", `
		INSERT INTO turns (session_id, status, created_at, dispatched_at, completed_at, cost_usd,
		                   dispatched_message_id, dispatched_sandbox_gen, dispatched_event_id)
		SELECT ($1::uuid[])[1 + g % cardinality($1::uuid[])], 'completed',
		       now() - make_interval(secs => $2::int - g), now() - make_interval(secs => $2::int - g),
		       now() - make_interval(secs => $2::int - g), 0.012345,
		       gen_random_uuid()::text, 1, g
		FROM generate_series(1, $2::int) g`, others, shape.others*shape.perOther)
	exec("the other sessions' pending turns", `
		INSERT INTO turns (session_id, status)
		SELECT o.id, 'pending' FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)
		WHERE o.n % $2::int = 0`, others, shape.pendingEvery)
	var claimOnly []string
	if err := pool.QueryRow(ctx, `
		WITH s AS (
			INSERT INTO sessions (spawn_source, repos, spawn_depth)
			SELECT 'github', '[]'::jsonb, 0 FROM generate_series(1, 5000)
			RETURNING id
		)
		SELECT array_agg(id::text) FROM s`).Scan(&claimOnly); err != nil {
		t.Fatalf("create claim-only sessions: %v", err)
	}
	claimed := append(append([]string{}, others...), claimOnly...)
	exec("pull-request claims", `
		INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id)
		SELECT 'acme/plan-' || (o.n % 2000), o.n, o.id FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n)`, claimed)
	exec("sentinel auto-fix children's claims", `
		INSERT INTO sentinel_fixes (repo_full_name, origin_pr_number, origin_review_session_id, origin_head_branch, fix_child_session_id)
		SELECT 'acme/plan-' || (o.n % 2000), o.n, ($1::uuid[])[1], 'feature', o.id
		FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n) WHERE o.n % 10 = 0`, claimed)
	exec("repository caps", `
		INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd)
		SELECT 'acme/plan-' || g, CASE WHEN g % 3 = 0 THEN NULL ELSE (10 + g % 40)::numeric END
		FROM generate_series(0, 1999) g`)

	automations, automationRuns := shape.automations, shape.automationRuns
	if automations == 0 {
		automations, automationRuns = 50, 5_000
	}
	var automationIDs []string
	{
		if err := pool.QueryRow(ctx, `
			WITH a AS (
				INSERT INTO automations (name, repos, session_spend_cap_usd)
				SELECT 'automation ' || g, '[]'::jsonb, CASE WHEN g % 2 = 0 THEN 25 END FROM generate_series(1, $1::int) g
				RETURNING id
			)
			SELECT array_agg(id::text) FROM a`, automations).Scan(&automationIDs); err != nil {
			t.Fatalf("create automations: %v", err)
		}
		exec("automation runs", `
			WITH inv AS (
				INSERT INTO automation_invocations (automation_id, targets, total_runs)
				SELECT a, '[]'::jsonb, 1 FROM unnest($1::uuid[]) a
				RETURNING id, automation_id
			), numbered AS (
				SELECT id, automation_id, row_number() OVER () AS n FROM inv
			)
			INSERT INTO automation_runs (invocation_id, automation_id, target, session_id)
			SELECT i.id, i.automation_id, jsonb_build_object('n', g), ($2::uuid[])[1 + g % cardinality($2::uuid[])]
			FROM generate_series(1, $3::int) g
			JOIN numbered i ON i.n = 1 + g % cardinality($1::uuid[])`, automationIDs, claimed, automationRuns)
	}

	var long pgtype.UUID
	var longProcessing pgtype.UUID
	const longRepo = "acme/plan-7"
	storeLong := func() {
		if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source, repos, spawn_depth)
			VALUES ('github', jsonb_build_array(jsonb_build_object('url', 'https://github.com/`+longRepo+`.git')), 0) RETURNING id`).Scan(&long); err != nil {
			t.Fatalf("create the long session: %v", err)
		}
		exec("the long session's dispatched turns", `
			INSERT INTO turns (session_id, status, created_at, dispatched_at, completed_at, cost_usd,
			                   dispatched_message_id, dispatched_sandbox_gen, dispatched_event_id)
			SELECT $1, 'completed', now() - make_interval(secs => $2::int - g), now() - make_interval(secs => $2::int - g),
			       now() - make_interval(secs => $2::int - g), 0.010000,
			       gen_random_uuid()::text, 1, g
			FROM generate_series(1, $2::int) g`, long, shape.longDispatched)
		exec("the long session's undispatched turns", `
			INSERT INTO turns (session_id, status, completed_at, end_reason)
			SELECT $1, CASE WHEN g % 2 = 0 THEN 'pending' ELSE 'failed' END::turn_status,
			       CASE WHEN g % 2 = 0 THEN NULL ELSE now() END,
			       CASE WHEN g % 2 = 0 THEN NULL ELSE 'context_moved' END
			FROM generate_series(1, $2::int) g`, long, shape.longUndispatched)
		if shape.longProcessing {
			if err := pool.QueryRow(ctx, `INSERT INTO turns (session_id, status, dispatched_at, cost_usd, dispatched_message_id, dispatched_sandbox_gen, dispatched_event_id)
				VALUES ($1, 'processing', now(), 0.5, gen_random_uuid()::text, 1, 1) RETURNING id`, long).Scan(&longProcessing); err != nil {
				t.Fatalf("store the long session's processing turn: %v", err)
			}
		}
		exec("the long session's claim", `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ($1, 1000000, $2)`, longRepo, long)
		// automationIDs[1] is "automation 2", which sets a cap.
		exec("the long session's automation run", `
			WITH inv AS (INSERT INTO automation_invocations (automation_id, targets, total_runs) VALUES ($1, '[]'::jsonb, 1) RETURNING id)
			INSERT INTO automation_runs (invocation_id, automation_id, target, session_id)
			SELECT inv.id, $1, '{"long": true}'::jsonb, $2 FROM inv`, automationIDs[1], long)
	}
	if !shape.longAfterAnalyze {
		storeLong()
	}
	exec("analyze", `ANALYZE`)
	if shape.longAfterAnalyze {
		storeLong()
	}

	var none pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source, repos, spawn_depth) VALUES ('web', '[]'::jsonb, 0) RETURNING id`).Scan(&none); err != nil {
		t.Fatalf("create the session with no turn: %v", err)
	}

	scan := func(s string) pgtype.UUID {
		t.Helper()
		var id pgtype.UUID
		if err := id.Scan(s); err != nil {
			t.Fatal(err)
		}
		return id
	}
	open := scan(others[shape.pendingEvery-1])
	var openPending pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1 AND status = 'pending'`, open).Scan(&openPending); err != nil {
		t.Fatalf("read the small session's pending turn: %v", err)
	}
	return []guardPlanProbe{
		{name: "the long session", sessionID: long, repos: []string{longRepo},
			turns: shape.longDispatched + shape.longUndispatched + boolInt(shape.longProcessing), dispatched: shape.longDispatched + boolInt(shape.longProcessing),
			processingTurn: longProcessing, automation: true},
		{name: "a small session with a pending turn", sessionID: open, repos: []string{"acme/plan-" + fmt.Sprint(shape.pendingEvery%2000)},
			turns: shape.perOther + 1, dispatched: shape.perOther, pendingTurn: openPending},
		{name: "a small session with none", sessionID: scan(others[0]), repos: []string{"acme/plan-1"},
			turns: shape.perOther, dispatched: shape.perOther},
		{name: "a session with no turn", sessionID: none},
	}
}

// guardPlanStatement is one statement the test measures, as
// holdPlanStatement is: run makes the store call, args is what EXPLAIN
// EXECUTE passes it, and applies tells whether the probe has what the
// statement needs (a pending or a processing turn).
type guardPlanStatement struct {
	name    string
	run     func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error
	args    func(p guardPlanProbe) string
	applies func(p guardPlanProbe) bool
}

// measureGuardPlan measures statement for probe as measureHoldPlan does --
// eight store calls, rolled back, so pgx's statement cache crosses
// Postgres's five custom plans, then EXPLAIN (ANALYZE, BUFFERS) EXECUTE
// under mode, also rolled back, on the pool's one connection -- except that
// each store call runs in a transaction of its own, as every production
// write of a turn does. Eight updates of one row in one transaction leave
// an aborted chain of eight versions, whose index entries the measured
// write's insertion then deletes bottom-up, reading their heap pages: a
// cost of the measure, not of the write. setup, when not "", runs first in
// each transaction -- "DROP INDEX turns_session_dispatched_idx", which the
// rollback undoes, measures a write as it runs without the index, on the
// very table it then runs on with it.
func measureGuardPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement guardPlanStatement, probe guardPlanProbe, mode, setup string) holdPlanMeasurement {
	t.Helper()
	inTx := func(fn func(tx pgx.Tx)) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if setup != "" {
			if _, err := tx.Exec(ctx, setup); err != nil {
				t.Fatalf("%s: %v", setup, err)
			}
		}
		fn(tx)
	}
	for i := 0; i < 8; i++ {
		inTx(func(tx pgx.Tx) {
			if err := statement.run(ctx, tx, probe); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s for %s: %v", statement.name, probe.name, err)
			}
		})
	}
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

// measureGuardWrite measures statement, a write of a turn's row, for probe
// in mode as measureGuardPlan does, once without turns_session_dispatched_idx
// (dropped inside each measuring transaction, which the rollback restores)
// and once with it, on tables settled for both, and returns how many levels
// the index has above its leaves.
//
// Each measure writes nine row versions of one turn (eight warm-up calls and
// the measured one, each rolled back), and every index of turns takes an
// entry for each. Two things in the index pages the writes reach then decide
// what the measured write reads, rather than the write itself: a leaf that
// fills splits, reading and writing pages a write that fits never touches;
// and dead entries a plain VACUUM leaves behind -- it skips index cleanup
// when few heap pages hold dead rows -- are deleted from a full leaf by the
// write that needs the room, reading their heap pages. Which measure meets
// either turns on where the session's entries fall in the leaves, which
// the tables' random ids decide, so an unsettled pair of measures can
// differ by a split on one side alone. So both measures run once
// unmeasured first: any leaf the nine entries do not fit splits then. And
// each measure runs on a table vacuumed with INDEX_CLEANUP ON, which removes
// every dead entry: each leaf the measure reaches then has room for at
// least the nine entries it takes, since it took them, or more, in the
// unmeasured run.
//
// Dropping the index, even in a transaction rolled back, invalidates turns'
// cached description, and the measured write rebuilds it, reading the
// catalog. So the measure with the index drops another index of turns in
// its transactions: turns_guard_measure_twin_idx, built here to hold no
// entry (its predicate is never true) and so cost no write anything. Both
// measures then rebuild the same description, each with one index fewer.
// What the index adds to the measured write is then its own descent alone
// (sessionDispatchedIndexEntryMaxBuffers).
func measureGuardWrite(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement guardPlanStatement, probe guardPlanProbe, mode string) (without, with holdPlanMeasurement, levels int) {
	t.Helper()
	const (
		dropIndex = `DROP INDEX turns_session_dispatched_idx`
		dropTwin  = `DROP INDEX turns_guard_measure_twin_idx`
	)
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS turns_guard_measure_twin_idx ON turns (id) WHERE id IS NULL`); err != nil {
		t.Fatalf("build the measure's twin index: %v", err)
	}
	vacuum := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `VACUUM (INDEX_CLEANUP ON) turns`); err != nil {
			t.Fatalf("vacuum: %v", err)
		}
	}
	measureGuardPlan(ctx, t, pool, statement, probe, mode, dropIndex)
	measureGuardPlan(ctx, t, pool, statement, probe, mode, dropTwin)
	vacuum()
	without = measureGuardPlan(ctx, t, pool, statement, probe, mode, dropIndex)
	vacuum()
	with = measureGuardPlan(ctx, t, pool, statement, probe, mode, dropTwin)
	if err := pool.QueryRow(ctx, `SELECT level FROM bt_metap('turns_session_dispatched_idx')`).Scan(&levels); err != nil {
		t.Fatalf("read the index's levels: %v", err)
	}
	return without, with, levels
}

// TestSessionGuardFacts_ReadsTheSessionsOwnTurns measures, by
// buffers read and by the scans the plan runs, the session guard's read of
// a session's spend, turn count and caps (GetSessionGuardFacts, technical
// plan §40.1)
// -- made before every turn it admits and before every queued turn's
// dispatch -- under a custom plan and under the generic plan pgx's
// statement cache lets Postgres settle on from a statement's sixth run, as
// the event reads' own plan tests measure theirs, on a matrix of tables:
// many small sessions beside a long one of 4,000 dispatched turns with one
// processing;
// fewer, longer sessions beside one of 20,000, a quarter of the table, the
// shape that baits a sequential scan; each of those with the long session
// stored after the tables were analyzed, or with as many of its turns never
// dispatched as dispatched; and automation runs by the tens of thousands,
// which the read reaches a session's automation through. In every cell, the
// read's only scans of turns are of turns_session_dispatched_idx (an index
// scan, or a bitmap heap scan whose bitmap comes from it alone), every
// other relation is read through its key or claim index, and the buffers
// read are at most a fixed number plus a few for each of the session's own
// turns -- never a term in the table.
//
// It also measures, before and after migration 000166 builds that index,
// the statements of turns it enters the plan space of. The reads of a
// session's turns (ListTurnsForSession, GetTurnByDispatchedMessageID,
// ReviewRetriggerHeld, GetSessionActivityFacts) read no more after; they
// are measured first, before any write leaves a dead row version behind.
// The writes of a turn's row (CreateTurn, RecordTurnStepCost, and
// UpdateTurnStatus as a dispatch and as an end) read at most
// sessionDispatchedIndexEntryMaxBuffers more, their new entry's descent of
// the index: each is measured without the index and with it, on tables
// settled for both (measureGuardWrite), so the two measures differ by the
// index alone. Every statement is measured serially (no parallel workers),
// so its buffers are its plan's own.
func TestSessionGuardFacts_ReadsTheSessionsOwnTurns(t *testing.T) {
	const budget = 10
	modes := []string{"force_custom_plan", "force_generic_plan"}

	guard := guardPlanStatement{
		name: "GetSessionGuardFacts",
		run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
			facts, err := narvipg.NewSessionGuardStore(nil).WithTx(tx).Facts(ctx, p.sessionID, p.repos)
			if err == nil && p.automation && facts.AutomationSource.ID == "" {
				return fmt.Errorf("an automation session's facts name no automation")
			}
			return err
		},
		args: func(p guardPlanProbe) string {
			quoted := make([]string, 0, len(p.repos))
			for _, r := range p.repos {
				quoted = append(quoted, "'"+r+"'")
			}
			return fmt.Sprintf("ARRAY[%s]::text[], '%s'::uuid", strings.Join(quoted, ", "), p.sessionID.String())
		},
		applies: func(guardPlanProbe) bool { return true },
	}
	always := func(guardPlanProbe) bool { return true }
	unchanged := []guardPlanStatement{
		{
			name: "ListTurnsForSession", applies: always,
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).ListForSession(ctx, p.sessionID)
				return err
			},
			args: func(p guardPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) },
		},
		{
			name: "GetTurnByDispatchedMessageID", applies: always,
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).GetByDispatchedMessageID(ctx, p.sessionID, "message-of-no-turn")
				return err
			},
			args: func(p guardPlanProbe) string {
				return fmt.Sprintf("'%s'::uuid, 'message-of-no-turn'", p.sessionID.String())
			},
		},
		{
			name: "ReviewRetriggerHeld", applies: always,
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).ReviewRetriggerHeld(ctx, p.sessionID)
				return err
			},
			args: func(p guardPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) },
		},
		{
			name: "GetSessionActivityFacts", applies: always,
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewSessionStore(nil).WithTx(tx).ActivityFacts(ctx, p.sessionID, budget)
				return err
			},
			args: func(p guardPlanProbe) string { return fmt.Sprintf("%d, '%s'::uuid", budget, p.sessionID.String()) },
		},
		{
			// A turn inserted pending: its row's first entry in the index.
			name: "CreateTurn", applies: always,
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{SessionID: p.sessionID, Status: sqlcgen.TurnStatusPending})
				return err
			},
			args: func(p guardPlanProbe) string {
				return fmt.Sprintf("'%s'::uuid, 'pending', NULL, NULL, false, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, false, NULL, NULL, NULL, NULL", p.sessionID.String())
			},
		},
		{
			name:    "RecordTurnStepCost",
			applies: func(p guardPlanProbe) bool { return p.processingTurn.Valid },
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).RecordStepCostUSD(ctx, p.sessionID, freshStepID(), 0.01)
				return err
			},
			args: func(p guardPlanProbe) string {
				return fmt.Sprintf("'%s'::uuid, '%s', 0.01", p.sessionID.String(), freshStepID())
			},
		},
		{
			// A pending turn dispatched: dispatched_at stamped, so its new
			// row version enters the index.
			name:    "UpdateTurnStatus",
			applies: func(p guardPlanProbe) bool { return p.pendingTurn.Valid },
			run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
					ID: p.pendingTurn, Status: sqlcgen.TurnStatusDispatched,
					DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
				})
				return err
			},
			args: func(p guardPlanProbe) string {
				return fmt.Sprintf("'%s'::uuid, 'dispatched', now(), NULL, NULL, NULL, NULL, NULL", p.pendingTurn.String())
			},
		},
	}
	// A processing turn ended -- measured on its own, by name, so its
	// before and after are its own.
	end := guardPlanStatement{
		name:    "UpdateTurnStatus",
		applies: func(p guardPlanProbe) bool { return p.processingTurn.Valid },
		run: func(ctx context.Context, tx pgx.Tx, p guardPlanProbe) error {
			_, err := narvipg.NewTurnStore(nil).WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
				ID: p.processingTurn, Status: sqlcgen.TurnStatusCompleted,
				CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			})
			return err
		},
		args: func(p guardPlanProbe) string {
			return fmt.Sprintf("'%s'::uuid, 'completed', NULL, now(), NULL, NULL, NULL, NULL", p.processingTurn.String())
		},
	}
	reads := unchanged[:4]
	writes := []guardPlanStatement{unchanged[4], unchanged[5], unchanged[6], end}
	writeLabels := []string{"CreateTurn", "RecordTurnStepCost", "UpdateTurnStatus (dispatch)", "UpdateTurnStatus (end)"}

	for _, shape := range []guardPlanShape{
		{name: "5,000 sessions of 6 dispatched turns, every 20th with one pending, beside one of 4,000 and one processing", others: 5_000, perOther: 6, pendingEvery: 20, longDispatched: 4_000, longProcessing: true},
		{name: "300 sessions of 200 dispatched turns beside one of 20,000, a quarter of the table", others: 300, perOther: 200, pendingEvery: 20, longDispatched: 20_000},
		{name: "the first, the long session stored after ANALYZE", others: 5_000, perOther: 6, pendingEvery: 20, longDispatched: 4_000, longProcessing: true, longAfterAnalyze: true},
		{name: "the second, half the long session's turns never dispatched", others: 300, perOther: 200, pendingEvery: 20, longDispatched: 10_000, longUndispatched: 10_000},
		{name: "the first, with 50,000 automation runs over 200 automations", others: 5_000, perOther: 6, pendingEvery: 20, longDispatched: 4_000, longProcessing: true, automations: 200, automationRuns: 50_000},
		{name: "the first, with 50,000 automation runs over 2,000 automations", others: 5_000, perOther: 6, pendingEvery: 20, longDispatched: 4_000, longProcessing: true, automations: 2_000, automationRuns: 50_000},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := holdPlanDatabase(ctx, t, guardPlanLatestVersion)
			if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pageinspect`); err != nil {
				t.Fatalf("create pageinspect, which reads the index's levels: %v", err)
			}
			// Every statement runs serially. A parallel plan's workers each
			// build turns' description from the catalog as they start, and
			// their reads count in the leader's Gather -- a few buffers that
			// vary from run to run with what the backends have cached, and
			// change with the index's own catalog rows, whatever the plan
			// reads of turns. Serial, a statement's buffers are its plan's.
			var database string
			if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{
				`ALTER DATABASE ` + pgx.Identifier{database}.Sanitize() + ` SET max_parallel_workers_per_gather = 0`,
				`SET max_parallel_workers_per_gather = 0`,
			} {
				if _, err := pool.Exec(ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			if _, err := pool.Exec(ctx, `DROP INDEX turns_session_dispatched_idx`); err != nil {
				t.Fatalf("take away 000166's index: %v", err)
			}
			probes := storeGuardPlanShape(ctx, t, pool, shape)

			// The reads, without the index and then with it.
			before := map[string]holdPlanMeasurement{}
			for _, statement := range reads {
				for _, probe := range probes {
					for _, mode := range modes {
						before[statement.name+", "+probe.name+", "+mode] = measureGuardPlan(ctx, t, pool, statement, probe, mode, "")
					}
				}
			}
			up, err := fs.ReadFile(migrations.FS, "000166_turns_session_dispatched_idx.up.sql")
			if err != nil {
				t.Fatalf("read 000166: %v", err)
			}
			if _, err := pool.Exec(ctx, string(up)); err != nil {
				t.Fatalf("build 000166's index again: %v", err)
			}
			for _, statement := range reads {
				for _, probe := range probes {
					for _, mode := range modes {
						key := statement.name + ", " + probe.name + ", " + mode
						after := measureGuardPlan(ctx, t, pool, statement, probe, mode, "")
						t.Logf("%s: before 000166 %v; after %v", key, before[key], after)
						if after.buffers > before[key].buffers {
							t.Errorf("%s: read %.0f buffers after 000166, %.0f before: the index made it worse", key, after.buffers, before[key].buffers)
						}
					}
				}
			}

			// The guard's read, with the index.
			for _, probe := range probes {
				for _, mode := range modes {
					got := measureGuardPlan(ctx, t, pool, guard, probe, mode, "")
					t.Logf("GetSessionGuardFacts, %s (%d dispatched turns of %d), %s: %v", probe.name, probe.dispatched, probe.turns, mode, got)
					if problem := guardReadProblem(got, probe.turns); problem != "" {
						t.Errorf("GetSessionGuardFacts, %s, %s: %s", probe.name, mode, problem)
					}
				}
			}

			// The writes, each without the index and with it, on tables
			// settled for both.
			for i, statement := range writes {
				for _, probe := range probes {
					if !statement.applies(probe) {
						continue
					}
					for _, mode := range modes {
						key := writeLabels[i] + ", " + probe.name + ", " + mode
						without, with, levels := measureGuardWrite(ctx, t, pool, statement, probe, mode)
						limit := sessionDispatchedIndexEntryMaxBuffers(levels)
						t.Logf("%s: without 000166's index %v; with it (%d levels above its leaves) %v", key, without, levels, with)
						if with.buffers > without.buffers+limit {
							t.Errorf("%s: read %.0f buffers with 000166's index, %.0f without: more than its new entry's descent, %.0f", key, with.buffers, without.buffers, limit)
						}
					}
				}
			}
		})
	}
}

// guardReadIndexes is the one index the guard's read may scan each
// relation through: turns, for the spend, through the session's own range
// of turns_session_dispatched_idx (and, for the turn count, through
// guardReadCountIndex), every other relation through its key or the
// session's claim on it.
var guardReadIndexes = map[string]string{
	"turns":              "turns_session_dispatched_idx",
	"sessions":           "sessions_pkey",
	"automation_runs":    "automation_runs_session_id_idx",
	"automations":        "automations_pkey",
	"repo_settings":      "repo_settings_pkey",
	"github_pr_sessions": "github_pr_sessions_session_id_idx",
	"sentinel_fixes":     "sentinel_fixes_fix_child_session_id_idx",
}

// guardReadCountIndex is the index the guard's read counts the session's
// turns through: (session_id, dispatched_message_id), 000131's, the one
// index of turns that holds every turn of a session.
const guardReadCountIndex = "turns_session_id_dispatched_message_id_idx"

// guardSmallTableSeqScanMaxBuffers bounds the one sequential scan the
// guard's read may run: of automations, the deployment's configured
// automations -- a table the size of its configuration, never of its
// traffic -- which the planner reads whole rather than through its key
// while it holds a few pages (at 50 and 200 automations, measured), and
// through its key once it holds more (at 2,000).
const guardSmallTableSeqScanMaxBuffers = 4

// guardReadProblem returns why the guard's read is not a range of the
// session's own turns and key probes elsewhere, or "": every
// scan of a relation is an index scan of its one index in
// guardReadIndexes, or a bitmap heap scan whose bitmap comes from that
// index alone -- never a sequential scan, nor a scan through another index,
// automations' few pages aside (guardSmallTableSeqScanMaxBuffers) -- and
// the buffers read are within guardReadMaxBuffers of the session's own
// turns.
func guardReadProblem(m holdPlanMeasurement, ownTurns int) string {
	if limit := guardReadMaxBuffers(ownTurns); m.buffers > limit {
		return fmt.Sprintf("reads %.0f buffers, over %.0f for %d turns of its own (%v)", m.buffers, limit, ownTurns, m.scans)
	}
	allowedBitmaps := map[string]bool{guardReadCountIndex: true}
	for _, index := range guardReadIndexes {
		allowedBitmaps[index] = true
	}
	sawSpend, sawCount := false, false
	saw := func(index string) {
		sawSpend = sawSpend || index == guardReadIndexes["turns"]
		sawCount = sawCount || index == guardReadCountIndex
	}
	for _, s := range m.scans {
		switch {
		case s.Node == "Bitmap Index Scan":
			if !allowedBitmaps[s.Index] {
				return fmt.Sprintf("reads a bitmap through an index it has no use for: %s", s)
			}
			saw(s.Index)
		case s.Node == "Bitmap Heap Scan":
			// Its Bitmap Index Scans are checked above.
		case s.Relation == "":
		case s.Relation == "automations" && s.Node == "Seq Scan" && s.Buffers <= guardSmallTableSeqScanMaxBuffers:
		default:
			want, ok := guardReadIndexes[s.Relation]
			if !ok {
				return fmt.Sprintf("scans a relation it has no use for: %s", s)
			}
			index := (s.Node == "Index Scan" || s.Node == "Index Only Scan") && (s.Index == want || (s.Relation == "turns" && s.Index == guardReadCountIndex))
			if !index {
				return fmt.Sprintf("scans %s other than through %s: %s", s.Relation, want, s)
			}
			saw(s.Index)
		}
	}
	if !sawSpend || !sawCount {
		return fmt.Sprintf("never reads turns through %s, for the spend, and %s, for the count (%v)", guardReadIndexes["turns"], guardReadCountIndex, m.scans)
	}
	return ""
}
