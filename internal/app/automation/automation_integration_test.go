//go:build integration

package automation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// testFixture bundles every store/engine a test in this file needs, built
// once per test against the shared pool (automation.IntegrationTestPool).
type testFixture struct {
	pool         *pgxpool.Pool
	automations  *narvipg.AutomationStore
	invocations  *narvipg.AutomationInvocationStore
	runs         *narvipg.AutomationRunStore
	sessions     *narvipg.SessionStore
	turns        *narvipg.TurnStore
	environments *narvipg.EnvironmentStore
	engine       *automation.Engine
}

func newFixture(t *testing.T) *testFixture {
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

	// nil hub/commander/sandboxProvider/sourceControl -- mirrors
	// internal/app/outboxworker's own sentinelautofix_integration_test.go
	// precedent exactly: TriggerDispatch's own GetOrSpawn call degrades to
	// a logged, non-fatal warning with no real SandboxProvider, which is
	// exactly what these tests want (a run's own session/turn is created
	// for real; nothing here needs a real sandbox to ever actually spawn).
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	// prSessions (§31.4): threaded through like every other
	// CreateSessionOnTx-reaching caller now requires -- createAutomation
	// below (this file's own shared target-repo fixture) always makes its
	// generated targets known via EnsureRow, so this Engine's own
	// entitlement gate admits them exactly like it would a real,
	// previously-mentioned repo.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	engine := automation.NewEngine(automations, invocations, runs, sessions, turns, environments, auditLog, pool, registry, platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions)

	return &testFixture{
		pool: pool, automations: automations, invocations: invocations, runs: runs,
		sessions: sessions, turns: turns, environments: environments, engine: engine,
	}
}

// createAutomation inserts a brand-new automations row with a real prompt
// (so every run it fans out actually dispatches a turn) and numTargets
// distinct targets.
func (f *testFixture) createAutomation(t *testing.T, name string, numTargets int) (sqlcgen.Automation, []domainautomation.Target) {
	t.Helper()
	ctx := context.Background()

	targets := make([]domainautomation.Target, numTargets)
	prSessions := narvipg.NewGitHubPRSessionStore(f.pool)
	for i := range targets {
		targets[i] = domainautomation.Target{
			Name: fmt.Sprintf("repo-%d", i),
			URL:  fmt.Sprintf("https://github.com/acme/repo-%d", i),
		}
		// §31.4: CreateSessionOnTx's own entitlement gate now requires
		// every named repo to be known (github_pr_sessions) regardless of
		// rollout mode -- every test in this file that expects fan-out to
		// actually create a session depends on this fixture's own targets
		// being admitted, not merely well-formed, so this is seeded HERE,
		// once, rather than by each test individually (mirrors this
		// function's own existing "one shared target-repo fixture" role).
		if err := prSessions.EnsureRow(ctx, fmt.Sprintf("acme/repo-%d", i), 1); err != nil {
			t.Fatalf("seed github_pr_sessions entitlement for repo-%d: %v", i, err)
		}
	}
	reposJSON, err := json.Marshal(targets)
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}

	prompt := "do the thing"
	row, err := f.automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: name, Prompt: &prompt, Repos: reposJSON, CreatedBy: pgtype.UUID{},
		// TriggerType/TriggerConfig/EnvVars ("automations: triggers
		// & extras", §8.4) are all NOT NULL columns as of migrations/
		// 000055_automations_triggers_and_extras.up.sql -- 'manual'/'{}'/'[]'
		// here mean exactly what they mean everywhere else in this Step:
		// this automation fires only via this file's own direct
		// CreateInvocation calls, with no trigger config or env vars of its
		// own. Every OTHER new column (webhook_token_hash, sandbox_*) is
		// left at its Go zero value, which is also its own column's exact
		// nullable/false default.
		TriggerType:   sqlcgen.AutomationTriggerTypeManual,
		TriggerConfig: []byte("{}"),
		EnvVars:       []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}
	return row, targets
}

// createInvocation calls automation.CreateInvocation and fails the test on
// error.
func (f *testFixture) createInvocation(t *testing.T, automationID pgtype.UUID, targets []domainautomation.Target) sqlcgen.AutomationInvocation {
	t.Helper()
	inv, err := automation.CreateInvocation(context.Background(), f.invocations, automationID, targets)
	if err != nil {
		t.Fatalf("create invocation: %v", err)
	}
	return inv
}

// listRunsForInvocation is a small test-only helper (there is no store
// method for this -- production code never needs it, only assertions do).
func (f *testFixture) listRunsForInvocation(t *testing.T, invocationID pgtype.UUID) []sqlcgen.AutomationRun {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "SELECT id FROM automation_runs WHERE invocation_id = $1 ORDER BY created_at", invocationID)
	if err != nil {
		t.Fatalf("list runs for invocation: %v", err)
	}
	defer rows.Close()

	var out []sqlcgen.AutomationRun
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan run id: %v", err)
		}
		run, err := f.runs.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("get run %s: %v", id.String(), err)
		}
		out = append(out, run)
	}
	return out
}

// setTurnStatus directly transitions the (single) turn belonging to run's
// own linked session -- a test-only shortcut standing in for what the
// real sandbox-agent/sessionactor pipeline would otherwise do over many
// wire events; this package's own ReconcileOnce is what's actually under
// test, not that pipeline.
func (f *testFixture) setTurnStatus(t *testing.T, run sqlcgen.AutomationRun, status sqlcgen.TurnStatus) {
	t.Helper()
	ctx := context.Background()
	turns, err := f.turns.ListForSession(ctx, run.SessionID)
	if err != nil {
		t.Fatalf("list turns for session: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected exactly one turn for run's session, got %d", len(turns))
	}
	if _, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turns[0].ID, Status: status}); err != nil {
		t.Fatalf("update turn status: %v", err)
	}
}

// ageRun back-dates run's own started_at/running_at columns directly via
// SQL -- the standard "time-travel" idiom for testing a threshold-based
// sweep without a real multi-minute wait (mirrors this codebase's own
// established pattern of manipulating a timestamp column directly in a
// test rather than injecting a fake clock into impure app-layer code,
// which platform.Timeouts-consuming loops like this one are not required
// to accept per §11 -- only /internal/domain is).
func (f *testFixture) ageRun(t *testing.T, runID pgtype.UUID, startedAt, runningAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	if startedAt != nil {
		if _, err := f.pool.Exec(ctx, "UPDATE automation_runs SET started_at = $2 WHERE id = $1", runID, *startedAt); err != nil {
			t.Fatalf("age run started_at: %v", err)
		}
	}
	if runningAt != nil {
		if _, err := f.pool.Exec(ctx, "UPDATE automation_runs SET running_at = $2 WHERE id = $1", runID, *runningAt); err != nil {
			t.Fatalf("age run running_at: %v", err)
		}
	}
}

func TestPumpOnce_FansOutOneRunPerTarget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "nightly audit", 3)
	inv := f.createInvocation(t, auto.ID, targets)

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3 (one per target)", len(runs))
	}
	for _, run := range runs {
		if run.Status != sqlcgen.AutomationRunStatusStarting {
			t.Errorf("run %s status = %s, want starting", run.ID.String(), run.Status)
		}
		if !run.SessionID.Valid {
			t.Errorf("run %s has no linked session", run.ID.String())
		}
	}

	invRow, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if !invRow.FannedOutAt.Valid {
		t.Errorf("invocation fanned_out_at not set after PumpOnce")
	}
	if invRow.Status != sqlcgen.AutomationInvocationStatusPending {
		t.Errorf("invocation status = %s, want pending (runs not yet terminal)", invRow.Status)
	}
}

func TestPumpOnce_RespectsMaxFanOutOfTen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "max fan-out", domainautomation.MaxFanOutTargets)
	inv := f.createInvocation(t, auto.ID, targets)

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != domainautomation.MaxFanOutTargets {
		t.Fatalf("got %d runs, want %d", len(runs), domainautomation.MaxFanOutTargets)
	}
}

func TestCreateInvocation_RejectsMoreThanMaxFanOut(t *testing.T) {
	f := newFixture(t)
	auto, targets := f.createAutomation(t, "too many targets", domainautomation.MaxFanOutTargets+1)

	if _, err := automation.CreateInvocation(context.Background(), f.invocations, auto.ID, targets); err == nil {
		t.Fatal("expected an error for more than MaxFanOutTargets, got nil")
	}

	rows, err := f.pool.Query(context.Background(), "SELECT count(*) FROM automation_invocations WHERE automation_id = $1", auto.ID)
	if err != nil {
		t.Fatalf("count invocations: %v", err)
	}
	defer rows.Close()
	rows.Next()
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero invocations persisted for a rejected request, got %d", count)
	}
}

// driveToOneRun creates a fresh automation+invocation with exactly one
// target, fans it out, and returns the resulting single run -- the shared
// setup every reconcile/auto-pause test in this file starts from.
func (f *testFixture) driveToOneRun(t *testing.T, name string) (sqlcgen.Automation, sqlcgen.AutomationInvocation, sqlcgen.AutomationRun) {
	t.Helper()
	ctx := context.Background()

	auto, targets := f.createAutomation(t, name, 1)
	inv := f.createInvocation(t, auto.ID, targets)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	return auto, inv, runs[0]
}

func TestReconcileOnce_PromotesRunWhenTurnStartsProcessing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, _, run := f.driveToOneRun(t, "promote test")

	f.setTurnStatus(t, run, sqlcgen.TurnStatusProcessing)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	got, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != sqlcgen.AutomationRunStatusRunning {
		t.Fatalf("run status = %s, want running", got.Status)
	}
	if !got.RunningAt.Valid {
		t.Fatalf("running_at not set after promotion")
	}
}

func TestReconcileOnce_TerminalizesSucceededRunAndClosesInvocationSucceeded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, inv, run := f.driveToOneRun(t, "succeed test")

	f.setTurnStatus(t, run, sqlcgen.TurnStatusProcessing)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce (promote): %v", err)
	}
	f.setTurnStatus(t, run, sqlcgen.TurnStatusCompleted)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce (terminalize): %v", err)
	}

	gotRun, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.Status != sqlcgen.AutomationRunStatusSucceeded {
		t.Fatalf("run status = %s, want succeeded", gotRun.Status)
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusSucceeded {
		t.Fatalf("invocation status = %s, want succeeded", gotInv.Status)
	}
	if !gotInv.ClosedAt.Valid {
		t.Fatalf("invocation closed_at not set")
	}
	if gotInv.FailureCountedAt.Valid {
		t.Fatalf("a succeeded invocation must never have failure_counted_at set")
	}

	gotAuto, err := f.automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive_failures = %d, want 0 after a success", gotAuto.ConsecutiveFailures)
	}
	if gotAuto.Status != sqlcgen.AutomationStatusActive {
		t.Fatalf("automation status = %s, want active", gotAuto.Status)
	}
}

func TestReconcileOnce_TerminalizesFailedRunAndRecordsOneStrike(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, inv, run := f.driveToOneRun(t, "fail test")

	f.setTurnStatus(t, run, sqlcgen.TurnStatusFailed)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	gotRun, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.Status != sqlcgen.AutomationRunStatusFailed {
		t.Fatalf("run status = %s, want failed", gotRun.Status)
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed", gotInv.Status)
	}
	if !gotInv.FailureCountedAt.Valid {
		t.Fatalf("failure_counted_at not set for a failed invocation")
	}

	gotAuto, err := f.automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d, want 1", gotAuto.ConsecutiveFailures)
	}
	if gotAuto.Status != sqlcgen.AutomationStatusActive {
		t.Fatalf("automation status = %s, want still active (only 1 of 3 strikes)", gotAuto.Status)
	}
}

// TestAutoPause_FiresAtExactlyThreeConsecutiveFailures is §3.5's own
// explicit "auto-pause after 3 consecutive failed invocations" -- driven
// end to end through the real Engine (PumpOnce fans out, ReconcileOnce
// reacts to each simulated turn outcome), asserting the boundary
// precisely: still active after 2, paused at exactly 3, and that an
// intervening SUCCESS resets the streak so 2 failures + 1 success + 2
// more failures never auto-pauses.
func TestAutoPause_FiresAtExactlyThreeConsecutiveFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "auto-pause test", 1)

	failOnce := func() {
		inv := f.createInvocation(t, auto.ID, targets)
		if err := f.engine.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		runs := f.listRunsForInvocation(t, inv.ID)
		if len(runs) != 1 {
			t.Fatalf("got %d runs, want 1", len(runs))
		}
		f.setTurnStatus(t, runs[0], sqlcgen.TurnStatusFailed)
		if err := f.engine.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}
	}
	succeedOnce := func() {
		inv := f.createInvocation(t, auto.ID, targets)
		if err := f.engine.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		runs := f.listRunsForInvocation(t, inv.ID)
		if len(runs) != 1 {
			t.Fatalf("got %d runs, want 1", len(runs))
		}
		f.setTurnStatus(t, runs[0], sqlcgen.TurnStatusCompleted)
		if err := f.engine.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}
	}
	assertStreak := func(wantConsecutive int32, wantStatus sqlcgen.AutomationStatus) {
		t.Helper()
		got, err := f.automations.Get(ctx, auto.ID)
		if err != nil {
			t.Fatalf("get automation: %v", err)
		}
		if got.ConsecutiveFailures != wantConsecutive {
			t.Fatalf("consecutive_failures = %d, want %d", got.ConsecutiveFailures, wantConsecutive)
		}
		if got.Status != wantStatus {
			t.Fatalf("status = %s, want %s", got.Status, wantStatus)
		}
	}

	failOnce()
	assertStreak(1, sqlcgen.AutomationStatusActive)

	failOnce()
	assertStreak(2, sqlcgen.AutomationStatusActive)

	succeedOnce()
	assertStreak(0, sqlcgen.AutomationStatusActive)

	failOnce()
	assertStreak(1, sqlcgen.AutomationStatusActive)
	failOnce()
	assertStreak(2, sqlcgen.AutomationStatusActive)
	failOnce()
	assertStreak(3, sqlcgen.AutomationStatusPaused)
}

func TestSweep_OrphanedStartingRunIsFailedAfterThreshold(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, inv, run := f.driveToOneRun(t, "starting sweep test")
	_ = auto

	old := time.Now().Add(-platform.DefaultTimeouts().AutomationRunStartingOrphanThreshold - time.Minute)
	f.ageRun(t, run.ID, &old, nil)

	if err := f.engine.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != sqlcgen.AutomationRunStatusFailed {
		t.Fatalf("run status = %s, want failed (swept as an orphan)", got.Status)
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed", gotInv.Status)
	}
}

func TestSweep_StartingRunJustUnderThresholdIsNotSwept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, _, run := f.driveToOneRun(t, "starting sweep not-yet test")

	// Comfortably under the threshold (half of it), not merely a hair
	// under it -- this test only needs to prove "not yet", not pin the
	// exact boundary (internal/domain/automation.TestIsOrphaned already
	// pins that precisely at the pure-function level).
	recent := time.Now().Add(-platform.DefaultTimeouts().AutomationRunStartingOrphanThreshold / 2)
	f.ageRun(t, run.ID, &recent, nil)

	if err := f.engine.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != sqlcgen.AutomationRunStatusStarting {
		t.Fatalf("run status = %s, want still starting (not yet past the threshold)", got.Status)
	}
}

func TestSweep_OrphanedRunningRunIsFailedAfterThreshold(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, inv, run := f.driveToOneRun(t, "running sweep test")

	f.setTurnStatus(t, run, sqlcgen.TurnStatusProcessing)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce (promote): %v", err)
	}

	old := time.Now().Add(-platform.DefaultTimeouts().AutomationRunRunningOrphanThreshold - time.Minute)
	f.ageRun(t, run.ID, nil, &old)

	if err := f.engine.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != sqlcgen.AutomationRunStatusFailed {
		t.Fatalf("run status = %s, want failed (swept as an orphan)", got.Status)
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed", gotInv.Status)
	}
}

func TestSweep_RunningRunJustUnderThresholdIsNotSwept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, _, run := f.driveToOneRun(t, "running sweep not-yet test")

	f.setTurnStatus(t, run, sqlcgen.TurnStatusProcessing)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce (promote): %v", err)
	}

	recent := time.Now().Add(-platform.DefaultTimeouts().AutomationRunRunningOrphanThreshold / 2)
	f.ageRun(t, run.ID, nil, &recent)

	if err := f.engine.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := f.runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != sqlcgen.AutomationRunStatusRunning {
		t.Fatalf("run status = %s, want still running (not yet past the threshold)", got.Status)
	}
}

// TestPumpOnce_SkipsFanOutForAPausedAutomation is the defense-in-depth
// check ListDueForFanOut's own "AND a.status = 'active'" join condition
// exists for: an invocation created BEFORE its own automation got
// auto-paused (e.g. by a different, concurrently-closing invocation) must
// never be fanned out into real sessions while paused -- it stays pending,
// un-fanned-out, picked up again only once (if ever) the automation is
// resumed.
func TestPumpOnce_SkipsFanOutForAPausedAutomation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "paused before fan-out", 1)
	inv := f.createInvocation(t, auto.ID, targets)

	if _, err := f.pool.Exec(ctx, "UPDATE automations SET status = 'paused' WHERE id = $1", auto.ID); err != nil {
		t.Fatalf("pause automation: %v", err)
	}

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 0 {
		t.Fatalf("got %d runs, want 0 -- a paused automation's own pending invocation must not be fanned out", len(runs))
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.FannedOutAt.Valid {
		t.Fatalf("fanned_out_at was set for a paused automation's own invocation")
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusPending {
		t.Fatalf("invocation status = %s, want still pending", gotInv.Status)
	}

	// Resuming the automation makes the SAME invocation eligible again on
	// the very next tick.
	if _, err := f.pool.Exec(ctx, "UPDATE automations SET status = 'active' WHERE id = $1", auto.ID); err != nil {
		t.Fatalf("resume automation: %v", err)
	}
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (after resume): %v", err)
	}
	runs = f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 1 {
		t.Fatalf("got %d runs after resume, want 1", len(runs))
	}
}

// TestPumpOnce_RunsCreateFailureRecordsFailedRunNotStrandedInvocation is
// fix #1's own regression test: createRunAndSession's own runs.Create
// failure branch (fanout.go) used to bare-return with no run row recorded
// at all for that target -- since total_runs is fixed at len(targets) when
// the invocation is created (invocationenqueue.go), a target with NO run
// row, terminal or otherwise, means EvaluateInvocationOutcome's own
// "terminalRuns >= totalRuns" check can never reach true, stranding the
// WHOLE invocation in 'pending' forever (no sweep anywhere scans pending
// invocations for this).
//
// A temporary CHECK constraint forbidding status='starting' forces every
// createRunAndSession attempt's own primary insert (which always uses
// status='starting') to fail deterministically, without needing a fault-
// injection seam -- while still permitting createFailedRun's own fallback
// insert (status='failed') straight through, letting this test observe
// exactly the "Create failed -> createFailedRun records a failed run"
// recovery path fix #1 adds.
func TestPumpOnce_RunsCreateFailureRecordsFailedRunNotStrandedInvocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, "ALTER TABLE automation_runs ADD CONSTRAINT test_reject_starting CHECK (status <> 'starting')"); err != nil {
		t.Fatalf("add test constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), "ALTER TABLE automation_runs DROP CONSTRAINT IF EXISTS test_reject_starting"); err != nil {
			t.Errorf("drop test constraint: %v", err)
		}
	})

	auto, targets := f.createAutomation(t, "runs.Create failure test", 2)
	inv := f.createInvocation(t, auto.ID, targets)

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 2 {
		t.Fatalf("got %d automation_runs rows, want 2 -- every target must still get exactly one recorded run even when the primary insert fails", len(runs))
	}
	for _, run := range runs {
		if run.Status != sqlcgen.AutomationRunStatusFailed {
			t.Errorf("run %s status = %s, want failed (recorded via the createFailedRun fallback)", run.ID.String(), run.Status)
		}
		if run.SessionID.Valid {
			t.Errorf("run %s has a linked session, want none -- its own tx (session+turn included) was rolled back when the primary insert failed", run.ID.String())
		}
	}

	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed -- it must NOT be stranded pending forever", gotInv.Status)
	}
	if !gotInv.ClosedAt.Valid {
		t.Fatalf("invocation closed_at not set")
	}
	if !gotInv.FailureCountedAt.Valid {
		t.Fatalf("failure_counted_at not set for a failed invocation")
	}

	gotAuto, err := f.automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d, want 1", gotAuto.ConsecutiveFailures)
	}
}

// TestAutomationRunStore_CreateIfAbsent_DuplicateTargetIsANoOp proves the
// idempotent fallback fix #1's commit-failure branch relies on:
// automation_runs_invocation_target_uniq (migrations/000054) plus
// CreateAutomationRunIfAbsent's own "ON CONFLICT ... DO NOTHING" makes a
// second insert for the SAME (invocation_id, target) a safe no-op --
// exactly the scenario an ambiguous tx.Commit failure (the earlier
// transaction actually succeeded server-side despite the client observing
// an error) produces when createFailedRun retries.
func TestAutomationRunStore_CreateIfAbsent_DuplicateTargetIsANoOp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "duplicate target no-op test", 1)
	inv := f.createInvocation(t, auto.ID, targets)

	targetJSON, err := json.Marshal([]domainautomation.Target{targets[0]})
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}

	first, err := f.runs.CreateIfAbsent(ctx, sqlcgen.CreateAutomationRunParams{
		InvocationID: inv.ID,
		AutomationID: auto.ID,
		Target:       targetJSON,
		Status:       sqlcgen.AutomationRunStatusFailed,
	})
	if err != nil {
		t.Fatalf("first CreateIfAbsent: %v", err)
	}

	_, err = f.runs.CreateIfAbsent(ctx, sqlcgen.CreateAutomationRunParams{
		InvocationID: inv.ID,
		AutomationID: auto.ID,
		Target:       targetJSON,
		Status:       sqlcgen.AutomationRunStatusFailed,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second CreateIfAbsent error = %v, want pgx.ErrNoRows (a safe no-op for an already-recorded target)", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 1 {
		t.Fatalf("got %d automation_runs rows for this target, want exactly 1 -- the duplicate insert must be a no-op, not a second row", len(runs))
	}
	if runs[0].ID != first.ID {
		t.Fatalf("existing row's own ID changed -- the duplicate insert must not have touched it")
	}
}
