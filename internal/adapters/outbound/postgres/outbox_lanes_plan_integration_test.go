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
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/migrations"
)

// outboxLanesIndexVersion is the migration that builds
// outbox_pending_kind_due_idx: the plan test migrates to it.
const outboxLanesIndexVersion = 164

const (
	// outboxLanesIndex is the index every lane must read through.
	outboxLanesIndex = "outbox_pending_kind_due_idx"
	// outboxLaneMaxBuffers bounds what one lane of a frozen tick reads on
	// the tables below: the index's range of the lane's due rows and the
	// heap pages those 3,000 rows lie on, and the row locks of the 20 it
	// claims. A scan of the table reads every page of it: 2,600 and more.
	outboxLaneMaxBuffers = 500
	// outboxLaneGrowthBuffers is how much more a lane may read on the
	// table of 400,000 delivered rows than on the one of 50,000: the same
	// due rows, so nothing but a level of the index more.
	outboxLaneGrowthBuffers = 4
)

// The lanes a frozen tick claims (outboxworker's claimBatch): the kinds
// the autonomy freeze holds, and every other kind the port declares.
var (
	outboxHeldKinds       = []string{"github_description_autofix", "sentinel_auto_fix"}
	outboxDeliveringKinds = []string{
		"blob_delete", "github", "github_preview_link", "github_review_check", "github_verdict",
		"github_workflow_decision", "handoff_sentinel", "linear", "linear_digest", "linear_progress",
		"linear_workflow_decision", "release_manifest", "rwx_preview_dispatch", "slack", "slack_digest",
		"slack_plan_approval", "slack_plan_decided", "slack_workflow_decision",
	}
)

// outboxLaneShapeDelivered are the delivered rows of the two outboxes the
// test reads, beside the same 9,000 pending rows: 3,000 due of the held
// kinds, 3,000 due Slack messages and 3,000 not yet due, of both.
var outboxLaneShapeDelivered = []int{50_000, 400_000}

// storeOutboxLaneShape stores delivered delivered rows and the pending rows
// beside them, with autovacuum off the outbox so every run reads its plans
// in one state, and analyzes it.
func storeOutboxLaneShape(ctx context.Context, t *testing.T, pool *pgxpool.Pool, delivered int) {
	t.Helper()
	for _, stmt := range []struct {
		what string
		sql  string
		args []any
	}{
		{"autovacuum off", `ALTER TABLE outbox SET (autovacuum_enabled = false)`, nil},
		{"the delivered rows", `
			INSERT INTO outbox (kind, payload, status, attempts, next_attempt_at, delivered_at, created_at)
			SELECT (ARRAY['slack', 'github_verdict', 'sentinel_auto_fix', 'github_description_autofix', 'github_review_check'])[1 + g % 5],
			       jsonb_build_object('text', repeat('x', 200)), 'delivered', 1,
			       now() - interval '2 days', now() - interval '2 days', now() - interval '2 days'
			FROM generate_series(1, $1::int) g`, []any{delivered}},
		{"the due held rows", `
			INSERT INTO outbox (kind, payload, next_attempt_at, created_at)
			SELECT (ARRAY['sentinel_auto_fix', 'github_description_autofix'])[1 + g % 2],
			       jsonb_build_object('text', repeat('x', 200)), now() - make_interval(secs => g), now() - interval '1 hour'
			FROM generate_series(1, 3000) g`, nil},
		{"the due Slack rows", `
			INSERT INTO outbox (kind, payload, next_attempt_at, created_at)
			SELECT 'slack', jsonb_build_object('text', repeat('x', 200)), now() - make_interval(secs => g), now() - interval '30 minutes'
			FROM generate_series(1, 3000) g`, nil},
		{"the rows not yet due", `
			INSERT INTO outbox (kind, payload, next_attempt_at, created_at)
			SELECT (ARRAY['slack', 'sentinel_auto_fix', 'github_verdict'])[1 + g % 3],
			       jsonb_build_object('text', repeat('x', 200)), now() + interval '1 hour', now()
			FROM generate_series(1, 3000) g`, nil},
		{"analyze", `ANALYZE outbox`, nil},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("store %s: %v", stmt.what, err)
		}
	}
}

// outboxLaneStatement is one outbox statement the test measures: run makes
// the store call (eight times, so pgx's statement cache crosses Postgres's
// five custom plans), name is its sqlc name and args what EXPLAIN EXECUTE
// passes it.
type outboxLaneStatement struct {
	label, name, args string
	run               func(ctx context.Context, store *narvipg.OutboxStore) error
}

func kindsArg(kinds []string) string {
	return "'{" + strings.Join(kinds, ",") + "}'::text[]"
}

var outboxLaneStatements = []outboxLaneStatement{
	{
		label: "the held lane", name: "ListDuePendingOutboxEntriesOfKinds", args: kindsArg(outboxHeldKinds) + ", 20",
		run: func(ctx context.Context, store *narvipg.OutboxStore) error {
			_, err := store.ListDuePendingOfKinds(ctx, 20, outboxHeldKinds)
			return err
		},
	},
	{
		label: "the delivering lane", name: "ListDuePendingOutboxEntriesOfKinds", args: kindsArg(outboxDeliveringKinds) + ", 20",
		run: func(ctx context.Context, store *narvipg.OutboxStore) error {
			_, err := store.ListDuePendingOfKinds(ctx, 20, outboxDeliveringKinds)
			return err
		},
	},
}

// outboxOneLaneStatements are the outbox reads every tick ran before the
// lanes and still runs while nothing is held: the one-lane claim and the
// pending count. The index must not make either read more.
var outboxOneLaneStatements = []outboxLaneStatement{
	{
		label: "the one-lane claim", name: "ListDuePendingOutboxEntries", args: "20",
		run: func(ctx context.Context, store *narvipg.OutboxStore) error {
			_, err := store.ListDuePending(ctx, 20)
			return err
		},
	},
	{
		label: "the pending count", name: "CountPendingOutboxEntries", args: "",
		run: func(ctx context.Context, store *narvipg.OutboxStore) error {
			_, err := store.CountPending(ctx)
			return err
		},
	},
}

// roundTwoExcludingLane is the delivering lane as the binary before this
// index sent it, `kind <> ALL`, an exclusion no index can seek on: measured
// before and after, for the record, never asserted.
const roundTwoExcludingLane = `SELECT * FROM outbox
WHERE status = 'pending' AND next_attempt_at <= now()
  AND kind <> ALL($1::text[])
ORDER BY next_attempt_at
LIMIT $2
FOR UPDATE SKIP LOCKED`

// outboxLaneMeasurement is one statement's read under one plan mode.
type outboxLaneMeasurement struct {
	buffers float64
	scans   []pagePlanScan
}

func (m outboxLaneMeasurement) String() string {
	return fmt.Sprintf("%.0f buffers: %v", m.buffers, m.scans)
}

// measureOutboxStatement runs statement eight times in a transaction rolled
// back, then once under EXPLAIN (ANALYZE, BUFFERS) EXECUTE in mode in
// another, also rolled back, all on the pool's one connection.
func measureOutboxStatement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement outboxLaneStatement, mode string) outboxLaneMeasurement {
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
		store := narvipg.NewOutboxStore(pool, false).WithTx(tx)
		for i := 0; i < 8; i++ {
			if err := statement.run(ctx, store); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s: %v", statement.label, err)
			}
		}
	})
	var m outboxLaneMeasurement
	inTx(func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
			t.Fatalf("set plan_cache_mode: %v", err)
		}
		plan := explainOutboxPlan(ctx, t, tx, statement.name, statement.args)
		scans, buffers, err := planScans(plan, "")
		if err != nil {
			t.Fatalf("read %s's plan: %v\n%s", statement.label, err, plan)
		}
		m = outboxLaneMeasurement{buffers: buffers, scans: scans}
	})
	return m
}

// explainOutboxPlan is explainPlan for a statement that may take no
// argument: EXECUTE then has no parenthesized list at all.
func explainOutboxPlan(ctx context.Context, t *testing.T, q planQuerier, statement, args string) string {
	t.Helper()
	if args != "" {
		return explainPlan(ctx, t, q, statement, args)
	}
	var name string
	if err := q.QueryRow(ctx, `SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`, "-- name: "+statement+" %").Scan(&name); err != nil {
		t.Fatalf("find %s among the connection's prepared statements: %v", statement, err)
	}
	var plan string
	if err := q.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+pgx.Identifier{name}.Sanitize(), pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	return plan
}

// measureRoundTwoExcludingLane measures roundTwoExcludingLane, prepared by
// hand, under mode, after six runs so the generic plan is in play.
func measureRoundTwoExcludingLane(ctx context.Context, t *testing.T, pool *pgxpool.Pool, mode string) outboxLaneMeasurement {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
		t.Fatal(err)
	}
	// A prepared statement outlives the transaction's rollback: it is the
	// session's, so it is deallocated once explained.
	if _, err := tx.Exec(ctx, "PREPARE round_two_excluding_lane(text[], int) AS "+roundTwoExcludingLane); err != nil {
		t.Fatalf("prepare the round-two lane: %v", err)
	}
	defer func() { _, _ = tx.Exec(ctx, "DEALLOCATE round_two_excluding_lane") }()
	var plan string
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE round_two_excluding_lane("+kindsArg(outboxHeldKinds)+", 20)",
		pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
		t.Fatalf("explain the round-two lane: %v", err)
	}
	scans, buffers, err := planScans(plan, "")
	if err != nil {
		t.Fatalf("read the round-two lane's plan: %v", err)
	}
	return outboxLaneMeasurement{buffers: buffers, scans: scans}
}

// outboxLaneProblem returns why a lane's plan reads more than its own due
// rows, or "": outbox read only through outbox_pending_kind_due_idx -- an
// index scan of it, or a bitmap heap scan its bitmap index scan feeds -- no
// other index read, no sequential scan, no row removed by a filter, and at
// most outboxLaneMaxBuffers in all.
func outboxLaneProblem(m outboxLaneMeasurement) string {
	readThroughIndex := false
	for _, s := range m.scans {
		switch {
		case s.Index != "" && s.Index != outboxLanesIndex:
			return fmt.Sprintf("reads index %s, not %s (%s)", s.Index, outboxLanesIndex, s)
		case s.Index == outboxLanesIndex:
			readThroughIndex = true
		case s.Relation == "outbox" && s.Node != "Bitmap Heap Scan":
			return fmt.Sprintf("reads outbox other than through %s (%s)", outboxLanesIndex, s)
		}
		if s.RowsRemoved > 0 {
			return fmt.Sprintf("filters out %.0f rows (%s)", s.RowsRemoved, s)
		}
	}
	if !readThroughIndex {
		return fmt.Sprintf("never reads %s", outboxLanesIndex)
	}
	if m.buffers > outboxLaneMaxBuffers {
		return fmt.Sprintf("reads %.0f buffers, over %d", m.buffers, outboxLaneMaxBuffers)
	}
	return ""
}

// rebuildOutboxLanesIndex builds outbox_pending_kind_due_idx again by
// running its migration's own up file.
func rebuildOutboxLanesIndex(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	matches, err := fs.Glob(migrations.FS, fmt.Sprintf("%06d_*.up.sql", outboxLanesIndexVersion))
	if err != nil || len(matches) != 1 {
		t.Fatalf("find migration %d's up file: %v (%v)", outboxLanesIndexVersion, matches, err)
	}
	up, err := fs.ReadFile(migrations.FS, matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("run %s: %v", matches[0], err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE outbox`); err != nil {
		t.Fatal(err)
	}
}

// TestOutboxLanes_PlansReadOnlyTheirOwnDueRows measures, by buffers read,
// the claim of each lane a frozen outbox tick runs (technical plan §40.2),
// under a custom plan and under the generic plan pgx's statement cache lets
// Postgres settle on from a statement's sixth run, on an outbox of 50,000
// and of 400,000 delivered rows beside the same 9,000 pending ones.
//
// The index is taken away before the outbox is stored, for the measure
// before it, and built again by its migration's own up file for the
// measure after. Before, each lane scans the whole outbox, and so did the
// round-two delivering lane's exclusion (logged, with the one-lane claim
// and the pending count). After, each lane must read only its kinds' due
// rows through outbox_pending_kind_due_idx -- no other scan of outbox, no
// row removed by a filter, at most outboxLaneMaxBuffers -- and read no
// more on the larger outbox than on the smaller one but a level of index;
// the one-lane claim and the pending count must read no more than before.
func TestOutboxLanes_PlansReadOnlyTheirOwnDueRows(t *testing.T) {
	ctx := context.Background()
	modes := []string{"force_custom_plan", "force_generic_plan"}
	after := map[string]float64{}
	for _, delivered := range outboxLaneShapeDelivered {
		t.Run(fmt.Sprintf("%d delivered rows", delivered), func(t *testing.T) {
			pool, _ := holdPlanDatabase(ctx, t, outboxLanesIndexVersion)
			if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+outboxLanesIndex); err != nil {
				t.Fatalf("take the index away: %v", err)
			}
			storeOutboxLaneShape(ctx, t, pool, delivered)

			before := map[string]outboxLaneMeasurement{}
			for _, mode := range modes {
				for _, st := range append(append([]outboxLaneStatement{}, outboxLaneStatements...), outboxOneLaneStatements...) {
					m := measureOutboxStatement(ctx, t, pool, st, mode)
					before[st.label+"/"+mode] = m
					t.Logf("before the index, %s, %s: %s", st.label, mode, m)
				}
				t.Logf("before the index, the round-two exclusion lane, %s: %s", mode, measureRoundTwoExcludingLane(ctx, t, pool, mode))
			}

			started := time.Now()
			rebuildOutboxLanesIndex(ctx, t, pool)
			t.Logf("the index built, and the outbox analyzed, in %v on %d delivered rows", time.Since(started), delivered)
			for _, mode := range modes {
				for _, st := range outboxLaneStatements {
					m := measureOutboxStatement(ctx, t, pool, st, mode)
					t.Logf("with the index, %s, %s: %s", st.label, mode, m)
					if problem := outboxLaneProblem(m); problem != "" {
						t.Errorf("%s, %s: %s", st.label, mode, problem)
					}
					key := st.label + "/" + mode
					if smaller, ok := after[key]; ok && m.buffers > smaller+outboxLaneGrowthBuffers {
						t.Errorf("%s, %s: reads %.0f buffers on %d delivered rows, %.0f on %d: it grows with the table",
							st.label, mode, m.buffers, delivered, smaller, outboxLaneShapeDelivered[0])
					}
					after[key] = m.buffers
				}
				for _, st := range outboxOneLaneStatements {
					m := measureOutboxStatement(ctx, t, pool, st, mode)
					t.Logf("with the index, %s, %s: %s", st.label, mode, m)
					if b := before[st.label+"/"+mode]; m.buffers > b.buffers {
						t.Errorf("%s, %s: reads %.0f buffers with the index, %.0f without: the index made it read more", st.label, mode, m.buffers, b.buffers)
					}
				}
				t.Logf("with the index, the round-two exclusion lane, %s: %s", mode, measureRoundTwoExcludingLane(ctx, t, pool, mode))
			}
		})
	}
}
