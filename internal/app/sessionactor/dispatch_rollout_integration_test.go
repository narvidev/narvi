//go:build integration

// This file proves §10's own dispatch-time half of the "fail-closed,
// twice" rule (§10 Phase 6, §32): tryPlanSpawn's own refuseIfRolloutUnenrolled
// (dispatch.go) refuses to spawn a session whose own named repo is not
// enrolled under rollout.ModeCohort -- mirrors dispatch_substrate_
// integration_test.go's own identical structure/reasoning exactly, one
// Step later, for the SAME "fail-closed, twice" family of guards. This is
// what makes §32's own rollback bound real: a session's Environment/repos
// are set DIRECTLY against Postgres here (createTestSessionWithRepos,
// pushpr_integration_test.go), never through httpapi.CreateSessionOnTx's
// own creation-time gate -- proving the dispatch-time check is a
// genuinely independent second guard, not merely a second call into logic
// the creation-time gate already exercised (the exact scenario a
// de-enrolled repo's own already-existing PR review session is in, every
// time a later @mention/re-review re-dispatches it).
package sessionactor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/platform"
)

// newDispatchTestRegistryWithRolloutMode mirrors newDispatchTestRegistry
// (dispatch_integration_test.go) exactly, with ONE addition:
// RegistryOptions.RolloutMode set to the caller's own value -- every
// OTHER newDispatchTestRegistry caller in this package implicitly gets
// rollout.ModeOpen (RegistryOptions' own zero value), proven not to
// change any of their behavior by this package's own pre-existing,
// untouched dispatch tests continuing to pass unmodified.
func newDispatchTestRegistryWithRolloutMode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, provider ports.SandboxProvider, mode platform.RolloutMode) *Registry {
	t.Helper()
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, provider, "http://localhost:8080", nil, nil, "", nil, false,
		RegistryOptions{RolloutMode: mode})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// newDispatchTestRegistryWithCommanderAndRolloutMode is
// newDispatchTestRegistryWithRolloutMode's own sibling for the
// TURN-DISPATCH path specifically (a SandboxCommander, not a
// SandboxProvider): branch (b)/(b') of planDispatch (dispatch.go) never
// calls a.provider at all -- the sandbox is already live -- so a test
// exercising THAT path needs a fakeSendCommander wired in instead, with
// provider left nil exactly like newDispatchTestRegistry's own existing
// "commander-only" callers (e.g.
// TestHandleEnsureDispatched_SandboxReady_DispatchesTurn,
// dispatch_integration_test.go) already do.
func newDispatchTestRegistryWithCommanderAndRolloutMode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commander ports.SandboxCommander, mode platform.RolloutMode) *Registry {
	t.Helper()
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false,
		RegistryOptions{RolloutMode: mode})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// TestDispatch_DeEnrolledRepo_ExistingReadySandbox_ReusedTurnRefusedAndTerminalizes
// is THE de-enrollment CONVERGENCE test Root Cause 1 of this Step's own
// adversarial review named explicitly: TestDispatch_
// RefusesDeEnrolledRepoOnRespawn (above) already proved de-enrollment
// stops a future RESPAWN (tryPlanSpawn's own refuseIfRolloutUnenrolled),
// but planDispatch's own branch (b) -- a Pending turn dispatched to an
// ALREADY-Ready sandbox, exactly the REUSE branch's own shape
// (internal/adapters/inbound/github's own coalesce.go enqueues a turn on
// an EXISTING session, never touching httpapi.CreateSessionOnTx's
// creation-time gate, and never needing a fresh spawn/restore/resume at
// all since the sandbox is already live) -- had NO rollout re-check
// anywhere before this Step's own fix (executeDispatch's own
// rolloutRefusalForDispatch, dispatch.go). This is the exact scenario
// §32.1 named as "exactly why §32.4 exists as a second, independent
// gate" and §32.8 promised was covered -- proving it now actually is.
//
// Asserts all three things the fix's own placement (executeDispatch, the
// ONE function every dispatchPlan must pass through before
// SandboxCommander.SendCommand is ever called) guarantees together: (1)
// SendCommand is NEVER called -- no prompt, no clone/push/comment reaches
// the de-enrolled repo's sandbox; (2) the turn reaches a REAL terminal
// state (Failed) rather than sitting Pending to be re-dispatched forever
// on the next EnsureDispatched round; (3) it gets there via the SAME
// Dispatched->Processing->Failed machinery a genuine send failure already
// uses (failDispatchedTurn) -- dispatched_at IS set (the commit that ran
// before the gate fired), proving this is the fix's own late,
// structurally-unbypassable placement, not an earlier one that would have
// left the turn still Pending.
func TestDispatch_DeEnrolledRepo_ExistingReadySandbox_ReusedTurnRefusedAndTerminalizes(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoFullName := "acme/" + t.Name()
	repoURL := "https://github.com/" + repoFullName + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	// Enrolled when the session (and its sandbox) were first created...
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	// ...then an operator's own §32.9 rollback write de-enrolls it,
	// mid-incident, while this session's sandbox is ALREADY Ready and
	// idle -- exactly the runbook scenario §32.8 describes.
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, false); err != nil {
		t.Fatalf("seed de-enrollment: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	// A fresh @mention/label re-trigger enqueuing a turn on this EXISTING
	// session -- the REUSE branch's own shape, never touching
	// CreateSessionOnTx's own creation-time gate again.
	created := createPendingTurn(ctx, t, turnStore, sessionID, "re-review after new commits")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistryWithCommanderAndRolloutMode(t, ctx, pool, commander, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	got, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if got.Status != sqlcgen.TurnStatusFailed {
		t.Errorf("turn status = %s, want %s -- a de-enrolled repo's turn must reach a terminal state, never sit Pending to be re-dispatched forever", got.Status, sqlcgen.TurnStatusFailed)
	}
	if !got.DispatchedAt.Valid {
		t.Error("turn dispatched_at not set -- want set (tryPlanDispatch's own Pending->Dispatched->Processing transact already committed before the rollout gate ran)")
	}
	if !got.CompletedAt.Valid {
		t.Error("turn completed_at not set, want a real terminal completion")
	}

	// The decisive assertion: SendCommand must NEVER be called -- no
	// prompt is ever sent, so the sandbox never clones, pushes, or
	// comments on the de-enrolled repo.
	if got := commander.callCount(); got != 0 {
		t.Errorf("commander.callCount() = %d, want 0 -- a de-enrolled repo's session must never dispatch to its sandbox, even one that is already Ready", got)
	}

	sessionStore := narvipg.NewSessionStore(pool)
	gotSession, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSession.Status != sqlcgen.SessionStatusFailed {
		t.Errorf("session status = %q, want %q", gotSession.Status, sqlcgen.SessionStatusFailed)
	}

	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		sessionID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("execution_complete event count = %d, want 1 (synthetic completion -- no real terminal event can ever arrive for a prompt that was never sent)", eventCount)
	}

	var timerCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerTurnDeadline,
	).Scan(&timerCount); err != nil {
		t.Fatalf("count turn_deadline timers: %v", err)
	}
	if timerCount != 0 {
		t.Errorf("turn_deadline timer count = %d, want 0 (armed by tryPlanDispatch, then deleted once the turn resolved)", timerCount)
	}
}

// TestDispatch_RefusesUnenrolledRepoUnderCohortMode is a MUTATION-TESTABLE
// guard (mirrors dispatch_substrate_integration_test.go's own identical
// "remove the dispatch re-check -> this test must fail" framing, §27.5
// brief, applied to §10's own dispatch-time gate): the session's own
// repo is never enrolled (no repo_settings row written at all) and
// rollout.ModeCohort is armed -- CreateSandbox must never be called.
func TestDispatch_RefusesUnenrolledRepoUnderCohortMode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoURL := "https://github.com/acme/" + t.Name() + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	provider := &fakeSpawnProvider{
		nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"},
	}
	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	// Generous, fixed window -- mirrors dispatch_substrate_integration_
	// test.go's own identical "prove a negative" precedent.
	time.Sleep(300 * time.Millisecond)

	if got := provider.callCount(); got != 0 {
		t.Errorf("provider.callCount() = %d, want 0 -- CreateSandbox must never be called for a session whose repo is not enrolled under cohort mode", got)
	}

	sandboxes := narvipg.NewSandboxStore(pool)
	if _, err := sandboxes.Get(ctx, sessionID); err == nil {
		t.Error("sandboxes.Get succeeded -- want no sandbox row at all (the refusal must run before ANY spawn-claim write)")
	}
}

// TestDispatch_AllowsEnrolledRepoUnderCohortMode is the refusal test's own
// positive control: the IDENTICAL session/repo, but repo_settings.
// sessions_enabled is true for it -- proves the dispatch-time gate is a
// real, bidirectional decision, not something that happens to always
// refuse.
func TestDispatch_AllowsEnrolledRepoUnderCohortMode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoFullName := "acme/" + t.Name()
	repoURL := "https://github.com/" + repoFullName + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	provider := &fakeSpawnProvider{
		nextRef: ports.SandboxRef{ProviderID: "provider-enrolled-repo"},
	}
	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 2*time.Second, func() bool { return provider.callCount() > 0 })

	if got := provider.callCount(); got != 1 {
		t.Errorf("provider.callCount() = %d, want 1 -- an enrolled repo must spawn normally", got)
	}
}

// TestDispatch_RefusesDeEnrolledRepoOnRespawn is §32's own documented
// rollback scenario, exercised directly: a session that WAS enrolled
// (and would have spawned) has its own repo's sessions_enabled flipped
// back to false, mid-session -- exactly what an operator's own rollback
// write does (§32.8/§32.9). The NEXT dispatch attempt against it must
// refuse, proving de-enrollment stops future respawns of an
// already-existing session, not just brand-new creates.
func TestDispatch_RefusesDeEnrolledRepoOnRespawn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoFullName := "acme/" + t.Name()
	repoURL := "https://github.com/" + repoFullName + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	// Enrolled at first...
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}
	// ...then an operator's own rollback write de-enrolls it, BEFORE this
	// session's own first dispatch attempt ever runs (simulating a
	// rollback that lands between a REUSE-branch turn enqueue and this
	// Actor's own dispatch).
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, false); err != nil {
		t.Fatalf("seed de-enrollment: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "re-review after new commits")

	provider := &fakeSpawnProvider{
		nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"},
	}
	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	time.Sleep(300 * time.Millisecond)

	if got := provider.callCount(); got != 0 {
		t.Errorf("provider.callCount() = %d, want 0 -- a de-enrolled repo's session must never respawn, even though it was enrolled when its turn was enqueued", got)
	}
}

// TestDispatch_OpenMode_SpawnsRegardlessOfEnrollment is the no-op proof
// at the dispatch layer (§32's own "byte-for-byte no-op" property,
// mirrored from the creation-time gate's own identical proof,
// rolloutgate_integration_test.go in package httpapi): rollout.ModeOpen
// (RegistryOptions' own zero value, and platform.Load's own default when
// NARVI_ROLLOUT_MODE is unset) spawns normally even though the session's
// own repo has NO repo_settings row at all.
func TestDispatch_OpenMode_SpawnsRegardlessOfEnrollment(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoURL := "https://github.com/acme/" + t.Name() + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	provider := &fakeSpawnProvider{
		nextRef: ports.SandboxRef{ProviderID: "provider-open-mode"},
	}
	// rollout.ModeOpen explicitly, though it is also RegistryOptions'
	// own zero value -- named here for clarity, not relying on the zero
	// value silently.
	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeOpen)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 2*time.Second, func() bool { return provider.callCount() > 0 })

	if got := provider.callCount(); got != 1 {
		t.Errorf("provider.callCount() = %d, want 1 -- open mode must spawn regardless of enrollment", got)
	}
}

// The four tests below are the Phase 6 audit's own fix for Finding 4:
// before this fix, session_rollout_refused_total was incremented ONLY by
// httpapi.checkRolloutGate -- refuseIfRolloutUnenrolled and
// rolloutRefusalForDispatch (both above) refused just as really, but
// never touched the metric at all. Each calls its own gate method
// DIRECTLY (this test file is package sessionactor, not sessionactor_test,
// exactly like every other test in this file) rather than driving the
// full Send/EnsureDispatched pipeline -- these are about the metric
// side-effect specifically, not dispatch sequencing already covered
// above.
//
// Mutation anchor (verified by hand as part of this fix, reverted
// byte-identical): removing the `a.recordRolloutRefusal(...)` call from
// either refuseIfRolloutUnenrolled or rolloutRefusalForDispatch
// (dispatch.go) makes the corresponding "_RecordsRolloutRefusedTotal"
// test below fail (the counter no longer moves); inverting either
// function's own `if !transient` guard makes the corresponding
// "_DoesNotRecord...OnTransient..." test below fail instead (the counter
// now moves when it must not).

// TestRefuseIfRolloutUnenrolled_RecordsRolloutRefusedTotal_OnGenuineRefusal
// proves the spawn/restore/resume-time gate (tryPlanSpawn's own
// refuseIfRolloutUnenrolled) increments session_rollout_refused_total,
// tagged spawn_source, for a genuine "repo not enrolled" fact -- no
// repo_settings row at all, exactly TestDispatch_RefusesUnenrolledRepoUnderCohortMode's
// own fixture, called directly instead of driven through EnsureDispatched.
func TestRefuseIfRolloutUnenrolled_RecordsRolloutRefusedTotal_OnGenuineRefusal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoURL := "https://github.com/acme/" + t.Name() + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	sessionStore := narvipg.NewSessionStore(pool)
	sessionRow, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, nil, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	if err := a.refuseIfRolloutUnenrolled(ctx, tx, sessionRow); err == nil {
		t.Fatal("refuseIfRolloutUnenrolled: got nil error, want a refusal -- this repo has no repo_settings row at all")
	}

	after := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))
	if after <= before {
		t.Errorf("session_rollout_refused_total{spawn_source=%s} = %d, want > %d -- a genuine spawn-time policy refusal must record it", sessionRow.SpawnSource, after, before)
	}
}

// TestRefuseIfRolloutUnenrolled_DoesNotRecordRolloutRefusedTotal_OnTransientReadError
// mirrors httpapi's own TestCreateSessionOnTx_RolloutGate_CohortMode_ReadErrorFailsClosedButNotAsPolicy
// (rolloutgate_integration_test.go) fault-injection idiom exactly: an
// already-rolled-back tx standing in for a genuine repo_settings read
// failure (RepoSettingsStore.WithTx(tx).Get is the first thing to fail).
// The gate must still refuse (fail-closed), but must NOT count it as a
// policy refusal -- the exact "fail-closed and terminal are different
// properties" rule §32.5/§32.7 states, now applied identically on the
// dispatch side.
func TestRefuseIfRolloutUnenrolled_DoesNotRecordRolloutRefusedTotal_OnTransientReadError(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoURL := "https://github.com/acme/" + t.Name() + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	sessionStore := narvipg.NewSessionStore(pool)
	sessionRow, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, nil, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx (fault injection setup): %v", err)
	}
	// tx is now closed -- refuseIfRolloutUnenrolled's own
	// repoSettings.WithTx(tx).Get call is the first thing to fail.

	if err := a.refuseIfRolloutUnenrolled(ctx, tx, sessionRow); err == nil {
		t.Fatal("refuseIfRolloutUnenrolled: got nil error, want a refusal -- a genuine read failure must still fail CLOSED")
	}

	after := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))
	if after != before {
		t.Errorf("session_rollout_refused_total{spawn_source=%s} = %d, want unchanged %d -- a transient repo_settings read failure is NOT a demonstrated policy decision and must never count here", sessionRow.SpawnSource, after, before)
	}
}

// TestRolloutRefusalForDispatch_RecordsRolloutRefusedTotal_OnGenuineRefusal
// is the turn-dispatch-time gate's own equivalent of the spawn-time test
// above -- an existing, already-Ready sandbox (the REUSE-branch shape
// TestDispatch_DeEnrolledRepo_ExistingReadySandbox_ReusedTurnRefusedAndTerminalizes
// exercises end to end). Unlike refuseIfRolloutUnenrolled (which logs AND
// records the metric itself), rolloutRefusalForDispatch only RETURNS the
// refusal/transient facts -- executeDispatch (its one caller) is what
// actually logs and calls recordRolloutRefusal, exactly mirroring this
// package's own pre-existing "gate returns, caller acts" shape for this
// function specifically (its own doc comment). This test therefore calls
// a.executeDispatch directly, with a real dispatchPlan, rather than
// rolloutRefusalForDispatch in isolation -- the metric side-effect lives
// one level up from that pure gate function.
func TestRolloutRefusalForDispatch_RecordsRolloutRefusedTotal_OnGenuineRefusal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoFullName := "acme/" + t.Name()
	repoURL := "https://github.com/" + repoFullName + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, false); err != nil {
		t.Fatalf("seed de-enrollment: %v", err)
	}

	sessionStore := narvipg.NewSessionStore(pool)
	sessionRow, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	// executeDispatch's own doc comment: "The turn is already committed
	// Processing by the time this runs" -- createProcessingTurn (not
	// createPendingTurn) mirrors that real precondition directly, so
	// failDispatchedTurn's own "turn no longer processing; ignoring"
	// defensive guard does not no-op this test's assertion below.
	turnStore := narvipg.NewTurnStore(pool)
	created := createProcessingTurn(ctx, t, turnStore, sessionID)

	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, nil, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// Sanity-check the pure gate function's own return values first --
	// the SAME facts executeDispatch below is about to act on.
	repo, refused, transient := a.rolloutRefusalForDispatch(ctx, sessionRow)
	if !refused {
		t.Fatal("rolloutRefusalForDispatch: refused = false, want true -- this repo is explicitly de-enrolled")
	}
	if transient {
		t.Error("rolloutRefusalForDispatch: transient = true, want false -- this is a genuine, demonstrated policy fact, not a read error")
	}
	if repo != repoFullName {
		t.Errorf("rolloutRefusalForDispatch: repo = %q, want %q", repo, repoFullName)
	}

	before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))

	if err := a.executeDispatch(ctx, &dispatchPlan{turnID: created.ID, payload: json.RawMessage(`{}`), sessionRow: sessionRow}, pgtype.Timestamptz{}); err != nil {
		t.Fatalf("executeDispatch: %v", err)
	}

	after := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))
	if after <= before {
		t.Errorf("session_rollout_refused_total{spawn_source=%s} = %d, want > %d -- a genuine dispatch-time policy refusal must record it", sessionRow.SpawnSource, after, before)
	}

	gotTurn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusFailed {
		t.Errorf("turn status = %s, want %s -- executeDispatch's own refusal must still fail the turn forward, exactly as before this fix", gotTurn.Status, sqlcgen.TurnStatusFailed)
	}
}

// TestRolloutRefusalForDispatch_DoesNotRecordRolloutRefusedTotal_OnTransientReadError
// forces rolloutDecisionForSession's own repoSettings.Get call to fail
// with a genuine, non-ErrNoRows error by passing an already-canceled
// context -- pgx surfaces a canceled/timed-out query the SAME way a real
// Postgres outage would (the exact equivalence
// TestCreateSessionOnTx_RolloutGate_CohortMode_ReadErrorFailsClosedButNotAsPolicy's
// own doc comment states for its own already-rolled-back-tx idiom).
// rolloutRefusalForDispatch itself has no tx parameter to inject the
// SAME way (it deliberately runs outside any transaction, see its own doc
// comment) -- a canceled context is this function's own equivalent fault.
func TestRolloutRefusalForDispatch_DoesNotRecordRolloutRefusedTotal_OnTransientReadError(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoFullName := "acme/" + t.Name()
	repoURL := "https://github.com/" + repoFullName + ".git"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", repoURL, "")

	sessionStore := narvipg.NewSessionStore(pool)
	sessionRow, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, nil, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	repo, refused, transient := a.rolloutRefusalForDispatch(canceledCtx, sessionRow)
	if !refused {
		t.Fatal("rolloutRefusalForDispatch: refused = false, want true -- a genuine read failure must still fail CLOSED")
	}
	if !transient {
		t.Errorf("rolloutRefusalForDispatch: transient = false, want true -- repo %q was forced not-enrolled by a canceled-context read failure, not a demonstrated policy fact", repo)
	}

	after := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sessionRow.SpawnSource))
	if after != before {
		t.Errorf("session_rollout_refused_total{spawn_source=%s} = %d, want unchanged %d -- a transient repo_settings read failure is NOT a demonstrated policy decision and must never count here", sessionRow.SpawnSource, after, before)
	}
}
