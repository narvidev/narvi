//go:build integration

// This file proves §30.7's own state-coherence resolution for the
// sentinel-auto-fix lane, per §30.9 (resolved: no git mirror --
// short-circuit before the claim, done properly): with the origin repo's
// egress suppressed, Deliver must stop BEFORE spawnClaimedChildSession
// ever runs -- never creating a branch nobody can really check out, never
// spawning a child session pinned to it, and never marking the addressed
// finding fix_pending (which would also disable the manual
// apply-suggestion action, §17.3, for a fix nothing is actually working
// on). Instead it records ONE ledger row naming what the lane would have
// done and returns nil -- a terminal, never-retried decision, mirroring
// rolloutgate_integration_test.go's own identical "confirmed negative"
// shape for the sibling refusal reason.
package outboxworker_test

import (
	"context"
	"encoding/json"
	"testing"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowledger"
	"github.com/narvidev/narvi/internal/platform"
)

// TestSentinelAutoFixNotifier_ShadowRepo_ShortCircuitsBeforeClaim_NeverFixPending
// is the MUTATION-TESTABLE guard for §30.7/§30.9's own resolved sentinel-
// fix short-circuit. Proves: (1) Deliver returns nil (never retried); (2)
// no branch is ever created and no child session is ever spawned
// (createBranchCallCount/shaCallCount both 0 -- stronger than the rollout-
// refusal sibling test, where createFixBranch already ran before that
// gate); (3) the addressed finding's own status stays 'open', never
// 'fix_pending'; (4) exactly one shadow_scm_writes row records the
// branch that would have been created and that a fix child session would
// have followed it.
func TestSentinelAutoFixNotifier_ShadowRepo_ShortCircuitsBeforeClaim_NeverFixPending(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	shadowLedgerStore := narvipg.NewShadowSCMWriteStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	const repoFullName = "acme/shadow-sentinel-fix-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 81, originSession.ID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const identityHash = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     81,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	shadowRepo := func(context.Context, string) bool { return false }
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		shadowRepo, shadowLedgerStore)

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        81,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/shadow-sentinel-fix-repo.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil -- a shadow repo is a confirmed, terminal decision for this delivery, never retried", err)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if updatedFix.FixChildSessionID.Valid {
		t.Errorf("FixChildSessionID = %v, want invalid -- no fix child session may ever be spawned for a shadow repo", updatedFix.FixChildSessionID)
	}

	finding, err := reviewFindings.Get(ctx, repoFullName, 81, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "open" {
		t.Errorf("finding Status = %q, want %q -- markFindingsFixPending must never run against a short-circuited claim (no real session exists to record)", finding.Status, "open")
	}
	if finding.FixChildSessionID.Valid {
		t.Errorf("finding FixChildSessionID = %v, want invalid", finding.FixChildSessionID)
	}

	// Unlike the rollout-refusal sibling test (rolloutgate_integration_test.go),
	// where createFixBranch already runs BEFORE that gate, this check sits
	// BEFORE spawnClaimedChildSession is ever called at all -- so neither
	// of createFixBranch's own two real outbound calls ever happens.
	if got := sourceControl.shaCallCount(); got != 0 {
		t.Errorf("ResolveBranchSHA called %d times, want 0 -- the shadow check runs before createFixBranch is ever called", got)
	}
	if got := sourceControl.createBranchCallCount(); got != 0 {
		t.Errorf("CreateBranch called %d times, want 0 -- the shadow check runs before createFixBranch is ever called", got)
	}

	rows, err := shadowLedgerStore.ListForRepo(ctx, repoFullName, 10)
	if err != nil {
		t.Fatalf("ListForRepo: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows for %s = %d, want 1", repoFullName, len(rows))
	}
	row := rows[0]
	if row.Operation != "sentinel_auto_fix" {
		t.Errorf("Operation = %q, want %q", row.Operation, "sentinel_auto_fix")
	}
	if row.ResultJson != nil {
		t.Errorf("ResultJson = %s, want nil -- nothing was invented in place of the branch/session", row.ResultJson)
	}
	var spec shadowledger.SentinelAutoFix
	if err := json.Unmarshal(row.SpecJson, &spec); err != nil {
		t.Fatalf("unmarshal spec_json: %v", err)
	}
	if spec.Owner != "acme" || spec.Repo != "shadow-sentinel-fix-repo" {
		t.Errorf("spec Owner/Repo = %q/%q, want acme/shadow-sentinel-fix-repo", spec.Owner, spec.Repo)
	}
	if spec.OriginPRNumber != 81 {
		t.Errorf("spec OriginPRNumber = %d, want 81", spec.OriginPRNumber)
	}
	if spec.OriginHeadBranch != "feature-fix-me" {
		t.Errorf("spec OriginHeadBranch = %q, want %q", spec.OriginHeadBranch, "feature-fix-me")
	}
	if spec.WouldCreateBranch == "" || spec.WouldCreateBranch == spec.OriginHeadBranch {
		t.Errorf("spec WouldCreateBranch = %q, want a distinct, deterministic fix-branch name", spec.WouldCreateBranch)
	}
	if len(spec.FindingIdentityHashes) != 1 || spec.FindingIdentityHashes[0] != identityHash {
		t.Errorf("spec FindingIdentityHashes = %v, want [%q]", spec.FindingIdentityHashes, identityHash)
	}

	// A redelivery of the SAME outbox row must record the identical
	// suppression again -- Deliver's own "confirmed negative" reasoning
	// applies per DELIVERY, not once globally -- but must still never
	// spawn or claim anything.
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() (redelivery) error = %v, want nil", err)
	}
	rows, err = shadowLedgerStore.ListForRepo(ctx, repoFullName, 10)
	if err != nil {
		t.Fatalf("ListForRepo (after redelivery): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("ledger rows for %s after redelivery = %d, want 2 (one per delivery attempt, exactly like a real branch/session lane would record once per attempt)", repoFullName, len(rows))
	}
	updatedFix, err = sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes (after redelivery): %v", err)
	}
	if updatedFix.FixChildSessionID.Valid {
		t.Error("FixChildSessionID is valid after a redelivery, want it to stay invalid -- a redelivery must never spawn either")
	}
}

// TestSentinelAutoFixNotifier_BornShadowRow_DeliveredAfterPromotion_StillShortCircuits
// is §30.8's epoch discipline for a PASS-THROUGH kind, and it is easy to
// get wrong in exactly one direction.
//
// The outbox builder applies its own enqueue-stamp check only to
// SUPPRESS-classified kinds. sentinel_auto_fix is PASS-THROUGH, so it
// reaches Deliver unconditionally -- and if this notifier consults only
// the CURRENT flag, a row enqueued while the repo was shadow and
// delivered after it was promoted takes the live path. Outbox backlogs
// reach tens of minutes, so that is an ordinary sequence, not a race.
//
// The repo here IS live. Only the row's own stamp says shadow, and it
// must win: §30.8 says a born-shadow row "can only end in the ledger".
func TestSentinelAutoFixNotifier_BornShadowRow_DeliveredAfterPromotion_StillShortCircuits(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	shadowLedgerStore := narvipg.NewShadowSCMWriteStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	const repoFullName = "acme/shadow-sentinel-fix-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 81, originSession.ID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const identityHash = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     81,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	shadowRepo := func(context.Context, string) bool { return true } // promoted since enqueue
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		shadowRepo, shadowLedgerStore)

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        81,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/shadow-sentinel-fix-repo.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload, SuppressedInShadow: true}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil -- a born-shadow row is a confirmed, terminal decision, never retried", err)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if updatedFix.FixChildSessionID.Valid {
		t.Errorf("FixChildSessionID = %v, want invalid -- no fix child session may ever be spawned for a shadow repo", updatedFix.FixChildSessionID)
	}

	finding, err := reviewFindings.Get(ctx, repoFullName, 81, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "open" {
		t.Errorf("finding Status = %q, want %q -- markFindingsFixPending must never run against a short-circuited claim (no real session exists to record)", finding.Status, "open")
	}
	if finding.FixChildSessionID.Valid {
		t.Errorf("finding FixChildSessionID = %v, want invalid", finding.FixChildSessionID)
	}

	// Unlike the rollout-refusal sibling test (rolloutgate_integration_test.go),
	// where createFixBranch already runs BEFORE that gate, this check sits
	// BEFORE spawnClaimedChildSession is ever called at all -- so neither
	// of createFixBranch's own two real outbound calls ever happens.
	if got := sourceControl.shaCallCount(); got != 0 {
		t.Errorf("ResolveBranchSHA called %d times, want 0 -- the shadow check runs before createFixBranch is ever called", got)
	}
	if got := sourceControl.createBranchCallCount(); got != 0 {
		t.Errorf("CreateBranch called %d times, want 0 -- the shadow check runs before createFixBranch is ever called", got)
	}

	rows, err := shadowLedgerStore.ListForRepo(ctx, repoFullName, 10)
	if err != nil {
		t.Fatalf("ListForRepo: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows for %s = %d, want 1", repoFullName, len(rows))
	}
	row := rows[0]
	if row.Operation != "sentinel_auto_fix" {
		t.Errorf("Operation = %q, want %q", row.Operation, "sentinel_auto_fix")
	}
	if row.ResultJson != nil {
		t.Errorf("ResultJson = %s, want nil -- nothing was invented in place of the branch/session", row.ResultJson)
	}
	var spec shadowledger.SentinelAutoFix
	if err := json.Unmarshal(row.SpecJson, &spec); err != nil {
		t.Fatalf("unmarshal spec_json: %v", err)
	}
	if spec.Owner != "acme" || spec.Repo != "shadow-sentinel-fix-repo" {
		t.Errorf("spec Owner/Repo = %q/%q, want acme/shadow-sentinel-fix-repo", spec.Owner, spec.Repo)
	}
	if spec.OriginPRNumber != 81 {
		t.Errorf("spec OriginPRNumber = %d, want 81", spec.OriginPRNumber)
	}
	if spec.OriginHeadBranch != "feature-fix-me" {
		t.Errorf("spec OriginHeadBranch = %q, want %q", spec.OriginHeadBranch, "feature-fix-me")
	}
	if spec.WouldCreateBranch == "" || spec.WouldCreateBranch == spec.OriginHeadBranch {
		t.Errorf("spec WouldCreateBranch = %q, want a distinct, deterministic fix-branch name", spec.WouldCreateBranch)
	}
	if len(spec.FindingIdentityHashes) != 1 || spec.FindingIdentityHashes[0] != identityHash {
		t.Errorf("spec FindingIdentityHashes = %v, want [%q]", spec.FindingIdentityHashes, identityHash)
	}

	// A redelivery of the SAME outbox row must record the identical
	// suppression again -- Deliver's own "confirmed negative" reasoning
	// applies per DELIVERY, not once globally -- but must still never
	// spawn or claim anything.
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload, SuppressedInShadow: true}); err != nil {
		t.Fatalf("Deliver() (redelivery) error = %v, want nil", err)
	}
	rows, err = shadowLedgerStore.ListForRepo(ctx, repoFullName, 10)
	if err != nil {
		t.Fatalf("ListForRepo (after redelivery): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("ledger rows for %s after redelivery = %d, want 2 (one per delivery attempt, exactly like a real branch/session lane would record once per attempt)", repoFullName, len(rows))
	}
	updatedFix, err = sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes (after redelivery): %v", err)
	}
	if updatedFix.FixChildSessionID.Valid {
		t.Error("FixChildSessionID is valid after a redelivery, want it to stay invalid -- a redelivery must never spawn either")
	}
}
