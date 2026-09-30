//go:build integration

package sessionactor

import (
	"context"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/platform"
)

// This file implements §8.2's own ("sentinels + suggestions", §17.2
// amendment) explicitly required test: "the fix PR's base being the
// origin head branch and never the repo default." Mirrors pushpr_
// integration_test.go's own established real-Postgres, real-Actor,
// fakeSourceControl-backed pattern exactly.

// TestHandleSandboxEvent_PushComplete_SentinelFixPR_BaseIsOriginHeadBranch_NeverRepoDefault
// proves createSentinelFixPRBestEffort's own central invariant: the fix
// PR's Base is the origin PR's own head branch (captured in
// sentinel_fixes.origin_head_branch at claim time), even when the fake
// SourceControl's own configured "repo default branch"
// (defaultBranchName) is a DIFFERENT value -- and that resolvePRBaseBranch
// (ResolveBranchSHA) is NEVER called at all for this session, proving the
// dedicated code path never routes through the ordinary happy-path
// resolution.
func TestHandleSandboxEvent_PushComplete_SentinelFixPR_BaseIsOriginHeadBranch_NeverRepoDefault(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionStore := narvipg.NewSessionStore(pool)
	sandboxStore := narvipg.NewSandboxStore(pool)
	sentinelFixStore := narvipg.NewSentinelFixStore(pool)

	// The origin review session -- a real session row, standing in for
	// the PR-mention-created review session whose posted verdict
	// triggered this fix (its own real content is irrelevant to this
	// test; only its id is referenced by sentinel_fixes).
	originSession, err := sessionStore.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
	})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	// The fix (child) session -- provenance_tag = sentinel_auto_fix, repos
	// naming the FIX session's own branch to push from ("fix-branch"),
	// deliberately different from the ORIGIN PR's own head branch
	// ("feature-x", captured on the sentinel_fixes row below) -- these are
	// two distinct branches, the whole point of a stacked fix PR.
	tag := provenance.SentinelAutoFix
	fixSession, err := sessionStore.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource:   sqlcgen.SessionSpawnSourceGithub,
		Repos:         reposJSONForTest(t, "repo1", "https://github.com/acme/repo1.git", "fix-branch"),
		ProvenanceTag: &tag,
	})
	if err != nil {
		t.Fatalf("create fix session: %v", err)
	}

	if _, err := sandboxStore.Create(ctx, fixSession.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Claim the sentinel_fixes row exactly like reviewverdict.go's own
	// handler would (Claim, then UpdateChildSession once the outbox
	// worker spawns the fix session) -- origin_head_branch is "feature-x",
	// the value this test asserts the fix PR's own Base must equal.
	claimed, err := sentinelFixStore.Claim(ctx, "acme/repo1", 7, originSession.ID, "feature-x")
	if err != nil {
		t.Fatalf("claim sentinel_fixes row: %v", err)
	}
	if _, err := sentinelFixStore.UpdateChildSession(ctx, claimed.ID, fixSession.ID); err != nil {
		t.Fatalf("update sentinel_fixes child session: %v", err)
	}

	// wantRepoDefaultBranch is what the fake SourceControl would resolve
	// the repo's OWN default branch to, via ResolveBranchSHA -- deliberately
	// NOT "feature-x", so a test failure (Base == wantRepoDefaultBranch)
	// would unambiguously mean the dedicated path fell through to the
	// ordinary resolvePRBaseBranch resolution instead of using the
	// literal, already-captured origin_head_branch.
	const wantRepoDefaultBranch = "main"
	const wantBotToken = "bot-static-token"
	sourceControl := &fakeSourceControl{
		nextRef:           ports.PRRef{Number: 99, URL: "https://github.com/acme/repo1/pull/99"},
		defaultBranchName: wantRepoDefaultBranch,
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false, RegistryOptions{GitHubOutbound: platform.MustNewGitHubOutboundConfig(wantBotToken)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, fixSession.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, fixSession.ID.String(), 1, "repo1", "fix-branch", "abc123"),
	})

	waitUntil(t, 5*time.Second, func() bool {
		return sourceControl.callCount() == 1
	})

	// The central invariant: NEVER resolvePRBaseBranch for this session.
	if got := sourceControl.shaCallCount(); got != 0 {
		t.Errorf("ResolveBranchSHA called %d times, want 0 -- a sentinel-auto-fix session must NEVER route through resolvePRBaseBranch", got)
	}

	spec := sourceControl.lastSpec()
	if spec.Base != "feature-x" {
		t.Errorf("CreatePRSpec.Base = %q, want %q (the origin PR's own head branch)", spec.Base, "feature-x")
	}
	if spec.Base == wantRepoDefaultBranch {
		t.Errorf("CreatePRSpec.Base = %q, want it to NEVER be the repo's own default branch (%q)", spec.Base, wantRepoDefaultBranch)
	}
	if spec.Head != "fix-branch" {
		t.Errorf("CreatePRSpec.Head = %q, want %q (the fix session's own pushed branch)", spec.Head, "fix-branch")
	}
	if spec.Token != wantBotToken {
		t.Errorf("CreatePRSpec.Token = %q, want the static bot token %q (a sentinel-auto-fix session has no human creator to decrypt an OAuth token for)", spec.Token, wantBotToken)
	}

	// Stack registration: a SECOND call, after CreatePR, with both PR
	// numbers bottom-to-top (§17.2/§17.6).
	waitUntil(t, 5*time.Second, func() bool {
		return sourceControl.registerStackCallCount() == 1
	})
	stackSpec := sourceControl.lastRegisterStackSpec()
	if len(stackSpec.PRNumbers) != 2 || stackSpec.PRNumbers[0] != 7 || stackSpec.PRNumbers[1] != 99 {
		t.Errorf("RegisterPRStackSpec.PRNumbers = %v, want [7, 99] (origin, then fix, bottom to top)", stackSpec.PRNumbers)
	}

	waitUntil(t, 5*time.Second, func() bool {
		row, err := sentinelFixStore.GetByID(ctx, claimed.ID)
		return err == nil && row.Status == "fix_open" && row.FixPrNumber != nil && *row.FixPrNumber == 99 && row.StackRegistered
	})
}

// TestHandleSandboxEvent_PushComplete_SentinelFixPR_HonorsFrozenShadowStamp
// closes a gap the sentinel path had by construction: it was dispatched
// BEFORE the frozen push/PR decision was ever read, so §30.8's stamp and
// §30.4's demotion cancellation simply did not apply to it.
//
// Having no human creator to attribute a PR to is a reason for a separate
// PR-BUILDING path. It is not a reason to sit outside the egress gate --
// and this path is the one that opens a pull request with the shared BOT
// token, so the consequence of missing the gate is a real PR in a
// customer's repository from a cycle already decided to be shadow.
//
// The repo here is promoted to live, so a re-read would say "create it".
// The frozen stamp says shadow. The stamp must win.
func TestHandleSandboxEvent_PushComplete_SentinelFixPR_HonorsFrozenShadowStamp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionStore := narvipg.NewSessionStore(pool)
	sandboxStore := narvipg.NewSandboxStore(pool)
	sentinelFixStore := narvipg.NewSentinelFixStore(pool)

	originSession, err := sessionStore.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	tag := provenance.SentinelAutoFix
	fixSession, err := sessionStore.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource:   sqlcgen.SessionSpawnSourceGithub,
		Repos:         reposJSONForTest(t, "repo1", "https://github.com/acme/sentinel-frozen.git", "fix-branch"),
		ProvenanceTag: &tag,
	})
	if err != nil {
		t.Fatalf("create fix session: %v", err)
	}
	if _, err := sandboxStore.Create(ctx, fixSession.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Promoted to live: a re-read of the CURRENT mode would say "create a
	// real PR". Only the frozen stamp says otherwise.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/sentinel-frozen", true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	claimed, err := sentinelFixStore.Claim(ctx, "acme/sentinel-frozen", 7, originSession.ID, "feature-x")
	if err != nil {
		t.Fatalf("claim sentinel_fixes row: %v", err)
	}
	if _, err := sentinelFixStore.UpdateChildSession(ctx, claimed.ID, fixSession.ID); err != nil {
		t.Fatalf("update sentinel_fixes child session: %v", err)
	}

	// The frozen decision this turn resolved at push-send time.
	shadow := true
	if _, err := sandboxStore.SetPendingPush(ctx, sqlcgen.SetSandboxPendingPushParams{
		SessionID: fixSession.ID, PendingPushSuppressedInShadow: &shadow,
	}); err != nil {
		t.Fatalf("persist the push/PR decision: %v", err)
	}

	sourceControl := &fakeSourceControl{
		nextRef:           ports.PRRef{Number: 99, URL: "https://github.com/acme/sentinel-frozen/pull/99"},
		defaultBranchName: "main",
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false, RegistryOptions{GitHubOutbound: platform.MustNewGitHubOutboundConfig("bot-static-token")})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, fixSession.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete", Gen: 1,
		Raw: pushCompleteRaw(t, fixSession.ID.String(), 1, "repo1", "fix-branch", "abc123"),
	})

	// Wait for the path to reach EITHER outcome, then say which it was --
	// waiting only on the suppressed count would fail with a bare timeout
	// and hide the fact that a real PR was created instead.
	waitUntil(t, 5*time.Second, func() bool {
		return sourceControl.callCount()+sourceControl.suppressCallCount() >= 1
	})
	if got := sourceControl.callCount(); got != 0 {
		t.Fatalf("CreatePR called %d times, want 0: the frozen stamp said shadow, and this path opened a REAL pull request with the bot token because a later promotion was re-read", got)
	}
	if got := sourceControl.suppressCallCount(); got != 1 {
		t.Errorf("SuppressCreatePR called %d times, want 1: the sentinel-fix PR must be suppressed AND recorded, never silently skipped", got)
	}
}
