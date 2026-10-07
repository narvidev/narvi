//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

const (
	// heldFactsMaxBuffers bounds what the session status's read of the
	// session's holds (GetSessionActivityFacts' EXISTS) reads of
	// workflow_advance_holds: one descent of workflow_advance_holds_session_idx
	// -- a root and a leaf on the tables below -- and, for a session that
	// holds an advance, the heap page of its row. A scan of the table reads
	// every page of it: well over a hundred on the larger table.
	heldFactsMaxBuffers = 4
	// heldPageMaxBuffers bounds what one page of the releaser
	// (ListWorkflowAdvanceHolds, fifty rows) reads of workflow_advance_holds:
	// one range of workflow_advance_holds_held_at_idx, a descent and a leaf
	// or two, and the heap pages its fifty rows lie on -- two or three where
	// the rows were stored in order.
	heldPageMaxBuffers = 10
)

// TestWorkflowAdvanceHolds_PlansReadOneSessionOrOnePage measures, by
// buffers read, the two statements that read workflow_advance_holds by more
// than its primary key, under a custom plan and under the generic plan
// pgx's statement cache lets Postgres settle on from a statement's sixth
// run, on a table of 2,000 holds and on one of 20,000 -- each many pages, so
// a scan of the table would read tens or hundreds of them (on a table of a
// page or two the planner rightly reads it whole):
//
//   - the session status's facts (GetSessionActivityFacts), whose EXISTS
//     reads whether the session holds an advance (technical plan §43.20):
//     for a session that holds one and for one that does not, it must read
//     at most heldFactsMaxBuffers of the table, through
//     workflow_advance_holds_session_idx, and no more on the larger table
//     than on the smaller but for one level of the index -- never a scan of
//     the table;
//   - one page of the releaser (ListWorkflowAdvanceHolds), the first and
//     one from the middle of the table: at most heldPageMaxBuffers, through
//     workflow_advance_holds_held_at_idx, its row comparison an index
//     condition, so a page in the middle of a long freeze's backlog reads
//     what the first does.
func TestWorkflowAdvanceHolds_PlansReadOneSessionOrOnePage(t *testing.T) {
	const budget = 10
	modes := []string{"force_custom_plan", "force_generic_plan"}
	type measured struct {
		facts map[string]float64
		page  map[string]float64
	}
	results := map[int]measured{}

	for _, holds := range []int{2_000, 20_000} {
		t.Run(fmt.Sprintf("%d holds", holds), func(t *testing.T) {
			ctx := context.Background()
			pool, _ := holdPlanDatabase(ctx, t, workflowAdvanceHoldsMigration)
			var database string
			if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{
				`ALTER DATABASE ` + pgx.Identifier{database}.Sanitize() + ` SET max_parallel_workers_per_gather = 0`,
				`SET max_parallel_workers_per_gather = 0`,
				`ALTER TABLE workflow_advance_holds SET (autovacuum_enabled = false)`,
			} {
				if _, err := pool.Exec(ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}

			var defID, stepID pgtype.UUID
			if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'plan-holds', false, 1) RETURNING id`).Scan(&defID); err != nil {
				t.Fatalf("insert the definition: %v", err)
			}
			if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
				t.Fatalf("insert the step: %v", err)
			}
			// One held run per session, the oldest hold stored first.
			if _, err := pool.Exec(ctx, `
				WITH s AS (
					INSERT INTO sessions (spawn_source) SELECT 'web' FROM generate_series(1, $1::int) RETURNING id
				), r AS (
					INSERT INTO workflow_runs (session_id, lane, workflow_definition_id, definition_version)
					SELECT id, 'request', $2, 1 FROM s RETURNING id, session_id
				), sr AS (
					INSERT INTO workflow_step_runs (workflow_run_id, step_definition_id, status, outcome_status, finished_at)
					SELECT id, $3, 'completed', 'ok', now() FROM r RETURNING id, workflow_run_id
				), ordered AS (
					SELECT sr.workflow_run_id, sr.id AS step_run_id, r.session_id,
					       now() - make_interval(secs => $1::int - row_number() OVER (ORDER BY sr.workflow_run_id)) AS held_at
					FROM sr JOIN r ON r.id = sr.workflow_run_id
				)
				INSERT INTO workflow_advance_holds (workflow_run_id, step_run_id, session_id, held_at)
				SELECT workflow_run_id, step_run_id, session_id, held_at FROM ordered ORDER BY held_at`, holds, defID, stepID); err != nil {
				t.Fatalf("store %d holds: %v", holds, err)
			}
			var held, notHeld pgtype.UUID
			if err := pool.QueryRow(ctx, `SELECT session_id FROM workflow_advance_holds ORDER BY held_at OFFSET $1 LIMIT 1`, holds/2).Scan(&held); err != nil {
				t.Fatalf("a held session: %v", err)
			}
			if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id`).Scan(&notHeld); err != nil {
				t.Fatalf("a session with no hold: %v", err)
			}
			var middle narvipg.AdvanceHoldCursor
			if err := pool.QueryRow(ctx, `SELECT held_at, workflow_run_id FROM workflow_advance_holds ORDER BY held_at, workflow_run_id OFFSET $1 LIMIT 1`, holds/2).Scan(&middle.HeldAt, &middle.RunID); err != nil {
				t.Fatalf("the middle cursor: %v", err)
			}
			if _, err := pool.Exec(ctx, `ANALYZE`); err != nil {
				t.Fatalf("analyze: %v", err)
			}

			facts := func(sessionID pgtype.UUID) holdPlanStatement {
				return holdPlanStatement{
					name: "GetSessionActivityFacts",
					run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
						_, err := narvipg.NewSessionStore(nil).WithTx(tx).ActivityFacts(ctx, sessionID, budget)
						return err
					},
					args: func(holdPlanProbe) string { return fmt.Sprintf("%d, '%s'::uuid", budget, sessionID.String()) },
				}
			}
			page := func(cursor narvipg.AdvanceHoldCursor, cursorSQL string) holdPlanStatement {
				return holdPlanStatement{
					name: "ListWorkflowAdvanceHolds",
					run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
						_, err := narvipg.NewWorkflowStore(nil).WithTx(tx).ListAdvanceHolds(ctx, cursor, 50)
						return err
					},
					args: func(holdPlanProbe) string { return cursorSQL + ", 50" },
				}
			}
			middleSQL := fmt.Sprintf("'%s'::timestamptz, '%s'::uuid", middle.HeldAt.Time.UTC().Format("2006-01-02 15:04:05.999999+00"), middle.RunID.String())

			m := measured{facts: map[string]float64{}, page: map[string]float64{}}
			for _, mode := range modes {
				for _, probe := range []struct {
					name      string
					statement holdPlanStatement
					max       float64
					index     string
					into      map[string]float64
				}{
					{"the facts of a session holding an advance", facts(held), heldFactsMaxBuffers, "workflow_advance_holds_session_idx", m.facts},
					{"the facts of a session holding none", facts(notHeld), heldFactsMaxBuffers, "workflow_advance_holds_session_idx", m.facts},
					{"the releaser's first page", page(narvipg.FirstAdvanceHolds, "'-infinity'::timestamptz, '00000000-0000-0000-0000-000000000000'::uuid"), heldPageMaxBuffers, "workflow_advance_holds_held_at_idx", m.page},
					{"a page from the middle of the table", page(middle, middleSQL), heldPageMaxBuffers, "workflow_advance_holds_held_at_idx", m.page},
				} {
					key := probe.name + ", " + mode
					got := measureHoldPlan(ctx, t, pool, probe.statement, holdPlanProbe{name: probe.name}, mode)
					var buffers float64
					var scans []string
					for _, s := range got.scans {
						if s.Relation != "workflow_advance_holds" && !strings.HasPrefix(s.Index, "workflow_advance_holds") {
							continue
						}
						scans = append(scans, s.String())
						buffers += s.Buffers
						if s.Index != probe.index || !strings.HasPrefix(s.Node, "Index") {
							t.Errorf("%s: %s, want an index scan of %s", key, s, probe.index)
						}
					}
					t.Logf("%d holds, %s: %.0f buffers of workflow_advance_holds %v (%.0f in all)", holds, key, buffers, scans, got.buffers)
					if len(scans) == 0 {
						t.Errorf("%s: the plan never read workflow_advance_holds", key)
					}
					if buffers > probe.max {
						t.Errorf("%s: read %.0f buffers of workflow_advance_holds, want at most %.0f", key, buffers, probe.max)
					}
					probe.into[key] = buffers
				}
			}
			results[holds] = m
		})
	}

	// Neither read grows with the table.
	small, large := results[2_000], results[20_000]
	for _, pair := range []struct{ small, large map[string]float64 }{{small.facts, large.facts}, {small.page, large.page}} {
		for key, l := range pair.large {
			if s, ok := pair.small[key]; ok && l > s+1 {
				t.Errorf("%s: %.0f buffers of workflow_advance_holds on 20,000 holds, %.0f on 2,000: it grows with the table", key, l, s)
			}
		}
	}
}
