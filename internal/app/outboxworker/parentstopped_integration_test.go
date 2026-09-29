//go:build integration

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

// TestSentinelAutoFixNotifier_ParentStopped_SkipsTerminallyNeverRetried: a
// person stopped the review session a fix would be a child of (technical
// plan §3.3), so httpapi.CreateSessionOnTx refuses the child with
// CreateSessionError.ParentStopped. The refusal lasts until a person
// resumes that session, never until a redelivery, so Deliver takes the
// terminal skip the rollout refusal takes: it returns nil, no child session
// exists, and the addressed finding stays 'open'.
func TestSentinelAutoFixNotifier_ParentStopped_SkipsTerminallyNeverRetried(t *testing.T) {
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

	origin, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET stop_requested_at = now() WHERE id = $1`, origin.ID); err != nil {
		t.Fatalf("stop the origin session: %v", err)
	}

	repoFullName := "example-org/stopped-parent-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 79, origin.ID, "feature-stopped")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	const identityHash = "abc789abc789abc789abc789abc789abc789abc789abc789abc789abc789ab"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName, PrNumber: 79, IdentityHash: identityHash,
		Severity: "medium", FilePath: "internal/foo/bar.go", Description: "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, narvipg.NewTurnStore(pool), narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), registry, sentinelFixes, reviewFindings,
		&fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(pool), narvipg.NewGitHubPRSessionStore(pool),
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        79,
		OriginReviewSessionID: origin.ID.String(),
		OriginHeadBranch:      "feature-stopped",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/example-org/stopped-parent-repo.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() = %v, want nil: a stopped parent is a terminal skip, never retried", err)
	}

	var children int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE parent_session_id = $1`, origin.ID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("%d child sessions of a stopped parent, want none", children)
	}
	updated, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if updated.FixChildSessionID.Valid {
		t.Errorf("FixChildSessionID = %v, want none recorded", updated.FixChildSessionID)
	}
	finding, err := reviewFindings.Get(ctx, repoFullName, 79, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "open" {
		t.Errorf("finding status = %q, want open: no fix is under way", finding.Status)
	}
}
