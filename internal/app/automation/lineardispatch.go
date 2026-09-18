package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// linearTriggerConfigJSON mirrors githubTriggerConfigJSON's own doc
// comment exactly, for Linear's own wire shape (internal/adapters/inbound/
// httpapi's own linearTriggerConfigWire).
type linearTriggerConfigJSON struct {
	EventType string `json:"eventType"`
	Action    string `json:"action,omitempty"`
	TeamKey   string `json:"teamKey,omitempty"`
}

func unmarshalLinearTriggerConfig(raw []byte) (domainautomation.LinearTriggerConfig, error) {
	var wire linearTriggerConfigJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return domainautomation.LinearTriggerConfig{}, fmt.Errorf("automation: unmarshal linear trigger config: %w", err)
		}
	}
	return domainautomation.LinearTriggerConfig{EventType: wire.EventType, Action: wire.Action, TeamKey: wire.TeamKey}, nil
}

// LinearTriggerLister mirrors GitHubTriggerLister, for Linear.
type LinearTriggerLister interface {
	ListActiveLinearAutomations(ctx context.Context) ([]sqlcgen.Automation, error)
}

// linearDeliveryProvider is the SAME literal
// internal/adapters/inbound/linear's own webhook.go already uses for
// WebhookDeliveryStore.Claim ("linear") -- this package's own copy,
// mirroring githubDeliveryProvider's own identical reasoning
// (githubdispatch.go).
const linearDeliveryProvider = "linear"

// DispatchLinearWebhookEvent is DispatchGitHubWebhookEvent's own Linear
// twin -- called inline from internal/adapters/inbound/linear's own
// webhook.go, for every already-claimed (deduplicated) Linear webhook
// delivery. domainautomation.ClassifyLinearDispatch(eventType) enforces
// the SAME kind of closed, typed allowlist GitHub's own dispatch does
// (see that function's own doc comment).
//
// Unlike GitHub, a Linear event carries no notion of "which git repo/
// branch does this concern" at all (Linear's own AgentSessionEvent
// category, which DOES carry a git-adjacent identity, is deliberately
// excluded from LinearDispatchAllowlist -- that function's own doc
// comment) -- so, once domainautomation.MatchesLinearTrigger accepts an
// automation, EVERY one of its own configured target repos fires,
// unnarrowed, exactly like the cron trigger pump's own evaluateCronAutomation
// and the generic webhook trigger's own automationwebhook.NewHandler.
//
// D1/D6/D8 audit fixes: mirrors DispatchGitHubWebhookEvent's own identical
// idempotent-on-delivery invocation creation (CreateInvocationForDelivery,
// keyed on (row.ID, linearDeliveryProvider, deliveryID)), per-automation
// dispatch throttle (checkDispatchThrottle), and bounded inline retry
// around every Postgres call (platform.Retry, timeouts.
// AutomationDispatchMaxAttempts et al.) -- see that function's own doc
// comment for the full "why" behind each, shared verbatim rather than
// re-implemented here.
func DispatchLinearWebhookEvent(ctx context.Context, logger *slog.Logger, automations LinearTriggerLister, invocations DeliveryInvocationCreator, timeouts platform.Timeouts, eventType string, deliveryID string, in domainautomation.LinearEventInput) {
	if reason := domainautomation.ClassifyLinearDispatch(eventType); reason != domainautomation.LinearDispatchNotSkipped {
		logger.Warn("automation: linear event not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
		return
	}

	var rows []sqlcgen.Automation
	retryErr := platform.Retry(ctx, timeouts.AutomationDispatchMaxAttempts, timeouts.AutomationDispatchRetryBaseDelay, timeouts.AutomationDispatchRetryMaxDelay, func() error {
		var err error
		rows, err = automations.ListActiveLinearAutomations(ctx)
		return err
	})
	if retryErr != nil {
		logger.Error("automation: list active linear automations failed", "error", retryErr)
		return
	}

	for _, row := range rows {
		dispatchOneLinearAutomation(ctx, logger, invocations, timeouts, row, eventType, deliveryID, in)
	}
}

func dispatchOneLinearAutomation(ctx context.Context, logger *slog.Logger, invocations DeliveryInvocationCreator, timeouts platform.Timeouts, row sqlcgen.Automation, eventType string, deliveryID string, in domainautomation.LinearEventInput) {
	logger = logger.With("automation_id", row.ID.String())

	cfg, err := unmarshalLinearTriggerConfig(row.TriggerConfig)
	if err != nil {
		logger.Error("automation: decode linear trigger config failed", "error", err)
		return
	}
	if !domainautomation.MatchesLinearTrigger(cfg, in) {
		return
	}

	targets, err := UnmarshalTargets(row.Repos)
	if err != nil {
		logger.Error("automation: decode automation repos for linear dispatch failed", "error", err)
		return
	}

	if !checkDispatchThrottle(ctx, logger, invocations, timeouts, row.ID) {
		return
	}

	created, err := createInvocationForDeliveryWithRetry(ctx, invocations, timeouts, row.ID, targets, linearDeliveryProvider, deliveryID)
	if err != nil {
		logger.Error("automation: create invocation for linear dispatch failed", "error", err, "event_type", eventType)
		return
	}
	if !created {
		logger.Info("automation: linear dispatch already created an invocation for this exact delivery, skipping", "event_type", eventType, "delivery_id", deliveryID)
	}
}
