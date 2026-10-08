//go:build integration

package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// checkoutPlanVersion is the migration the review checkout's plan test
// migrates to: the latest whose columns its statements name -- 000167 and
// 000168 add the checkout's, and ClearSandboxSnapshot clears the
// snapshot's provenance too, which snapshotProvenanceMigration adds.
const checkoutPlanVersion = snapshotProvenanceMigration

// The review checkout's statements, each the only access of its rows in
// its transaction, read by key alone (technical plan §21.1):
const (
	// checkoutReadMaxBuffers bounds GetTurnCheckoutState: a descent of
	// turns_pkey and the turn's heap page, then one of
	// events_session_id_message_id_idx for the reply's key and the
	// reply's heap page -- 7 measured, 3 with no request yet. Never a scan
	// of either table.
	checkoutReadMaxBuffers = 8
	// checkoutTurnWriteMaxBuffers bounds the turn's checkout writes
	// (RecordTurnCheckoutRequest, SetTurnCheckedOut,
	// SetTurnCheckoutRetiredGen), each the row's first write in its
	// transaction: a descent of turns_pkey and the row's heap page, and the
	// new version. No checkout column is indexed, so no update adds an
	// index entry: 6 measured on a page with room, 8 on a full one.
	checkoutTurnWriteMaxBuffers = 12
	// checkoutTimerWriteMaxBuffers bounds ArmSessionDispatchTimerAfter: a
	// probe of session_timers' (session_id, name) key, the row's heap page,
	// and a new version with its index entries -- fires_at is indexed, so
	// it is never heap-only: 17 measured on a full page, the shape of the
	// hold's re-arm, held to the same allowance (holdWakeMaxBuffers).
	checkoutTimerWriteMaxBuffers = holdWakeMaxBuffers
	// checkoutSandboxWriteMaxBuffers bounds ClearSandboxSnapshot: a probe
	// of sandboxes' session_id key, the row's heap page and its new
	// version with its index entries: 17 or 18 measured, the same shape of
	// write.
	checkoutSandboxWriteMaxBuffers = holdWakeMaxBuffers
)

// TestReviewCheckout_PlansReadByKey measures, by buffers read, every
// statement technical plan §21.1's review checkout adds, under a custom
// plan and under the generic plan pgx's statement cache lets Postgres
// settle on from a statement's sixth run, on tables of 3,000 review
// sessions with 15 turns each, every turn carrying a checkout and every
// one's reply stored among 90,000 events: each reads its rows by key, never
// scanning turns, events, session_timers or sandboxes.
func TestReviewCheckout_PlansReadByKey(t *testing.T) {
	ctx := context.Background()
	pool, _ := holdPlanDatabase(ctx, t, checkoutPlanVersion)
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	for _, table := range []string{"sessions", "turns", "events", "session_timers", "sandboxes"} {
		exec("autovacuum off "+table, `ALTER TABLE `+table+` SET (autovacuum_enabled = false)`)
	}
	exec("sessions", `
		INSERT INTO sessions (spawn_source, repos, spawn_depth)
		SELECT 'github', jsonb_build_array(jsonb_build_object('name', 'widgets', 'url', 'https://github.com/acme/plan-' || g || '.git', 'branch', NULL)), 0
		FROM generate_series(1, 3000) g`)
	exec("sandboxes", `
		INSERT INTO sandboxes (session_id, gen, status, snapshot_id, review_checkout_gen)
		SELECT id, 1, 'ready', 'snap-' || id, 1 FROM sessions`)
	exec("timers", `
		INSERT INTO session_timers (session_id, name, fires_at)
		SELECT s.id, k.name, now() + interval '1 hour'
		FROM sessions s CROSS JOIN (VALUES ('liveness_check'), ('inactivity'), ('dispatch')) AS k(name)`)
	// One pending turn stored first, so the turns stored after it fill its
	// heap page: a write of it finds no room there.
	var session pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM sessions ORDER BY id LIMIT 1 OFFSET 1500`).Scan(&session); err != nil {
		t.Fatal(err)
	}
	var crowded pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO turns (session_id, status, review_head_sha, is_review_attempt) VALUES ($1, 'pending', 'c0ffee0000000000000000000000000000000003', true) RETURNING id`, session).Scan(&crowded); err != nil {
		t.Fatal(err)
	}
	// Every turn was dispatched and checked out, as a review session's
	// are; its checkout's reply and its prompt's receipt are stored.
	exec("turns", `
		INSERT INTO turns (session_id, status, created_at, completed_at, review_head_sha, is_review_attempt,
		                   dispatched_message_id, dispatched_sandbox_gen, dispatched_event_id,
		                   checkout_message_id, checkout_gen, checkout_requested_at, checkout_sent_at,
		                   checkout_sent_ready_seq, checkout_sends, checked_out_sha)
		SELECT s.id, 'completed', now() - make_interval(secs => 100 - g), now() - make_interval(secs => 99 - g),
		       md5(s.id::text || g), true, gen_random_uuid()::text, 1, g,
		       gen_random_uuid()::text, 1, now() - interval '1 hour', now() - interval '1 hour', 1, 1, md5(s.id::text || g)
		FROM sessions s CROSS JOIN generate_series(1, 15) g`)
	exec("events", `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT t.session_id, 'checkout_result', 'checkout_result:' || t.checkout_message_id, '{"type":"checkout_result","repos":[]}'::jsonb
		FROM turns t WHERE t.checkout_message_id IS NOT NULL
		UNION ALL
		SELECT t.session_id, 'prompt_received', 'prompt_received:' || t.dispatched_message_id, '{"type":"prompt_received"}'::jsonb
		FROM turns t WHERE t.dispatched_message_id IS NOT NULL`)

	// The probes: a pending turn whose checkout's reply is stored, one with
	// no request yet, and the crowded one above.
	var answered, fresh pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO turns (session_id, status, review_head_sha, is_review_attempt, checkout_message_id, checkout_gen,
		                   checkout_requested_at, checkout_sent_at, checkout_sent_ready_seq, checkout_sends)
		VALUES ($1, 'pending', 'c0ffee0000000000000000000000000000000001', true, 'probe-checkout', 1, now(), now(), 1, 1)
		RETURNING id`, session).Scan(&answered); err != nil {
		t.Fatal(err)
	}
	exec("the probe's reply", `INSERT INTO events (session_id, type, message_id, payload) VALUES ($1, 'checkout_result', 'checkout_result:probe-checkout', '{"repos":[]}')`, session)
	if err := pool.QueryRow(ctx, `INSERT INTO turns (session_id, status, review_head_sha) VALUES ($1, 'pending', 'c0ffee0000000000000000000000000000000002') RETURNING id`, session).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	exec("analyze", `ANALYZE`)

	turns := narvipg.NewTurnStore(pool)
	timers := narvipg.NewTimerStore(pool)
	sandboxes := narvipg.NewSandboxStore(pool)
	quote := func(id pgtype.UUID) string { return "'" + id.String() + "'::uuid" }
	type statement struct {
		holdPlanStatement
		max       float64
		relations []string
	}
	var statements []statement
	for _, probe := range []struct {
		name string
		id   pgtype.UUID
	}{{"a turn whose reply is stored", answered}, {"a turn with no request yet", fresh}, {"a turn on a full heap page", crowded}} {
		id := probe.id
		statements = append(statements,
			statement{holdPlanStatement{
				name: "GetTurnCheckoutState",
				run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
					_, err := turns.WithTx(tx).CheckoutState(ctx, id)
					return err
				},
				args: func(holdPlanProbe) string { return quote(id) },
			}, checkoutReadMaxBuffers, []string{"turns", "events"}},
			statement{holdPlanStatement{
				name: "RecordTurnCheckoutRequest",
				run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
					_, err := turns.WithTx(tx).RecordCheckoutRequest(ctx, id, 1, "probe-next", 2, true)
					return err
				},
				args: func(holdPlanProbe) string { return "1, true, 'probe-next', 2, " + quote(id) },
			}, checkoutTurnWriteMaxBuffers, []string{"turns"}},
			statement{holdPlanStatement{
				name: "SetTurnCheckedOut",
				run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
					_, err := turns.WithTx(tx).SetCheckedOut(ctx, id, "c0ffee0000000000000000000000000000000001")
					return err
				},
				args: func(holdPlanProbe) string { return "'c0ffee0000000000000000000000000000000001', " + quote(id) },
			}, checkoutTurnWriteMaxBuffers, []string{"turns"}},
			statement{holdPlanStatement{
				name: "SetTurnCheckoutRetiredGen",
				run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
					_, err := turns.WithTx(tx).SetCheckoutRetiredGen(ctx, id, 1)
					return err
				},
				args: func(holdPlanProbe) string { return "1, " + quote(id) },
			}, checkoutTurnWriteMaxBuffers, []string{"turns"}},
		)
	}
	statements = append(statements,
		statement{holdPlanStatement{
			name: "ArmSessionDispatchTimerAfter",
			run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
				return timers.WithTx(tx).ArmDispatchAfter(ctx, session, 15*time.Minute)
			},
			args: func(holdPlanProbe) string { return quote(session) + ", 900" },
		}, checkoutTimerWriteMaxBuffers, []string{"session_timers"}},
		statement{holdPlanStatement{
			name: "ClearSandboxSnapshot",
			run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
				_, err := sandboxes.WithTx(tx).ClearSnapshot(ctx, session, "snap-"+session.String())
				return err
			},
			args: func(holdPlanProbe) string { return quote(session) + ", 'snap-" + session.String() + "'" },
		}, checkoutSandboxWriteMaxBuffers, []string{"sandboxes"}},
	)

	probe := holdPlanProbe{name: "review session", sessionID: session}
	for _, s := range statements {
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			m := measureHoldPlan(ctx, t, pool, s.holdPlanStatement, probe, mode)
			t.Logf("%s (%s, args %s): %s", s.name, mode, s.args(probe), m)
			if m.buffers > s.max {
				t.Errorf("%s under %s reads %.0f buffers, over %.0f: %v", s.name, mode, m.buffers, s.max, m.scans)
			}
			for _, scan := range m.scans {
				if scan.Node == "Seq Scan" || strings.HasPrefix(scan.Node, "Bitmap") {
					t.Errorf("%s under %s scans %s with %s, want an index probe by key: %v", s.name, mode, scan.Relation, scan.Node, m.scans)
				}
			}
			if len(m.scans) == 0 {
				t.Errorf("%s under %s: no scan in its plan", s.name, mode)
			}
			for _, scan := range m.scans {
				if scan.Relation != "" && !slices.Contains(s.relations, scan.Relation) {
					t.Errorf("%s under %s reads %s, want only %v: %v", s.name, mode, scan.Relation, s.relations, m.scans)
				}
			}
		}
	}
}
