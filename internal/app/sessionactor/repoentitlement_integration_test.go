//go:build integration

// This file proves the session actor's half of technical plan §31.4's
// "Un-entitlement" (repoentitlement.go): an administrator's revocation of
// a session's repository stops the session's pending turns before they
// reach a sandbox -- at spawn (refuseIfRepoRevoked) and at dispatch
// (revocationRefusalForDispatch) -- in every rollout mode, through the
// session's clone URLs and through its pull-request claim when the URL
// names a fork; a turn already running finishes; a restore lets the next
// turn run; and a revocation read that fails is an undelivered prompt,
// never a refusal.
package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/platform"
)

// revokeRepoForActorTest records an administrator's revocation of
// repoFullName.
func revokeRepoForActorTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string) {
	t.Helper()
	if _, err := narvipg.NewRepoEntitlementRevocationStore(pool).Revoke(ctx, repoFullName, pgtype.UUID{}, "frozen by an administrator"); err != nil {
		t.Fatalf("revoke %s: %v", repoFullName, err)
	}
}

// reviewSessionOn is a pull request's review session on repoFullName's PR
// #11, cloned from cloneURL (a fork's, when it names another repository),
// holding one pending review attempt; with ready, its sandbox is ready at
// gen 1, otherwise it has none.
func reviewSessionOn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName, cloneURL string, ready bool) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       reposJSONForTest(t, "widgets", cloneURL, ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimPullRequest(ctx, t, pool, repoFullName, 11, session.ID)
	if ready {
		seedReadySandbox(ctx, t, pool, session.ID)
	}
	head := "f00dcafe"
	attempt, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID, Status: sqlcgen.TurnStatusPending, Prompt: strPtr("review this pull request"),
		ReviewHeadSha: &head, IsReviewAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID, attempt
}

// assertRefusedForRevocation checks that turn ended failed because repo is
// revoked: the synthetic execution_complete names the revocation, one
// banner names it with the remedy, wantBanner's words included.
func assertRefusedForRevocation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, turn sqlcgen.Turn, repo, wantBanner string) {
	t.Helper()
	got, err := narvipg.NewTurnStore(pool).Get(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sqlcgen.TurnStatusFailed {
		t.Errorf("turn status = %s, want failed", got.Status)
	}
	reasons := syntheticEndReasons(ctx, t, pool, sessionID)
	if len(reasons) != 1 || reasons[0] != `repo "`+repo+`" entitlement revoked by an administrator` {
		t.Errorf("synthetic execution_complete reasons = %q, want the revocation of %s", reasons, repo)
	}
	warnings := sessionWarnings(ctx, t, pool, sessionID)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `an administrator of this deployment revoked new work on its repository "`+repo+`"`) ||
		!strings.Contains(warnings[0], wantBanner) || !strings.Contains(warnings[0], "An administrator can restore the repository; then send the turn again.") {
		t.Errorf("session warnings = %q, want one naming the revocation of %s, %q and the remedy", warnings, repo, wantBanner)
	}
	for _, w := range warnings {
		for _, banned := range []string{"installed", "configured", "access"} {
			if strings.Contains(w, banned) {
				t.Errorf("banner %q says %q", w, banned)
			}
		}
	}
}

// assertReviewCheckNamesRevocation checks that the review attempt's check
// closed as not assessed, naming the revocation.
func assertReviewCheckNamesRevocation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, attempt sqlcgen.Turn) {
	t.Helper()
	var closing []map[string]any
	for _, c := range outboxRows(ctx, t, pool, sessionID, ports.NotificationKindGitHubReviewCheck) {
		if c["phase"] == string(reviewcheck.PhaseTerminalNotAssessed) {
			closing = append(closing, c)
		}
	}
	if len(closing) != 1 || closing[0]["attempt_id"] != attempt.ID.String() || closing[0]["not_assessed_reason"] != string(reviewcheck.NotAssessedRepoEntitlementRevoked) {
		t.Errorf("closing review-check rows = %v, want one for attempt %s naming %q", closing, attempt.ID.String(), reviewcheck.NotAssessedRepoEntitlementRevoked)
	}
}

// TestDispatch_RevokedRepo_ExistingReadySandbox_QueuedTurnRefusedNeverSent:
// a review session whose sandbox is already ready, its repository revoked
// after it was created, gets its queued review attempt refused at dispatch
// -- committed processing, then failed, and its prompt never sent -- with
// the banner, the review check closed as not assessed naming the
// revocation, and the refusal counted once. Rollout mode is open: the
// revocation binds without it.
func TestDispatch_RevokedRepo_ExistingReadySandbox_QueuedTurnRefusedNeverSent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/revoked-ready"
	sessionID, attempt := reviewSessionOn(ctx, t, pool, repo, "https://github.com/"+repo+".git", true)
	revokeRepoForActorTest(ctx, t, pool, repo)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistryWithCommanderAndRolloutMode(t, ctx, pool, commander, rollout.ModeOpen)
	t.Cleanup(func() { _ = r.Shutdown() })
	before := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageDispatch)

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	turns := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, attempt.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	if got := commander.callCount(); got != 0 {
		t.Errorf("SendCommand calls = %d, want 0: a revoked repository's turn never reaches its sandbox", got)
	}
	got, err := turns.Get(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DispatchedAt.Valid {
		t.Error("dispatched_at not set: the refusal must be the dispatch-time one, after the turn was committed processing")
	}
	assertRefusedForRevocation(ctx, t, pool, sessionID, attempt, repo, "so the turn was not sent to its sandbox")
	assertReviewCheckNamesRevocation(ctx, t, pool, sessionID, attempt)
	if grew := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageDispatch) - before; grew != 1 {
		t.Errorf("session_repo_entitlement_denied_total{stage=dispatch} grew by %d, want 1", grew)
	}
}

// TestDispatch_RevokedRepo_RefusesSpawn: a session with no sandbox whose
// repository is revoked never asks the provider for one -- its pending
// turn ends at spawn time, with the spawn banner, and no sandbox row is
// written.
func TestDispatch_RevokedRepo_RefusesSpawn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/revoked-spawn"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+repo+".git", "")
	queued := createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")
	revokeRepoForActorTest(ctx, t, pool, repo)

	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"}}
	r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeOpen)
	t.Cleanup(func() { _ = r.Shutdown() })
	before := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageSpawn)

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	turns := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, queued.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	if got := provider.callCount(); got != 0 {
		t.Errorf("CreateSandbox calls = %d, want 0", got)
	}
	if _, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID); err == nil {
		t.Error("a sandbox row was written: the refusal must come before any spawn claim")
	}
	assertRefusedForRevocation(ctx, t, pool, sessionID, queued, repo, "so no sandbox could be started")
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageSpawn) - before; got != 1 {
		t.Errorf("session_repo_entitlement_denied_total{stage=spawn} grew by %d, want 1: the spawn refusal is counted once", got)
	}
	if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
		t.Error("a dispatch timer is left: the refusal would be evaluated again")
	}
}

// TestDispatch_RevokedForkPRReviewSession_RefusedByClaimKey: a fork pull
// request's review session clones the fork, which no revocation names; the
// revocation of the pull request's base repository refuses it anyway,
// through the session's github_pr_sessions claim -- at dispatch to a ready
// sandbox and at spawn alike.
func TestDispatch_RevokedForkPRReviewSession_RefusedByClaimKey(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		ready      bool
		wantBanner string
	}{
		{"dispatch to a ready sandbox", true, "so the turn was not sent to its sandbox"},
		{"spawn", false, "so no sandbox could be started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			const base = "acme/revoked-fork-base"
			sessionID, attempt := reviewSessionOn(ctx, t, pool, base, "https://github.com/contributor/revoked-fork-base.git", tc.ready)
			revokeRepoForActorTest(ctx, t, pool, base)

			commander := &fakeSendCommander{}
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"}}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, provider, "http://localhost:8080", nil, nil, "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			sendEnsureDispatched(ctx, t, a)
			turns := narvipg.NewTurnStore(pool)
			waitUntil(t, 5*time.Second, func() bool {
				got, err := turns.Get(ctx, attempt.ID)
				return err == nil && got.Status == sqlcgen.TurnStatusFailed
			})

			if got := commander.callCount(); got != 0 {
				t.Errorf("SendCommand calls = %d, want 0", got)
			}
			if got := provider.callCount(); got != 0 {
				t.Errorf("CreateSandbox calls = %d, want 0", got)
			}
			assertRefusedForRevocation(ctx, t, pool, sessionID, attempt, base, tc.wantBanner)
			assertReviewCheckNamesRevocation(ctx, t, pool, sessionID, attempt)
		})
	}
}

// TestDispatch_RevokedRepo_RunningTurnFinishesThenQueuedTurnRefused: the
// revocation lands while a turn is processing. That turn's own
// execution_complete completes it as it would have -- running work
// finishes -- and the queued turn behind it is refused when its dispatch
// comes, after the post-turn snapshot returns the sandbox to ready: only
// the snapshot command ever reaches the sandbox, never the queued prompt.
func TestDispatch_RevokedRepo_RunningTurnFinishesThenQueuedTurnRefused(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/revoked-while-running"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+repo+".git", "")
	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	turns := narvipg.NewTurnStore(pool)
	running := createProcessingTurn(ctx, t, turns, sessionID)
	queued := createPendingTurn(ctx, t, turns, sessionID, "the next queued prompt")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, &fakeSpawnProvider{}, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	revokeRepoForActorTest(ctx, t, pool, repo)

	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})
	if !outcome.Persisted {
		t.Fatal("execution_complete: Persisted = false, want true")
	}
	gotRunning, err := turns.Get(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRunning.Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("running turn status = %s, want completed: a turn already running finishes after a revocation", gotRunning.Status)
	}

	// The post-turn snapshot: the one command the sandbox gets.
	waitUntil(t, 5*time.Second, func() bool { return commander.callCount() == 1 })
	var snapshotCmd sandboxws.Snapshot
	if err := json.Unmarshal(commander.lastPayload(), &snapshotCmd); err != nil || snapshotCmd.Type != "snapshot" {
		t.Fatalf("the first command = %s (%v), want the snapshot", commander.lastPayload(), err)
	}
	snapshotReady := json.RawMessage(`{"type":"snapshot_ready","messageId":"sr-revoked-1","sessionId":"` + sessionID.String() +
		`","gen":1,"ackId":"snapshot_ready:sr-revoked-1","snapshotId":"snap-revoked-1","commandMessageId":"` + snapshotCmd.MessageId + `"}`)
	if outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: snapshotReady}); !outcome.Persisted {
		t.Fatal("snapshot_ready: Persisted = false, want true")
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, queued.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})
	if got := commander.callCount(); got != 1 {
		t.Errorf("SendCommand calls = %d, want the snapshot alone: the queued prompt is never sent", got)
	}
	assertRefusedForRevocation(ctx, t, pool, sessionID, queued, repo, "so the turn was not sent to its sandbox")
}

// TestDispatch_RestoredRepo_NextTurnRuns: after a refusal, restoring the
// repository lets the next turn a person sends reach the sandbox.
func TestDispatch_RestoredRepo_NextTurnRuns(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/restored-repo"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+repo+".git", "")
	seedReadySandbox(ctx, t, pool, sessionID)
	turns := narvipg.NewTurnStore(pool)
	refused := createPendingTurn(ctx, t, turns, sessionID, "while revoked")
	revokeRepoForActorTest(ctx, t, pool, repo)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistryWithCommanderAndRolloutMode(t, ctx, pool, commander, rollout.ModeOpen)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, refused.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})
	if got := commander.callCount(); got != 0 {
		t.Fatalf("SendCommand calls = %d while revoked, want 0", got)
	}

	if _, err := narvipg.NewRepoEntitlementRevocationStore(pool).Restore(ctx, repo); err != nil {
		t.Fatalf("restore: %v", err)
	}
	next := createPendingTurn(ctx, t, turns, sessionID, "after the restore")
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })
	got, err := turns.Get(ctx, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sqlcgen.TurnStatusProcessing {
		t.Errorf("next turn status = %s, want processing: its prompt was sent", got.Status)
	}
}

// TestDispatch_RevokedRepo_RefusedInOpenAndCohortRolloutModes: the
// revocation is read whatever the rollout mode -- open, and cohort with the
// repository enrolled -- at spawn and at dispatch.
func TestDispatch_RevokedRepo_RefusedInOpenAndCohortRolloutModes(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []platform.RolloutMode{rollout.ModeOpen, rollout.ModeCohort} {
		for _, ready := range []bool{true, false} {
			name := string(mode) + map[bool]string{true: ", dispatch", false: ", spawn"}[ready]
			t.Run(name, func(t *testing.T) {
				pool := newTestPool(t)
				const repo = "acme/revoked-any-mode"
				sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+repo+".git", "")
				if _, err := narvipg.NewRepoSettingsStore(pool).UpsertSessionsEnabled(ctx, repo, true); err != nil {
					t.Fatalf("enroll: %v", err)
				}
				if ready {
					seedReadySandbox(ctx, t, pool, sessionID)
				}
				turns := narvipg.NewTurnStore(pool)
				queued := createPendingTurn(ctx, t, turns, sessionID, "do the thing")
				revokeRepoForActorTest(ctx, t, pool, repo)

				commander := &fakeSendCommander{}
				provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"}}
				r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, provider, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{RolloutMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Shutdown() })
				a, err := r.GetOrSpawn(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				sendEnsureDispatched(ctx, t, a)
				waitUntil(t, 5*time.Second, func() bool {
					got, err := turns.Get(ctx, queued.ID)
					return err == nil && got.Status == sqlcgen.TurnStatusFailed
				})
				if commander.callCount() != 0 || provider.callCount() != 0 {
					t.Errorf("SendCommand / CreateSandbox calls = %d / %d, want none", commander.callCount(), provider.callCount())
				}
				reasons := syntheticEndReasons(ctx, t, pool, sessionID)
				if len(reasons) != 1 || !strings.Contains(reasons[0], "entitlement revoked by an administrator") {
					t.Errorf("synthetic reasons = %q, want the revocation", reasons)
				}
			})
		}
	}
}

// TestDispatch_RevocationReadError_UndeliveredNotRefused: a revocation read
// that fails is not a fact about the repository. At dispatch it fails the
// turn as an undelivered prompt -- prompt_not_delivered on its review
// check, no banner, nothing counted -- never as a refusal; at spawn it
// leaves the turn pending and backs its dispatch timer off. The read is made
// to fail by hiding the table for the test's duration. At dispatch the
// back-off is observable only once something re-arms the timer, which
// TestDispatchRevocation_WorkflowStepEscalatesOnce_ReadErrorIsRetriedBackedOff
// shows with a workflow step's blocked self edge.
func TestDispatch_RevocationReadError_UndeliveredNotRefused(t *testing.T) {
	ctx := context.Background()
	hideRevocations := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		if _, err := pool.Exec(ctx, `ALTER TABLE repo_entitlement_revocations RENAME TO repo_entitlement_revocations_hidden`); err != nil {
			t.Fatalf("hide revocations: %v", err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `ALTER TABLE repo_entitlement_revocations_hidden RENAME TO repo_entitlement_revocations`); err != nil {
				t.Errorf("restore revocations table: %v", err)
			}
		})
	}

	t.Run("dispatch", func(t *testing.T) {
		pool := newTestPool(t)
		sessionID, attempt := reviewSessionOn(ctx, t, pool, "acme/unreadable", "https://github.com/acme/unreadable.git", true)
		if _, err := pool.Exec(ctx, `INSERT INTO session_timers (session_id, name, fires_at) VALUES ($1, $2, now())`, sessionID, TimerDispatch); err != nil {
			t.Fatalf("arm dispatch timer: %v", err)
		}
		hideRevocations(t, pool)

		timeouts := platform.DefaultTimeouts()
		timeouts.DispatchRetryBackoff = time.Minute
		timeouts.DispatchRetryBackoffMax = 10 * time.Minute
		commander := &fakeSendCommander{}
		r, err := NewRegistry(ctx, pool, timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Shutdown() })
		before := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "reason", "revoked")
		a, err := r.GetOrSpawn(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		sendEnsureDispatched(ctx, t, a)
		turns := narvipg.NewTurnStore(pool)
		waitUntil(t, 5*time.Second, func() bool {
			got, err := turns.Get(ctx, attempt.ID)
			return err == nil && got.Status == sqlcgen.TurnStatusFailed
		})

		if got := commander.callCount(); got != 0 {
			t.Errorf("SendCommand calls = %d, want 0: an unverified entitlement never sends", got)
		}
		if reasons := syntheticEndReasons(ctx, t, pool, sessionID); len(reasons) != 1 || reasons[0] != repoEntitlementUnverifiedReason {
			t.Errorf("synthetic reasons = %q, want %q", reasons, repoEntitlementUnverifiedReason)
		}
		if warnings := sessionWarnings(ctx, t, pool, sessionID); len(warnings) != 0 {
			t.Errorf("session warnings = %q, want none: a failed read is not a refusal", warnings)
		}
		var closing []map[string]any
		for _, c := range outboxRows(ctx, t, pool, sessionID, ports.NotificationKindGitHubReviewCheck) {
			if c["phase"] == string(reviewcheck.PhaseTerminalNotAssessed) {
				closing = append(closing, c)
			}
		}
		if len(closing) != 1 || closing[0]["not_assessed_reason"] != string(reviewcheck.NotAssessedPromptNotDelivered) {
			t.Errorf("closing review-check rows = %v, want one naming %q", closing, reviewcheck.NotAssessedPromptNotDelivered)
		}
		if grew := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "reason", "revoked") - before; grew != 0 {
			t.Errorf("session_repo_entitlement_denied_total{reason=revoked} grew by %d, want 0", grew)
		}
	})

	t.Run("spawn", func(t *testing.T) {
		pool := newTestPool(t)
		sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/acme/unreadable-spawn.git", "")
		queued := createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")
		hideRevocations(t, pool)

		provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"}}
		r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeOpen)
		t.Cleanup(func() { _ = r.Shutdown() })
		a, err := r.GetOrSpawn(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		sendEnsureDispatched(ctx, t, a)
		waitUntil(t, 5*time.Second, func() bool {
			timer, ok := dispatchTimer(ctx, t, pool, sessionID)
			return ok && timer.FiresAt.Time.After(time.Now())
		})

		got, err := narvipg.NewTurnStore(pool).Get(ctx, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != sqlcgen.TurnStatusPending {
			t.Errorf("turn status = %s, want pending: a failed read refuses nothing", got.Status)
		}
		if got := provider.callCount(); got != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0", got)
		}
		if warnings := sessionWarnings(ctx, t, pool, sessionID); len(warnings) != 0 {
			t.Errorf("session warnings = %q, want none", warnings)
		}
	})
}

// receiptResendAfterRevocation drives a capable gen-1 dispatch of the
// rig's turn (its prompt asks for a receipt), then, with stored receipt or
// not, an administrator's revocation of the session's repository and a
// same-gen reconnect -- the point at which a prompt the sandbox has not
// receipted would be sent again. It returns the rig and the repository.
func receiptResendAfterRevocation(ctx context.Context, t *testing.T, receiptStored bool) (*receiptRig, string) {
	t.Helper()
	rig, repo := receiptRigDispatchedOnce(ctx, t, "acme/zz-revoked-resend-", receiptStored, nil)
	revokeRepoForActorTest(ctx, t, rig.pool, repo)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	return rig, repo
}

// receiptRigDispatchedOnce is a receipt rig on a fresh repository named
// from prefix, enrolled in the cohort rollout, with provider as its sandbox
// provider, whose turn was dispatched once to a capable gen 1 -- its prompt
// asking for a receipt -- and, with receiptStored, receipted: the agent
// confirmed it is running the prompt.
func receiptRigDispatchedOnce(ctx context.Context, t *testing.T, prefix string, receiptStored bool, provider ports.SandboxProvider) (*receiptRig, string) {
	t.Helper()
	repo := prefix + uuid.NewString()[:8]
	rig := newReceiptRig(ctx, t, receiptRigOptions{repoFullName: repo, provider: provider})
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	prompts := rig.commander.prompts(t)
	if len(prompts) != 1 || !prompts[0].asksReceipt(t) {
		t.Fatalf("prompts after the first dispatch = %d, want one asking for a receipt", len(prompts))
	}
	if receiptStored {
		sendAndSettle(ctx, t, rig.actor, promptReceivedEvent(prompts[0].MessageId, 1, false), 1)
	}
	return rig, repo
}

// assertProcessingNothingResent checks that the turn's prompt was sent
// once only, none of its re-sends is spent and it is still processing,
// with no synthetic execution_complete: the prompt may be running, and a
// refused re-send never fails the turn.
func assertProcessingNothingResent(ctx context.Context, t *testing.T, rig *receiptRig) {
	t.Helper()
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Errorf("prompts sent = %d, want the first dispatch alone: nothing is re-sent to a revoked repository", got)
	}
	got := rig.turn(ctx, t)
	if got.Status != sqlcgen.TurnStatusProcessing {
		t.Errorf("turn status = %s, want processing: a refused re-send never fails the turn", got.Status)
	}
	if got.ReceiptResendCount != 0 {
		t.Errorf("receipt_resend_count = %d, want 0: nothing was written, so none of the turn's re-sends is spent", got.ReceiptResendCount)
	}
	if _, synthetic := rig.executionCompleteRows(ctx, t); synthetic != 0 {
		t.Errorf("synthetic execution_complete events = %d, want none", synthetic)
	}
}

// revokedResendMsg is refuseResendIfRepoRevoked's WARN line.
const revokedResendMsg = "sessionactor: prompt re-send refused: an administrator revoked the session's repository (§31.4); the reconnect stays unanswered and the turn processing, until a restore or the re-send window's end"

// assertReconnectUnclaimed checks that the turn's receipt check has not
// moved past wantChecked: the reconnect is still to be answered.
func assertReconnectUnclaimed(ctx context.Context, t *testing.T, rig *receiptRig, wantChecked int32) {
	t.Helper()
	got := rig.turn(ctx, t)
	if got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != wantChecked {
		t.Errorf("receipt_checked_ready_seq = %s, want %d: a revocation leaves the reconnect unclaimed", int32PtrString(got.ReceiptCheckedReadySeq), wantChecked)
	}
}

// TestPromptReceipt_RevokedRepo_LostPromptNotResent: a prompt lost on its
// way to a capable gen, then an administrator's revocation, then a same-gen
// reconnect: no receipt is stored, so the re-send would start the work
// after the revocation. It is refused, the turn stays processing, and the
// reconnect is left unclaimed -- receipt_checked_ready_seq and
// receipt_resend_count do not move, and turn_prompt_resend_total counts
// nothing. Heartbeats read the revocation again, but the refusal is
// logged and counted (session_repo_entitlement_denied_total{stage=resend})
// once per turn and reconnect: once for the first ready and its
// heartbeats, once more for a second ready.
func TestPromptReceipt_RevokedRepo_LostPromptNotResent(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	rig, repo := receiptRigDispatchedOnce(ctx, t, "acme/zz-revoked-resend-", false, nil)
	checked := *rig.turn(ctx, t).ReceiptCheckedReadySeq
	refusedBefore := promptResendCount(ctx, t, promptResendOutcomeRefused)
	revokedBefore := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageResend)

	revokeRepoForActorTest(ctx, t, rig.pool, repo)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	for i := 0; i < 3; i++ {
		sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	}

	assertProcessingNothingResent(ctx, t, rig)
	assertReconnectUnclaimed(ctx, t, rig, checked)
	if got := promptResendCount(ctx, t, promptResendOutcomeRefused) - refusedBefore; got != 0 {
		t.Errorf("turn_prompt_resend_total{refused} moved by %d, want 0: the reconnect is not answered yet", got)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageResend) - revokedBefore; got != 1 {
		t.Errorf("session_repo_entitlement_denied_total{stage=resend} moved by %d, want 1: once per turn and reconnect, not per heartbeat", got)
	}
	if got := countLogLines(t, logs, revokedResendMsg); got != 1 {
		t.Errorf("%d revoked re-send WARN lines after one reconnect and its heartbeats, want 1", got)
	}

	// A second reconnect while still revoked: one more WARN and count.
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	assertProcessingNothingResent(ctx, t, rig)
	assertReconnectUnclaimed(ctx, t, rig, checked)
	if got := countLogLines(t, logs, revokedResendMsg); got != 2 {
		t.Errorf("%d revoked re-send WARN lines after two reconnects, want 2", got)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageResend) - revokedBefore; got != 2 {
		t.Errorf("session_repo_entitlement_denied_total{stage=resend} moved by %d, want 2", got)
	}
}

// TestPromptReceipt_RevokedRepo_RestoredWithinWindow_NextHeartbeatResendsOnce:
// a prompt lost on its way to a capable gen, the repository revoked, one
// same-gen reconnect -- the only one a lost prompt usually brings -- and
// then the repository restored inside PromptResendWindow. No further
// reconnect comes; the first heartbeat after the restore answers the one
// the revocation left unclaimed, and re-sends the prompt once, under its
// own messageId, as the turn's first re-send. Later heartbeats send
// nothing more.
func TestPromptReceipt_RevokedRepo_RestoredWithinWindow_NextHeartbeatResendsOnce(t *testing.T) {
	ctx := context.Background()
	rig, repo := receiptRigDispatchedOnce(ctx, t, "acme/zz-restored-resend-", false, nil)
	sentBefore := promptResendCount(ctx, t, promptResendOutcomeSent)

	revokeRepoForActorTest(ctx, t, rig.pool, repo)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("prompts after the revoked reconnect = %d, want the first dispatch alone", got)
	}
	if _, err := narvipg.NewRepoEntitlementRevocationStore(rig.pool).Restore(ctx, repo); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i := 0; i < 10; i++ {
		sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	}

	prompts := rig.commander.prompts(t)
	if len(prompts) != 2 || prompts[1].MessageId != prompts[0].MessageId || !prompts[1].asksReceipt(t) {
		t.Fatalf("after the restore and 10 heartbeats: %d prompts; want exactly one re-send of the first, under its own messageId, asking", len(prompts))
	}
	got := rig.turn(ctx, t)
	if got.Status != sqlcgen.TurnStatusProcessing || got.ReceiptResendCount != 1 {
		t.Errorf("turn status %s, receipt_resend_count %d; want processing, 1", got.Status, got.ReceiptResendCount)
	}
	if ready := rig.sandbox(ctx, t).ReadySeq; got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != ready {
		t.Errorf("receipt_checked_ready_seq = %s, want the reconnect's ready_seq %d, answered", int32PtrString(got.ReceiptCheckedReadySeq), ready)
	}
	if moved := promptResendCount(ctx, t, promptResendOutcomeSent) - sentBefore; moved != 1 {
		t.Errorf("turn_prompt_resend_total{sent} moved by %d, want 1", moved)
	}
}

// TestPromptReceipt_RevokedRepo_WindowExpiresWhileRevoked_RestoreResendsNothing:
// the unclaimed reconnect of a revoked repository is bounded by
// PromptResendWindow. Once the dispatch asked longer ago than the window,
// the next heartbeat answers the reconnect as window_expired, revoked or
// not: the mark moves, nothing is sent, and one window WARN is logged. A
// restore after that re-sends nothing; the turn ends at its deadline, as
// any prompt not receipted inside the window does.
func TestPromptReceipt_RevokedRepo_WindowExpiresWhileRevoked_RestoreResendsNothing(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	rig, repo := receiptRigDispatchedOnce(ctx, t, "acme/zz-expired-resend-", false, nil)
	checked := *rig.turn(ctx, t).ReceiptCheckedReadySeq
	expiredBefore := promptResendCount(ctx, t, promptResendOutcomeWindowExpired)

	revokeRepoForActorTest(ctx, t, rig.pool, repo)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	assertReconnectUnclaimed(ctx, t, rig, checked)

	window := platform.DefaultTimeouts().PromptResendWindow
	if _, err := rig.pool.Exec(ctx,
		`UPDATE turns SET receipt_requested_at = now() - make_interval(secs => $2::double precision) WHERE id = $1`,
		rig.turnID, (window + time.Minute).Seconds()); err != nil {
		t.Fatalf("age the request past the window: %v", err)
	}
	sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)

	got := rig.turn(ctx, t)
	if ready := rig.sandbox(ctx, t).ReadySeq; got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != ready {
		t.Fatalf("receipt_checked_ready_seq = %s, want the reconnect's ready_seq %d: past the window it is answered", int32PtrString(got.ReceiptCheckedReadySeq), ready)
	}
	if moved := promptResendCount(ctx, t, promptResendOutcomeWindowExpired) - expiredBefore; moved != 1 {
		t.Errorf("turn_prompt_resend_total{window_expired} moved by %d, want 1", moved)
	}
	if n := countLogLines(t, logs, windowExpiredMsg); n != 1 {
		t.Errorf("%d window WARN lines, want 1", n)
	}

	if _, err := narvipg.NewRepoEntitlementRevocationStore(rig.pool).Restore(ctx, repo); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i := 0; i < 3; i++ {
		sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	}
	assertProcessingNothingResent(ctx, t, rig)
}

// TestPromptReceipt_RevokedRepo_ReceiptStoredNothingSentNotFailed: the same,
// with the prompt's receipt stored, so the agent confirmed it is running:
// the reconnect sends nothing and the turn is not failed.
func TestPromptReceipt_RevokedRepo_ReceiptStoredNothingSentNotFailed(t *testing.T) {
	ctx := context.Background()
	rig, _ := receiptResendAfterRevocation(ctx, t, true)
	assertProcessingNothingResent(ctx, t, rig)
}

// TestPromptReceipt_RevocationReadError_ReconnectAnsweredOnceReadable: a
// revocation read that fails at a same-gen reconnect is read in the
// transaction that would claim the reconnect, so the evaluation rolls back
// with it: nothing is sent, nothing fails, nothing is counted, and neither
// the reconnect nor one of the turn's re-sends is spent. Heartbeats while
// the read still fails change nothing; the first heartbeat once it
// succeeds answers the reconnect, and the prompt is re-sent once.
func TestPromptReceipt_RevocationReadError_ReconnectAnsweredOnceReadable(t *testing.T) {
	ctx := context.Background()
	rig, _ := receiptRigDispatchedOnce(ctx, t, "acme/zz-unreadable-resend-", false, nil)
	checkedBefore := rig.turn(ctx, t).ReceiptCheckedReadySeq
	refusedBefore := promptResendCount(ctx, t, promptResendOutcomeRefused)
	sentBefore := promptResendCount(ctx, t, promptResendOutcomeSent)

	hidden := true
	if _, err := rig.pool.Exec(ctx, `ALTER TABLE repo_entitlement_revocations RENAME TO repo_entitlement_revocations_hidden`); err != nil {
		t.Fatalf("hide revocations: %v", err)
	}
	unhide := func() {
		if !hidden {
			return
		}
		hidden = false
		if _, err := rig.pool.Exec(context.Background(), `ALTER TABLE repo_entitlement_revocations_hidden RENAME TO repo_entitlement_revocations`); err != nil {
			t.Errorf("restore revocations table: %v", err)
		}
	}
	t.Cleanup(unhide)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)

	assertProcessingNothingResent(ctx, t, rig)
	got := rig.turn(ctx, t)
	if checkedBefore == nil {
		t.Fatal("receipt_checked_ready_seq is NULL after the first dispatch, want the dispatch's ready_seq")
	}
	if got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != *checkedBefore {
		t.Errorf("receipt_checked_ready_seq = %s, want %d: a failed read claims no reconnect", int32PtrString(got.ReceiptCheckedReadySeq), *checkedBefore)
	}
	if moved := promptResendCount(ctx, t, promptResendOutcomeRefused) - refusedBefore; moved != 0 {
		t.Errorf("turn_prompt_resend_total{refused} moved by %d, want 0: a failed read decided nothing", moved)
	}

	unhide()
	sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	prompts := rig.commander.prompts(t)
	if len(prompts) != 2 || prompts[1].MessageId != prompts[0].MessageId {
		t.Fatalf("once the read succeeds: %d prompts; want the first re-sent under its own messageId", len(prompts))
	}
	got = rig.turn(ctx, t)
	if got.Status != sqlcgen.TurnStatusProcessing || got.ReceiptResendCount != 1 || got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != rig.sandbox(ctx, t).ReadySeq {
		t.Errorf("turn status %s, receipt_resend_count %d, receipt_checked_ready_seq %s; want processing, 1, the reconnect's ready_seq %d",
			got.Status, got.ReceiptResendCount, int32PtrString(got.ReceiptCheckedReadySeq), rig.sandbox(ctx, t).ReadySeq)
	}
	if moved := promptResendCount(ctx, t, promptResendOutcomeSent) - sentBefore; moved != 1 {
		t.Errorf("turn_prompt_resend_total{sent} moved by %d, want 1", moved)
	}
}

// int32PtrString prints p's value, or NULL.
func int32PtrString(p *int32) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprint(*p)
}

// assertEndedForRevocationAfterSandboxWent checks that rig's turn, which
// was processing when its sandbox went, ended failed with repo's
// revocation as its reason, and that its prompt was sent once only.
func assertEndedForRevocationAfterSandboxWent(ctx context.Context, t *testing.T, rig *receiptRig, repo string) {
	t.Helper()
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Errorf("prompts sent = %d, want the first dispatch alone", got)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusFailed {
		t.Errorf("turn status = %s, want failed: the gen it ran on is gone and the repository is revoked", got.Status)
	}
	reasons := syntheticEndReasons(ctx, t, rig.pool, rig.sessionID)
	if len(reasons) != 1 || reasons[0] != repoRevokedReason(repo) {
		t.Errorf("synthetic execution_complete reasons = %q, want the revocation of %s", reasons, repo)
	}
}

// TestRevokedRepo_ProcessingTurnEndsWhenItsSandboxGoes pins §31.4's one
// exception to "a turn already processing finishes": its sandbox restarts
// or dies while the repository is revoked. The agent confirmed it was
// running the prompt (its receipt is stored), the repository is revoked,
// and then the gen it ran on is gone:
//
//   - respawned as a new gen, Ready: re-sending the prompt to that gen is
//     a dispatch to a gen that has not had it, refused like any other --
//     the turn fails with the revocation reason, and its synthetic
//     execution_complete carries "delivered": false, read for the new gen,
//     on which nothing was sent;
//   - stopped: the respawn is refused before any sandbox is created, and
//     the turn ends with the revocation reason.
func TestRevokedRepo_ProcessingTurnEndsWhenItsSandboxGoes(t *testing.T) {
	ctx := context.Background()

	t.Run("re-enqueued to a new Ready gen", func(t *testing.T) {
		rig, repo := receiptRigDispatchedOnce(ctx, t, "acme/zz-revoked-reenqueue-", true, nil)
		revokeRepoForActorTest(ctx, t, rig.pool, repo)
		if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: rig.sessionID}); err != nil {
			t.Fatalf("respawn: %v", err)
		}
		if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
			t.Fatalf("move sandbox to ready: %v", err)
		}
		sendAndSettle(ctx, t, rig.actor, receiptReady(2, true), 2)

		assertEndedForRevocationAfterSandboxWent(ctx, t, rig, repo)
		var delivered *string
		if err := rig.pool.QueryRow(ctx, `SELECT payload->>'delivered' FROM events WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`,
			rig.sessionID).Scan(&delivered); err != nil {
			t.Fatalf("read the synthetic execution_complete: %v", err)
		}
		if delivered == nil || *delivered != "false" {
			t.Errorf(`synthetic execution_complete "delivered" = %v, want false: nothing was sent to gen 2`, delivered)
		}
		if got := rig.turn(ctx, t); got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 2 {
			t.Errorf("dispatched_sandbox_gen = %v, want 2: the mark is read for the gen the re-send was refused on", got.DispatchedSandboxGen)
		}
	})

	t.Run("its sandbox stopped", func(t *testing.T) {
		provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-should-never-be-called"}}
		rig, repo := receiptRigDispatchedOnce(ctx, t, "acme/zz-revoked-stopped-", true, provider)
		revokeRepoForActorTest(ctx, t, rig.pool, repo)
		if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusStopped}); err != nil {
			t.Fatalf("move sandbox to stopped: %v", err)
		}
		sendEnsureDispatched(ctx, t, rig.actor)
		waitUntil(t, 5*time.Second, func() bool { return rig.turn(ctx, t).Status != sqlcgen.TurnStatusProcessing })

		assertEndedForRevocationAfterSandboxWent(ctx, t, rig, repo)
		if got := provider.callCount(); got != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0: the respawn is refused", got)
		}
		if got := rig.sandbox(ctx, t).Gen; got != 1 {
			t.Errorf("sandbox gen = %d, want 1: no respawn was claimed", got)
		}
	})
}

// workflowStepWithBlockedSelfEdge attaches attempt to a fresh workflow run
// whose one step has a blocked self edge, and returns the run.
func workflowStepWithBlockedSelfEdge(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, attempt sqlcgen.Turn, name string) sqlcgen.WorkflowRun {
	t.Helper()
	var defID, stepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('review', $1, false, 1) RETURNING id`, name).Scan(&defID); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $2, 'blocked')`, defID, stepID); err != nil {
		t.Fatalf("insert blocked self edge: %v", err)
	}
	workflows := narvipg.NewWorkflowStore(pool)
	run, err := workflows.CreateRun(ctx, sessionID, "review", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepRun, err := workflows.CreateStepRun(ctx, run.ID, stepID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	if err := workflows.AttachTurn(ctx, stepRun.ID, attempt.ID); err != nil {
		t.Fatalf("attach turn: %v", err)
	}
	return run
}

// TestDispatchRevocation_WorkflowStepEscalatesOnce_ReadErrorIsRetriedBackedOff
// pins how the two dispatch-time outcomes reach a workflow step with a
// blocked self edge:
//
//   - a revocation is a refusal (OnTurnRefused): with the pump ticking, the
//     refusal is made once, no attempt is queued again, the run waits for a
//     person (needs_review), and no dispatch timer is left;
//   - a revocation read that fails is an undelivered prompt
//     (OnTurnCompleted, then backOffAfterUndeliveredPrompt): the step's
//     blocked self edge queues it again, the run is not escalated, and the
//     dispatch timer the re-queued turn armed is backed off.
func TestDispatchRevocation_WorkflowStepEscalatesOnce_ReadErrorIsRetriedBackedOff(t *testing.T) {
	ctx := context.Background()
	t.Run("revoked", func(t *testing.T) {
		pool := newTestPool(t)
		const repo = "acme/revoked-workflow"
		sessionID, attempt := reviewSessionOn(ctx, t, pool, repo, "https://github.com/"+repo+".git", true)
		run := workflowStepWithBlockedSelfEdge(ctx, t, pool, sessionID, attempt, "test-revoked-blocked-self-edge")
		revokeRepoForActorTest(ctx, t, pool, repo)

		timeouts := platform.DefaultTimeouts()
		timeouts.TimerClaimDuration = 300 * time.Millisecond
		commander := &fakeSendCommander{}
		r, err := NewRegistry(ctx, pool, timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Shutdown() })
		before := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageDispatch)
		a, err := r.GetOrSpawn(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		sendEnsureDispatched(ctx, t, a)
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if err := r.PumpOnce(ctx); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}

		if n := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID); n != 1 {
			t.Errorf("turns = %d, want the refused attempt alone: the refusal followed the blocked self edge", n)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID); n != 1 {
			t.Errorf("step runs = %d, want the refused attempt alone", n)
		}
		gotRun, err := narvipg.NewWorkflowStore(pool).GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.Status != sqlcgen.WorkflowRunStatusNeedsReview {
			t.Errorf("run status = %s, want needs_review", gotRun.Status)
		}
		if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
			t.Error("a dispatch timer is left: something was queued behind the refusal")
		}
		if got := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageDispatch) - before; got != 1 {
			t.Errorf("session_repo_entitlement_denied_total{stage=dispatch} grew by %d, want 1: the refusal is made once", got)
		}
		if got := commander.callCount(); got != 0 {
			t.Errorf("SendCommand calls = %d, want 0", got)
		}
	})

	t.Run("read error", func(t *testing.T) {
		pool := newTestPool(t)
		const repo = "acme/unreadable-workflow"
		sessionID, attempt := reviewSessionOn(ctx, t, pool, repo, "https://github.com/"+repo+".git", true)
		run := workflowStepWithBlockedSelfEdge(ctx, t, pool, sessionID, attempt, "test-unreadable-blocked-self-edge")
		if _, err := pool.Exec(ctx, `ALTER TABLE repo_entitlement_revocations RENAME TO repo_entitlement_revocations_hidden`); err != nil {
			t.Fatalf("hide revocations: %v", err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `ALTER TABLE repo_entitlement_revocations_hidden RENAME TO repo_entitlement_revocations`); err != nil {
				t.Errorf("restore revocations table: %v", err)
			}
		})

		timeouts := platform.DefaultTimeouts()
		timeouts.DispatchRetryBackoff = time.Minute
		timeouts.DispatchRetryBackoffMax = 10 * time.Minute
		commander := &fakeSendCommander{}
		r, err := NewRegistry(ctx, pool, timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Shutdown() })
		a, err := r.GetOrSpawn(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		sendEnsureDispatched(ctx, t, a)
		turns := narvipg.NewTurnStore(pool)
		waitUntil(t, 5*time.Second, func() bool {
			got, err := turns.Get(ctx, attempt.ID)
			return err == nil && got.Status == sqlcgen.TurnStatusFailed
		})
		waitUntil(t, 5*time.Second, func() bool {
			timer, ok := dispatchTimer(ctx, t, pool, sessionID)
			return ok && timer.FiresAt.Time.After(time.Now().Add(30*time.Second))
		})

		gotRun, err := narvipg.NewWorkflowStore(pool).GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.Status == sqlcgen.WorkflowRunStatusNeedsReview {
			t.Errorf("run status = %s: a failed revocation read escalated the run", gotRun.Status)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID); n != 2 {
			t.Errorf("turns = %d, want the failed attempt and the step queued again behind the backoff", n)
		}
		if got := commander.callCount(); got != 0 {
			t.Errorf("SendCommand calls = %d, want 0", got)
		}
	})
}

// TestReviewRetriggerDebounce_RevokedRepo_NoTurnNoBudgetNoNotice: pushes to
// a pull request of a repository an administrator revoked (§31.4) create
// no automatic re-review -- through eleven debounce firings, one past the
// budget, no turn is inserted, no GitHub read is made, none of the pull
// request's budget is spent, and no budget-exhausted notice is enqueued.
// After a restore, the next push re-reviews as it always did.
func TestReviewRetriggerDebounce_RevokedRepo_NoTurnNoBudgetNoNotice(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	revokeRepoForActorTest(ctx, t, pool, f.repoFullName)

	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live", nextBaseRef: "main", nextDiff: "+ line changed"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	before := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageAutoRetrigger)
	for i := 1; i <= ReviewAutoRetriggerBudget+1; i++ {
		f.setPendingHeadSHA(ctx, t, fmt.Sprintf("sha-push-%d", i))
		f.armDebounceTimer(ctx, t)
		fireDebounceTimer(ctx, t, r, f)
	}

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns = %d, want 0: a revoked repository gets no automatic re-review", len(turns))
	}
	row := f.getPRSession(ctx, t)
	if row.AutoRetriggerCount != 0 || row.AutoRetriggerBudgetNoticeSentAt.Valid {
		t.Errorf("auto_retrigger_count = %d, notice sent = %v; want 0 and no notice: nothing ran, nothing is spent", row.AutoRetriggerCount, row.AutoRetriggerBudgetNoticeSentAt.Valid)
	}
	if n := f.countOutboxVerdictRows(ctx, t); n != 0 {
		t.Errorf("github_verdict outbox rows = %d, want 0: no budget notice for reviews that never ran", n)
	}
	if n := len(outboxRows(ctx, t, pool, f.sessionID, ports.NotificationKindGitHubReviewCheck)); n != 0 {
		t.Errorf("review-check outbox rows = %d, want 0", n)
	}
	diffFetcher.mu.Lock()
	reads := diffFetcher.getPRCalls
	diffFetcher.mu.Unlock()
	if reads != 0 {
		t.Errorf("GitHub pull-request reads = %d, want 0: the revocation is read first", reads)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "stage", repoEntitlementStageAutoRetrigger) - before; got != int64(ReviewAutoRetriggerBudget+1) {
		t.Errorf("session_repo_entitlement_denied_total{stage=auto_retrigger} grew by %d, want one per firing (%d)", got, ReviewAutoRetriggerBudget+1)
	}

	if _, err := narvipg.NewRepoEntitlementRevocationStore(pool).Restore(ctx, f.repoFullName); err != nil {
		t.Fatalf("restore: %v", err)
	}
	f.setPendingHeadSHA(ctx, t, "sha-after-restore")
	f.armDebounceTimer(ctx, t)
	fireDebounceTimer(ctx, t, r, f)
	turns, err = f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("turns after the restore = %d, want 1: the next push re-reviews", len(turns))
	}
	if row := f.getPRSession(ctx, t); row.AutoRetriggerCount != 1 {
		t.Errorf("auto_retrigger_count after the restore = %d, want 1", row.AutoRetriggerCount)
	}
}
