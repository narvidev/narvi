//go:build integration

// This file proves §10's own per-channel refusal contract (§10 Phase
// 6, §32) for the sentinel-autofix outbox notifier specifically: a
// rollout refusal must map to the existing terminal-skip precedent
// (descriptionautofix.go's own "confirmed negative -> nil, never
// retried"), never the outbox's ordinary backoff/retry/dead-letter path
// -- Deliver returns nil, no fix child session is ever spawned, and
// markFindingsFixPending is never called (the addressed finding stays
// 'open', never incorrectly marked 'fix_pending' against a session that
// does not exist). Mirrors sentinelautofix_integration_test.go's own
// TestSentinelAutoFixNotifier_SpawnsChildSessionAndUpdatesStores
// conventions exactly, one refusal reason further.
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
	"github.com/narvidev/narvi/internal/platform"
)

// TestSentinelAutoFixNotifier_RolloutRefusal_SkipsTerminallyNeverRetried
// is the MUTATION-TESTABLE guard for §32's own sentinel-autofix-outbox
// refusal contract: rollout mode is armed to cohort and the origin repo
// is NEVER enrolled, so spawnClaimedChildSession's own httpapi.
// CreateSessionOnTx call refuses with CreateSessionError.RolloutRefusal
// == true, mapped to errRolloutRefused. Proves: (1) Deliver returns nil
// (never a real error -- the outbox marks this row delivered, never
// retried, mirroring descriptionautofix.go's own identical "confirmed
// negative" precedent); (2) sentinel_fixes.fix_child_session_id stays
// invalid (no child session was ever spawned); (3) the addressed
// finding's own status stays 'open', NEVER 'fix_pending' -- proving
// markFindingsFixPending was never called against a session that does
// not exist.
func TestSentinelAutoFixNotifier_RolloutRefusal_SkipsTerminallyNeverRetried(t *testing.T) {
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

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	// Deliberately NEVER enrolled.
	repoFullName := "acme/rollout-refused-autofix-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 78, originSession.ID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const identityHash = "def456def456def456def456def456def456def456def456def456def456de"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     78,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeCohort, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        78,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/rollout-refused-autofix-repo.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil -- a rollout refusal is a confirmed-negative terminal skip, never retried", err)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if updatedFix.FixChildSessionID.Valid {
		t.Errorf("FixChildSessionID = %v, want invalid -- no fix child session may ever be spawned for an unenrolled repo", updatedFix.FixChildSessionID)
	}

	finding, err := reviewFindings.Get(ctx, repoFullName, 78, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "open" {
		t.Errorf("finding Status = %q, want %q -- markFindingsFixPending must never run against a refused claim (no real session exists to record)", finding.Status, "open")
	}
	if finding.FixChildSessionID.Valid {
		t.Errorf("finding FixChildSessionID = %v, want invalid", finding.FixChildSessionID)
	}

	// createFixBranch (a real outbound GitHub call) runs BEFORE the
	// claim-and-spawn transaction the rollout gate lives on (Deliver's own
	// doc comment: "OUTSIDE any transaction ... then atomically claims ...
	// and spawns ... on ONE transaction") -- so the branch IS created even
	// though the session spawn that would have used it is refused. This is
	// not a §10 regression: identical for ANY CreateSessionOnTx
	// failure past this point (a generic validation error would leave the
	// same orphaned branch). What §10 specifically guarantees is the
	// ONE thing downstream of that: no false 'fix_pending' finding, no
	// spawned session, no retry storm -- asserted above.
	if got := sourceControl.createBranchCallCount(); got != 1 {
		t.Errorf("CreateBranch called %d times, want 1 (createFixBranch runs before the rollout gate, unrelated to this Step)", got)
	}
}

// TestSentinelAutoFixNotifier_RolloutGate_EnrolledRepoStillSpawns is the
// refusal test's own positive control: the IDENTICAL setup, except the
// repo IS enrolled -- proves cohort mode is a real, bidirectional gate
// here too.
func TestSentinelAutoFixNotifier_RolloutGate_EnrolledRepoStillSpawns(t *testing.T) {
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

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/rollout-enrolled-autofix-repo"
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}

	fix, err := sentinelFixes.Claim(ctx, repoFullName, 79, originSession.ID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const identityHash = "111111222222333333444444555555666666777777888888999999aaaaaaaa"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     79,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeCohort, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        79,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/rollout-enrolled-autofix-repo.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if !updatedFix.FixChildSessionID.Valid {
		t.Error("FixChildSessionID is invalid, want a real spawned child session -- an enrolled repo must not be refused")
	}
}
