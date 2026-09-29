//go:build integration

// Resilience test #1 (§9.3, design decision 12): "Kill the CP pod mid-turn -> actor rehydrates, turn
// resumes or fails-with-reason; no stuck processing." This Step's own
// resilience test targets the "fails-with-reason" branch ONLY -- real
// turn RESUME (continuing a Processing turn on a fresh actor without
// failing it) is explicitly §3.2's ("resume") and §3.3's ("turn recovery")
// own job; no turn-resume machinery of any kind exists anywhere in this
// codebase today (confirmed: domain/turn's own transition table has no
// edge out of Processing except Completed/Failed/Cancelled), and §9.3
// scenario #1's own "or" phrasing already permits targeting only this
// branch.
//
// This is a genuinely self-contained integration test using this
// codebase's own already-established testcontainers-Postgres pattern
// (matching every prior Step's own *_integration_test.go convention
// exactly) -- it does NOT build reusable resilience-harness
// infrastructure, which is explicitly §9.3's own job
// (test/resilience/README.md, left completely untouched by this Step).
package sessionactor

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/migrations"
)

// newTestPoolPair spins up ONE throwaway Postgres container (running every
// embedded migration up), then returns TWO INDEPENDENT *pgxpool.Pool
// instances connected to it -- mirroring exactly how two real pods would
// each hold their own connection pool to one shared database. This
// matters for THIS test specifically: "kill pod A" must abruptly drop
// only pod A's own connections (including the lock connection pod A's
// Registry dialled from poolA's settings, which holds the session's
// advisory lock -- lockholder.go), never pod B's -- a single shared pool
// would make that distinction impossible to express at all. Neither pool
// is registered for auto-cleanup via t.Cleanup here: poolA is
// deliberately NEVER closed by this test at all (see
// killAdvisoryLockHolder's own call site for why -- a real killed process
// leaves nothing to gracefully close either); poolB's own cleanup is
// registered explicitly by the test body, ordered relative to
// registryB.Shutdown().
func newTestPoolPair(t *testing.T) (poolA, poolB *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// startCtx bounds the container-startup call below via the ambient
	// context (image pull + Docker daemon round trip + Postgres's own
	// internal ready-wait) -- kept as defense in depth, but NOT solely
	// relied upon any more: CI run 30834918806 showed this exact bound
	// (added after CI run 30831633470's own ContainerStart hang) itself
	// fail to actually cut the call off when the hang recurred one layer
	// deeper, inside testcontainers-go's own wait.(*LogStrategy).
	// WaitUntilReady -- the goroutine dump showed it looping on a 100ms
	// poll for the FULL 10-minute panic window, never once observing
	// ctx.Done(), despite this same context chain being correctly wired
	// all the way through (confirmed directly: reproducing an
	// impossible-to-satisfy wait condition locally against this exact
	// call DOES correctly time out via this same context mechanism, at
	// testcontainers' own hardcoded 60s deadline -- so the mechanism is
	// sound in isolation, but evidently not dependable against whatever a
	// genuinely stalled CI-runner Docker daemon does to it in practice).
	//
	// Rather than keep chasing exactly why context cancellation isn't
	// always honored deep inside a third-party library under conditions
	// this dev machine cannot reproduce, the startup call now ALSO runs on
	// its own goroutine (via errgroup.Group.Go -- no naked `go` statement,
	// §11) raced against an independent, plain time.After watchdog:
	// whichever of "the call returned" or "the watchdog fired" happens
	// first decides the outcome, with no dependency on any context
	// cancellation actually being honored by anything downstream. If the
	// watchdog wins, the goroutine is deliberately abandoned (leaked, not
	// joined) rather than blocking this test's own cleanup on a call that
	// has already demonstrated it can ignore its own cancellation signal.
	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	const containerStartWatchdog = 2*time.Minute + 15*time.Second
	type containerStartResult struct {
		container *tcpostgres.PostgresContainer
		err       error
	}
	startCh := make(chan containerStartResult, 1)
	var startGroup errgroup.Group
	startGroup.Go(func() error {
		container, err := tcpostgres.Run(startCtx, "postgres:17-alpine",
			tcpostgres.WithDatabase("narvi_test"),
			tcpostgres.WithUsername("narvi"),
			tcpostgres.WithPassword("narvi"),
			tcpostgres.BasicWaitStrategies(),
		)
		startCh <- containerStartResult{container: container, err: err}
		return nil
	})

	var container *tcpostgres.PostgresContainer
	var err error
	select {
	case res := <-startCh:
		container, err = res.container, res.err
		if err != nil {
			t.Fatalf("start postgres container: %v", err)
		}
	case <-time.After(containerStartWatchdog):
		t.Fatalf("start postgres container: tcpostgres.Run did not return within %s -- Docker daemon likely "+
			"stalled without honoring context cancellation (see this function's own doc comment)", containerStartWatchdog)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrateDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = migrateDB.Close() }()

	dbDriver, err := migratepg.WithInstance(migrateDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migratepg.WithInstance: %v", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	poolA, err = narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool (A): %v", err)
	}
	poolB, err = narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool (B): %v", err)
	}
	return poolA, poolB
}

// killAdvisoryLockHolder terminates the Postgres backend(s) currently
// holding a granted session-level advisory lock (via a SEPARATE
// administrative connection, adminPool) -- see this test's own inline
// comment at its call site for why this, not pgxpool.Pool.Close(), is
// the correct way to simulate "pod A" abruptly disappearing. Fails the
// test if no such backend is found (a real bug in the test's own setup,
// not a condition this test is designed to tolerate).
//
// Returns only once the kill's EFFECT is actually observable: no granted
// advisory lock remains in pg_locks. pg_terminate_backend only ever SENDS
// the termination signal -- it returns true the moment the signal is
// delivered, without waiting for the target backend to be scheduled,
// process it, abort, and release its locks (Postgres's own documented
// semantics), so the lock release is asynchronous with respect to this
// function's own Exec returning. That gap is normally sub-millisecond,
// but on a CPU-starved runner the dying backend can go unscheduled for
// tens of milliseconds -- CI runs 31008526590 and 31009652346 (two
// unrelated PRs, same day, same test-integration group) each caught
// exactly that window: TestResilience_ConcurrentPlainSpawnAcrossActors_
// CreateSandboxCalledAtMostOnce's own single-shot registryB.GetOrSpawn
// (a deliberately non-blocking, fail-fast pg_try_advisory_lock --
// registry.go's own documented §2 contract, which must NOT grow retries
// for a test harness's sake) ran inside it and got
// ErrSessionActorElsewhere from a backend that was already dead but not
// yet reaped. A mechanism probe against a deliberately CPU-throttled
// (0.4-CPU) Postgres reproduced the window at a 3.2% hit rate (16/500,
// release lag up to 44ms; 0/500 unthrottled). Polling pg_locks here is
// authoritative, not a heuristic: pg_try_advisory_lock and pg_locks read
// the same shared lock-manager state, so "no granted advisory lock
// remains" is exactly the postcondition every call site's own next step
// already assumes ("this only succeeds because the kill released the
// advisory lock" -- each caller's own doc comment). This keeps the kill
// itself every bit as abrupt as before (nothing graceful is added to the
// dying side); it only makes this helper's return honest about when the
// simulated `kill -9` has actually COMPLETED -- a real pod B never wins
// the lock earlier than that either.
func killAdvisoryLockHolder(ctx context.Context, t *testing.T, adminPool *pgxpool.Pool) {
	t.Helper()

	rows, err := adminPool.Query(ctx,
		`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted = true`)
	if err != nil {
		t.Fatalf("query advisory lock holders: %v", err)
	}
	var pids []int32
	for rows.Next() {
		var pid int32
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			t.Fatalf("scan pid: %v", err)
		}
		pids = append(pids, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate advisory lock holders: %v", err)
	}
	if len(pids) == 0 {
		t.Fatal("no granted advisory lock found to kill -- did pod A actually acquire it?")
	}

	for _, pid := range pids {
		if _, err := adminPool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("pg_terminate_backend(%d): %v", pid, err)
		}
	}

	// Wait for the terminated backend(s) to have actually released their
	// advisory locks -- see this helper's own doc comment for why this is
	// asynchronous, and why pg_locks is the authoritative signal to poll.
	// The zero-granted-locks postcondition deliberately matches the kill's
	// own breadth above (every granted advisory lock's holder was just
	// terminated), and is only meaningful against newTestPoolPair's own
	// dedicated container (sharedpool_integration_test.go's top doc comment
	// already documents why this whole helper is never safe against the
	// shared one). In the common case the first poll already observes zero
	// (probe p50 lag: ~318µs), so this adds one round trip, not latency.
	waitUntil(t, 10*time.Second, func() bool {
		var stillGranted int
		if err := adminPool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted = true`,
		).Scan(&stillGranted); err != nil {
			t.Fatalf("recheck granted advisory locks: %v", err)
		}
		return stillGranted == 0
	})
}

// TestResilience_KillPodMidTurn_TurnFailsWithReason_NoStuckProcessing is
// §9.3 scenario #1's own "fails-with-reason; no stuck processing" half.
//
// Sequencing (each step's own reasoning matters, see inline comments):
//  1. Seed a session + a turn already Processing, with dispatched_at
//     comfortably in the past against a tiny injected TurnDeadline, and
//     an already-overdue turn_deadline row -- directly via the stores,
//     bypassing the live dispatch path entirely (mirrors
//     TestTurnDeadlineTimerFired_FullRoundTrip's own EXISTING precedent
//     exactly): this test's own narrow purpose is proving REHYDRATION
//     correctness (a fresh actor on a fresh pod picks up and correctly
//     resolves a timer nobody else is going to handle), not dispatch
//     correctness (already covered by dispatch_integration_test.go and
//     timerfired_integration_test.go respectively).
//  2. Hydrate an actor for this session via registryA.GetOrSpawn -- this
//     is what makes "pod A" a genuine, real owner of the session (holding
//     the real Postgres advisory lock on pod A's own lock connection,
//     dialled from poolA's settings), not a hypothetical one.
//  3. "Kill pod A": terminate the exact Postgres BACKEND holding the
//     advisory lock directly, via a separate administrative connection
//     (killAdvisoryLockHolder, using pg_terminate_backend), WITHOUT
//     calling registryA.Shutdown() first. A real `kill -9` never runs
//     graceful shutdown code either -- calling Shutdown() here would test
//     the ALREADY-PROVEN graceful path (registry_integration_test.go),
//     not the scenario this test exists for. This is the actual, precise
//     causal mechanism a real `kill -9` relies on too (the OS closes the
//     dead process's TCP sockets; Postgres notices the connection is gone
//     and reaps that backend, releasing every lock it held) -- reproduced
//     faithfully at exactly the layer that matters for this test's own
//     assertions. pgxpool.Pool.Close() would not do it: the lock lives on
//     pod A's lock connection, which is not one of poolA's connections
//     (lockholder.go), so closing poolA would leave the lock held -- see
//     this test's own inline comment at the call site for the full
//     reasoning. Postgres auto-releases an advisory lock the
//     instant the backend connection holding it terminates (this is what
//     makes the simulation FAITHFUL, not merely "stop referencing a Go
//     object": if this test instead just dropped its own reference to
//     registryA/poolA without closing the underlying connections, the
//     advisory lock would remain held by a live-but-orphaned Postgres
//     backend process indefinitely, and pod B's own hydration attempt
//     below would correctly, but unhelpfully, report
//     ErrSessionActorElsewhere forever -- exactly the failure this test
//     must NOT exhibit).
//  4. Construct registryB on a FRESH pool (poolB) and run ITS OWN
//     PumpOnce (exported specifically for deterministic test-driving)
//     until the overdue timer is claimed and delivered to a freshly
//     hydrated actor on pod B.
//  5. Assert, reading everything back from Postgres (never from in-memory
//     state): the turn transitioned to Failed with a real failure reason,
//     the session's own derived status/failure_reason followed, and a
//     synthetic execution_complete event was appended (§3.3's own
//     "clients always see one terminal event per turn" contract) -- i.e.
//     exactly the "fails-with-reason; no stuck processing" half of §9.3
//     #1, honestly not attempting the "resumes" half (out of scope, see
//     this file's own top comment).
func TestResilience_KillPodMidTurn_TurnFailsWithReason_NoStuckProcessing(t *testing.T) {
	ctx := context.Background()
	poolA, poolB := newTestPoolPair(t)

	sessionID := createTestSession(ctx, t, poolA)

	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default

	turnStore := narvipg.NewTurnStore(poolA)
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	dispatchedAt := time.Now().Add(-1 * time.Hour) // comfortably past the tiny deadline
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:           created.ID,
		Status:       sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	timerStore := narvipg.NewTimerStore(poolA)
	overdue := time.Now().Add(-1 * time.Minute) // already due
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      TimerTurnDeadline,
		FiresAt:   pgtype.Timestamptz{Time: overdue, Valid: true},
	}); err != nil {
		t.Fatalf("arm overdue turn_deadline timer: %v", err)
	}

	// --- Step 2: pod A hydrates and genuinely owns this session (a real
	// advisory lock on pod A's own lock connection). ---
	registryA, err := NewRegistry(ctx, poolA, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := registryA.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("registryA.GetOrSpawn: %v", err)
	}

	// --- Step 3: kill pod A. Deliberately NOT registryA.Shutdown() first
	// -- see this test's own doc comment for why. ---
	//
	// pgxpool.Pool.Close() itself is NOT the right tool here, and
	// deliberately not used: actor A's advisory lock lives on pod A's own
	// lock connection, which is not one of poolA's connections
	// (lockholder.go) -- closing poolA would leave it, and the lock, in
	// place, and pod B could never take over. (Before the lock connection
	// existed, the lock lived on a poolA connection actor A held for its
	// whole life, and Close() would instead have hung the test waiting
	// for it.)
	//
	// Instead, terminate the exact Postgres BACKEND holding the advisory
	// lock directly (via a separate administrative connection, poolB) --
	// this is the actual, precise causal mechanism a real `kill -9`
	// relies on too (the OS closes the dead process's TCP sockets;
	// Postgres notices the connection is gone and reaps that backend,
	// releasing every lock it held) reproduced faithfully at exactly the
	// layer that matters for this test's own assertions, without fighting
	// puddle's own cooperative-release bookkeeping. poolA's own Go-level
	// resources are deliberately never Close()'d afterward, matching a
	// real killed process leaving nothing to gracefully clean up either.
	killAdvisoryLockHolder(ctx, t, poolB)

	// --- Step 4: pod B, a genuinely fresh pool, claims and delivers the
	// now-orphaned overdue timer via its own timer pump. ---
	// Cleanup order matters here: t.Cleanup runs LIFO, so registering
	// poolB.Close() FIRST and registryB.Shutdown() SECOND means Shutdown
	// actually runs FIRST at test end -- gracefully stopping actor B (and
	// closing its lock connection) BEFORE poolB itself is closed.
	// Pool.Close blocks until every checked-out connection is returned,
	// so it must not run while actor B could still be inside a command
	// holding one.
	registryB, err := NewRegistry(ctx, poolB, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(poolB.Close)
	t.Cleanup(func() { _ = registryB.Shutdown() })

	waitUntil(t, 10*time.Second, func() bool {
		if err := registryB.PumpOnce(ctx); err != nil {
			t.Logf("PumpOnce: %v (retrying)", err)
			return false
		}
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	// --- Step 5: assert, from Postgres, that the turn failed with a real
	// reason and nothing is left stuck in Processing. ---
	gotTurn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("turn status = %q, want %q (must not be stuck in %q)",
			gotTurn.Status, sqlcgen.TurnStatusFailed, sqlcgen.TurnStatusProcessing)
	}
	if !gotTurn.CompletedAt.Valid {
		t.Error("turn completed_at not set")
	}

	sessionStore := narvipg.NewSessionStore(poolB)
	gotSession, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSession.Status != sqlcgen.SessionStatusFailed {
		t.Errorf("session status = %q, want %q", gotSession.Status, sqlcgen.SessionStatusFailed)
	}
	if gotSession.FailureReason == nil || *gotSession.FailureReason != sqlcgen.SessionFailureReasonTimeout {
		t.Errorf("session failure_reason = %v, want %q (a real failure reason, not stuck/unset)",
			gotSession.FailureReason, sqlcgen.SessionFailureReasonTimeout)
	}

	var eventCount int
	if err := poolB.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		sessionID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("execution_complete event count = %d, want 1 (synthetic completion, §3.3 -- "+
			"clients must always see exactly one terminal event per turn, even across a pod kill)", eventCount)
	}

	var timerCount int
	if err := poolB.QueryRow(ctx,
		`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerTurnDeadline,
	).Scan(&timerCount); err != nil {
		t.Fatalf("count turn_deadline timers: %v", err)
	}
	if timerCount != 0 {
		t.Errorf("turn_deadline timer count = %d, want 0 (pod B's handler must delete it once handled, "+
			"exactly like every other timer handler's own re-arm-or-delete contract)", timerCount)
	}
}
