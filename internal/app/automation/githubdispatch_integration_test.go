//go:build integration

// Covers §8.4's own GitHub live-dispatch entry point
// (DispatchGitHubWebhookEvent, githubdispatch.go) against a real Postgres
// instance -- mirrors triggerandextras_integration_test.go's own
// black-box, real-Engine-against-a-real-Postgres shape exactly, reusing
// this package's own shared testFixture/newFixture.
package automation_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// createGitHubAutomation inserts an automation with TriggerTypeGitHub, the
// given event/action/label filter, and one target repo -- mirrors
// createCronAutomation's own shape (triggerandextras_integration_test.go).
func (f *testFixture) createGitHubAutomation(t *testing.T, name string, cfg domainautomation.GitHubTriggerConfig, target domainautomation.Target) sqlcgen.Automation {
	t.Helper()
	ctx := context.Background()

	prSessions := narvipg.NewGitHubPRSessionStore(f.pool)
	if repoFullName, ok := domainautomation.RepoFullNameFromCloneURL(target.URL); ok {
		if err := prSessions.EnsureRow(ctx, repoFullName, 1); err != nil {
			t.Fatalf("seed github_pr_sessions entitlement: %v", err)
		}
	}

	reposJSON, err := json.Marshal([]domainautomation.Target{target})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{
		"event": cfg.Event, "action": cfg.Action, "label": cfg.Label,
		"name": cfg.Name, "conclusion": cfg.Conclusion,
	})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}

	row, err := f.automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: name, Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeGithub, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create github automation: %v", err)
	}
	return row
}

func TestDispatchGitHubWebhookEvent_FiresMatchingAutomation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createGitHubAutomation(t, "on pr labeled", domainautomation.GitHubTriggerConfig{Event: "pull_request", Action: "labeled", Label: "automation:run"}, target)

	in := domainautomation.GitHubEventInput{
		EventType:    "pull_request",
		Action:       "labeled",
		Labels:       []string{"automation:run", "bug"},
		RepoFullName: "acme/repo",
	}
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, "pull_request", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 1 {
		t.Fatalf("invocations for automation = %d, want 1", got)
	}
}

func TestDispatchGitHubWebhookEvent_EventTypeOutsideAllowlistNeverFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	// A trigger whose own Event value equals the disallowed event type --
	// if the allowlist gate were missing, this WOULD match.
	auto := f.createGitHubAutomation(t, "on release", domainautomation.GitHubTriggerConfig{Event: "release"}, target)

	in := domainautomation.GitHubEventInput{EventType: "release", RepoFullName: "acme/repo"}
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, "release", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations for automation = %d, want 0 (event type not in GitHubDispatchAllowlist)", got)
	}
}

func TestDispatchGitHubWebhookEvent_WrongRepoNeverFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createGitHubAutomation(t, "on pr opened", domainautomation.GitHubTriggerConfig{Event: "pull_request"}, target)

	// Event/Action/Label filter matches, but the webhook's own repository
	// is NOT one of this automation's configured targets.
	in := domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "someoneelse/unrelated"}
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, "pull_request", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations for automation = %d, want 0 (event's own repo is not a configured target)", got)
	}
}

// TestDispatchGitHubWebhookEvent_BranchTipNotContainment is §8.4's own
// end-to-end proof (mirrors internal/domain/automation's own unit-level
// TestTargetMatchesGitHubEvent_BranchTipNotContainment, but through the
// real dispatch path, against a real Postgres row): a status event whose
// commit is main's own current tip AND is merely CONTAINED by feature-x
// (per GitHub's own branches[] containment semantics) must fire an
// automation scoped to main and must NOT fire one scoped to feature-x.
func TestDispatchGitHubWebhookEvent_BranchTipNotContainment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	mainTarget := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "main"}
	featureTarget := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "feature-x"}
	mainAuto := f.createGitHubAutomation(t, "on main status success", domainautomation.GitHubTriggerConfig{Event: "status", Conclusion: "success"}, mainTarget)
	featureAuto := f.createGitHubAutomation(t, "on feature-x status success", domainautomation.GitHubTriggerConfig{Event: "status", Conclusion: "success"}, featureTarget)

	in := domainautomation.GitHubEventInput{
		EventType:    "status",
		RepoFullName: "acme/repo",
		SHA:          "shaMain",
		Conclusion:   "success",
		Branches: []domainautomation.GitHubEventBranch{
			{Name: "main", HeadSHA: "shaMain"},
			// feature-x CONTAINS shaMain but its own tip has moved on.
			{Name: "feature-x", HeadSHA: "shaFeatureTip"},
		},
	}
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, "status", in)

	if got := f.countInvocationsForAutomation(t, mainAuto.ID); got != 1 {
		t.Fatalf("invocations for main-scoped automation = %d, want 1 (main IS this commit's own tip)", got)
	}
	if got := f.countInvocationsForAutomation(t, featureAuto.ID); got != 0 {
		t.Fatalf("invocations for feature-x-scoped automation = %d, want 0 (feature-x only CONTAINS this commit, it is not its tip)", got)
	}
}

func TestDispatchGitHubWebhookEvent_PausedAutomationNeverFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createGitHubAutomation(t, "paused", domainautomation.GitHubTriggerConfig{Event: "pull_request"}, target)
	if _, err := f.pool.Exec(ctx, "UPDATE automations SET status = 'paused' WHERE id = $1", auto.ID); err != nil {
		t.Fatalf("pause automation: %v", err)
	}

	in := domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"}
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, "pull_request", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations for paused automation = %d, want 0", got)
	}
}
