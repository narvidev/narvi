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
	"fmt"
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
// CreatedBy is deliberately left invalid (pgtype.UUID{}): every test using
// this helper directly exercises a HUMAN-origin event type, which never
// consults it (D12 audit fix) -- see createGitHubAutomationWithCreator
// below for the "status"/"check_run" (GitHubEventOriginMachine) case.
func (f *testFixture) createGitHubAutomation(t *testing.T, name string, cfg domainautomation.GitHubTriggerConfig, target domainautomation.Target) sqlcgen.Automation {
	t.Helper()
	return f.createGitHubAutomationWithCreator(t, name, cfg, target, pgtype.UUID{})
}

// createGitHubAutomationWithCreator is createGitHubAutomation's own
// creator-supplying variant -- D12 audit fix's own required fixture: a
// GitHubEventOriginMachine delivery (check_run, status) authorizes THIS
// automation's own createdBy, so a test proving such a delivery actually
// dispatches must wire a genuinely linked, authorized creator, not the
// default invalid one every human-origin-event test in this file still
// uses unchanged.
func (f *testFixture) createGitHubAutomationWithCreator(t *testing.T, name string, cfg domainautomation.GitHubTriggerConfig, target domainautomation.Target, createdBy pgtype.UUID) sqlcgen.Automation {
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
		Name: name, Repos: reposJSON, CreatedBy: createdBy,
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
		// D4 audit fix: the fixture's own target is unconfigured
		// (Branch == "") -- matches only the repo's own default branch,
		// resolved from DefaultBranch here (a real GitHub payload's own
		// "repository.default_branch"), never any branch unconditionally.
		DefaultBranch: "main",
		SHA:           "shaHead",
		Branches:      []domainautomation.GitHubEventBranch{{Name: "main", HeadSHA: "shaHead"}},
	}
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, f.users, platform.DefaultTimeouts(), "pull_request", "delivery-github-fires-1", in)

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
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, f.users, platform.DefaultTimeouts(), "release", "delivery-github-notallowlisted-1", in)

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
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, f.users, platform.DefaultTimeouts(), "pull_request", "delivery-github-wrongrepo-1", in)

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
//
// "status" is GitHubEventOriginMachine (D12 audit fix) -- both automations
// below must carry a genuinely linked, authorized creator (createdBy)
// via createGitHubAutomationWithCreator, or neither would ever dispatch
// regardless of branch scoping, which is not what this test exists to
// prove.
func TestDispatchGitHubWebhookEvent_BranchTipNotContainment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	creator := f.createAutomationCreator(t, "branchtip", sqlcgen.UserRoleMaintainer)

	mainTarget := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "main"}
	featureTarget := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "feature-x"}
	mainAuto := f.createGitHubAutomationWithCreator(t, "on main status success", domainautomation.GitHubTriggerConfig{Event: "status", Conclusion: "success"}, mainTarget, creator.ID)
	featureAuto := f.createGitHubAutomationWithCreator(t, "on feature-x status success", domainautomation.GitHubTriggerConfig{Event: "status", Conclusion: "success"}, featureTarget, creator.ID)

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
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, f.users, platform.DefaultTimeouts(), "status", "delivery-github-branchtip-1", in)

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
	automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, f.users, platform.DefaultTimeouts(), "pull_request", "delivery-github-paused-1", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations for paused automation = %d, want 0", got)
	}
}

// TestDispatchGitHubWebhookEvent_ThrottlesUnboundedInvocations is D8's own
// required, missing security proof: a single automation's own dispatch
// path must not create unbounded invocations no matter how many distinct,
// genuinely-matching deliveries arrive -- each delivery here has a
// DIFFERENT delivery id (D1's own idempotency, keyed per-delivery, would
// otherwise mask this: reusing the SAME id would only ever prove
// dedup, never the throttle). Fires domainautomation.
// DispatchThrottleThreshold+5 distinct, matching deliveries and asserts
// the invocation count stops growing at exactly the threshold.
func TestDispatchGitHubWebhookEvent_ThrottlesUnboundedInvocations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createGitHubAutomation(t, "on issue_comment (throttle)", domainautomation.GitHubTriggerConfig{Event: "issue_comment"}, target)

	in := domainautomation.GitHubEventInput{EventType: "issue_comment", RepoFullName: "acme/repo"}

	const attempts = domainautomation.DispatchThrottleThreshold + 5
	for i := 0; i < attempts; i++ {
		deliveryID := fmt.Sprintf("delivery-github-throttle-%d", i)
		automation.DispatchGitHubWebhookEvent(ctx, logger, f.automations, f.invocations, f.users, platform.DefaultTimeouts(), "issue_comment", deliveryID, in)
	}

	if got := f.countInvocationsForAutomation(t, auto.ID); got != domainautomation.DispatchThrottleThreshold {
		t.Fatalf("invocations for automation after %d distinct matching deliveries = %d, want exactly %d (D8 audit fix: the per-automation dispatch throttle must cap it)", attempts, got, domainautomation.DispatchThrottleThreshold)
	}
}
