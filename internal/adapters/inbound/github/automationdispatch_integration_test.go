//go:build integration

// End-to-end proof for §8.4 ("automations never dispatch on a real
// webhook") at the adapter boundary: a real POST to /webhooks/github
// results in a real automation_invocations row through the existing
// engine (internal/app/automation), exactly the way a real GitHub webhook
// would. Also covers the two properties §8.4's own PR body must
// report on: deduplication via a REPLAYED delivery (not a helper-function
// assertion), and panic isolation from the @mention pipeline sharing the
// same delivery.
package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// D2 audit fix: the live dispatch path now requires an authorized,
// LINKED sender (actorauthz.AuthorizeLinkedActor) -- every test below that
// expects a real invocation must first link its own sender.id via
// createLinkedGitHubUser (handler_integration_test.go), mirroring every
// OTHER test in this package that already links its own commenter for the
// identical reason (batch fix/deny-unlinked-github-actors).

// createGitHubAutomation inserts an automation with TriggerTypeGitHub and
// one target repo, mirroring internal/app/automation's own
// githubdispatch_integration_test.go createGitHubAutomation helper --
// duplicated here (rather than exported/shared) because that package's
// own testFixture is private to its _test package, exactly like every
// other cross-package test-fixture boundary in this codebase.
func createGitHubAutomation(ctx context.Context, t *testing.T, automations *narvipg.AutomationStore, name string, cfg domainautomation.GitHubTriggerConfig, target domainautomation.Target) sqlcgen.Automation {
	t.Helper()

	reposJSON, err := json.Marshal([]domainautomation.Target{target})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{
		"event": cfg.Event, "action": cfg.Action, "label": cfg.Label,
	})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}

	row, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: name, Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeGithub, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create github automation: %v", err)
	}
	return row
}

func countAutomationInvocations(t *testing.T, rig testRig, automationID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := rig.pool.QueryRow(context.Background(), "SELECT count(*) FROM automation_invocations WHERE automation_id = $1", automationID).Scan(&count); err != nil {
		t.Fatalf("count automation invocations: %v", err)
	}
	return count
}

// TestGitHubIntegration_AutomationDispatchFiresOnRealWebhook is §8.4's
// own required end-to-end proof: a real, correctly-signed `pull_request`/
// "labeled" webhook -- carrying NO bot mention at all -- results in a real
// automation_invocations row, dispatched through the SAME
// invocation/run engine (internal/app/automation) every other trigger
// path (cron, the generic webhook trigger) already uses.
func TestGitHubIntegration_AutomationDispatchFiresOnRealWebhook(t *testing.T) {
	repoFullName := "acme/automation-dispatch-repo"
	cloneURL := "https://github.com/acme/automation-dispatch-repo"

	pool := newTestPool(t)
	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	auto := createGitHubAutomation(context.Background(), t, automations,
		"on labeled automation:run",
		domainautomation.GitHubTriggerConfig{Event: "pull_request", Action: "labeled", Label: "automation:run"},
		domainautomation.Target{Name: "repo", URL: cloneURL},
	)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Automations = automations
		cfg.AutomationInvocations = invocations
	})

	const senderID = 90000001
	createLinkedGitHubUser(context.Background(), t, rig.users, rig.identities, senderID, sqlcgen.UserRoleMember)

	body := pullRequestLabeledBody(repoFullName, "automation-dispatch-repo", cloneURL, 42, "automation:run", senderID, "some-sender")
	status := postWebhookEventType(t, rig, body, "delivery-automation-dispatch-1", "pull_request")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got := countAutomationInvocations(t, rig, auto.ID); got != 1 {
		t.Fatalf("automation_invocations for automation = %d, want 1", got)
	}
}

// TestGitHubIntegration_AutomationDispatchDedupesRedeliveredDelivery
// replays the EXACT SAME delivery (identical X-GitHub-Delivery id) a
// second time and asserts exactly one invocation exists -- proving
// dedup by actually replaying a delivery, not by asserting a helper
// function returns true twice. This reuses the SAME webhookDeliveryStore.
// Claim/Release mechanism the @mention pipeline already relies on for its
// own dedup (see githubingress's own doc.go) -- §8.4 does not invent a
// second dedup mechanism.
func TestGitHubIntegration_AutomationDispatchDedupesRedeliveredDelivery(t *testing.T) {
	repoFullName := "acme/automation-dedup-repo"
	cloneURL := "https://github.com/acme/automation-dedup-repo"

	pool := newTestPool(t)
	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	auto := createGitHubAutomation(context.Background(), t, automations,
		"on labeled automation:run (dedup)",
		domainautomation.GitHubTriggerConfig{Event: "pull_request", Action: "labeled", Label: "automation:run"},
		domainautomation.Target{Name: "repo", URL: cloneURL},
	)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Automations = automations
		cfg.AutomationInvocations = invocations
	})

	const senderID = 90000002
	createLinkedGitHubUser(context.Background(), t, rig.users, rig.identities, senderID, sqlcgen.UserRoleMember)

	body := pullRequestLabeledBody(repoFullName, "automation-dedup-repo", cloneURL, 43, "automation:run", senderID, "some-sender")
	const deliveryID = "delivery-automation-dedup-1"

	first := postWebhookEventType(t, rig, body, deliveryID, "pull_request")
	if first != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", first, http.StatusOK)
	}
	if got := countAutomationInvocations(t, rig, auto.ID); got != 1 {
		t.Fatalf("after first delivery: automation_invocations = %d, want 1", got)
	}

	// A REAL redelivery: identical body, identical X-GitHub-Delivery id --
	// GitHub's own manual-redelivery shape (see handler.go's own L4 audit
	// fix comment on this exact scenario).
	second := postWebhookEventType(t, rig, body, deliveryID, "pull_request")
	if second != http.StatusOK {
		t.Fatalf("redelivered status = %d, want %d", second, http.StatusOK)
	}
	if got := countAutomationInvocations(t, rig, auto.ID); got != 1 {
		t.Fatalf("after redelivered delivery: automation_invocations = %d, want 1 (dedup must prevent a second invocation)", got)
	}
}

// panicAutomationLister deliberately panics on ListActiveGitHubAutomations
// -- injected via githubingress.Config.Automations (typed as the narrow
// automation.GitHubTriggerLister interface specifically so a test can
// substitute a fake, per that field's own doc comment) to prove
// dispatchAutomationsBestEffort's own panic recovery actually isolates a
// defect in automation dispatch from the @mention pipeline sharing the
// same delivery.
type panicAutomationLister struct{}

func (panicAutomationLister) ListActiveGitHubAutomations(ctx context.Context) ([]sqlcgen.Automation, error) {
	panic("forced panic: automation dispatch must not suppress the mention pipeline")
}

// TestGitHubIntegration_AutomationDispatchPanicDoesNotSuppressMention posts
// a single `issue_comment` delivery that is BOTH a real @mention (should
// create a session/turn) AND a delivery automation dispatch would
// evaluate (issue_comment is in GitHubDispatchAllowlist) -- with
// automation dispatch wired to panic unconditionally. The mention
// pipeline must still succeed: this delivery is proof that automation
// dispatch is an ADDITIONAL, independent consumer of the same delivery,
// never a gate the @mention pipeline runs through.
func TestGitHubIntegration_AutomationDispatchPanicDoesNotSuppressMention(t *testing.T) {
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Automations = panicAutomationLister{}
		cfg.AutomationInvocations = narvipg.NewAutomationInvocationStore(newTestPool(t))
	})

	ctx := context.Background()
	const commenterID = 90000003
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMember)

	repoFullName := "acme/automation-panic-repo"
	body := issueCommentBodyWithCommenter(repoFullName, "automation-panic-repo", "https://github.com/acme/automation-panic-repo", 44, "panic-isolation", commenterID, "some-commenter")

	status := postWebhookEventType(t, rig, body, "delivery-automation-panic-1", "issue_comment")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (a panic inside automation dispatch must not surface as a failed request)", status, http.StatusOK)
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, "SELECT count(*) FROM turns").Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 1 {
		t.Fatalf("turn count = %d, want 1 (the mention pipeline must still have created a turn despite automation dispatch panicking on the SAME delivery)", turnCount)
	}
}

// forceFailUpsertPatterns is a FalsePositivePatternCapturer fake that
// always fails Upsert -- injected via cfg.FalsePositivePatternCapture
// (that field's own doc comment: "a fake in this package's own tests only
// needs to implement whichever subset the test actually exercises")
// specifically to force a genuine, deterministic LATER-lane failure.
type forceFailUpsertPatterns struct{}

func (forceFailUpsertPatterns) Upsert(ctx context.Context, repoFullName string, commentID int64, commentType, reason string, createdBy pgtype.UUID) (sqlcgen.ReviewFalsePositivePattern, bool, error) {
	return sqlcgen.ReviewFalsePositivePattern{}, false, errors.New("forced upsert failure: D1 audit fix repro")
}

// TestGitHubIntegration_AutomationDispatchSurvivesClaimReleasedByALaterLaneFailure
// is D1's own required, missing test: "a first delivery that FAILS in a
// later lane (releasing the claim), then a redelivery, asserting exactly
// one invocation" -- the exact gap TestGitHubIntegration_
// AutomationDispatchDedupesRedeliveredDelivery (above) does NOT cover,
// because that test's own first delivery always succeeds end to end, so
// deliveries.Release is never called at all.
//
// Here, automation dispatch (handler.go's own FIRST consumer of this
// delivery, dispatched before every other lane) succeeds and creates
// exactly one invocation -- but the false-positive-capture lane
// (checked LATER, same delivery, same event type) is wired to a forced,
// deterministic Postgres failure (forceFailUpsertPatterns above),
// releasing the webhook-delivery claim exactly the way a real transient
// failure would. Before D1's fix, a redelivery of this SAME delivery id
// then created a SECOND invocation (a second run, a second sandboxed
// agent session) even though automation dispatch itself had already fully
// succeeded on the very first attempt -- CreateInvocationForDelivery's own
// idempotency (keyed on (automation_id, provider, delivery_id)) is what
// this test proves closes that gap.
func TestGitHubIntegration_AutomationDispatchSurvivesClaimReleasedByALaterLaneFailure(t *testing.T) {
	ctx := context.Background()
	repoFullName := "acme/automation-claim-release-repo"
	cloneURL := "https://github.com/acme/automation-claim-release-repo"

	pool := newTestPool(t)
	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	auto := createGitHubAutomation(ctx, t, automations,
		"on issue_comment (claim-release repro)",
		domainautomation.GitHubTriggerConfig{Event: "issue_comment"},
		domainautomation.Target{Name: "repo", URL: cloneURL},
	)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Automations = automations
		cfg.AutomationInvocations = invocations
		cfg.FalsePositivePatternCapture = forceFailUpsertPatterns{}
	})

	const automationSenderID = 90000010
	const falsePositiveCommenterID = 90000011
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, automationSenderID, sqlcgen.UserRoleMember)
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, falsePositiveCommenterID, sqlcgen.UserRoleMaintainer)

	body, err := json.Marshal(map[string]any{
		"action": "created",
		"sender": map[string]any{"id": automationSenderID, "login": "automation-sender"},
		"issue": map[string]any{
			"number":       1,
			"pull_request": map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/%s/pulls/1", repoFullName)},
		},
		"comment": map[string]any{
			"id":   int64(700100),
			"body": "false positive: this is intentional",
			"user": map[string]any{"id": falsePositiveCommenterID, "login": "fp-user"},
		},
		"repository": map[string]any{
			"full_name": repoFullName,
			"name":      "automation-claim-release-repo",
			"clone_url": cloneURL,
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	const deliveryID = "delivery-automation-claim-release-1"

	first := postWebhookEventType(t, rig, body, deliveryID, "issue_comment")
	if first != http.StatusInternalServerError {
		t.Fatalf("first delivery status = %d, want %d (the false-positive capture lane must fail AFTER automation dispatch already succeeded)", first, http.StatusInternalServerError)
	}
	if got := countAutomationInvocations(t, rig, auto.ID); got != 1 {
		t.Fatalf("invocations after first delivery = %d, want 1 (automation dispatch, the FIRST consumer of this delivery, must have already succeeded)", got)
	}

	var deliveryRowCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE provider = 'github' AND delivery_id = $1`, deliveryID).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 0 {
		t.Fatalf("webhook_deliveries row count after the failed attempt = %d, want 0 (the false-positive lane's own genuine failure must release the claim)", deliveryRowCount)
	}

	second := postWebhookEventType(t, rig, body, deliveryID, "issue_comment")
	if second != http.StatusInternalServerError {
		t.Fatalf("redelivered status = %d, want %d", second, http.StatusInternalServerError)
	}
	if got := countAutomationInvocations(t, rig, auto.ID); got != 1 {
		t.Fatalf("invocations after the redelivered delivery = %d, want 1 (D1 audit fix: idempotent on (automation_id, provider, delivery_id) -- a redelivery must NOT re-fire the automation a second time)", got)
	}
}

// TestGitHubIntegration_AutomationDispatchDeniesUnauthorizedSender is D2's
// own required, missing security proof: an event whose top-level sender is
// NOT a linked Narvi identity at all must NOT create an invocation, even
// though the trigger's own Event/Action/Label filter and repo/branch
// scoping both genuinely match -- before this fix, ANY GitHub account
// (an unlinked one included) that could open an issue, post a comment, or
// open a fork PR on a watched public repository could create an
// automation invocation, and thus sandboxed agent runs holding this
// deployment's own repository credentials.
func TestGitHubIntegration_AutomationDispatchDeniesUnauthorizedSender(t *testing.T) {
	repoFullName := "acme/automation-unauthorized-repo"
	cloneURL := "https://github.com/acme/automation-unauthorized-repo"

	pool := newTestPool(t)
	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	auto := createGitHubAutomation(context.Background(), t, automations,
		"on labeled automation:run (unauthorized sender)",
		domainautomation.GitHubTriggerConfig{Event: "pull_request", Action: "labeled", Label: "automation:run"},
		domainautomation.Target{Name: "repo", URL: cloneURL},
	)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Automations = automations
		cfg.AutomationInvocations = invocations
	})

	// senderID 90000099 is DELIBERATELY never linked via
	// createLinkedGitHubUser -- an account that has never signed into
	// Narvi via GitHub OAuth at all.
	body := pullRequestLabeledBody(repoFullName, "automation-unauthorized-repo", cloneURL, 45, "automation:run", 90000099, "unlinked-sender")
	status := postWebhookEventType(t, rig, body, "delivery-automation-unauthorized-1", "pull_request")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (automation dispatch is best-effort -- an unauthorized sender is skipped, never a failed request)", status, http.StatusOK)
	}

	if got := countAutomationInvocations(t, rig, auto.ID); got != 0 {
		t.Fatalf("automation_invocations for automation = %d, want 0 (D2 audit fix: an unlinked/unauthorized sender must never create an invocation)", got)
	}
}
