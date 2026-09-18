//go:build integration

// Integration test for coalesce.go's own PhaseQueued emission -- the
// review's GitHub-native result surface (§8.2/§21.1/§21.1b), decision 2's
// "a check published in queued as soon as a pull request enters scope"
// (narrowed, per Deps.Outbox's own doc comment, to "as soon as a review
// is TRIGGERED"). Fourth review round, E2: this WINNER-path emission had
// no positive-path test anywhere in this package -- newTestRig's own
// default SessionCoalescer leaves Outbox nil, so `c.Outbox != nil`
// (coalesce.go) was never true in any pre-existing test here, and the
// call site could be deleted wholesale with this package's own entire
// integration suite still green. This file's own rig wires Outbox for
// real, and a real DiffFetcher so reviewHeadSHA genuinely resolves
// (coalesce.go's own "no head sha, no row" gate would otherwise silently
// skip this emission too, masking the exact same gap a different way).
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestRigWithOutbox mirrors newTestRigWithReviewTriage's own exact
// pattern (same registry/store construction), except the SessionCoalescer
// it builds ALSO carries a real Outbox store -- newTestRig's own default
// wiring leaves SessionCoalescer.Outbox at its nil zero value, which
// coalesce.go's own WINNER path treats as "review-check publishing not
// wired" and silently skips, never exercising the PhaseQueued emission at
// all through this package's webhook ingress.
func newTestRigWithOutbox(t *testing.T) (testRig, *fakeReviewContextFetcher) {
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

	coalescer := &githubingress.SessionCoalescer{
		Pool:         pool,
		PRSessions:   narvipg.NewGitHubPRSessionStore(pool),
		Sessions:     narvipg.NewSessionStore(pool),
		Turns:        rig.turns,
		Environments: narvipg.NewEnvironmentStore(pool),
		Registry:     registry,
		AuditLog:     narvipg.NewAuditLogStore(pool),
		Plans:        rig.plans,
		Identities:   rig.identities,
		Users:        rig.users,
		Participants: narvipg.NewParticipantStore(pool),
		// Outbox -- the ONE addition versus newTestRig: a real store, so
		// the WINNER path's own PhaseQueued emission (coalesce.go) is
		// genuinely exercised through this package's webhook ingress,
		// instead of silently no-op'd by a nil check.
		Outbox: narvipg.NewOutboxStore(pool, false),
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	fetcher := &fakeReviewContextFetcher{}

	cfg := githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
		LinkNotices:   rig.linkNotices,
		BotToken:      "test-bot-token",
		Timeouts:      platform.DefaultTimeouts(),
		PullRequests:  fetcher,
		DiffFetcher:   fetcher,
	}

	handler := githubingress.NewHandler(coalescer, deliveries, cfg)

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)

	return rig, fetcher
}

// TestGitHubIntegration_WinnerMention_EnqueuesQueuedReviewCheck is the
// fourth review round's own E2 pinning test for coalesce.go's PhaseQueued
// emission: a resolved @mention that WINS the per-PR claim (the first
// mention on a not-yet-tracked PR) must enqueue exactly one
// ports.NotificationKindGitHubReviewCheck outbox row, carrying
// reviewcheck.PhaseQueued, in the SAME transaction as the claim itself.
// Deleting the emission call site entirely (coalesce.go's own
// `if reviewHeadSHA != "" && c.Outbox != nil { ... }` block) makes this
// test fail with zero matching outbox rows -- verified by mutation before
// this test existed, restored from a byte-exact copy afterward.
func TestGitHubIntegration_WinnerMention_EnqueuesQueuedReviewCheck(t *testing.T) {
	ctx := context.Background()
	rig, fetcher := newTestRigWithOutbox(t)

	const repoFullName = "acme/queued-check-repo"
	const repoName = "queued-check-repo"
	const cloneURL = "https://github.com/acme/queued-check-repo.git"
	const prNumber = 6100
	const commenterID = 80006100

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	fetcher.pr = githubapi.PullRequest{
		HeadRef: "feature-queued-check",
		HeadSHA: "queuedchecksha1",
		BaseRef: "main",
	}

	body := issueCommentBodyWithCommenter(repoFullName, repoName, cloneURL, prNumber, "queued-check", commenterID, "queued-check-user")
	status := postWebhook(t, rig, body, "delivery-queued-check-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var sessionID, kind, payloadText string
	if err := rig.pool.QueryRow(ctx,
		`SELECT session_id::text, kind, payload::text FROM outbox WHERE kind = $1 ORDER BY created_at DESC LIMIT 1`,
		string(ports.NotificationKindGitHubReviewCheck),
	).Scan(&sessionID, &kind, &payloadText); err != nil {
		t.Fatalf("query outbox: %v (no github_review_check row was ever enqueued by the WINNER mention)", err)
	}

	var payload ports.ReviewCheckPayload
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}

	if payload.Phase != string(reviewcheck.PhaseQueued) {
		t.Errorf("payload.Phase = %q, want %q", payload.Phase, reviewcheck.PhaseQueued)
	}
	if payload.Owner != "acme" || payload.Repo != repoName {
		t.Errorf("payload.Owner/Repo = %q/%q, want acme/%s", payload.Owner, payload.Repo, repoName)
	}
	if payload.PRNumber != prNumber {
		t.Errorf("payload.PRNumber = %d, want %d", payload.PRNumber, prNumber)
	}
	if payload.HeadSHA != "queuedchecksha1" {
		t.Errorf("payload.HeadSHA = %q, want %q", payload.HeadSHA, "queuedchecksha1")
	}
	// PhaseQueued carries no attempt yet (ports.ReviewCheckPayload's own
	// doc comment: "no attempt exists yet") -- asserted so a future
	// regression that starts stamping a bogus AttemptID onto a queued
	// emission is caught here too.
	if payload.AttemptID != "" {
		t.Errorf("payload.AttemptID = %q, want empty (PhaseQueued has no attempt yet)", payload.AttemptID)
	}

	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = $1`, string(ports.NotificationKindGitHubReviewCheck)).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Errorf("github_review_check outbox row count = %d, want exactly 1", outboxCount)
	}
}
