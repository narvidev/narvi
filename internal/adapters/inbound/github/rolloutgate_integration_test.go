//go:build integration

// This file proves §10's own per-channel refusal contract (§10 Phase
// 6, §32) for GitHub specifically: a rollout refusal must take the
// permanent-denial idiom -- acknowledge (200) WITHOUT releasing the
// webhook-delivery claim, and post NO reply on the PR thread at all,
// STRICTER than the unlinked-actor branch (which DOES reply) -- mirrors
// denyunlinkedactors_integration_test.go's own TestGitHubIntegration_
// LinkedButDeniedCommenter_NoSignInReply (no reply) and
// TestGitHubIntegration_UnlinkedCommenter_DeniedOnUntrackedPR_ReplyPosted's
// own claim-row assertion pattern (webhook_deliveries kept) exactly, one
// refusal reason further. Also proves the WINNER path's own
// github_pr_sessions claim row is never orphaned (rolled back along with
// the refused session), mirroring TestGitHubIntegration_
// DeniedWinnerAttempt_ClaimRowNotOrphaned -- this is what makes §32.6's
// own "REST enrollment is structurally impossible for exactly the repos
// rollout needs to enroll" claim true.
package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/platform"
)

// newRolloutTestRig mirrors newTestRig (handler_integration_test.go)
// exactly, with SessionCoalescer.RolloutMode/RepoSettings additionally
// set and cfg.Comments wired to a fakeCommentPoster this func returns
// alongside the rig -- every ORIGINAL newTestRig caller stays on
// rollout.ModeOpen (SessionCoalescer's own zero value), proven not to
// change any of their behavior by this package's own pre-existing tests
// continuing to pass unmodified. Also returns repoSettings, so a test can
// enroll/de-enroll repos against the SAME store instance the coalescer
// itself reads.
func newRolloutTestRig(t *testing.T, mode platform.RolloutMode) (testRig, *narvipg.RepoSettingsStore, *fakeCommentPoster) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	rig := testRig{
		pool:        pool,
		turns:       narvipg.NewTurnStore(pool),
		plans:       narvipg.NewPlanStore(pool),
		users:       narvipg.NewUserStore(pool),
		identities:  narvipg.NewIdentityStore(pool),
		linkNotices: narvipg.NewGitHubActorLinkNoticeStore(pool),
	}

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	poster := &fakeCommentPoster{}

	coalescer := &githubingress.SessionCoalescer{
		Pool:         pool,
		PRSessions:   narvipg.NewGitHubPRSessionStore(pool),
		Sessions:     narvipg.NewSessionStore(pool),
		Turns:        rig.turns,
		Environments: narvipg.NewEnvironmentStore(pool),
		Registry:     registry,
		SessionGuard: turnguard.New(pool, nil, false),
		AuditLog:     narvipg.NewAuditLogStore(pool),
		Plans:        rig.plans,
		Identities:   rig.identities,
		Users:        rig.users,
		Participants: narvipg.NewParticipantStore(pool),
		RolloutMode:  mode,
		RepoSettings: repoSettings,
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	cfg := githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
		LinkNotices:   rig.linkNotices,
		Comments:      poster,
		Outbound:      platform.MustNewGitHubOutboundConfig("test-bot-token"),
		PublicBaseURL: testPublicBaseURL,
		Timeouts:      platform.DefaultTimeouts(),
	}

	handler, handlerErr := githubingress.NewHandler(coalescer, deliveries, cfg)
	if handlerErr != nil {
		t.Fatalf("githubingress.NewHandler: %v", handlerErr)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)

	return rig, repoSettings, poster
}

// TestGitHubIntegration_RolloutRefusal_SilentNoReplyClaimKept is the
// MUTATION-TESTABLE guard for §32's own GitHub refusal contract: rollout
// mode is armed to cohort and the mentioned repo is NEVER enrolled, so the
// WINNER path's own httpapi.CreateSessionOnTx call refuses with
// CreateSessionError.RolloutRefusal == true, mapped to coalesce.go's own
// ErrRolloutNotEnrolled. Proves, in one HTTP round trip: (1) status 200,
// never 500 (acknowledged, not retried); (2) NO comment posted on the PR
// (§32's own "zero platform egress" requirement -- stricter than the
// unlinked-actor branch, which DOES reply); (3) the webhook-delivery claim
// is KEPT (a redelivery of the SAME delivery id must be treated as an
// already-claimed duplicate); (4) the github_pr_sessions claim row is
// NEVER orphaned -- EnsureRow's own insert rolls back along with the
// refused session, exactly like a denied WINNER attempt already does for
// an unlinked actor.
func TestGitHubIntegration_RolloutRefusal_SilentNoReplyClaimKept(t *testing.T) {
	ctx := context.Background()
	rig, _, poster := newRolloutTestRig(t, platform.RolloutModeCohort)

	const repoFullName = "acme/rollout-refused-repo"
	const cloneURL = "https://github.com/acme/rollout-refused-repo.git"
	const prNumber = 950
	const commenterID = 90090100

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter(repoFullName, "rollout-refused-repo", cloneURL, prNumber, "rollout-refused-mention", commenterID, "rollout-refused-user")
	const deliveryID = "delivery-rollout-refused-1"

	status := postWebhook(t, rig, body, deliveryID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (acknowledged, not retried)", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%rollout-refused-repo%'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (an unenrolled repo must never get a session)", sessionCount)
	}

	if len(poster.calls) != 0 {
		t.Errorf("len(poster.calls) = %d, want 0 -- §32's own \"zero platform egress\" requirement: an unenrolled repo must get NO reply at all, not even an honest one", len(poster.calls))
	}

	var deliveryRowCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'github' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries rows: %v", err)
	}
	if deliveryRowCount != 1 {
		t.Errorf("webhook_deliveries row count = %d, want exactly 1 (claim must NOT be released -- a redelivery would only reproduce this same refusal)", deliveryRowCount)
	}

	var claimRowCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, prNumber,
	).Scan(&claimRowCount); err != nil {
		t.Fatalf("count github_pr_sessions claim rows: %v", err)
	}
	if claimRowCount != 0 {
		t.Fatalf("github_pr_sessions claim row count = %d, want 0 (§32.6: EnsureRow's own insert must roll back along with the refusal, never orphaned -- this is what makes REST enrollment structurally impossible for exactly this repo)", claimRowCount)
	}
}

// TestGitHubIntegration_RolloutGate_EnrolledRepoStillCreatesSession is
// the refusal test's own positive control: the IDENTICAL setup, except
// the repo IS enrolled -- proves cohort mode is a real, bidirectional
// gate here too.
func TestGitHubIntegration_RolloutGate_EnrolledRepoStillCreatesSession(t *testing.T) {
	ctx := context.Background()
	rig, repoSettings, _ := newRolloutTestRig(t, platform.RolloutModeCohort)

	const repoFullName = "acme/rollout-enrolled-repo"
	const cloneURL = "https://github.com/acme/rollout-enrolled-repo.git"
	const prNumber = 951
	const commenterID = 90090101

	if _, err := repoSettings.UpsertSessionsEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter(repoFullName, "rollout-enrolled-repo", cloneURL, prNumber, "rollout-enrolled-mention", commenterID, "rollout-enrolled-user")
	status := postWebhook(t, rig, body, "delivery-rollout-enrolled-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%rollout-enrolled-repo%'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("session count = %d, want 1 -- an enrolled repo must not be refused", sessionCount)
	}
}
