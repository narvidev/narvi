//go:build integration

// Integration tests proving app/sessionactor's locking/fencing/pump
// mechanics against a REAL Postgres instance (§9.1, §2's own
// verification requirement) -- gated behind the "integration" build tag,
// matching internal/adapters/outbound/postgres/postgres_integration_test.
// go's own conventions exactly (testcontainers Postgres, embedded
// migrations via golang-migrate's iofs source driver, a real *pgxpool.
// Pool). Run via `make test-integration`. newTestPool (below) no longer
// starts its OWN container per call -- see sharedpool_integration_test.
// go's own top doc comment for why this package's own uniquely large test
// count (~126 functions, second only to internal/adapters/inbound/
// httpapi's ~170 -- see that package's own perf/httpapi-integration-
// container-reuse PR for the original precedent this mirrors) made that
// worth changing, and for the container/pool this now shares with every
// other test in this package (except resilience_killpod_integration_
// test.go's own newTestPoolPair, which deliberately keeps its own
// dedicated, killable container -- see that file's own doc comment).
//
// This file (and its sibling _test.go files under the same build tag) use
// `package sessionactor`, not `sessionactor_test`: TestActorTransact_
// StaleEpochEvictsSelf specifically needs white-box access to Actor's own
// unexported transact method to assert ErrStaleEpoch directly, rather
// than inferring it indirectly through logs.
package sessionactor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go), not freshly per test/container as this function
// used to do itself. Kept as a thin wrapper under its own original
// name/signature so every existing call site in this package's own
// *_integration_test.go files keeps compiling unchanged. See sharedpool_
// integration_test.go's own top doc comment for the full container-reuse
// story: why per-test containers were never a deliberate correctness
// requirement here, why sharing one is safe against this package's own
// async Actor background work (this package IS the actor registry's own
// home package, so this mattered more here than almost anywhere else),
// and why each test still gets a byte-for-byte-empty (plus restored seed
// data), freshly-migrated-equivalent database via a t.Cleanup-registered
// reset rather than a real fresh container.
//
// The pool this now returns is sized MaxConns=20 (was previously
// "pool_max_conns=20" appended to a dedicated per-test connection
// string) -- chosen when each live Actor pinned a pool connection, which
// none does any more; see TestMain's own comment (sharedpool_
// integration_test.go) for why the size stays.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// createTestSession inserts a minimal session row and returns its id.
func createTestSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()

	created, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
	})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	return created.ID
}

// waitUntil polls cond every 20ms until it reports true, or fails the
// test once timeout elapses. Used throughout these integration tests to
// observe the eventual effect of an actor's own asynchronous mailbox
// processing (Send only enqueues; the actual write/eviction happens on
// the actor's own goroutine) without coupling the test to internal
// timing.
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
