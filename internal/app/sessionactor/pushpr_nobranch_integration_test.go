//go:build integration

package sessionactor

import (
	"context"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves review round 3's own finding Q1:
// completeProcessingTurn's own push-blocked-warning gate
// (pushBlockedByMissingGitHubIdentity / recordPushBlockedNoGitHubIdentity,
// pushpr.go) must apply the SAME "does this session have any repo with an
// explicit branch" filter sendPushBestEffort and recordSuppressedPush
// already apply -- a session whose repos all leave branch nil never sends
// a push at all, so warning its OIDC-only creator that "this push could
// not be authenticated" on every completed turn was pure noise, stacking
// forever. A session with an explicit branch must still get the warning,
// once per turn that would have pushed.

// TestCompleteProcessingTurn_NoGitHubIdentity_NoExplicitBranch_NeverWarns
// proves the fix: a live, non-review session whose creator has no linked
// github identity, and whose one repo has NO explicit branch (mirroring
// every Slack/Linear/automation-spawned session, none of which ever set
// repos[].branch), must record zero warning events across two completed
// turns -- no push was ever going to be attempted for it, so nothing about
// a missing github identity is even relevant.
func TestCompleteProcessingTurn_NoGitHubIdentity_NoExplicitBranch_NeverWarns(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "no-github-identity-no-branch@example.com",
		DisplayName:  "No GitHub Identity, No Branch",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Deliberately NO identities row of any provider.

	const repoFullName = "acme/no-github-identity-no-branch"
	// branch="" mirrors a nil repos[].branch: reposFromJSON's own
	// sessionconfig.SessionConfigReposElem treats both the same way
	// (repoHasExplicitBranch, pushpr.go), and every Slack/Linear/
	// automation-spawned session sends exactly this shape.
	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID, "repo", "https://github.com/"+repoFullName+".git", "")

	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)

	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	eventStore := narvipg.NewEventStore(pool)

	// Two completed turns, back to back -- the gate must stay silent for
	// both, not merely the first.
	for i := 0; i < 2; i++ {
		processing := createProcessingTurn(ctx, t, turnStore, sessionID)
		sendSandboxEventForTest(ctx, t, a, SandboxEvent{
			Type: "execution_complete", Gen: 1,
			Raw: executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
		})
		waitUntil(t, 5*time.Second, func() bool {
			row, err := turnStore.Get(ctx, processing.ID)
			return err == nil && row.Status == sqlcgen.TurnStatusCompleted
		})
	}

	// A brief settle window for any post-commit broadcast/send to have run
	// -- there is nothing to wait FOR here (the assertion is an absence),
	// unlike the sibling explicit-branch test below.
	time.Sleep(300 * time.Millisecond)

	if got := commander.callCount(); got != 0 {
		t.Errorf("SandboxCommander.SendCommand called %d times, want 0 -- no repo names an explicit branch, so no push was ever going to be sent", got)
	}

	events, err := eventStore.ListForSession(ctx, sessionID, 0, 100)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	var warnings int
	for _, e := range events {
		if e.Type == "warning" {
			warnings++
		}
	}
	if warnings != 0 {
		t.Errorf("warning events = %d, want 0 over two completed turns -- a no-explicit-branch session never attempts a push, so the push-blocked warning must never fire for it", warnings)
	}
}

// TestCompleteProcessingTurn_NoGitHubIdentity_ExplicitBranch_WarnsEveryTurn
// proves the fix does not over-correct: a session with an explicit branch
// (a push WOULD have been attempted) must still get exactly one warning
// per completed turn -- the gate the round-2 fix (finding P1) added stays
// intact for the case it exists for.
func TestCompleteProcessingTurn_NoGitHubIdentity_ExplicitBranch_WarnsEveryTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "no-github-identity-explicit-branch@example.com",
		DisplayName:  "No GitHub Identity, Explicit Branch",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Deliberately NO identities row of any provider.

	const repoFullName = "acme/no-github-identity-explicit-branch"
	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID, "repo", "https://github.com/"+repoFullName+".git", "main")

	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)

	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	eventStore := narvipg.NewEventStore(pool)

	for i := 1; i <= 2; i++ {
		wantWarnings := i
		processing := createProcessingTurn(ctx, t, turnStore, sessionID)
		sendSandboxEventForTest(ctx, t, a, SandboxEvent{
			Type: "execution_complete", Gen: 1,
			Raw: executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
		})
		waitUntil(t, 5*time.Second, func() bool {
			row, err := turnStore.Get(ctx, processing.ID)
			return err == nil && row.Status == sqlcgen.TurnStatusCompleted
		})

		waitUntil(t, 5*time.Second, func() bool {
			events, listErr := eventStore.ListForSession(ctx, sessionID, 0, 100)
			if listErr != nil {
				return false
			}
			var warnings int
			for _, e := range events {
				if e.Type == "warning" {
					warnings++
				}
			}
			return warnings == wantWarnings
		})
	}

	if got := commander.callCount(); got != 0 {
		t.Errorf("SandboxCommander.SendCommand called %d times, want 0 -- a github-less creator's push is certain to be denied, and must never even be attempted", got)
	}

	events, err := eventStore.ListForSession(ctx, sessionID, 0, 100)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	var warnings int
	for _, e := range events {
		if e.Type == "warning" {
			warnings++
		}
	}
	if warnings != 2 {
		t.Errorf("warning events = %d, want 2 -- exactly one per completed turn that would have pushed", warnings)
	}
}
