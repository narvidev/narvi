//go:build integration

// Integration tests for §8.2's ("sentinels + suggestions", §17.4/§17.5)
// own merge-gating `pull_request`/action=="closed" webhook lane
// (pullrequestevent.go), against a real Postgres instance -- gated behind
// the "integration" build tag, sharing this package's own test helpers
// (handler_integration_test.go: newTestPool/sign/postWebhookEventType).
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/platform"
)

// pullRequestClosedBody builds a real-shaped `pull_request` webhook
// payload with action=="closed" and the given merged value.
func pullRequestClosedBody(repoFullName string, prNumber int, merged bool) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "closed",
		"repository": map[string]any{
			"full_name": repoFullName,
		},
		"pull_request": map[string]any{
			"number": prNumber,
			"merged": merged,
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// newSentinelFixTestServer builds a minimal githubingress.NewHandler-backed
// httptest.Server directly on pool (an externally-supplied, already-
// migrated pool from newTestPool(t)) -- deliberately NOT going through
// this file's own newTestRig (handler_integration_test.go), which builds
// its OWN pool internally and returns it only via the already-constructed
// testRig, too late for a mutate func (which only ever sees *Config) to
// thread pool-derived stores into. Returns just the server URL --
// postWebhookEventType (handler_integration_test.go) only ever needs
// rig.server.URL, so a bare testRig{server: ...} wrapping this suffices.
func newSentinelFixTestServer(t *testing.T, pool *pgxpool.Pool, sentinelFixes *narvipg.SentinelFixStore, repoSettings *narvipg.RepoSettingsStore, auditLog *narvipg.AuditLogStore) testRig {
	t.Helper()

	coalescer := &githubingress.SessionCoalescer{
		Pool:         pool,
		SessionGuard: turnguard.New(pool, nil, false),
		PRSessions:   narvipg.NewGitHubPRSessionStore(pool),
		Sessions:     narvipg.NewSessionStore(pool),
		Turns:        narvipg.NewTurnStore(pool),
		Environments: narvipg.NewEnvironmentStore(pool),
		AuditLog:     auditLog,
		Plans:        narvipg.NewPlanStore(pool),
		Identities:   narvipg.NewIdentityStore(pool),
		Users:        narvipg.NewUserStore(pool),
		Participants: narvipg.NewParticipantStore(pool),
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	cfg := githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
		Outbound:      platform.MustNewGitHubOutboundConfig("test-bot-token"),
		SentinelFixes: sentinelFixes,
		RepoSettings:  repoSettings,
		AuditLog:      auditLog,
	}

	handler, handlerErr := githubingress.NewHandler(coalescer, deliveries, cfg)
	if handlerErr != nil {
		t.Fatalf("githubingress.NewHandler: %v", handlerErr)
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return testRig{server: server}
}

// createSentinelFixTestSession creates a minimal, real session row --
// standing in for either an origin review session or a fix child
// session; this file's own tests only ever reference these rows by id.
func createSentinelFixTestSession(ctx context.Context, t *testing.T, sessions *narvipg.SessionStore) sqlcgen.Session {
	t.Helper()
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	return session
}

func TestPullRequestClosed_NoSentinelFix_NoOp(t *testing.T) {
	pool := newTestPool(t)
	rig := newSentinelFixTestServer(t, pool, narvipg.NewSentinelFixStore(pool), narvipg.NewRepoSettingsStore(pool), narvipg.NewAuditLogStore(pool))

	body := pullRequestClosedBody("acme/no-fix-repo", 100, true)
	status := postWebhookEventType(t, rig, body, "pr-closed-delivery-1", "pull_request")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
}

func TestPullRequestClosed_MergedFalse_MarksAbandoned(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sessions := narvipg.NewSessionStore(pool)

	originSession := createSentinelFixTestSession(ctx, t, sessions)
	repoFullName := "acme/abandon-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 101, originSession.ID, "feature-x")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	childSession := createSentinelFixTestSession(ctx, t, sessions)
	if _, err := sentinelFixes.UpdateChildSession(ctx, fix.ID, childSession.ID); err != nil {
		t.Fatalf("update child session: %v", err)
	}
	if _, err := sentinelFixes.UpdateOpened(ctx, fix.ID, 202); err != nil {
		t.Fatalf("update opened: %v", err)
	}

	rig := newSentinelFixTestServer(t, pool, sentinelFixes, repoSettings, auditLog)

	body := pullRequestClosedBody(repoFullName, 101, false)
	status := postWebhookEventType(t, rig, body, "pr-closed-delivery-2", "pull_request")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status != "abandoned" {
		t.Errorf("Status = %q, want %q (origin PR closed without merging, §17.5)", got.Status, "abandoned")
	}
}

// TestPullRequestClosed_Merged_NeverIncorrectlyMerges_RecordsAudit proves
// this Step's own central safety property: given its own honest,
// not-yet-implemented CI-status/mergeable-cleanly checks
// (pullrequestevent.go's own top doc comment), this lane NEVER marks a
// sentinel fix merged -- it always safely denies, recording exactly one
// §17.5 audit_log row explaining why, and leaves the fix PR as an
// ordinary needs_review item rather than ever forcing an incorrect merge.
func TestPullRequestClosed_Merged_NeverIncorrectlyMerges_RecordsAudit(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sessions := narvipg.NewSessionStore(pool)

	repoFullName := "acme/merge-gate-repo"
	if _, err := repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
		t.Fatalf("upsert repo settings: %v", err)
	}

	originSession := createSentinelFixTestSession(ctx, t, sessions)
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 110, originSession.ID, "feature-y")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	childSession := createSentinelFixTestSession(ctx, t, sessions)
	if _, err := sentinelFixes.UpdateChildSession(ctx, fix.ID, childSession.ID); err != nil {
		t.Fatalf("update child session: %v", err)
	}
	if _, err := sentinelFixes.UpdateOpened(ctx, fix.ID, 211); err != nil {
		t.Fatalf("update opened: %v", err)
	}

	rig := newSentinelFixTestServer(t, pool, sentinelFixes, repoSettings, auditLog)

	body := pullRequestClosedBody(repoFullName, 110, true)
	status := postWebhookEventType(t, rig, body, "pr-closed-delivery-3", "pull_request")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status == "fix_merged" {
		t.Errorf("Status = %q, want it to NEVER auto-merge given this Step's own honest, not-yet-implemented CI/mergeable checks", got.Status)
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE resource_type = 'sentinel_fix' AND resource_id = $1`, fix.ID.String()).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit_log rows = %d, want exactly 1 (the merge-gate-evaluated record, §17.5)", auditCount)
	}
}
