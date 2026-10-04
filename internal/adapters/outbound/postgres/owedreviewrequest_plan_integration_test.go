//go:build integration

package postgres_test

import (
	"context"
	"fmt"
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

const (
	// owedReadMaxBuffers bounds each read and delete of one session's owed
	// review requests (migrations/000160): a descent of
	// owed_review_requests_session_id_idx, or of its primary key, and the
	// heap page of the row it finds. A scan of the table reads every page
	// of it: tens of pages beside 5,000 sessions below, so a plan that read
	// other sessions' rows fails the bound there. Beside 300 sessions the
	// table is two pages, and a scan of them is the planner's honest pick.
	owedReadMaxBuffers = 6
	// owedStopPerRowBuffers is what the stop's delete reads for each of the
	// session's own owed requests it deletes, beyond owedReadMaxBuffers:
	// the row's heap page and its entries.
	owedStopPerRowBuffers = 2
	// heldWithOwedMaxBuffers bounds the re-review hold's read once it reads
	// the owed term too (ReviewRetriggerHeld): holdReadMaxBuffers' probe of
	// the open turns' index, plus one descent of the owed requests' index.
	heldWithOwedMaxBuffers = 10
)

// storeOwedRequests owes a request on every fourth of the shape's other
// sessions and three on the long one, each naming its session's newest
// turn, and analyzes the table: some 1,250 rows, enough that a scan of
// them reads many times owedReadMaxBuffers.
func storeOwedRequests(ctx context.Context, t *testing.T, pool *pgxpool.Pool, long pgtype.UUID) {
	t.Helper()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	exec("autovacuum off owed_review_requests", `ALTER TABLE owed_review_requests SET (autovacuum_enabled = false)`)
	exec("the other sessions' owed requests", `
		INSERT INTO owed_review_requests (session_id, trigger, request_text, is_review_attempt, context_moves, moved_turn_id)
		SELECT s.id, 'button', 'Manual re-review requested via the web review button.', true, 1,
		       (SELECT t.id FROM turns t WHERE t.session_id = s.id ORDER BY t.created_at DESC LIMIT 1)
		FROM (SELECT id, row_number() OVER (ORDER BY id) AS n FROM sessions WHERE id <> $1) s
		WHERE s.n % 4 = 0 AND EXISTS (SELECT 1 FROM turns t WHERE t.session_id = s.id)`, long)
	exec("the long session's owed requests", `
		INSERT INTO owed_review_requests (session_id, trigger, request_text, is_review_attempt, context_moves, moved_turn_id, created_at)
		SELECT $1, 'label', 'Manual re-review requested via the configured GitHub label.', true, g,
		       (SELECT t.id FROM turns t WHERE t.session_id = $1 ORDER BY t.created_at DESC LIMIT 1),
		       now() - make_interval(secs => 10 - g)
		FROM generate_series(1, 3) g`, long)
	exec("analyze owed_review_requests", `ANALYZE owed_review_requests`)
}

// heldWithOwedProblem returns why the extended hold read is more than a
// probe of each of its two relations, or "": at most
// heldWithOwedMaxBuffers, and turns read only through
// turns_open_session_id_idx (holdReadProblem's rule).
func heldWithOwedProblem(m holdPlanMeasurement) string {
	if m.buffers > heldWithOwedMaxBuffers {
		return fmt.Sprintf("reads %.0f buffers, over %d (%v)", m.buffers, heldWithOwedMaxBuffers, m.scans)
	}
	turnsOnly := holdPlanMeasurement{buffers: 0, scans: nil}
	for _, s := range m.scans {
		if s.Relation == "owed_review_requests" || (s.Node == "Bitmap Index Scan" && s.Index == "owed_review_requests_session_id_idx") {
			continue
		}
		turnsOnly.scans = append(turnsOnly.scans, s)
	}
	return holdReadProblem(turnsOnly)
}

// owedScanProblem returns why a statement of the owed requests reads more
// than limit, or "".
func owedScanProblem(m holdPlanMeasurement, limit float64) string {
	if m.buffers > limit {
		return fmt.Sprintf("reads %.0f buffers, over %.0f (%v)", m.buffers, limit, m.scans)
	}
	return ""
}

// TestOwedReviewRequest_PlansReadTheSessionsOwnRows measures, by buffers
// read, every statement technical plan §24.9's owed human review requests
// add or change, under a custom plan and under the generic plan pgx's
// statement cache lets Postgres settle on from a statement's sixth run, on
// the context check's tables -- among them a long review session whose
// 4,000 turns lie among 316,000 of other sessions' -- with some 1,250 owed
// requests stored beside them, three of the long session's:
//
//   - ReviewRetriggerHeld, which now reads the owed term too: at most
//     heldWithOwedMaxBuffers, turns only through the open turns' index;
//   - the owed requests' reads and deletes (GetOldestOwedReviewRequest,
//     ExistsOwedReviewRequest, DeleteOwedReviewRequest): at most
//     owedReadMaxBuffers; the stop's delete
//     (DeleteOwedReviewRequestsForStop), owedStopPerRowBuffers more for each
//     of the session's own requests it deletes -- none for another
//     session's;
//   - the writes (InsertOwedReviewRequest, ArmOwedReviewRequestTimer,
//     BackOffOwedReviewRequestTimer): at most contextWriteMaxBuffers each,
//     whatever the tables hold;
//   - CreateTurn, which now writes the request's requester, text and
//     moves: main's access path, at most contextWriteMaxBuffers;
//   - GetReviewAttemptToCheck, whose walk now runs for a person's attempt
//     too: for a person's pick, the context check's own bound (no more
//     than holdPlanMaxBuffers of the session's own turns, never a scan of
//     turns); for any other pick, no more than main's text of it, the two
//     measured once the tables are settled for both (measureAgainstMain):
//     the CreateTurn measurements just before leave rolled-back pending
//     turns in the open turns' index, which whichever text first reads
//     through a plain index scan pays to mark dead.
func TestOwedReviewRequest_PlansReadTheSessionsOwnRows(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	uuidArg := func(p holdPlanProbe) string { return fmt.Sprintf("'%s'::uuid", p.sessionID.String()) }
	// newestTurn is the probe's newest turn; oldestOwed its oldest owed
	// request, invalid when it owes none; owedSince that request's
	// created_at, or now.
	var newestTurn func(p holdPlanProbe) pgtype.UUID
	var oldestOwed func(p holdPlanProbe) pgtype.UUID
	var owedSince func(p holdPlanProbe) pgtype.Timestamptz
	tsArg := func(ts pgtype.Timestamptz) string {
		return fmt.Sprintf("'%s'::timestamptz", ts.Time.Format(time.RFC3339Nano))
	}
	idArg := func(id pgtype.UUID) string {
		if !id.Valid {
			return "'00000000-0000-0000-0000-000000000000'::uuid"
		}
		return fmt.Sprintf("'%s'::uuid", id.String())
	}

	held := holdPlanStatement{
		name: "ReviewRetriggerHeld",
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			_, err := narvipg.NewTurnStore(nil).WithTx(tx).ReviewRetriggerHeld(ctx, p.sessionID)
			return err
		},
		args: uuidArg,
	}
	reads := []holdPlanStatement{
		{
			name: "GetOldestOwedReviewRequest",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewOwedReviewRequestStore(nil).WithTx(tx).Oldest(ctx, p.sessionID)
				return err
			},
			args: uuidArg,
		},
		{
			name: "ExistsOwedReviewRequest",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewOwedReviewRequestStore(nil).WithTx(tx).Exists(ctx, p.sessionID)
				return err
			},
			args: uuidArg,
		},
		{
			name: "DeleteOwedReviewRequest",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewOwedReviewRequestStore(nil).WithTx(tx).Delete(ctx, oldestOwed(p))
				return err
			},
			args: func(p holdPlanProbe) string { return idArg(oldestOwed(p)) },
		},
		{
			name: "DeleteOwedReviewRequestsForStop",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewOwedReviewRequestStore(nil).WithTx(tx).DeleteForStop(ctx, p.sessionID, pgtype.Timestamptz{Time: time.Now(), Valid: true})
				return err
			},
			args: func(p holdPlanProbe) string { return uuidArg(p) + ", now()" },
		},
	}
	writes := []holdPlanStatement{
		{
			name: "InsertOwedReviewRequest",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				text := "Manual re-review requested via the web review button."
				_, err := narvipg.NewOwedReviewRequestStore(nil).WithTx(tx).Insert(ctx, sqlcgen.InsertOwedReviewRequestParams{
					SessionID: p.sessionID, Trigger: "button", RequestText: &text, IsReviewAttempt: true, ContextMoves: 1, MovedTurnID: newestTurn(p),
				})
				return err
			},
			args: func(p holdPlanProbe) string {
				return uuidArg(p) + ", NULL, 'button', 'Manual re-review requested via the web review button.', true, 1, " + idArg(newestTurn(p))
			},
		},
		{
			name: "ArmOwedReviewRequestTimer",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				return narvipg.NewTimerStore(nil).WithTx(tx).ArmOwedReviewRequest(ctx, p.sessionID)
			},
			args: uuidArg,
		},
		{
			name: "BackOffOwedReviewRequestTimer",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				_, err := narvipg.NewTimerStore(nil).WithTx(tx).BackOffOwedReviewRequest(ctx, p.sessionID, owedSince(p), timeouts.DispatchRetryBackoff, timeouts.DispatchRetryBackoffMax)
				return err
			},
			args: func(p holdPlanProbe) string {
				return fmt.Sprintf("%s, %v, %v, %s", tsArg(owedSince(p)), timeouts.DispatchRetryBackoff.Seconds(), timeouts.DispatchRetryBackoffMax.Seconds(), uuidArg(p))
			},
		},
	}
	createTurn := generatedQuery(t, "turns.sql.go", "createTurn")
	createArgs := func(p holdPlanProbe) string {
		return uuidArg(p) + ", 'pending', 'review', NULL, false, NULL, 'sha-rerun', NULL, NULL, NULL, NULL, NULL, NULL, NULL, true"
	}
	createPair := struct{ changed, before holdPlanStatement }{
		changed: holdPlanStatement{
			name: "CreateTurn",
			run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
				prompt, head, trigger, text, moves := "review", "sha-rerun", "button", "Manual re-review requested via the web review button.", int32(1)
				_, err := narvipg.NewTurnStore(nil).WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{
					SessionID: p.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true,
					RequestTrigger: &trigger, RequestText: &text, ContextMoves: &moves,
				})
				return err
			},
			args: func(p holdPlanProbe) string {
				return createArgs(p) + ", 'button', NULL, 'Manual re-review requested via the web review button.', 1"
			},
		},
		before: preparedStatement160("CreateTurn",
			before160(t, createTurn, "CreateTurn", []textEdit{{old: ", requested_by, request_text, context_moves", n: 2}, {old: ", $17, $18, $19", n: 1}}),
			func(p holdPlanProbe) []any {
				prompt, head, trigger := "review", "sha-rerun", "button"
				return []any{p.sessionID, "pending", &prompt, nil, false, nil, &head, nil, nil, nil, nil, nil, nil, nil, true, &trigger}
			},
			func(p holdPlanProbe) string { return createArgs(p) + ", 'button'" }),
	}
	preRead := holdPlanStatement{
		name: "GetReviewAttemptToCheck",
		run: func(ctx context.Context, tx pgx.Tx, p holdPlanProbe) error {
			_, err := narvipg.NewTurnStore(nil).WithTx(tx).ReviewAttemptToCheck(ctx, p.sessionID)
			return err
		},
		args: uuidArg,
	}
	// main's text of the pre-read compared the trigger with 'auto' alone.
	preReadBefore := preparedStatement160("GetReviewAttemptToCheck",
		before160(t, generatedQuery(t, "turns.sql.go", "getReviewAttemptToCheck"), "GetReviewAttemptToCheck",
			[]textEdit{{old: "COALESCE(b.request_trigger, '') IN ('auto', 'label', 'button')", new: "COALESCE(b.request_trigger, '') = 'auto'", n: 1}}),
		func(p holdPlanProbe) []any { return []any{p.sessionID} }, uuidArg)
	modes := []string{"force_custom_plan", "force_generic_plan"}

	for _, shape := range []contextPlanShape{
		{name: "5,000 sessions of 6 turns, every 20th open, beside a review session of 4,000 ended turns and one processing", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "processing"},
		{name: "300 sessions of 200 ended turns beside one of 20,000, none open", others: 300, perOther: 200, longEnded: 20_000},
		{name: "the first, the review session stored after ANALYZE", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "processing", longAfterAnalyze: true},
		{name: "the review session's 4,000 turns among 316,000 of the others', its last a person's request that waited behind no turn", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "attempt", interleave: 80, tailTrigger: "button"},
		{name: "the same, the person's request queued behind the session's last ended turn", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "queued", interleave: 80, tailTrigger: "button"},
		{name: "the same, an automatic attempt queued behind it", others: 5_000, perOther: 6, openEvery: 20, longEnded: 4_000, longTail: "queued", interleave: 80},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := holdPlanDatabase(ctx, t, holdPlanLatestVersion)
			probes := storeContextPlanShape(ctx, t, pool, shape)
			storeOwedRequests(ctx, t, pool, probes[0].sessionID)
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
			owedCount := map[pgtype.UUID]int{}
			oldest := map[pgtype.UUID]pgtype.UUID{}
			since := map[pgtype.UUID]pgtype.Timestamptz{}
			for _, p := range probes {
				var id pgtype.UUID
				if err := pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, p.sessionID).Scan(&id); err != nil {
					t.Fatalf("the newest turn of %s: %v", p.name, err)
				}
				newest[p.sessionID] = id
				var owedID pgtype.UUID
				var owedAt pgtype.Timestamptz
				if err := pool.QueryRow(ctx, `SELECT (SELECT id FROM owed_review_requests WHERE session_id = $1 ORDER BY created_at, id LIMIT 1),
					COALESCE((SELECT min(created_at) FROM owed_review_requests WHERE session_id = $1), now())`, p.sessionID).Scan(&owedID, &owedAt); err != nil {
					t.Fatalf("the oldest owed request of %s: %v", p.name, err)
				}
				oldest[p.sessionID], since[p.sessionID] = owedID, owedAt
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM owed_review_requests WHERE session_id = $1`, p.sessionID).Scan(&n); err != nil {
					t.Fatal(err)
				}
				owedCount[p.sessionID] = n
			}
			newestTurn = func(p holdPlanProbe) pgtype.UUID { return newest[p.sessionID] }
			oldestOwed = func(p holdPlanProbe) pgtype.UUID { return oldest[p.sessionID] }
			owedSince = func(p holdPlanProbe) pgtype.Timestamptz { return since[p.sessionID] }

			for _, probe := range probes {
				owes := oldest[probe.sessionID].Valid
				for _, mode := range modes {
					key := func(name string) string { return fmt.Sprintf("%s, %s (owes %v), %s", name, probe.name, owes, mode) }

					got := measureHoldPlan(ctx, t, pool, held, probe, mode)
					t.Logf("%s: %v", key(held.name), got)
					if problem := heldWithOwedProblem(got); problem != "" {
						t.Errorf("%s: %s", key(held.name), problem)
					}
					for _, r := range reads {
						got := measureHoldPlan(ctx, t, pool, r, probe, mode)
						t.Logf("%s: %v", key(r.name), got)
						limit := float64(owedReadMaxBuffers)
						if r.name == "DeleteOwedReviewRequestsForStop" {
							limit += float64(owedStopPerRowBuffers * owedCount[probe.sessionID])
						}
						if problem := owedScanProblem(got, limit); problem != "" {
							t.Errorf("%s: %s", key(r.name), problem)
						}
					}
					for _, w := range writes {
						got := measureHoldPlan(ctx, t, pool, w, probe, mode)
						t.Logf("%s: %v", key(w.name), got)
						if problem := owedScanProblem(got, contextWriteMaxBuffers); problem != "" {
							t.Errorf("%s: %s", key(w.name), problem)
						}
					}
					before := measureHoldPlan(ctx, t, pool, createPair.before, probe, mode)
					after := measureHoldPlan(ctx, t, pool, createPair.changed, probe, mode)
					t.Logf("%s: main's text %v; this release's %v", key("CreateTurn"), before, after)
					if g, w := scanShape(after.scans), scanShape(before.scans); g != w {
						t.Errorf("%s: plans %s, main's text %s: the change moved its access path", key("CreateTurn"), g, w)
					}
					if after.buffers > contextWriteMaxBuffers {
						t.Errorf("%s: reads %.0f buffers, over %d (%v)", key("CreateTurn"), after.buffers, contextWriteMaxBuffers, after.scans)
					}

					// A person's pick is held to the context check's bound
					// alone; any other pick to main's text of it too, both
					// measured on settled tables (measureAgainstMain).
					personsPick := shape.tailTrigger != "" && probe.sessionID == probes[0].sessionID
					var pick, mainPick holdPlanMeasurement
					if personsPick {
						pick = measureHoldPlan(ctx, t, pool, preRead, probe, mode)
						t.Logf("%s: %v", key(preRead.name), pick)
					} else {
						pick, mainPick = measureAgainstMain(ctx, t, pool, preRead, preReadBefore, probe, mode)
						t.Logf("%s: main's text %v; this release's %v", key(preRead.name), mainPick, pick)
					}
					if problem := contextReadProblem(pick, probe.turns, true); problem != "" {
						t.Errorf("%s: %s", key(preRead.name), problem)
					}
					if !personsPick && pick.buffers > mainPick.buffers {
						t.Errorf("%s: reads %.0f buffers, main's text %.0f, for a pick that is no person's request", key(preRead.name), pick.buffers, mainPick.buffers)
					}
				}
			}
		})
	}
}

// textEdit is one edit before160 makes: old, which must occur exactly n
// times, replaced by new.
type textEdit struct {
	old, new string
	n        int
}

// before160 is query as main sent it before this release: the generated
// text with each edit undone -- each old text must occur exactly as many
// times as listed, so a change to the statement fails here instead of
// measuring a stranger -- and renamed, so its prepared statement is found
// apart from the store's.
func before160(t *testing.T, query, name string, edits []textEdit) string {
	t.Helper()
	for _, e := range edits {
		if got := strings.Count(query, e.old); got != e.n {
			t.Fatalf("%s: %q occurs %d times, want %d: the statement changed, update the edit", name, e.old, got, e.n)
		}
		query = strings.ReplaceAll(query, e.old, e.new)
	}
	if !strings.HasPrefix(query, "-- name: "+name+" ") {
		t.Fatalf("%s: the generated text does not open with its name", name)
	}
	return "-- name: Before160" + strings.TrimPrefix(query, "-- name: ")
}

// preparedStatement160 is preparedStatement for main's text before160
// returned: prepared by the test on the pool's one connection and run by
// name.
func preparedStatement160(name, sql string, args func(p holdPlanProbe) []any, explain func(p holdPlanProbe) string) holdPlanStatement {
	stmt := "before160_" + strings.ToLower(name)
	return holdPlanStatement{
		name: "Before160" + name,
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
