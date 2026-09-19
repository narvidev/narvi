//go:build integration

// Integration test for AutomationStore.ListActiveLinearAutomations' own
// tenant predicate -- W3 audit fix (confirmed HIGH, TENANT ISOLATION
// finding: "the only workspace check is 'some linear_installations row
// exists for this organizationId' -- the sending workspace is never
// compared to anything the automation names, and an event from any
// installed workspace dispatches every active Linear automation, across
// organisations"). Kept in its own file, mirroring this package's own
// established "one focused file per query/claim primitive" precedent
// (githubprsession_store_integration_test.go's own top doc comment).
//
// Deliberately calls AutomationStore.ListActiveLinearAutomations directly
// -- never through app/automation's own DispatchLinearWebhookEvent/
// MatchesLinearTrigger -- so this test isolates the SQL-level predicate
// specifically, independent of the SEPARATE, defense-in-depth app-layer
// check MatchesLinearTrigger also applies (internal/domain/automation/
// trigger_test.go's own "organization mismatch" case already pins THAT
// layer in isolation). internal/app/automation's own
// TestDispatchLinearWebhookEvent_OrganizationMismatchNeverFires pins the
// END-TO-END behavior through both layers together.
package postgres_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// createTestLinearAutomation inserts a minimal, active, Linear-triggered
// automation row scoped to organizationID.
func createTestLinearAutomation(ctx context.Context, t *testing.T, automations *narvipg.AutomationStore, name, organizationID string) sqlcgen.Automation {
	t.Helper()

	repos := []map[string]string{{"name": "repo", "url": "https://github.com/acme/repo"}}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{
		"eventType": "Issue", "organizationId": organizationID,
	})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}

	row, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: name, Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeLinear, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create linear automation: %v", err)
	}
	return row
}

// TestAutomationStore_ListActiveLinearAutomations_ScopesByOrganization is
// W3's own required, SQL-level proof: two active Linear automations exist,
// each scoped to a DIFFERENT organization -- ListActiveLinearAutomations,
// called directly (never through any app-layer trigger-matching code),
// must return ONLY the row whose own trigger_config.organizationId matches
// the argument, never both, and never the wrong one alone.
func TestAutomationStore_ListActiveLinearAutomations_ScopesByOrganization(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	automations := narvipg.NewAutomationStore(pool)

	autoOrg1 := createTestLinearAutomation(ctx, t, automations, "org-1 automation", "org-1")
	autoOrg2 := createTestLinearAutomation(ctx, t, automations, "org-2 automation", "org-2")

	rowsOrg1, err := automations.ListActiveLinearAutomations(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListActiveLinearAutomations(org-1): %v", err)
	}
	if !containsAutomationID(rowsOrg1, autoOrg1.ID) {
		t.Errorf("ListActiveLinearAutomations(org-1) = %d rows, missing org-1's own automation", len(rowsOrg1))
	}
	if containsAutomationID(rowsOrg1, autoOrg2.ID) {
		t.Errorf("ListActiveLinearAutomations(org-1) incorrectly includes org-2's own automation -- the tenant predicate is not filtering")
	}

	rowsOrg2, err := automations.ListActiveLinearAutomations(ctx, "org-2")
	if err != nil {
		t.Fatalf("ListActiveLinearAutomations(org-2): %v", err)
	}
	if !containsAutomationID(rowsOrg2, autoOrg2.ID) {
		t.Errorf("ListActiveLinearAutomations(org-2) = %d rows, missing org-2's own automation", len(rowsOrg2))
	}
	if containsAutomationID(rowsOrg2, autoOrg1.ID) {
		t.Errorf("ListActiveLinearAutomations(org-2) incorrectly includes org-1's own automation -- the tenant predicate is not filtering")
	}

	// A third, never-installed/never-configured organization must see
	// neither row.
	rowsOrg3, err := automations.ListActiveLinearAutomations(ctx, "org-3-never-seen")
	if err != nil {
		t.Fatalf("ListActiveLinearAutomations(org-3-never-seen): %v", err)
	}
	if len(rowsOrg3) != 0 {
		t.Errorf("ListActiveLinearAutomations(org-3-never-seen) = %d rows, want 0", len(rowsOrg3))
	}
}

func containsAutomationID(rows []sqlcgen.Automation, id pgtype.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
