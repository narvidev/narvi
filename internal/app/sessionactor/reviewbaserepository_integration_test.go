//go:build integration

package sessionactor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/rollout"
)

// This file pins reviewbaserepository.go on real Postgres, through the real
// actor (technical plan §21.1, §30.4): a review session whose spec still
// names its pull request's fork is moved onto the base repository in the
// transaction that spawns or restores its next gen -- before that gen's
// SESSION_CONFIG is assembled and before anything else in the spawn reads
// the spec -- and never under a live gen, which keeps the spec it booted
// with.

const (
	legacyForkURL = "https://github.com/contributor/widgets.git"
	legacyBase    = "acme/widgets"
)

// legacyForkReviewSession creates a review session of legacyBase#prNumber
// whose spec names the fork, with the fork's branch, as the GitHub ingress
// wrote one before its spec named the base repository.
func legacyForkReviewSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, prNumber int32) pgtype.UUID {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       reposJSONForTest(t, "widgets", legacyForkURL, "main"),
	})
	if err != nil {
		t.Fatalf("create the review session: %v", err)
	}
	claimPullRequest(ctx, t, pool, legacyBase, prNumber, session.ID)
	return session.ID
}

// storedRepos reads the session's spec as stored.
func storedRepos(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) string {
	t.Helper()
	var stored string
	if err := pool.QueryRow(ctx, `SELECT repos::text FROM sessions WHERE id = $1`, sessionID).Scan(&stored); err != nil {
		t.Fatalf("read the session's repos: %v", err)
	}
	return stored
}

const (
	movedSpec  = `[{"url": "https://github.com/acme/widgets.git", "name": "widgets", "branch": null}]`
	legacySpec = `[{"url": "https://github.com/contributor/widgets.git", "name": "widgets", "branch": "main"}]`
)

// TestSpawn_ALegacyForkReviewSessionIsRestoredOntoItsBaseRepository: the
// session's sandbox stopped with a snapshot; its next gen is a restore,
// whose SESSION_CONFIG names the base repository and the pull request's
// ref, and the stored spec is moved in the same transaction, its branch
// dropped and its name kept. The base repository is live and the fork has
// no settings: the restore's own egress check, which reads the spec, sees
// the base -- read before the move, the fork would resolve shadow and turn
// the restore into a fresh spawn.
func TestSpawn_ALegacyForkReviewSessionIsRestoredOntoItsBaseRepository(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := legacyForkReviewSession(ctx, t, pool, 21)
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, legacyBase, true); err != nil {
		t.Fatalf("promote the base repository live: %v", err)
	}
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")
	seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-legacy-fork")

	provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-legacy-fork"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount()+provider.callCount() > 0 })

	if got := provider.restoreCallCount(); got != 1 || provider.callCount() != 0 {
		t.Fatalf("restores %d, fresh spawns %d; want the snapshot restored once", got, provider.callCount())
	}
	repos := provider.lastRestoreCall().spec.SessionConfig.Repos
	if len(repos) != 1 || repos[0].Url != "https://github.com/acme/widgets.git" || repos[0].Name != "widgets" ||
		repos[0].Ref == nil || *repos[0].Ref != "refs/pull/21/head" || repos[0].Branch != nil {
		t.Fatalf("the restore's SESSION_CONFIG repos = %+v, want widgets at the base repository, refs/pull/21/head, no branch", repos)
	}
	if got := storedRepos(ctx, t, pool, sessionID); got != movedSpec {
		t.Errorf("sessions.repos = %s, want %s", got, movedSpec)
	}
}

// TestDispatch_ALegacyForkReviewSessionIsNeverMovedUnderItsLiveGen: the
// session's gen is live, booted on the fork's spec; its review turn is
// dispatched as before -- the prompt, no checkout of a ref its clone's
// origin does not have -- and its spec is left for its next boot.
func TestDispatch_ALegacyForkReviewSessionIsNeverMovedUnderItsLiveGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := legacyForkReviewSession(ctx, t, pool, 22)
	seedReadySandbox(ctx, t, pool, sessionID)
	head := coHead
	prompt := "review the pull request"
	created, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rig := newContextRig(ctx, t, pool, sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "prompt" {
		t.Fatalf("commands %v, want the prompt alone", got)
	}
	if got, err := narvipg.NewTurnStore(pool).Get(ctx, created.ID); err != nil || got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn %+v (%v), want processing", got.Status, err)
	}
	if got := storedRepos(ctx, t, pool, sessionID); got != legacySpec {
		t.Errorf("sessions.repos = %s under the live gen, want it unchanged, %s", got, legacySpec)
	}
}

// TestRolloutRecheck_ForkPullRequestUsesTheBase: in cohort mode, a
// session's spawn re-checks that every repository its spec names is
// enrolled. A pull request's base repository is enrolled and the
// contributor's fork never is: a legacy session's spec, naming the fork,
// is moved onto the base before the re-check reads it, so its next gen is
// spawned, as a session the GitHub ingress opens now on the base is.
func TestRolloutRecheck_ForkPullRequestUsesTheBase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
	}{
		{name: "a legacy session naming the fork", setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			return legacyForkReviewSession(ctx, t, pool, 23)
		}},
		{name: "a session the ingress opens on the base", setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
				Repos:       []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`),
			})
			if err != nil {
				t.Fatal(err)
			}
			claimPullRequest(ctx, t, pool, legacyBase, 24, session.ID)
			return session.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := tc.setup(ctx, t, pool)
			if _, err := narvipg.NewRepoSettingsStore(pool).UpsertSessionsEnabled(ctx, legacyBase, true); err != nil {
				t.Fatalf("enroll the base repository: %v", err)
			}
			created := createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")

			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "spawned-" + tc.name}}
			r := newDispatchTestRegistryWithRolloutMode(t, ctx, pool, provider, rollout.ModeCohort)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			turns := narvipg.NewTurnStore(pool)
			waitUntil(t, 5*time.Second, func() bool {
				got, err := turns.Get(ctx, created.ID)
				return provider.callCount() == 1 || (err == nil && got.Status == sqlcgen.TurnStatusFailed)
			})
			if got := provider.callCount(); got != 1 {
				turn, _ := turns.Get(ctx, created.ID)
				t.Fatalf("spawns %d (turn %s), want the base repository's enrollment to admit the session's gen", got, turn.Status)
			}
			if repos := provider.lastSpec().SessionConfig.Repos; len(repos) != 1 || repos[0].Url != "https://github.com/acme/widgets.git" {
				t.Errorf("SESSION_CONFIG repos = %+v, want the base repository", repos)
			}
			if got := storedRepos(ctx, t, pool, sessionID); got != movedSpec {
				t.Errorf("sessions.repos = %s, want %s", got, movedSpec)
			}
		})
	}
}

// TestSpawn_ALegacyForkReviewSessionIsNeverMovedOnAResume: a resume takes
// back the provider object the session's last gen ran in, and delivers no
// new SESSION_CONFIG, so the resumed gen keeps the fork's spec it booted
// with -- and so does the session.
func TestSpawn_ALegacyForkReviewSessionIsNeverMovedOnAResume(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := legacyForkReviewSession(ctx, t, pool, 25)
	createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")
	seedStoppedSandboxWithProviderID(ctx, t, pool, sessionID, "resumable-legacy-fork")

	provider := &fakeSpawnProvider{resumeSupported: true}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.resumeCallCount() == 1 })

	if got := storedRepos(ctx, t, pool, sessionID); got != legacySpec {
		t.Errorf("sessions.repos = %s after a resume, want it unchanged, %s: the resumed gen keeps the spec it booted with", got, legacySpec)
	}
}
