//go:build integration

// This file proves §10's own per-channel refusal contract (§10 Phase
// 6, §32) for automation fan-out specifically: "already does the right
// thing unmodified" -- createRunAndSession's own existing cerr != nil
// branch (fanout.go) already routes ANY httpapi.CreateSessionOnTx
// refusal, rollout or otherwise, into createFailedRun's terminal
// RunStatusFailed row with no linked session. This file proves that
// property actually holds for a rollout refusal specifically, mirroring
// automation_integration_test.go's own TestPumpOnce_
// RunsCreateFailureRecordsFailedRunNotStrandedInvocation exactly, one
// underlying cause further (a rollout refusal instead of a DB constraint
// violation).
package automation_test

import (
	"context"
	"testing"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newFixtureWithRolloutMode mirrors newFixture exactly, with rolloutMode
// set on the Engine -- newFixture itself stays on rollout.ModeOpen
// (unchanged), proven not to change any of its own callers' behavior by
// every pre-existing test in this package continuing to pass unmodified.
func newFixtureWithRolloutMode(t *testing.T, mode platform.RolloutMode) *testFixture {
	t.Helper()
	pool := automation.IntegrationTestPool(t)
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
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	engine := automation.NewEngine(automations, invocations, runs, sessions, turns, environments, auditLog, pool, registry, platform.DefaultTimeouts(), false, mode, repoSettings, prSessions)

	return &testFixture{
		pool: pool, automations: automations, invocations: invocations, runs: runs,
		sessions: sessions, turns: turns, environments: environments, engine: engine,
	}
}

// TestPumpOnce_RolloutRefusal_RecordsFailedRunNotStrandedInvocation is the
// confirmation test for §32's own "automation fan-out already does the
// right thing unmodified" claim: rollout mode is armed to cohort and
// createAutomation's own targets (repo-0, repo-1, ...) are NEVER
// enrolled, so createRunAndSession's own httpapi.CreateSessionOnTx call
// refuses with CreateSessionError.RolloutRefusal == true for every
// target. Proves: every target still gets exactly one recorded
// RunStatusFailed row (never silently dropped, never left 'starting'
// forever) with no linked session, and the invocation itself closes
// Failed rather than being stranded pending.
func TestPumpOnce_RolloutRefusal_RecordsFailedRunNotStrandedInvocation(t *testing.T) {
	f := newFixtureWithRolloutMode(t, platform.RolloutModeCohort)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "rollout refusal fan-out test", 2)
	inv := f.createInvocation(t, auto.ID, targets)

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 2 {
		t.Fatalf("got %d automation_runs rows, want 2 -- every target must still get exactly one recorded run even when refused by the rollout gate", len(runs))
	}
	for _, run := range runs {
		if run.Status != sqlcgen.AutomationRunStatusFailed {
			t.Errorf("run %s status = %s, want failed (recorded via the createFailedRun fallback)", run.ID.String(), run.Status)
		}
		if run.SessionID.Valid {
			t.Errorf("run %s has a linked session, want none -- a refused CreateSessionOnTx call must never leave a session behind", run.ID.String())
		}
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed -- it must NOT be stranded pending forever", gotInv.Status)
	}
}

// TestPumpOnce_RolloutGate_EnrolledReposStillRun is the refusal test's own
// positive control: the IDENTICAL setup, except every target repo IS
// enrolled -- proves cohort mode is a real, bidirectional gate here too.
func TestPumpOnce_RolloutGate_EnrolledReposStillRun(t *testing.T) {
	f := newFixtureWithRolloutMode(t, platform.RolloutModeCohort)
	ctx := context.Background()

	// §31.4: createAutomation below already makes every target repo known
	// (github_pr_sessions, via EnsureRow) -- CreateSessionOnTx's own
	// entitlement gate runs BEFORE the rollout gate this test exercises,
	// unconditionally, so without that this positive control would be
	// refused by entitlement instead, never even reaching the rollout
	// decision it means to prove.
	auto, targets := f.createAutomation(t, "rollout enrolled fan-out test", 2)
	repoSettings := narvipg.NewRepoSettingsStore(f.pool)
	for _, target := range targets {
		if _, err := repoSettings.UpsertSessionsEnabled(ctx, "acme/"+target.Name, true); err != nil {
			t.Fatalf("seed enrollment for %s: %v", target.Name, err)
		}
	}
	inv := f.createInvocation(t, auto.ID, targets)

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 2 {
		t.Fatalf("got %d automation_runs rows, want 2", len(runs))
	}
	for _, run := range runs {
		if run.Status != sqlcgen.AutomationRunStatusStarting {
			t.Errorf("run %s status = %s, want starting -- an enrolled repo must not be refused", run.ID.String(), run.Status)
		}
		if !run.SessionID.Valid {
			t.Errorf("run %s has no linked session, want a real one", run.ID.String())
		}
	}
}
