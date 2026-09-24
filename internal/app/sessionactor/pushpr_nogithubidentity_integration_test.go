//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves review round 2's own finding P1 (HIGH) gate:
// pushBlockedByMissingGitHubIdentity/recordPushBlockedNoGitHubIdentity
// (pushpr.go). scmcredentials.go's own step 10 now denies a live,
// non-review session's push whose creator has no linked github identity
// unconditionally (the round-1 bot-token fallback was reverted) --
// sendPushBestEffort must never even attempt that doomed push, and must
// instead leave a session-visible "warning" event naming the honest,
// actionable reason.

// TestCompleteProcessingTurn_NoGitHubIdentity_BlocksPushAndRecordsWarning
// proves the core P1 gate: a live, non-review session whose creator has
// no linked github identity at all must never have a push command sent
// for it (scmcredentials.go's own step 10 would 403 it unconditionally
// now), and must instead get exactly one session-visible "warning" event
// naming the honest, actionable reason.
func TestCompleteProcessingTurn_NoGitHubIdentity_BlocksPushAndRecordsWarning(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "no-github-identity-push-block@example.com",
		DisplayName:  "No GitHub Identity",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Deliberately NO identities row of any provider.

	const repoFullName = "acme/no-github-identity-push-block"
	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID, "repo", "https://github.com/"+repoFullName+".git", "main")

	// Promoted LIVE so this test reaches the identity gate rather than
	// the (unrelated) shadow-suppression gate immediately above it in
	// completeProcessingTurn.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	processing := createProcessingTurn(ctx, t, turnStore, sessionID)

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

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete", Gen: 1,
		Raw: executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	waitUntil(t, 5*time.Second, func() bool {
		row, err := turnStore.Get(ctx, processing.ID)
		return err == nil && row.Status == sqlcgen.TurnStatusCompleted
	})

	// Poll for the warning event the same way pushpr_shadow_integration_
	// test.go polls its own ledger row: sendPushBestEffort/the post-commit
	// broadcast run AFTER the reply the wait above observed.
	eventStore := narvipg.NewEventStore(pool)
	var events []sqlcgen.Event
	waitUntil(t, 5*time.Second, func() bool {
		var listErr error
		events, listErr = eventStore.ListForSession(ctx, sessionID, 0, 100)
		if listErr != nil {
			return false
		}
		for _, e := range events {
			if e.Type == "warning" {
				return true
			}
		}
		return false
	})

	if got := commander.callCount(); got != 0 {
		t.Errorf("SandboxCommander.SendCommand called %d times, want 0 -- a github-less creator's push is certain to be denied by scmcredentials.go's own step 10, and must never even be attempted", got)
	}

	var found *sandboxws.Warning
	for _, e := range events {
		if e.Type != "warning" {
			continue
		}
		var w sandboxws.Warning
		if err := json.Unmarshal(e.Payload, &w); err != nil {
			t.Fatalf("unmarshal persisted warning event: %v", err)
		}
		found = &w
	}
	if found == nil {
		t.Fatal("no warning event found in the session's own journal")
	}
	if found.Message == "" {
		t.Error("warning Message is empty, want an honest, actionable reason")
	}
	wantSubstrings := []string{"no linked GitHub account", "GitHub", "link"}
	for _, want := range wantSubstrings {
		if !strings.Contains(strings.ToLower(found.Message), strings.ToLower(want)) {
			t.Errorf("warning Message = %q, want it to mention %q", found.Message, want)
		}
	}
}

// TestCompleteProcessingTurn_ReviewSession_NoGitHubIdentity_PushNotBlocked
// proves the gate correctly EXCLUDES a review session: scmcredentials.go's
// own step 7 always mints the bot token for a review session's sandbox,
// regardless of the creator's own github identity, so
// pushBlockedByMissingGitHubIdentity must never fire for one -- a push
// command must still be sent normally.
func TestCompleteProcessingTurn_ReviewSession_NoGitHubIdentity_PushNotBlocked(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "review-session-no-identity-push@example.com",
		DisplayName:  "Review Session, No GitHub Identity",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Deliberately NO identities row of any provider -- proving this
	// gate's own review-session check, not merely that a github identity
	// happens to make the push succeed anyway.

	const repoFullName = "acme/review-session-no-identity-push"
	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID, "repo", "https://github.com/"+repoFullName+".git", "feature-review")
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, repoFullName, 77); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, 77, sessionID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	processing := createProcessingTurn(ctx, t, turnStore, sessionID)

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

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete", Gen: 1,
		Raw: executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	waitUntil(t, 5*time.Second, func() bool {
		row, err := turnStore.Get(ctx, processing.ID)
		return err == nil && row.Status == sqlcgen.TurnStatusCompleted
	})

	waitUntil(t, 5*time.Second, func() bool {
		return commander.callCount() == 1
	})

	eventStore := narvipg.NewEventStore(pool)
	events, err := eventStore.ListForSession(ctx, sessionID, 0, 100)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	for _, e := range events {
		if e.Type == "warning" {
			t.Errorf("a warning event was recorded for a review session, want none -- review sessions always mint the bot token (scmcredentials.go step 7) regardless of the creator's own github identity")
		}
	}
}
