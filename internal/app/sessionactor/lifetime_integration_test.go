//go:build integration

package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves technical plan §35.2's exit on the real dispatch path:
// a spawned sandbox's row carries a deadline within its provider's own, a
// restored one a fresh one, a resumed one the deadline it already had, and
// a session's kind decides the lifetime in one place.
//
// "The provider's own deadline" is modelled by fakeSpawnProvider's
// providerCreated hook: the database's clock_timestamp() at the moment the
// provider creates the sandbox, plus the lifetime the provider is assumed
// to give (§35.2's assumption: at least the kind's lifetime; here exactly
// it, the tightest case). The row's deadline must not be later.

// providerClock records, per provider call, the database's clock as the
// fake provider creates the sandbox.
type providerClock struct {
	t    *testing.T
	pool *pgxpool.Pool

	mu       sync.Mutex
	instants []time.Time
}

func newProviderClock(t *testing.T, pool *pgxpool.Pool) *providerClock {
	return &providerClock{t: t, pool: pool}
}

func (c *providerClock) record(ctx context.Context) {
	var now time.Time
	if err := c.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		c.t.Errorf("read the database's clock in the provider call: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instants = append(c.instants, now)
}

func (c *providerClock) last(t *testing.T) time.Time {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.instants) == 0 {
		t.Fatal("the provider recorded no creation instant")
	}
	return c.instants[len(c.instants)-1]
}

// lifetimeRow is the part of a sandbox row the deadline is about.
type lifetimeRow struct {
	gen                  int32
	createdAt            time.Time
	deadline             *time.Time
	seconds, deadlineGen *int32
}

func readLifetimeRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) lifetimeRow {
	t.Helper()
	var row lifetimeRow
	if err := pool.QueryRow(ctx, `SELECT gen, created_at, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen FROM sandboxes WHERE session_id = $1`, sessionID).
		Scan(&row.gen, &row.createdAt, &row.deadline, &row.seconds, &row.deadlineGen); err != nil {
		t.Fatalf("read the sandbox's lifetime columns: %v", err)
	}
	return row
}

func int32Value(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

// waitForConnecting waits until the provider's answer is recorded: the
// claim and the provider call are both behind it.
func waitForConnecting(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
	t.Helper()
	store := narvipg.NewSandboxStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		row, err := store.Get(ctx, sessionID)
		return err == nil && row.Status == sqlcgen.SandboxStatusConnecting
	})
}

// assertDeadlineWithinTheProvidersOwn fails unless row's deadline is
// recorded for its live gen with lifetime, and lies at or before the
// provider's own -- the provider's creation instant plus lifetime -- by no
// more than the claim's own latency.
func assertDeadlineWithinTheProvidersOwn(t *testing.T, row lifetimeRow, providerCreated time.Time, lifetime time.Duration) {
	t.Helper()
	if row.deadline == nil || row.deadlineGen == nil || *row.deadlineGen != row.gen {
		t.Fatalf("gen %d: lifetime_deadline_at %v recorded at gen %v; want a deadline for the live gen", row.gen, row.deadline, int32Value(row.deadlineGen))
	}
	if row.seconds == nil || time.Duration(*row.seconds)*time.Second != lifetime {
		t.Errorf("lifetime_seconds = %v, want %v", int32Value(row.seconds), lifetime)
	}
	providersOwn := providerCreated.Add(lifetime)
	if row.deadline.After(providersOwn) {
		t.Errorf("lifetime_deadline_at = %v, later than the provider's own %v (created %v plus %v)", *row.deadline, providersOwn, providerCreated, lifetime)
	}
	if gap := providersOwn.Sub(*row.deadline); gap > time.Minute {
		t.Errorf("lifetime_deadline_at = %v, %v before the provider's own %v: want it stamped at the claim, moments before the provider call", *row.deadline, gap, providersOwn)
	}
}

// TestSandboxLifetime_SpawnedSandboxCarriesADeadlineWithinTheProvidersOwn
// is §35.2's first exit on a fresh spawn: the claim stamps the deadline in
// the transaction that creates the gen, before the provider is called, so
// it is never later than the provider's own.
func TestSandboxLifetime_SpawnedSandboxCarriesADeadlineWithinTheProvidersOwn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "spawn me")

	clock := newProviderClock(t, pool)
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "lifetime-spawn-1"}, providerCreated: clock.record}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })
	waitForConnecting(ctx, t, pool, sessionID)

	row := readLifetimeRow(ctx, t, pool, sessionID)
	if row.gen != 1 {
		t.Fatalf("gen = %d, want 1", row.gen)
	}
	lifetime := platform.DefaultTimeouts().SandboxLifetimeFor(sandbox.LifetimeKindDefault)
	assertDeadlineWithinTheProvidersOwn(t, row, clock.last(t), lifetime)
	if got := row.deadline.Sub(row.createdAt); got != lifetime {
		t.Errorf("lifetime_deadline_at - created_at = %v, want exactly %v (one statement's now())", got, lifetime)
	}
}

// TestSandboxLifetime_RestoredSandboxCarriesAFreshDeadline is §35.2's
// second exit: a restore is a new provider object, so the gen it creates
// gets a fresh deadline at the claim plus the lifetime -- not the old
// gen's, and not created_at plus the lifetime, which a restore leaves at
// the first gen's value and which would already be past.
func TestSandboxLifetime_RestoredSandboxCarriesAFreshDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "restore me")
	seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-lifetime-1")

	// The first gen ran its whole life three hours ago: created then, its
	// deadline created_at plus two hours, an hour past.
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET
		    created_at = now() - interval '3 hours',
		    last_seen_at = now() - interval '3 hours',
		    lifetime_deadline_at = now() - interval '1 hour',
		    lifetime_seconds = 7200,
		    lifetime_deadline_gen = gen
		 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("back-date the stopped sandbox: %v", err)
	}
	old := readLifetimeRow(ctx, t, pool, sessionID)

	clock := newProviderClock(t, pool)
	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "lifetime-restore-1"}, providerCreated: clock.record}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount() == 1 })
	waitForConnecting(ctx, t, pool, sessionID)
	if got := provider.callCount(); got != 0 {
		t.Fatalf("CreateSandbox called %d times, want 0: this is the restore path", got)
	}

	row := readLifetimeRow(ctx, t, pool, sessionID)
	if row.gen != 2 {
		t.Fatalf("gen = %d, want 2", row.gen)
	}
	lifetime := platform.DefaultTimeouts().SandboxLifetimeFor(sandbox.LifetimeKindDefault)
	assertDeadlineWithinTheProvidersOwn(t, row, clock.last(t), lifetime)
	if !row.deadline.After(*old.deadline) {
		t.Errorf("lifetime_deadline_at = %v, want later than the old gen's %v", *row.deadline, *old.deadline)
	}
	if !row.createdAt.Equal(old.createdAt) {
		t.Errorf("created_at = %v, want unchanged %v (a restore never re-stamps it)", row.createdAt, old.createdAt)
	}
	if fromCreatedAt := row.createdAt.Add(lifetime); !row.deadline.After(fromCreatedAt) {
		t.Errorf("lifetime_deadline_at = %v, no later than created_at plus the lifetime %v: want the claim's own now()", *row.deadline, fromCreatedAt)
	}
}

// TestSandboxLifetime_ResumeCarriesTheDeadline is the resume half of
// §35.2: a resume claims the same provider object, never younger than it
// was, so the new gen keeps the deadline the previous gen had -- a fresh
// one would be later than the provider's own.
func TestSandboxLifetime_ResumeCarriesTheDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "resume me")

	store := narvipg.NewSandboxStore(pool)
	tokenHash := "token-hash-lifetime-resume"
	if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: int32Ptr(7200),
	}); err != nil {
		t.Fatalf("seed the first gen: %v", err)
	}
	// It has run for an hour and a half: thirty minutes of its life left.
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped', provider_id = 'lifetime-resume-1',
		    lifetime_deadline_at = now() + interval '30 minutes'
		 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("stop the first gen: %v", err)
	}
	old := readLifetimeRow(ctx, t, pool, sessionID)

	provider := &fakeSpawnProvider{resumeSupported: true}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.resumeCallCount() == 1 })
	waitForConnecting(ctx, t, pool, sessionID)

	row := readLifetimeRow(ctx, t, pool, sessionID)
	if row.gen != 2 {
		t.Fatalf("gen = %d, want 2", row.gen)
	}
	if row.deadline == nil || !row.deadline.Equal(*old.deadline) {
		t.Errorf("lifetime_deadline_at = %v, want the resumed object's own %v, carried", row.deadline, *old.deadline)
	}
	if row.deadlineGen == nil || *row.deadlineGen != row.gen || int32Value(row.seconds) != int32Value(old.seconds) {
		t.Errorf("lifetime_seconds %v at gen %v; want %v at the live gen %d", int32Value(row.seconds), int32Value(row.deadlineGen), int32Value(old.seconds), row.gen)
	}
}

// TestSandboxLifetime_ReviewSessionTakesItsKindsLifetime is §35.2's
// "per-session-type lifetime in one place": a pull request's review
// session is stamped with ReviewSandboxLifetime and every other session
// with SandboxLifetime, both read through SandboxLifetimeFor, on a spawn
// and on a restore, and the lifetime is read back from the row.
func TestSandboxLifetime_ReviewSessionTakesItsKindsLifetime(t *testing.T) {
	const reviewLifetime = 100 * time.Minute
	tests := []struct {
		name    string
		review  bool
		restore bool
		want    time.Duration
	}{
		{"a review session's spawn", true, false, reviewLifetime},
		{"a review session's restore", true, true, reviewLifetime},
		{"any other session's spawn", false, false, 2 * time.Hour},
		{"any other session's restore", false, true, 2 * time.Hour},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "lifetime by kind")
			if tc.review {
				prSessions := narvipg.NewGitHubPRSessionStore(pool)
				repo := "narvi-test/lifetime-kind"
				prNumber := int32(500 + i)
				if err := prSessions.EnsureRow(ctx, repo, prNumber); err != nil {
					t.Fatalf("ensure github_pr_sessions row: %v", err)
				}
				if err := prSessions.SetSessionID(ctx, repo, prNumber, sessionID); err != nil {
					t.Fatalf("set github_pr_sessions session id: %v", err)
				}
			}
			if tc.restore {
				seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-lifetime-kind")
			}

			timeouts := platform.DefaultTimeouts()
			timeouts.ReviewSandboxLifetime = reviewLifetime
			if err := timeouts.Validate(); err != nil {
				t.Fatalf("the test's timeouts are invalid: %v", err)
			}
			clock := newProviderClock(t, pool)
			provider := &fakeSpawnProvider{
				nextRef:         ports.SandboxRef{ProviderID: "lifetime-kind"},
				nextRestoreRef:  ports.SandboxRef{ProviderID: "lifetime-kind"},
				providerCreated: clock.record,
			}
			r, err := NewRegistry(ctx, pool, timeouts, nil, nil, provider, "http://localhost:8080", nil, nil, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount()+provider.restoreCallCount() == 1 })
			waitForConnecting(ctx, t, pool, sessionID)
			if restored := provider.restoreCallCount() == 1; restored != tc.restore {
				t.Fatalf("restored = %v, want %v", restored, tc.restore)
			}

			assertDeadlineWithinTheProvidersOwn(t, readLifetimeRow(ctx, t, pool, sessionID), clock.last(t), tc.want)
		})
	}
}

// TestSandboxLifetime_ARolledBackClaimLogsNoDeadline pins where the
// "claimed with a lifetime deadline" line is written: after the claim's
// transaction commits, never inside it. Here the claim's upsert runs and
// stamps a deadline, then assembleSessionConfig refuses the public base
// URL's scheme, a step after the upsert, and the whole claim rolls back --
// no sandbox row, no provider call. The line must not name that gen and
// deadline, which were never persisted.
func TestSandboxLifetime_ARolledBackClaimLogsNoDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "never claimed")

	logs := captureDefaultLoggerJSONSync(t)
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "lifetime-rolled-back"}}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, provider, "ftp://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	failed := waitForLogEntry(t, logs, 5*time.Second, "sessionactor: command handling failed")
	if msg, _ := failed["error"].(string); !strings.Contains(msg, "unrecognized scheme") {
		t.Fatalf("the claim failed with %q, want the public base URL's scheme refused after the upsert", msg)
	}
	if _, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("sandbox row after the failed claim: err = %v, want none (the claim rolled back)", err)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("CreateSandbox called %d times, want 0", got)
	}
	// The actor writes its log lines in order, so a line written before
	// the failure would already be in the buffer.
	if n := countLogLines(t, logs, lifetimeClaimedLogMessage); n != 0 {
		t.Errorf("a rolled-back claim was logged as claimed %d times:\n%s", n, logs.String())
	}
}

// TestSandboxLifetime_ACommittedClaimLogsItsDeadline is the other half: a
// spawn and a restore that commit each log their claim once, naming the
// gen, the kind, the lifetime and the deadline the row holds.
func TestSandboxLifetime_ACommittedClaimLogsItsDeadline(t *testing.T) {
	tests := []struct {
		name    string
		restore bool
		claim   string
		gen     int32
	}{
		{"a spawn", false, "spawn", 1},
		{"a restore", true, "restore", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "claimed")
			if tc.restore {
				seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-lifetime-log")
			}

			logs := captureDefaultLoggerJSONSync(t)
			provider := &fakeSpawnProvider{
				nextRef:        ports.SandboxRef{ProviderID: "lifetime-log"},
				nextRestoreRef: ports.SandboxRef{ProviderID: "lifetime-log"},
			}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitForConnecting(ctx, t, pool, sessionID)

			line := waitForLogEntry(t, logs, 5*time.Second, lifetimeClaimedLogMessage)
			row := readLifetimeRow(ctx, t, pool, sessionID)
			deadline, err := time.Parse(time.RFC3339Nano, fmt.Sprint(line["lifetime_deadline_at"]))
			if err != nil || row.deadline == nil || !deadline.Equal(*row.deadline) {
				t.Errorf("logged lifetime_deadline_at %v (%v), want the row's %v", line["lifetime_deadline_at"], err, row.deadline)
			}
			if line["claim"] != tc.claim || line["gen"] != float64(tc.gen) || row.gen != tc.gen ||
				line["lifetime_kind"] != string(sandbox.LifetimeKindDefault) || line["lifetime_seconds"] != float64(7200) ||
				line["session_id"] != sessionID.String() {
				t.Errorf("logged %v, want claim %q, gen %d, kind default, 7200 seconds, for session %s", line, tc.claim, tc.gen, sessionID.String())
			}
			if n := countLogLines(t, logs, lifetimeClaimedLogMessage); n != 1 {
				t.Errorf("%d claimed lines, want 1", n)
			}
		})
	}
}
