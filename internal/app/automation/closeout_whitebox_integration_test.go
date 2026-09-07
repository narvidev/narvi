//go:build integration

// This file needs white-box (package automation, not automation_test)
// access to closeInvocation/applyFailureStrike specifically to stress
// §3.5's own literal CAS requirement ("At-most-one failure strike per
// invocation via CAS") directly and precisely, under genuine concurrency
// -- mirroring internal/app/imagebuild's own builder_whitebox_integration_
// test.go precedent (unexported access needed for a test that the
// production, black-box call pattern alone cannot reach with enough
// precision). See this package's own sharedpool_integration_test.go top
// doc comment for why the shared container/pool lives there instead of
// here.
package automation

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// newWhiteboxEngine builds a real Engine against the shared pool, exactly
// like automation_integration_test.go's own newFixture -- duplicated here
// (rather than reused) because that helper lives in package
// automation_test, which this white-box file cannot import (Go disallows
// the reverse), mirroring builder_whitebox_integration_test.go's own
// identical "small, deliberate duplication" precedent in app/imagebuild.
func newWhiteboxEngine(t *testing.T) (*Engine, *narvipg.AutomationStore, *narvipg.AutomationInvocationStore) {
	t.Helper()
	pool := IntegrationTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	runs := narvipg.NewAutomationRunStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	// prSessions (§31.4): threaded through like every other
	// CreateSessionOnTx-reaching caller now requires -- never actually
	// dereferenced by this file's own tests, which drive applyFailureStrike/
	// closeInvocation directly and never call PumpOnce/fan-out.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	engine := NewEngine(automations, invocations, runs, sessions, turns, environments, auditLog, pool, registry, platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions)
	return engine, automations, invocations
}

// TestApplyFailureStrike_ConcurrentAttemptsRecordExactlyOneStrike is the
// most surgical proof of §3.5's own literal requirement: "At-most-one
// failure strike per invocation via CAS (UPDATE ... WHERE
// failure_counted_at IS NULL)". It calls applyFailureStrike -- the ONE
// function that owns that CAS -- directly, many times concurrently, for
// the SAME invocation, bypassing closeInvocation's own outer status CAS
// entirely (which would otherwise let at most one goroutine ever reach
// applyFailureStrike at all, making this specific guard untested in
// isolation). This is exactly the scenario the CAS exists to survive: a
// crash between closeInvocation's own two steps (or two independent pods
// racing to re-run the same close-out) leaving MULTIPLE callers convinced
// they are the one that should apply this invocation's failure
// consequence.
func TestApplyFailureStrike_ConcurrentAttemptsRecordExactlyOneStrike(t *testing.T) {
	engine, automations, invocations := newWhiteboxEngine(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	reposJSON, err := json.Marshal([]domainautomation.Target{{Name: "repo", URL: "https://github.com/acme/repo"}})
	if err != nil {
		t.Fatalf("marshal targets: %v", err)
	}

	autoRow, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "cas strike test", Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeManual, TriggerConfig: []byte("{}"), EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}

	invRow, err := invocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
		AutomationID: autoRow.ID, Targets: reposJSON, TotalRuns: 1,
	})
	if err != nil {
		t.Fatalf("create invocation: %v", err)
	}

	const concurrency = 50
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			engine.applyFailureStrike(ctx, logger, autoRow.ID, invRow.ID)
		}()
	}
	wg.Wait()

	gotAuto, err := automations.Get(ctx, autoRow.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d, want exactly 1 -- the CAS must prevent %d concurrent callers from each counting their own strike",
			gotAuto.ConsecutiveFailures, concurrency)
	}

	gotInv, err := invocations.Get(ctx, invRow.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if !gotInv.FailureCountedAt.Valid {
		t.Fatalf("failure_counted_at not set after applyFailureStrike")
	}
}

// TestCloseInvocation_ConcurrentClosesOnlyOneWinnerCascades stresses the
// OUTER CAS (automation_invocations.status, guarded by "AND status =
// 'pending'") the SAME way -- many concurrent callers all trying to close
// the SAME still-pending invocation as failed -- and confirms only one
// ever wins through to applying the failure-strike consequence, exactly
// mirroring the invocation-level race two ticks (a reconcile pump and a
// sweep, or two pods) could genuinely produce against the SAME
// invocation's last remaining run.
func TestCloseInvocation_ConcurrentClosesOnlyOneWinnerCascades(t *testing.T) {
	engine, automations, invocations := newWhiteboxEngine(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	reposJSON, err := json.Marshal([]domainautomation.Target{{Name: "repo", URL: "https://github.com/acme/repo"}})
	if err != nil {
		t.Fatalf("marshal targets: %v", err)
	}

	autoRow, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "cas close test", Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeManual, TriggerConfig: []byte("{}"), EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}

	invRow, err := invocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
		AutomationID: autoRow.ID, Targets: reposJSON, TotalRuns: 1,
	})
	if err != nil {
		t.Fatalf("create invocation: %v", err)
	}

	const concurrency = 50
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			engine.closeInvocation(ctx, logger, invRow.ID, autoRow.ID, true)
		}()
	}
	wg.Wait()

	gotAuto, err := automations.Get(ctx, autoRow.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d, want exactly 1 -- only one of %d concurrent closers must ever win",
			gotAuto.ConsecutiveFailures, concurrency)
	}

	gotInv, err := invocations.Get(ctx, invRow.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed", gotInv.Status)
	}
	if !gotInv.FailureCountedAt.Valid {
		t.Fatalf("failure_counted_at not set")
	}
}

// TestApplyFailureStrike_FailureBetweenGuardAndStrikeRollsBackBothAtomically
// is fix #2's own regression test. Before this fix, MarkFailureCounted
// (the failure_counted_at CAS guard) committed standalone, on the bare
// pool, BEFORE the separate strike-accounting transaction (pool.Acquire ->
// conn.Begin -> LockForUpdate -> ApplyFailureStrike -> tx.Commit) ever
// opened -- so ANY failure in that later transaction left the guard
// permanently set with NO strike ever recorded, and nothing anywhere reads
// failure_counted_at to retry or reconcile it.
//
// A temporary CHECK constraint on automations.consecutive_failures forces
// ApplyFailureStrike's own UPDATE (which would set consecutive_failures to
// exactly 1 for this automation's own first-ever strike,
// domainautomation.EvaluateFailureStrike's own verdict for
// current=0/failed=true) to fail deterministically, without needing a
// fault-injection seam. This proves BOTH halves of the fix: (1) the forced
// failure leaves failure_counted_at unset (the guard and its consequence
// rolled back TOGETHER, atomically, exactly like the rest of this
// transaction), and (2) a subsequent retry -- once the forced failure is
// lifted -- still wins the (still-unset) guard and records the strike for
// real, proving the earlier failed attempt did not PERMANENTLY lose it.
func TestApplyFailureStrike_FailureBetweenGuardAndStrikeRollsBackBothAtomically(t *testing.T) {
	engine, automations, invocations := newWhiteboxEngine(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	reposJSON, err := json.Marshal([]domainautomation.Target{{Name: "repo", URL: "https://github.com/acme/repo"}})
	if err != nil {
		t.Fatalf("marshal targets: %v", err)
	}

	autoRow, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "atomic strike test", Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeManual, TriggerConfig: []byte("{}"), EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}

	invRow, err := invocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
		AutomationID: autoRow.ID, Targets: reposJSON, TotalRuns: 1,
	})
	if err != nil {
		t.Fatalf("create invocation: %v", err)
	}

	if _, err := engine.pool.Exec(ctx, "ALTER TABLE automations ADD CONSTRAINT test_reject_first_strike CHECK (consecutive_failures <> 1)"); err != nil {
		t.Fatalf("add test constraint: %v", err)
	}
	constraintDropped := false
	dropConstraint := func() {
		if constraintDropped {
			return
		}
		if _, err := engine.pool.Exec(context.Background(), "ALTER TABLE automations DROP CONSTRAINT IF EXISTS test_reject_first_strike"); err != nil {
			t.Errorf("drop test constraint: %v", err)
		}
		constraintDropped = true
	}
	t.Cleanup(dropConstraint)

	// First attempt: ApplyFailureStrike's own UPDATE is forced to fail --
	// the whole transaction, MarkFailureCounted included, must roll back.
	engine.applyFailureStrike(ctx, logger, autoRow.ID, invRow.ID)

	gotAuto, err := automations.Get(ctx, autoRow.ID)
	if err != nil {
		t.Fatalf("get automation (after forced failure): %v", err)
	}
	if gotAuto.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive_failures = %d, want 0 -- the forced failure must roll back the whole transaction", gotAuto.ConsecutiveFailures)
	}

	gotInv, err := invocations.Get(ctx, invRow.ID)
	if err != nil {
		t.Fatalf("get invocation (after forced failure): %v", err)
	}
	if gotInv.FailureCountedAt.Valid {
		t.Fatalf("failure_counted_at set even though the strike transaction failed -- the guard and its consequence must commit or roll back together")
	}

	// Lift the forced failure and retry -- the earlier attempt must NOT
	// have permanently lost this invocation's own strike.
	dropConstraint()

	engine.applyFailureStrike(ctx, logger, autoRow.ID, invRow.ID)

	gotAuto, err = automations.Get(ctx, autoRow.ID)
	if err != nil {
		t.Fatalf("get automation (after retry): %v", err)
	}
	if gotAuto.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d, want 1 after retry -- the earlier failed attempt must not have permanently lost this invocation's own strike", gotAuto.ConsecutiveFailures)
	}

	gotInv, err = invocations.Get(ctx, invRow.ID)
	if err != nil {
		t.Fatalf("get invocation (after retry): %v", err)
	}
	if !gotInv.FailureCountedAt.Valid {
		t.Fatalf("failure_counted_at still not set after a successful retry")
	}
}
