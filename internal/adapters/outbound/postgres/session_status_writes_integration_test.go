//go:build integration

// Integration tests for the writes and the read technical plan §43.20
// relies on for a session's status: a release manifest check claimed from
// release_manifest_pending and recorded as running in
// release_manifest_checks_running (migrations/000146) in one transaction,
// how that claim coexists with the previous binary's claim during a
// rolling deploy, the migration itself, and GetSessionActivityFacts
// issuing exactly one statement.
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
)

// previousClaimReleaseManifestPending is ClaimDueReleaseManifestPending
// exactly as the previous binary (origin/main before migration 000146)
// generated and runs it: a pod still on that binary during a rolling
// deploy claims with this statement, and scans these nine columns.
const previousClaimReleaseManifestPending = `-- name: ClaimDueReleaseManifestPending :many
DELETE FROM release_manifest_pending
WHERE id IN (
    SELECT id FROM release_manifest_pending
    ORDER BY created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, session_id, owner, repo, pr_number, base_ref, head_ref, correlation_id, created_at
`

// previousClaim runs previousClaimReleaseManifestPending the way the
// previous binary does, and returns the ids it claimed.
func previousClaim(ctx context.Context, pool *pgxpool.Pool, limit int32) ([]pgtype.UUID, error) {
	rows, err := pool.Query(ctx, previousClaimReleaseManifestPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []pgtype.UUID
	for rows.Next() {
		var i sqlcgen.ReleaseManifestPending
		if err := rows.Scan(&i.ID, &i.SessionID, &i.Owner, &i.Repo, &i.PrNumber, &i.BaseRef, &i.HeadRef, &i.CorrelationID, &i.CreatedAt); err != nil {
			return nil, err
		}
		ids = append(ids, i.ID)
	}
	return ids, rows.Err()
}

// emptyReleaseManifestQueue deletes every pending and running release
// manifest check in the shared database: claims take the oldest rows
// first, so another test's leftovers would be claimed before this one's.
func emptyReleaseManifestQueue(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM release_manifest_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM release_manifest_checks_running`); err != nil {
		t.Fatal(err)
	}
}

// runningCheck reads the running row of the check claimed from pendingID.
func runningCheck(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pendingID pgtype.UUID) (sessionID pgtype.UUID, claimedAt time.Time, found bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT session_id, claimed_at FROM release_manifest_checks_running WHERE pending_id = $1`, pendingID).Scan(&sessionID, &claimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read running check %v: %v", pendingID, err)
	}
	return sessionID, claimedAt, true
}

func pendingExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// TestReleaseManifestCheck_ClaimFinishPurge pins the claim the session's
// status relies on: ClaimDue deletes the pending row -- the previous
// binary's claim, unchanged -- and records the check as running
// (release_manifest_checks_running, stamped with the database's now()) in
// the same transaction, so ActivityFacts reads it while it runs; a claimed
// check is never claimed again, so it runs at most once; Finish deletes
// the running row; PurgeStaleClaimed deletes only running rows claimed
// longer ago than its bound, never a pending one.
func TestReleaseManifestCheck_ClaimFinishPurge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReleaseManifestPendingStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	emptyReleaseManifestQueue(ctx, t, pool)

	enqueue := func(pr int32) sqlcgen.ReleaseManifestPending {
		t.Helper()
		row, err := store.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{SessionID: sessionID, Owner: "acme", Repo: "widgets", PrNumber: pr, BaseRef: "main", HeadRef: "release/x"})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return row
	}
	first := enqueue(1)
	second := enqueue(2)

	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDue(ctx, 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("ClaimDue(1) = %+v, %v; want the oldest row", claimed, err)
	}
	if pendingExists(ctx, t, pool, first.ID) {
		t.Fatal("the claimed row is still pending: a claim deletes it")
	}
	runningSession, claimedAt, found := runningCheck(ctx, t, pool, first.ID)
	if !found || runningSession != sessionID || claimedAt.Before(before) {
		t.Fatalf("running row = (%v, %v, found %v), want the session's check stamped with the database's now(), not before %v", runningSession, claimedAt, found, before)
	}
	facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
	if err != nil {
		t.Fatalf("ActivityFacts: %v", err)
	}
	if !facts.ReleaseCheckClaimedAt.Time.Equal(claimedAt) || !facts.ReleaseCheckPendingSince.Time.Equal(second.CreatedAt.Time) {
		t.Fatalf("facts while the first check runs: claimed %v pending %v, want the claim %v and the second row %v", facts.ReleaseCheckClaimedAt, facts.ReleaseCheckPendingSince, claimedAt, second.CreatedAt)
	}

	again, err := store.ClaimDue(ctx, 5)
	if err != nil || len(again) != 1 || again[0].ID != second.ID {
		t.Fatalf("ClaimDue(5) = %+v, %v; want only the second row -- a claimed check is never claimed again", again, err)
	}
	if none, err := store.ClaimDue(ctx, 5); err != nil || len(none) != 0 {
		t.Fatalf("ClaimDue on an empty queue = %+v, %v; want nothing", none, err)
	}

	if err := store.Finish(ctx, first.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if _, _, found := runningCheck(ctx, t, pool, first.ID); found {
		t.Fatal("after Finish: the first check is still running")
	}

	// The second check's worker "died": claimed long ago. A third check
	// waits, enqueued even longer ago.
	if _, err := pool.Exec(ctx, `UPDATE release_manifest_checks_running SET claimed_at = now() - interval '1 hour' WHERE pending_id = $1`, second.ID); err != nil {
		t.Fatal(err)
	}
	third := enqueue(3)
	if _, err := pool.Exec(ctx, `UPDATE release_manifest_pending SET created_at = now() - interval '2 hours' WHERE id = $1`, third.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PurgeStaleClaimed(ctx, 2*time.Hour); err != nil || n != 0 {
		t.Fatalf("PurgeStaleClaimed(2h) = %d, %v; want nothing: the claim is only an hour old", n, err)
	}
	if n, err := store.PurgeStaleClaimed(ctx, 30*time.Minute); err != nil || n != 1 {
		t.Fatalf("PurgeStaleClaimed(30m) = %d, %v; want the dead claim", n, err)
	}
	if _, _, found := runningCheck(ctx, t, pool, second.ID); found {
		t.Fatal("after the purge: the dead check is still running")
	}
	if !pendingExists(ctx, t, pool, third.ID) {
		t.Fatal("the purge deleted a pending check: it only ever touches running rows")
	}
}

// claimObservation is what claimObserver saw at one edge of one statement
// the claim sent: the statement, the connection it went out on and whether
// that connection was inside a transaction, and whether a snapshot taken
// through ANOTHER connection at that instant still held the claimed check,
// waiting or running.
type claimObservation struct {
	edge    string // "start" or "end"
	sql     string
	conn    *pgx.Conn
	inTx    bool
	visible bool
}

type claimObserverSQLKey struct{}

// claimObserver is a pgx.QueryTracer on the pool ClaimDue runs on. At the
// start and at the end of every statement that pool sends while it is
// armed -- begin, the claim's delete, the running row's insert, commit --
// it reads the session's status facts through reader, a separate pool, so
// another connection and another transaction, before the statement goes
// out or after its result is in.
type claimObserver struct {
	reader    *narvipg.SessionStore
	sessionID pgtype.UUID

	mu    sync.Mutex
	armed bool
	seen  []claimObservation
	errs  []error
}

func (o *claimObserver) observe(ctx context.Context, edge string, conn *pgx.Conn, sql string) {
	o.mu.Lock()
	armed := o.armed
	o.mu.Unlock()
	if !armed {
		return
	}
	facts, err := o.reader.ActivityFacts(context.WithoutCancel(ctx), o.sessionID, sessionactor.ReviewAutoRetriggerBudget)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.errs = append(o.errs, fmt.Errorf("read facts at the %s of %q: %w", edge, sql, err))
		return
	}
	o.seen = append(o.seen, claimObservation{
		edge: edge, sql: sql, conn: conn, inTx: conn.PgConn().TxStatus() == 'T',
		visible: facts.ReleaseCheckPendingSince.Valid || facts.ReleaseCheckClaimedAt.Valid,
	})
}

func (o *claimObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	o.observe(ctx, "start", conn, data.SQL)
	return context.WithValue(ctx, claimObserverSQLKey{}, data.SQL)
}

func (o *claimObserver) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(claimObserverSQLKey{}).(string)
	o.observe(ctx, "end", conn, sql)
}

func (o *claimObserver) arm(on bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.armed = on
}

// TestReleaseManifestCheck_ClaimRecordsTheRunningCheckInTheSameTransaction
// is review round 5's P8 pin, deterministic where the race tests are
// probabilistic: the real ClaimDue deletes the pending row and inserts the
// running row in ONE transaction, so no snapshot ever lacks both and a
// session whose turns have all ended never reads finished while its check
// is being claimed. ClaimDue runs on a pool traced by claimObserver, which
// reads the status facts from a second connection at both edges of every
// statement the claim sends -- between the delete and the insert included
// -- and each read must still hold the check. It also asserts the two
// writes go out on one connection, each inside a transaction, with no
// commit between them. Splitting them into two transactions fails it on
// every run: the read after the first commit holds neither row.
func TestReleaseManifestCheck_ClaimRecordsTheRunningCheckInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	shared, connStr := IntegrationTestPoolAndConnStr(t)
	sessionID := createTestSession(ctx, t, shared)
	emptyReleaseManifestQueue(ctx, t, shared)
	pending, err := narvipg.NewReleaseManifestPendingStore(shared).Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{SessionID: sessionID, Owner: "acme", Repo: "widgets", PrNumber: 1, BaseRef: "main", HeadRef: "release/x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	observer := &claimObserver{reader: narvipg.NewSessionStore(shared), sessionID: sessionID}
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	cfg.ConnConfig.Tracer = observer
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	if err := traced.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	observer.arm(true)
	claimed, err := narvipg.NewReleaseManifestPendingStore(traced).ClaimDue(ctx, 1)
	observer.arm(false)
	if err != nil || len(claimed) != 1 || claimed[0].ID != pending.ID {
		t.Fatalf("ClaimDue(1) = %+v, %v; want the one waiting check", claimed, err)
	}
	if len(observer.errs) > 0 {
		t.Fatalf("reading the facts during the claim: %v", observer.errs)
	}

	claimAt, startAt := -1, -1
	for i, o := range observer.seen {
		if !o.visible {
			t.Errorf("at the %s of %q: a snapshot from another connection holds neither the waiting nor the running check -- the session would read finished while its check is claimed", o.edge, firstLine(o.sql))
		}
		if o.edge != "start" {
			continue
		}
		switch {
		case strings.Contains(o.sql, "name: ClaimDueReleaseManifestPending"):
			claimAt = i
		case strings.Contains(o.sql, "name: StartReleaseManifestCheck"):
			startAt = i
		}
	}
	if claimAt < 0 || startAt < 0 || startAt < claimAt {
		t.Fatalf("statements seen %q: want the claim's delete, then the running row's insert", sentStatements(observer.seen))
	}
	claim, start := observer.seen[claimAt], observer.seen[startAt]
	if claim.conn != start.conn || !claim.inTx || !start.inTx {
		t.Fatalf("the delete went out in a transaction %v and the insert %v, on the same connection %v; want both in one transaction", claim.inTx, start.inTx, claim.conn == start.conn)
	}
	for _, o := range observer.seen[claimAt:startAt] {
		if s := strings.ToLower(strings.TrimSpace(o.sql)); s == "commit" || s == "rollback" {
			t.Fatalf("statements seen %q: the transaction ended between the delete and the insert", sentStatements(observer.seen))
		}
	}

	if pendingExists(ctx, t, shared, pending.ID) {
		t.Fatal("the claimed row is still pending")
	}
	if _, _, found := runningCheck(ctx, t, shared, pending.ID); !found {
		t.Fatal("the claimed check is not recorded as running")
	}
}

func firstLine(sql string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(sql), "\n")
	return line
}

func sentStatements(seen []claimObservation) []string {
	var out []string
	for _, o := range seen {
		if o.edge == "start" {
			out = append(out, firstLine(o.sql))
		}
	}
	return out
}

// TestReleaseManifestCheck_PreviousClaimNeverTakesARunningCheck is review
// round 4's P2/P4 case: during a rolling deploy, pods still on the previous
// binary claim with previousClaimReleaseManifestPending while new pods
// claim with ClaimDue. A check a new pod is running must never be returned
// to an old pod (it would run twice: two manifest comments, possibly two
// composition review turns), and the session's status must keep counting
// it for its whole run. Sequentially first, then both claims racing over
// many rows: every row is claimed exactly once, and exactly the rows the
// new claim took are recorded as running.
func TestReleaseManifestCheck_PreviousClaimNeverTakesARunningCheck(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReleaseManifestPendingStore(pool)
	sessions := narvipg.NewSessionStore(pool)

	t.Run("an old pod's tick while a new pod's check runs", func(t *testing.T) {
		emptyReleaseManifestQueue(ctx, t, pool)
		sessionID := createTestSession(ctx, t, pool)
		var enqueued []sqlcgen.ReleaseManifestPending
		for pr := int32(1); pr <= 3; pr++ {
			row, err := store.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{SessionID: sessionID, Owner: "acme", Repo: "widgets", PrNumber: pr, BaseRef: "main", HeadRef: "release/x"})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			enqueued = append(enqueued, row)
		}
		claimed, err := store.ClaimDue(ctx, 1)
		if err != nil || len(claimed) != 1 || claimed[0].ID != enqueued[0].ID {
			t.Fatalf("new ClaimDue(1) = %+v, %v; want the oldest row", claimed, err)
		}
		old, err := previousClaim(ctx, pool, 5)
		if err != nil {
			t.Fatalf("previous claim: %v", err)
		}
		for _, id := range old {
			if id == claimed[0].ID {
				t.Fatalf("the previous claim returned %v, the check a new pod is running", id)
			}
		}
		if len(old) != 2 {
			t.Fatalf("previous claim returned %d rows, want the two still waiting", len(old))
		}
		facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
		if err != nil {
			t.Fatalf("ActivityFacts: %v", err)
		}
		if !facts.ReleaseCheckClaimedAt.Valid {
			t.Fatal("after an old pod's tick the status no longer counts the check the new pod is running")
		}
		if err := store.Finish(ctx, claimed[0].ID); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	})

	t.Run("both claims racing", func(t *testing.T) {
		emptyReleaseManifestQueue(ctx, t, pool)
		const rows = 200
		want := map[pgtype.UUID]bool{}
		for i := 0; i < rows; i++ {
			sessionID := createTestSession(ctx, t, pool)
			row, err := store.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{SessionID: sessionID, Owner: "acme", Repo: "widgets", PrNumber: int32(i + 1), BaseRef: "main", HeadRef: "release/x"})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			want[row.ID] = true
		}

		var mu sync.Mutex
		byNew, byOld := map[pgtype.UUID]int{}, map[pgtype.UUID]int{}
		g, gctx := errgroup.WithContext(ctx)
		for w := 0; w < 6; w++ {
			g.Go(func() error {
				for {
					claimed, err := store.ClaimDue(gctx, 1)
					if err != nil {
						return fmt.Errorf("new claim: %w", err)
					}
					if len(claimed) == 0 {
						return nil
					}
					mu.Lock()
					for _, r := range claimed {
						byNew[r.ID]++
					}
					mu.Unlock()
				}
			})
			g.Go(func() error {
				for {
					ids, err := previousClaim(gctx, pool, 3)
					if err != nil {
						return fmt.Errorf("previous claim: %w", err)
					}
					if len(ids) == 0 {
						return nil
					}
					mu.Lock()
					for _, id := range ids {
						byOld[id]++
					}
					mu.Unlock()
				}
			})
		}
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}

		for id := range want {
			if n := byNew[id] + byOld[id]; n != 1 {
				t.Errorf("row %v claimed %d times (new %d, previous %d), want exactly once", id, n, byNew[id], byOld[id])
			}
		}
		if len(byNew) == 0 || len(byOld) == 0 {
			t.Logf("new claimed %d rows, previous claimed %d: the race did not mix both claims this run", len(byNew), len(byOld))
		}
		var running []pgtype.UUID
		r, err := pool.Query(ctx, `SELECT pending_id FROM release_manifest_checks_running`)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var id pgtype.UUID
			if err := r.Scan(&id); err != nil {
				t.Fatal(err)
			}
			running = append(running, id)
		}
		r.Close()
		if len(running) != len(byNew) {
			t.Errorf("%d running rows, want exactly the %d checks the new claim took", len(running), len(byNew))
		}
		for _, id := range running {
			if byNew[id] != 1 || byOld[id] != 0 {
				t.Errorf("running row %v: claimed by new %d, previous %d; want the new claim alone", id, byNew[id], byOld[id])
			}
		}
	})
}

// TestMigration000146_UpDownUp runs migration 000146 through golang-migrate
// on a database of its own at 145, with a check waiting: up creates the
// running table and both session_id indexes and leaves the waiting check
// alone; down drops them, again leaving the waiting check, which the
// previous binary claims as before; up again works on that state.
func TestMigration000146_UpDownUp(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 145)

	var sessionID, pendingID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO release_manifest_pending (session_id, owner, repo, pr_number, base_ref, head_ref) VALUES ($1, 'acme', 'widgets', 1, 'main', 'release/x') RETURNING id`, sessionID).Scan(&pendingID); err != nil {
		t.Fatalf("insert pending check: %v", err)
	}

	schema := func() (table bool, indexes []string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('release_manifest_checks_running') IS NOT NULL`).Scan(&table); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT indexname FROM pg_indexes WHERE indexname IN ('release_manifest_checks_running_session_id_idx', 'release_manifest_pending_session_id_idx')`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			indexes = append(indexes, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(indexes)
		return table, indexes
	}
	pendingCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM release_manifest_pending WHERE id = $1`, pendingID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	wantIndexes := "release_manifest_checks_running_session_id_idx,release_manifest_pending_session_id_idx"

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	for i, step := range []struct {
		version     uint
		wantTable   bool
		wantIndexes string
	}{
		{146, true, wantIndexes},
		{145, false, ""},
		{146, true, wantIndexes},
	} {
		if err := m.Migrate(step.version); err != nil {
			t.Fatalf("step %d: migrate to %d: %v", i, step.version, err)
		}
		table, indexes := schema()
		if table != step.wantTable || strings.Join(indexes, ",") != step.wantIndexes {
			t.Fatalf("step %d at %d: table %v indexes %v, want table %v indexes %q", i, step.version, table, indexes, step.wantTable, step.wantIndexes)
		}
		if n := pendingCount(); n != 1 {
			t.Fatalf("step %d at %d: %d pending rows, want the waiting check untouched", i, step.version, n)
		}
		if step.wantTable {
			if _, err := db.ExecContext(ctx, `INSERT INTO release_manifest_checks_running (pending_id, session_id) VALUES (gen_random_uuid(), $1)`, sessionID); err != nil {
				t.Fatalf("step %d: record a running check: %v", i, err)
			}
		}
	}
	if version, dirty, err := m.Version(); err != nil || dirty || version != 146 {
		t.Fatalf("final version %d dirty %v (%v), want 146 clean", version, dirty, err)
	}
}

// statementCounter is a pgx.QueryTracer that records the SQL of every
// statement a pool sends.
type statementCounter struct {
	mu  sync.Mutex
	sql []string
}

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = append(c.sql, data.SQL)
	return ctx
}

func (*statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *statementCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = nil
}

func (c *statementCounter) statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sql...)
}

// TestSessionActivityFacts_IssuesExactlyOneStatement is review round 4's
// P8 pin, deterministic where the race tests are probabilistic: every fact
// the status reads comes from ONE statement, so one MVCC snapshot.
// ActivityFacts runs on a pool whose tracer records each statement it
// sends, over a session holding every kind of fact -- a second statement
// before or after the facts query, for release checks, timers or anything
// else, fails it on every run.
func TestSessionActivityFacts_IssuesExactlyOneStatement(t *testing.T) {
	ctx := context.Background()
	shared, connStr := IntegrationTestPoolAndConnStr(t)
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	counter := &statementCounter{}
	cfg.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	if err := traced.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	sessionID := createTestSession(ctx, t, shared)
	emptyReleaseManifestQueue(ctx, t, shared)
	store := narvipg.NewReleaseManifestPendingStore(shared)
	for pr := int32(1); pr <= 2; pr++ {
		if _, err := store.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{SessionID: sessionID, Owner: "acme", Repo: "widgets", PrNumber: pr, BaseRef: "main", HeadRef: "release/x"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if _, err := store.ClaimDue(ctx, 1); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := narvipg.NewTimerStore(shared).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: sessionID, Name: sessionactor.TimerReviewRetriggerDebounce, FiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}); err != nil {
		t.Fatalf("arm debounce: %v", err)
	}
	if _, err := narvipg.NewTurnStore(shared).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending}); err != nil {
		t.Fatalf("create turn: %v", err)
	}

	sessions := narvipg.NewSessionStore(traced)
	for i := 0; i < 3; i++ {
		counter.reset()
		facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
		if err != nil {
			t.Fatalf("ActivityFacts: %v", err)
		}
		if !facts.ReleaseCheckClaimedAt.Valid || !facts.ReleaseCheckPendingSince.Valid || len(facts.ArmedTimerNames) != 1 {
			t.Fatalf("facts = %+v, want a running check, a waiting one and the armed debounce", facts)
		}
		if got := counter.statements(); len(got) != 1 || !strings.Contains(got[0], "name: GetSessionActivityFacts") {
			t.Fatalf("read %d: ActivityFacts sent %d statements, want exactly one (GetSessionActivityFacts): %q", i, len(got), got)
		}
	}
}
