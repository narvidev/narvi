package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
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
func DispatchLinearWebhookEvent(ctx context.Context, logger *slog.Logger, automations LinearTriggerLister, invocations InvocationCreator, eventType string, in domainautomation.LinearEventInput) {
	if reason := domainautomation.ClassifyLinearDispatch(eventType); reason != domainautomation.LinearDispatchNotSkipped {
		logger.Debug("automation: linear event not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
		return
	}

	rows, err := automations.ListActiveLinearAutomations(ctx)
	if err != nil {
		logger.Error("automation: list active linear automations failed", "error", err)
		return
	}

	for _, row := range rows {
		dispatchOneLinearAutomation(ctx, logger, invocations, row, eventType, in)
	}
}

func dispatchOneLinearAutomation(ctx context.Context, logger *slog.Logger, invocations InvocationCreator, row sqlcgen.Automation, eventType string, in domainautomation.LinearEventInput) {
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

	if _, err := CreateInvocation(ctx, invocations, row.ID, targets); err != nil {
		logger.Error("automation: create invocation for linear dispatch failed", "error", err, "event_type", eventType)
	}
}
