//go:build integration

// End-to-end proof for §8.4 ("automations never dispatch on a real
// webhook") at the Linear adapter boundary -- mirrors github's own
// automationdispatch_integration_test.go exactly: a real POST to
// /webhooks/linear, carrying an "Issue" category delivery (never
// AgentSessionEvent -- this package's own existing pipeline already fully
// owns that category, see domainautomation.LinearDispatchAllowlist's own
// doc comment), results in a real automation_invocations row through the
// SAME engine every other trigger path already uses. Also covers
// deduplication via a REPLAYED delivery and panic isolation from the
// AgentSessionEvent pipeline sharing the same delivery.
package linear_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// postWebhookEventType is postWebhook's own generalization (this file's
// own new tests need a "Issue"/"Comment" Linear-Event category, never the
// hardcoded "AgentSessionEvent" postWebhook always sends).
func postWebhookEventType(t *testing.T, handler http.HandlerFunc, body []byte, deliveryID, eventType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Linear-Event", eventType)
	req.Header.Set("Linear-Delivery", deliveryID)
	req.Header.Set("Linear-Signature", signBody([]byte(testWebhookSecret), body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// issueEventPayload builds a synthetic, real-shaped "Issue" category
// webhook body -- Linear's own generic action/type/data/organizationId/
// webhookTimestamp envelope (this file's own top doc comment), with
// data.team.key for LinearTriggerConfig.TeamKey's own filter.
func issueEventPayload(teamKey string) []byte {
	body := fmt.Sprintf(`{
		"action": "create",
		"type": "Issue",
		"organizationId": "org-automation-dispatch",
		"webhookTimestamp": %d,
		"data": {"team": {"key": %q}}
	}`, time.Now().UnixMilli(), teamKey)
	return []byte(body)
}

func createLinearAutomation(ctx context.Context, t *testing.T, automations *narvipg.AutomationStore, name string, cfg domainautomation.LinearTriggerConfig, target domainautomation.Target) sqlcgen.Automation {
	t.Helper()

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

	row, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: name, Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeLinear, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create linear automation: %v", err)
	}
	return row
}

func countAutomationInvocations(t *testing.T, pool *pgxpool.Pool, automationID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM automation_invocations WHERE automation_id = $1", automationID).Scan(&count); err != nil {
		t.Fatalf("count automation invocations: %v", err)
	}
	return count
}

func TestWebhookHandler_AutomationDispatchFiresOnRealWebhook(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	auto := createLinearAutomation(ctx, t, automations, "on ENG issue create", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG"}, target)

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	// D9 audit fix: the dispatch path now fails closed unless the sending
	// workspace ("org-automation-dispatch", issueEventPayload's own fixed
	// organizationId) has a real installation row.
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)
	handler := linear.NewWebhookHandler(deps)

	rec := postWebhookEventType(t, handler, issueEventPayload("ENG"), "delivery-linear-automation-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocations(t, pool, auto.ID); got != 1 {
		t.Fatalf("automation_invocations for automation = %d, want 1", got)
	}
}

func TestWebhookHandler_AutomationDispatchDedupesRedeliveredDelivery(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	auto := createLinearAutomation(ctx, t, automations, "on ENG issue create (dedup)", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG"}, target)

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	// D9 audit fix: see the identical comment in
	// TestWebhookHandler_AutomationDispatchFiresOnRealWebhook above.
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)
	handler := linear.NewWebhookHandler(deps)

	body := issueEventPayload("ENG")
	const deliveryID = "delivery-linear-automation-dedup-1"

	first := postWebhookEventType(t, handler, body, deliveryID, "Issue")
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d; body = %s", first.Code, http.StatusOK, first.Body.String())
	}
	if got := countAutomationInvocations(t, pool, auto.ID); got != 1 {
		t.Fatalf("after first delivery: automation_invocations = %d, want 1", got)
	}

	second := postWebhookEventType(t, handler, body, deliveryID, "Issue")
	if second.Code != http.StatusOK {
		t.Fatalf("redelivered status = %d, want %d; body = %s", second.Code, http.StatusOK, second.Body.String())
	}
	if got := countAutomationInvocations(t, pool, auto.ID); got != 1 {
		t.Fatalf("after redelivered delivery: automation_invocations = %d, want 1 (dedup must prevent a second invocation)", got)
	}
}

// panicAutomationLister deliberately panics on ListActiveLinearAutomations
// -- mirrors github's own identical fixture, proving Linear's own
// dispatchAutomationsBestEffort recovers a panic without corrupting the
// request.
type panicAutomationLister struct{}

func (panicAutomationLister) ListActiveLinearAutomations(ctx context.Context) ([]sqlcgen.Automation, error) {
	panic("forced panic: automation dispatch must not suppress the rest of this delivery's handling")
}

// TestWebhookHandler_AutomationDispatchPanicDoesNotBreakTheRequest proves
// dispatchAutomationsBestEffort's own panic recovery in isolation: an
// "Issue" category delivery (never processed by the AgentSessionEvent
// pipeline either way -- ClassifyLinearDispatch admits it, but this
// package's own webhook.go "ignoring non-AgentSessionEvent" branch always
// acknowledges it as a no-op) must still receive a normal 200, not a
// crashed/500 response, when automation dispatch panics internally.
func TestWebhookHandler_AutomationDispatchPanicDoesNotBreakTheRequest(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := newHandlerDeps(t, pool)
	deps.Automations = panicAutomationLister{}
	deps.AutomationInvocations = narvipg.NewAutomationInvocationStore(pool)
	// D9 audit fix: without a real installation row for this org, dispatch
	// would now skip BEFORE ever reaching panicAutomationLister -- install
	// it so this test still genuinely exercises the panic-recovery path.
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)
	handler := linear.NewWebhookHandler(deps)

	rec := postWebhookEventType(t, handler, issueEventPayload("ENG"), "delivery-linear-automation-panic-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (a panic inside automation dispatch must not surface as a failed request); body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestWebhookHandler_AutomationDispatchDeniesUninstalledWorkspace is D9's
// own required, missing security proof: a correctly-signed delivery from
// a workspace this deployment has NO installation row for must NOT create
// an invocation, even though the trigger's own filter genuinely matches --
// Linear signs every webhook delivery from EVERY workspace that has this
// app installed with the SAME shared secret (per-app, not per-workspace),
// so a valid signature alone says nothing about whether the SENDING
// workspace is one this deployment actually recognizes.
func TestWebhookHandler_AutomationDispatchDeniesUninstalledWorkspace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	auto := createLinearAutomation(ctx, t, automations, "on ENG issue create (uninstalled workspace)", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG"}, target)

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	// Deliberately NO installLinearFixture call for this organization --
	// this workspace has never installed this app.
	handler := linear.NewWebhookHandler(deps)

	rec := postWebhookEventType(t, handler, issueEventPayload("ENG"), "delivery-linear-uninstalled-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocations(t, pool, auto.ID); got != 0 {
		t.Fatalf("automation_invocations for automation = %d, want 0 (D9 audit fix: an uninstalled workspace must never create an invocation)", got)
	}
}
