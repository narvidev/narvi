//go:build integration

// Package resilience_test is Step 30's ("resilience suite", §9.3) own
// harness: a real, reusable "mini control plane" that assembles the same
// components cmd/control-plane/main.go wires in production -- a real
// Postgres (via testcontainers, embedded migrations applied), the real
// Postgres store types, a real sessionactor.Registry, and a real
// wshub.Hub -- using ONLY exported APIs from the packages it wires
// together, exactly like main.go itself does. This is deliberately a
// MORE realistic, less white-box style of test than the existing
// internal-package integration tests (e.g. internal/app/sessionactor's
// own resilience_killpod_integration_test.go): appropriate for scenarios
// that genuinely span multiple packages, not one package's own internals
// in isolation.
//
// Kept intentionally minimal, driven by this PR's own scenario #12's real
// needs (test/resilience/scenario12_rolling_restart_test.go) -- a second
// PR is expected to extend this harness further once its own scenarios'
// (§9.3 #3 slow boot, #5's plain-SPAWN race variant, #7 WS-drop ack
// redelivery) real needs are known, per test/resilience/README.md's own
// scenario-by-scenario index.
package resilience_test

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
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver, used only for the migrate handle below
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/migrations"
)

// Harness is the shared, real multi-component rig every test/resilience
// scenario builds on: one throwaway Postgres container (migrated up
// once), the real store types wrapping it, and a real wshub.Hub -- all
// exposed so a scenario test can seed state directly via the stores (the
// same way internal/app/sessionactor's own resilience tests seed
// Processing turns/Ready sandboxes directly, bypassing the live
// dispatch path) and can construct one or more real Registry "pods"
// against the SAME underlying database via NewRegistry.
//
// Unlike resilience_killpod_integration_test.go's own newTestPoolPair
// (which deliberately hands back TWO INDEPENDENT *pgxpool.Pool instances,
// because that scenario's whole point is severing exactly one pod's own
// connections without touching the other's), this harness exposes a
// SINGLE shared pool: scenario #12 (graceful rolling restart) needs
// multiple Registry "pods" racing for the same session's advisory lock
// over time, never two pods with truly independent connection pools torn
// down independently mid-test -- see scenario12_rolling_restart_test.go's
// own doc comment for why a shared pool is the faithful shape here. The
// pods still hold their locks apart: each Registry dials its own lock
// connection (sessionactor/lockholder.go) from the pool's settings, so
// sharing the query pool never shares a lock backend.
type Harness struct {
	Pool     *pgxpool.Pool
	Timeouts platform.Timeouts
	Hub      *wshub.Hub

	Sessions  *narvipg.SessionStore
	Turns     *narvipg.TurnStore
	Sandboxes *narvipg.SandboxStore
	Timers    *narvipg.TimerStore
	// Events is this follow-up PR's own addition (scenario #7, "WS-drop ack
	// redelivery"), following the exact same pattern as Sessions/Turns/
	// Sandboxes/Timers above: a thin, real store wrapping this harness's
	// own shared pool, exposed so a scenario can assert directly on the
	// events table's own dedupe-on-(session_id, message_id) behavior
	// (TestEventStore_Create_DedupesOnSessionIDAndMessageID's own already-
	// proven primitive, internal/adapters/outbound/postgres) end to end
	// through the real inbound WS pipeline, not just at the store level.
	Events *narvipg.EventStore
}

// newHarness spins up one throwaway Postgres container, applies every
// embedded migration, and returns a ready Harness. t.Cleanup tears down
// the pool and the container. Mirrors internal/app/sessionactor's and
// internal/adapters/inbound/httpapi's own newTestPool convention exactly
// (same testcontainers image/options, same golang-migrate iofs source) --
// this package builds its own copy rather than importing either of
// theirs, matching that same established precedent of each DB-touching
// package/test-package owning a small copy of this helper rather than
// sharing one across package boundaries.
func newHarness(t *testing.T) *Harness {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("narvi_test"),
		tcpostgres.WithUsername("narvi"),
		tcpostgres.WithPassword("narvi"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
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

	// pool_max_conns is pinned explicitly (pgxpool's own default is
	// max(4, numCPU), a HOST-dependent value), so these scenarios run
	// against the same pool on every machine. It was pinned when every
	// hydrated sessionactor.Actor held one Acquire()-held pooled
	// connection (its advisory lock) for its whole lifetime: scenario #7
	// hydrates six actors across its subtests against ONE shared registry,
	// and on a 4-core CI runner the default pool of 4 was fully consumed by
	// the first four subtests' actors, deadlocking the fifth inside
	// CreateSession's own pool.Acquire. Actors hold no pool connection now
	// -- every lock lives on the Registry's own lock connection
	// (sessionactor/lockholder.go) -- so 20 is headroom, no longer a
	// requirement. Appended only to the pgxpool DSN, not the shared
	// connStr: pool_max_conns is a pgxpool-specific parameter the migrate
	// handle's database/sql driver (sql.Open above) rejects as an unknown
	// connection option.
	pool, err := narvipg.NewPool(ctx, connStr+"&pool_max_conns=20")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	return &Harness{
		Pool:      pool,
		Timeouts:  platform.DefaultTimeouts(),
		Hub:       wshub.NewHub(),
		Sessions:  narvipg.NewSessionStore(pool),
		Turns:     narvipg.NewTurnStore(pool),
		Sandboxes: narvipg.NewSandboxStore(pool),
		Timers:    narvipg.NewTimerStore(pool),
		Events:    narvipg.NewEventStore(pool),
	}
}

// NewRegistry constructs a fresh *sessionactor.Registry sharing this
// harness's Pool/Timeouts/Hub -- mirroring cmd/control-plane/main.go's own
// real wiring order (pool -> hub -> sessionactor.NewRegistry(...,
// broadcaster: hub, ...)) exactly, but with commander/provider/
// sourceControl left nil: scenario #12 (this PR) and a future #3 (slow
// boot) never exercise the spawn/dispatch/push-PR path, only hydration +
// the advisory lock + the timer/shutdown machinery -- the SAME "nil is
// fine" precedent internal/app/sessionactor's own resilience tests and
// internal/adapters/inbound/httpapi's own newTestRig already establish
// for exactly this reason.
//
// A future #5 (plain-SPAWN race variant) or #7 (WS-drop ack redelivery)
// scenario will NOT be able to reuse this nil wiring as-is: #5 needs a
// real/fake ports.SandboxProvider (mirroring
// dispatch_integration_test.go's own fakeSpawnProvider precedent) and #7
// needs a live wshub-backed ports.SandboxCommander plus the WS handler
// pipeline. Extending for either should add a sibling constructor here
// (or call sessionactor.NewRegistry directly, still reusing h.Pool/
// h.Timeouts/h.Hub) rather than parameterizing this method, so #12's own
// simplest case stays simple.
//
// Call this once per simulated "pod": each call is a fully independent
// Registry value with its own actors map and its own lifecycleCtx/cancel
// pair, so a scenario simulating a rolling restart (pod A shuts down,
// pod B picks the session back up) calls this twice, against the SAME
// underlying database, exactly like two real replicas of this binary
// would.
func (h *Harness) NewRegistry(ctx context.Context, t *testing.T) *sessionactor.Registry {
	t.Helper()

	r, err := sessionactor.NewRegistry(ctx, h.Pool, h.Timeouts, h.Hub, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("sessionactor.NewRegistry: %v", err)
	}
	return r
}

// NewRegistryWithCommander is this follow-up PR's own sibling constructor
// (scenario #7, "WS-drop ack redelivery") -- exactly the extension this
// file's own doc comment above anticipated ("#7 will need a sibling
// constructor... rather than reusing NewRegistry as-is"): NewRegistry's own
// existing signature and every one of its current callers (scenario #12,
// and any future #3) are completely unaffected by this addition. Mirrors
// NewRegistry's own call shape exactly, except commander is a real,
// caller-supplied ports.SandboxCommander (a real wshub.SandboxRegistry in
// production usage) instead of the hard-coded nil -- provider/sourceControl
// stay nil, since #7 never exercises spawn/push, only the sandbox-WS
// inbound event pipeline a live commander is needed for.
func (h *Harness) NewRegistryWithCommander(ctx context.Context, t *testing.T, commander ports.SandboxCommander) *sessionactor.Registry {
	t.Helper()

	r, err := sessionactor.NewRegistry(ctx, h.Pool, h.Timeouts, h.Hub, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("sessionactor.NewRegistry (with commander): %v", err)
	}
	return r
}

// waitUntil polls cond every 20ms until it reports true, or fails the test
// once timeout elapses -- shared by every scenario in this package that
// needs to observe the eventual effect of an actor's own asynchronous
// mailbox processing without coupling the test to internal timing. Mirrors
// internal/app/sessionactor/integration_helpers_test.go's own identical
// helper (and internal/adapters/inbound/wshub's own copy of it) exactly --
// this package builds its own copy rather than importing either of theirs,
// matching that same established "each test package owns a small copy"
// precedent this file's own doc comment already follows for newHarness.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// CreateSession inserts a minimal session row and returns its id --
// mirrors internal/app/sessionactor's own createTestSession helper
// exactly (the shape every resilience scenario in this package needs to
// start from).
func (h *Harness) CreateSession(ctx context.Context, t *testing.T) pgtype.UUID {
	t.Helper()

	created, err := h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
	})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	return created.ID
}
