package linear

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/domain/authz"
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
//
// Actor ("actor") is D15 audit fix's own addition -- SECURITY finding: the
// installation check above establishes TENANT scoping ("this workspace
// once completed OAuth"), never authorization of the PERSON who actually
// acted. Linear's own docs describe this top-level field as "a User, OAuth
// client, or Integration" -- may be null "if the user or integration that
// triggered the action has since been deleted" -- present on every
// data-change category this deployment's own LinearDispatchAllowlist
// covers ("Issue", "Comment"), the SAME actor/type/id shape Linear's own
// webhook docs show for both. Actor.ID is resolved through the IDENTICAL
// deps.resolveActor auto-linking algorithm the pre-existing AgentSessionEvent
// path already uses for AgentSession.CreatorID/AgentActivity.UserID
// (identity.go) -- never a second, independently-invented model: an actor
// representing a non-user (an OAuth client, an Integration) simply never
// has a matching identities row, and resolves to "not linked" exactly like
// a genuine, never-signed-in human Linear user id would, with no separate
// type-based branch needed to reach that same, correct, fail-closed
// verdict.
type linearAutomationEventEnvelope struct {
	Action         string `json:"action"`
	OrganizationID string `json:"organizationId"`
	Actor          *struct {
		ID string `json:"id"`
	} `json:"actor"`
	Data struct {
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
// organizationID/actorExternalID are returned alongside (D9/D15 audit
// fixes) rather than folded into LinearEventInput itself --
// installation/actor authorization is a SEPARATE concern from trigger
// matching, exactly the same separation githubEventSenderID keeps from
// buildGitHubEventInput (internal/adapters/inbound/github/
// automationdispatch.go). actorExternalID is "" when Actor is nil (the
// actor has since been deleted, per Linear's own docs) -- deps.resolveActor
// already treats an empty externalID as "nothing to resolve, bot
// attribution" (identity.go), so this needs no special-casing here.
func buildLinearEventInput(eventType string, rawBody []byte) (in domainautomation.LinearEventInput, organizationID string, actorExternalID string, ok bool) {
	var env linearAutomationEventEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return domainautomation.LinearEventInput{}, "", "", false
	}
	if env.Actor != nil {
		actorExternalID = env.Actor.ID
	}
	return domainautomation.LinearEventInput{
		EventType: eventType,
		Action:    env.Action,
		TeamKey:   env.Data.Team.Key,
	}, env.OrganizationID, actorExternalID, true
}

// dispatchAutomationsBestEffort mirrors github's own identical function
// (internal/adapters/inbound/github/automationdispatch.go) in shape --
// same "additional, independent consumer of this already-claimed
// delivery", same deliberately narrow, reviewed panic-recovery scope --
// but is NOT identical in its authorization design: see the D15 section
// below for the one place it genuinely differs, and why.
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
//
// # D15 audit fix: the installation check above is tenant scoping, not actor authorization
//
// Confirmed, HIGH-severity finding: D9's installation check establishes
// only that the SENDING WORKSPACE once completed OAuth -- it says nothing
// about the PERSON who caused this specific event, and a signed, correctly-
// tenant-scoped "Issue"/"Comment" delivery from a workspace with no
// authorization check on its acting user could still create an automation
// invocation (and, downstream, a sandboxed agent run holding this
// deployment's own repository credentials) for an actor with no Narvi
// identity at all -- reproduced directly against a real Postgres instance
// (see this package's own automationdispatch_integration_test.go). This
// closes that gap by reusing GitHub's own D2 design exactly, never a
// second, independently-invented model: the event's own top-level "actor.id"
// (buildLinearEventInput's own actorExternalID above) is resolved via
// deps.resolveActor -- the SAME auto-linking algorithm the pre-existing
// AgentSessionEvent path already runs for AgentSession.CreatorID/
// AgentActivity.UserID (identity.go), no separate/duplicated lookup --
// then authorized via actorauthz.AuthorizeLinkedActor(...,
// authz.ActionCreateSession, ...), the IDENTICAL primitive/action/resource
// GitHub's own dispatchAutomationsBestEffort authorizes its own sender
// against. An unlinked/unauthorized actor (including Actor == nil, "since
// deleted" per Linear's own docs, which resolves to an empty
// actorExternalID) is skipped with a named, logged reason, never silently.
// resolveActor's own notice (a magic-link prompt, when it minted one) is
// deliberately NOT surfaced here -- unlike the AgentSessionEvent path,
// automation dispatch posts no outbound activity of its own for this
// delivery to append it to, and inventing a new one purely to carry this
// notice is out of this fix's own scope.
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

	in, organizationID, actorExternalID, ok := buildLinearEventInput(eventType, rawBody)
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

	// D15 audit fix: deps.resolveActor/actorauthz.AuthorizeLinkedActor
	// below need deps.IdentityLink.Users/Identities -- both nil (a
	// minimal test rig, or any other wiring that never populates
	// IdentityLink at all) would otherwise reach a nil-pointer
	// dereference deep inside identitylink/actorauthz rather than a
	// named, logged, fail-closed skip, mirroring github's own identical
	// "identities == nil || users == nil" nil-safety check
	// (dispatchAutomationsBestEffort, automationdispatch.go). Deliberately
	// checked AFTER the installation-row lookup above, not before: an
	// uninstalled workspace must still be denied for THAT reason
	// specifically (organization_not_installed), never masked by this
	// unrelated wiring gap.
	if deps.IdentityLink.Users == nil || deps.IdentityLink.Identities == nil {
		logger.Error("linear: automation dispatch: identity link store not wired, skipping (fail closed)", "event_type", eventType)
		return
	}

	actorUserID, _ := deps.resolveActor(ctx, logger, organizationID, actorExternalID)
	if !actorauthz.AuthorizeLinkedActor(ctx, logger, authzSurface, deps.IdentityLink.Users, actorUserID, authz.ActionCreateSession, authz.Resource{}) {
		logger.Info("linear: automation dispatch: actor not authorized, skipping", "event_type", eventType, "reason", "actor_unlinked_or_unauthorized")
		return
	}

	automation.DispatchLinearWebhookEvent(ctx, logger, deps.Automations, deps.AutomationInvocations, deps.Timeouts, eventType, deliveryID, in)
}
