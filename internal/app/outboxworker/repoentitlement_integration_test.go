//go:build integration

// This file proves technical plan §31.4's "Un-entitlement" for the
// sentinel auto-fix outbox notifier: a fix whose origin pull request's
// repository an administrator revoked is skipped terminally -- Deliver
// returns nil, so the outbox never retries it to a dead letter -- no fix
// branch is created on the repository, no child session is spawned, and
// the addressed finding stays open.
package outboxworker_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// TestSentinelAutoFixNotifier_RevokedRepo_SkipsTerminallyNeverRetried: the
// origin repository is revoked, and the fix is cloned from a fork whose
// clone URL no revocation names -- the base repository's revocation still
// refuses it. Delivered twice (a redelivery), it is skipped both times
// with nothing done.
func TestSentinelAutoFixNotifier_RevokedRepo_SkipsTerminallyNeverRetried(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	const repoFullName = "acme/revoked-autofix-repo"
	const prNumber = 152
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("EnsureRow: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, originSession.ID); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	if _, err := narvipg.NewRepoEntitlementRevocationStore(pool).Revoke(ctx, repoFullName, pgtype.UUID{}, "frozen by an administrator"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	fix, err := sentinelFixes.Claim(ctx, repoFullName, prNumber, originSession.ID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	const identityHash = "152152152152152152152152152152152152152152152152152152152152aa"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	notifier := mustNotifier(outboxworker.NewSentinelAutoFixNotifier(pool, sessions, narvipg.NewTurnStore(pool), narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), registry, sentinelFixes, reviewFindings,
		sourceControl, platform.MustNewGitHubOutboundConfig("gh-fake-bot-token"), platform.DefaultTimeouts(), false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(pool), prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool)))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        prNumber,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me",
		RepoName:              "revoked-autofix-repo",
		RepoCloneURL:          "https://github.com/contributor/revoked-autofix-repo.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
			t.Fatalf("Deliver() attempt %d error = %v, want nil -- a revoked repository is a terminal skip, never retried", attempt, err)
		}
	}

	if got := sourceControl.createBranchCallCount(); got != 0 {
		t.Errorf("CreateBranch called %d times, want 0: a revoked repository gets no fix branch", got)
	}
	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if updatedFix.FixChildSessionID.Valid {
		t.Errorf("FixChildSessionID = %v, want invalid: no fix child session for a revoked repository", updatedFix.FixChildSessionID)
	}
	finding, err := reviewFindings.Get(ctx, repoFullName, prNumber, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "open" {
		t.Errorf("finding Status = %q, want open", finding.Status)
	}
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("sessions = %d, want the origin alone", sessionCount)
	}
	var denials int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'session.repo_entitlement_denied' AND resource_id = $1 AND detail_json->>'reason' = 'revoked'`, repoFullName).Scan(&denials); err != nil {
		t.Fatalf("count denial audit rows: %v", err)
	}
	if denials != 2 {
		t.Errorf("revoked denial audit rows = %d, want one per delivery", denials)
	}
}
