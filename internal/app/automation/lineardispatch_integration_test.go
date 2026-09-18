//go:build integration

// Covers §8.4's own Linear live-dispatch entry point
// (DispatchLinearWebhookEvent, lineardispatch.go) against a real Postgres
// instance -- mirrors githubdispatch_integration_test.go's own shape.
package automation_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// createLinearAutomation inserts an automation with TriggerTypeLinear, the
// given event/action/team filter, and one target repo.
func (f *testFixture) createLinearAutomation(t *testing.T, name string, cfg domainautomation.LinearTriggerConfig, target domainautomation.Target) sqlcgen.Automation {
	t.Helper()
	ctx := context.Background()

	reposJSON, err := json.Marshal([]domainautomation.Target{target})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{
		"eventType": cfg.EventType, "action": cfg.Action, "teamKey": cfg.TeamKey,
	})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}

	row, err := f.automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: name, Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeLinear, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create linear automation: %v", err)
	}
	return row
}

func TestDispatchLinearWebhookEvent_FiresMatchingAutomation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createLinearAutomation(t, "on issue create", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG"}, target)

	in := domainautomation.LinearEventInput{EventType: "Issue", Action: "create", TeamKey: "ENG"}
	automation.DispatchLinearWebhookEvent(ctx, logger, f.automations, f.invocations, "Issue", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 1 {
		t.Fatalf("invocations for automation = %d, want 1", got)
	}
}

func TestDispatchLinearWebhookEvent_EventTypeOutsideAllowlistNeverFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	// AgentSessionEvent is deliberately excluded from LinearDispatchAllowlist
	// (that category already belongs to the existing agent-session
	// pipeline) -- even a trigger explicitly configured for it must not
	// fire through this generic path.
	auto := f.createLinearAutomation(t, "on agent session", domainautomation.LinearTriggerConfig{EventType: "AgentSessionEvent"}, target)

	in := domainautomation.LinearEventInput{EventType: "AgentSessionEvent"}
	automation.DispatchLinearWebhookEvent(ctx, logger, f.automations, f.invocations, "AgentSessionEvent", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations for automation = %d, want 0 (event type not in LinearDispatchAllowlist)", got)
	}
}

func TestDispatchLinearWebhookEvent_TeamMismatchNeverFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	logger := platform.Logger(ctx)

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createLinearAutomation(t, "on ENG issue", domainautomation.LinearTriggerConfig{EventType: "Issue", TeamKey: "ENG"}, target)

	in := domainautomation.LinearEventInput{EventType: "Issue", TeamKey: "OPS"}
	automation.DispatchLinearWebhookEvent(ctx, logger, f.automations, f.invocations, "Issue", in)

	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations for automation = %d, want 0 (team key mismatch)", got)
	}
}
