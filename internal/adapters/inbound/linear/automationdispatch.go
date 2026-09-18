package linear

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

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
//
// OrganizationID ("organizationId") is D9 audit fix's own addition --
// SECURITY finding: the dispatch path used to drop this field entirely and
// never check that the sending workspace has an installation, unlike the
// pre-existing AgentSessionEvent path (handleCreated/handlePrompted,
// webhook.go), which fails closed via Installations.GetByOrganizationID.
// So any Linear workspace whose deliveries are signed with THIS app's
// shared webhook secret (Linear issues one secret per registered app, not
// per installing workspace) could fire every matching Linear-triggered
// automation against all of this deployment's configured target repos,
// regardless of whether that workspace ever actually installed this app.
type linearAutomationEventEnvelope struct {
	Action         string `json:"action"`
	OrganizationID string `json:"organizationId"`
	Data           struct {
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
// organizationID is returned alongside (D9 audit fix) rather than folded
// into LinearEventInput itself -- installation-scoping is an
// authorization concern, not a trigger-matching one, exactly the same
// separation githubEventSenderID keeps from buildGitHubEventInput
// (internal/adapters/inbound/github/automationdispatch.go).
func buildLinearEventInput(eventType string, rawBody []byte) (in domainautomation.LinearEventInput, organizationID string, ok bool) {
	var env linearAutomationEventEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return domainautomation.LinearEventInput{}, "", false
	}
	return domainautomation.LinearEventInput{
		EventType: eventType,
		Action:    env.Action,
		TeamKey:   env.Data.Team.Key,
	}, env.OrganizationID, true
}

// dispatchAutomationsBestEffort mirrors github's own identical function
// (internal/adapters/inbound/github/automationdispatch.go) exactly -- see
// that function's own doc comment for the full design, including the
// deliberately narrow, reviewed panic-recovery scope.
//
// # D9 audit fix: fail closed unless the sending workspace is installed
//
// Before this fix, an event's own "organizationId" was parsed by nothing
// on this path at all -- Linear signs every webhook delivery from EVERY
// workspace that has this app installed with the SAME shared secret (it is
// per-app, not per-workspace), so a correctly-signed delivery says nothing
// on its own about whether the SENDING workspace is one this deployment
// actually recognizes. This mirrors the pre-existing AgentSessionEvent
// path's own check exactly (handleCreated/handlePrompted already fail
// closed via deps.Installations.GetByOrganizationID, decryptLinearAccessToken.go
// -- identity.go) -- the SAME store, the SAME lookup, never a second,
// independently-maintained one.
func dispatchAutomationsBestEffort(ctx context.Context, logger *slog.Logger, deps Deps, eventType string, deliveryID string, rawBody []byte) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("linear: automation dispatch panicked, isolated from the rest of this delivery", "panic", r, "event_type", eventType)
		}
	}()

	if deps.Automations == nil || deps.AutomationInvocations == nil {
		return
	}
	if deps.Installations == nil {
		logger.Error("linear: automation dispatch: installations store not wired, skipping (fail closed)", "event_type", eventType)
		return
	}

	in, organizationID, ok := buildLinearEventInput(eventType, rawBody)
	if !ok {
		logger.Warn("linear: automation dispatch: malformed webhook body, skipping (the AgentSessionEvent pipeline handles/logs this independently)", "event_type", eventType)
		return
	}

	if _, err := deps.Installations.GetByOrganizationID(ctx, organizationID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("linear: automation dispatch: no installation for organization, skipping", "event_type", eventType, "reason", "organization_not_installed", "organization_id", organizationID)
			return
		}
		logger.Error("linear: automation dispatch: look up installation failed, skipping (fail closed)", "error", err, "event_type", eventType)
		return
	}

	automation.DispatchLinearWebhookEvent(ctx, logger, deps.Automations, deps.AutomationInvocations, deps.Timeouts, eventType, deliveryID, in)
}
