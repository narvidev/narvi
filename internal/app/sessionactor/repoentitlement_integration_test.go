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
	"strings"
	"testing"
	"time"

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
	if grew := readCounterSumByAttr(ctx, t, otelReader, "session_repo_entitlement_denied_total", "reason", "revoked") - before; grew != 1 {
		t.Errorf("session_repo_entitlement_denied_total{reason=revoked} grew by %d, want 1", grew)
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
// check, no banner, nothing counted, the dispatch timer backed off -- never
// as a refusal; at spawn it leaves the turn pending and backs off. The
// read is made to fail by hiding the table for the test's duration.
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
