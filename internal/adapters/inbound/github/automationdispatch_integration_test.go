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
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

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

	body := pullRequestLabeledBody(repoFullName, "automation-dispatch-repo", cloneURL, 42, "automation:run", 90000001, "some-sender")
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

	body := pullRequestLabeledBody(repoFullName, "automation-dedup-repo", cloneURL, 43, "automation:run", 90000002, "some-sender")
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
