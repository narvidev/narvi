package linear

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/narvidev/narvi/internal/app/automation"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// linearAutomationEventEnvelope is this file's own minimal, generic
// top-level shape shared by every Linear webhook category (not just
// AgentSessionEvent) -- Linear's own docs describe the SAME action/type/
// data/organizationId/webhookTimestamp envelope for every category
// (agentSessionEventWebhookPayload's own top doc comment, payload.go,
// notes this too). Data.Team.Key is the one field this Step's own
// LinearTriggerConfig.TeamKey filter needs that agentSessionEventWebhookPayload
// does not carry at all (that struct has no Data field -- it is
// AgentSessionEvent-specific).
type linearAutomationEventEnvelope struct {
	Action string `json:"action"`
	Data   struct {
		Team struct {
			Key string `json:"key"`
		} `json:"team"`
	} `json:"data"`
}

// buildLinearEventInput normalizes rawBody (the raw webhook payload for
// eventType, the Linear-Event header's own value) into
// domainautomation.LinearEventInput -- ok is false only on a JSON decode
// failure, mirroring github's own buildGitHubEventInput's identical "false
// means nothing to dispatch, never a reason to fail this request" contract.
func buildLinearEventInput(eventType string, rawBody []byte) (domainautomation.LinearEventInput, bool) {
	var env linearAutomationEventEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return domainautomation.LinearEventInput{}, false
	}
	return domainautomation.LinearEventInput{
		EventType: eventType,
		Action:    env.Action,
		TeamKey:   env.Data.Team.Key,
	}, true
}

// dispatchAutomationsBestEffort mirrors github's own identical function
// (internal/adapters/inbound/github/automationdispatch.go) exactly -- see
// that function's own doc comment for the full design, including the
// deliberately narrow, reviewed panic-recovery scope.
func dispatchAutomationsBestEffort(ctx context.Context, logger *slog.Logger, deps Deps, eventType string, rawBody []byte) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("linear: automation dispatch panicked, isolated from the rest of this delivery", "panic", r, "event_type", eventType)
		}
	}()

	if deps.Automations == nil || deps.AutomationInvocations == nil {
		return
	}

	in, ok := buildLinearEventInput(eventType, rawBody)
	if !ok {
		logger.Warn("linear: automation dispatch: malformed webhook body, skipping (the AgentSessionEvent pipeline handles/logs this independently)", "event_type", eventType)
		return
	}

	automation.DispatchLinearWebhookEvent(ctx, logger, deps.Automations, deps.AutomationInvocations, eventType, in)
}
