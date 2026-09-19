package linear

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/app/identitylink"
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
// webhook docs show for both.
//
// # U3 audit fix, SECURITY: Actor.ID is now resolved through a PURE lookup, never the auto-linking algorithm
//
// Confirmed HIGH finding ("the gate creates the identity it then checks"):
// Actor.ID used to be resolved through deps.resolveActor -- the SAME
// side-effecting auto-linking algorithm (identitylink.Resolve) the
// pre-existing AgentSessionEvent path runs for AgentSession.CreatorID/
// AgentActivity.UserID (identity.go) -- on THIS gate too. That call does
// more than look up: for an actor with NO identities row at all, it
// fetches a profile email from Linear's own API and, if that email
// matches no known user either, MINTS a fresh identity_link_prompts row
// (a live magic-link nonce, U4's own sibling finding) -- an authorization
// gate that AUTO-LINKS the very actor it is supposed to be checking
// authorizes everyone eventually: a never-before-seen Linear user simply
// walks straight through the FIRST time they are ever observed, exactly
// the gate this code exists to prevent. Actor.ID is now resolved through
// identitylink.LookupLinkedUserID -- the SAME pure, side-effect-free
// (provider, external_id) lookup github's own resolveCommenterActor uses
// (internal/adapters/inbound/github/identity.go's own doc comment explains
// why GitHub needs no auto-linking algorithm at all): either this exact
// Linear actor id already has a linked Narvi account, or it does not --
// nothing is minted, fetched, or auto-linked as a SIDE EFFECT of checking.
// Auto-linking remains a legitimate operation with its own entry point
// (the pre-existing AgentSessionEvent path, handleCreated/handlePrompted,
// webhook.go) -- it is simply never something this authorization gate may
// trigger itself. See U4's own sibling finding for the mint/discard half
// this fix also closes as a direct consequence (an unlinked actor is no
// longer given a magic-link prompt by THIS path at all, so there is
// nothing left to mint and throw away here).
type linearAutomationEventEnvelope struct {
	Action         string `json:"action"`
	OrganizationID string `json:"organizationId"`
	Actor          *struct {
		ID string `json:"id"`
		// Type is U7 audit fix's own addition -- Linear's own docs
		// (https://linear.app/developers/webhooks, live-fetched during
		// this fix's own investigation): "user" for a real Linear
		// account; a non-"user", non-empty value for a non-human actor
		// (an OAuth client or an Integration, per that same doc's own
		// "Could be a User, OAuth client, or Integration" wording).
		// domainautomation.ClassifyLinearActorOrigin (dispatch.go) is
		// this package's own single point of interpretation for this
		// field's value -- see that function's own doc comment for why
		// only "user" is a closed, confirmed allowlist entry rather than
		// enumerating every possible non-human value.
		Type string `json:"type"`
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
// organizationID/actorExternalID/actorType are ALSO returned alongside
// (D9/D15/U7 audit fixes), for the installation/actor authorization this
// adapter runs BEFORE any automation is even listed -- a separate concern
// from trigger matching, exactly the same separation githubEventSenderID
// keeps from buildGitHubEventInput (internal/adapters/inbound/github/
// automationdispatch.go). W3 audit fix: organizationID is now ALSO folded
// into in.OrganizationID (domainautomation.LinearTriggerConfig.
// OrganizationID's own doc comment) -- unlike actor/installation
// authorization, tenant scoping IS part of trigger matching itself (the
// tenant boundary GitHub gets for free from Target.URL, Linear does not),
// so this one value legitimately serves both call sites from the SAME
// parse, never a second, independently-extracted copy that could drift
// from it. actorExternalID/actorType are both "" when Actor is nil (the
// actor has since been deleted, per Linear's own docs) --
// identitylink.LookupLinkedUserID already treats an empty externalID as
// "nothing to resolve, not linked" (mirrors deps.resolveActor's own
// identical "bot attribution" convention, identity.go), and
// domainautomation.ClassifyLinearActorOrigin treats an empty actorType as
// "origin unknown" (that function's own doc comment) -- neither needs any
// special-casing here.
func buildLinearEventInput(eventType string, rawBody []byte) (in domainautomation.LinearEventInput, organizationID string, actorExternalID string, actorType string, ok bool) {
	var env linearAutomationEventEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return domainautomation.LinearEventInput{}, "", "", "", false
	}
	if env.Actor != nil {
		actorExternalID = env.Actor.ID
		actorType = env.Actor.Type
	}
	return domainautomation.LinearEventInput{
		EventType:      eventType,
		Action:         env.Action,
		TeamKey:        env.Data.Team.Key,
		OrganizationID: env.OrganizationID,
	}, env.OrganizationID, actorExternalID, actorType, true
}

// dispatchAutomationsBestEffort mirrors github's own identical function
// (internal/adapters/inbound/github/automationdispatch.go) in shape --
// same "additional, independent consumer of this already-claimed
// delivery", same deliberately narrow, reviewed panic-recovery scope --
// but is NOT identical in its authorization design: see the D15/U3
// sections below for the places it genuinely differs, and why.
//
// # U6/U10 audit fix: classify the event type FIRST, before any Postgres or Linear API call
//
// Confirmed MEDIUM finding: domainautomation.ClassifyLinearDispatch used
// to run INSIDE automation.DispatchLinearWebhookEvent, i.e. AFTER the
// installation lookup, the actor lookup, AND authorization -- so every
// Linear webhook category this deployment's automation dispatch can never
// act on (anything outside LinearDispatchAllowlist) still paid an
// installation-store round trip and an identity-resolution lookup, every
// single delivery, for nothing: reproduced quantitatively (a delivery
// against an unresponsive provider took 1.75s inside the HTTP handler; see
// the PR body for the exact reproduction). Classification needs only
// eventType -- already this function's own parameter, available before
// ANY of that work -- so it now runs immediately after the (equally
// cheap, no-I/O) nil-dependency checks, before rawBody is even decoded.
func dispatchAutomationsBestEffort(ctx context.Context, logger *slog.Logger, deps Deps, eventType string, deliveryID string, rawBody []byte) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("linear: automation dispatch panicked, isolated from the rest of this delivery", "panic", r, "event_type", eventType)
		}
	}()

	if deps.Automations == nil || deps.AutomationInvocations == nil {
		return
	}

	// U6/U10 audit fix: see this function's own doc comment above. Cheapest
	// check first is not an optimisation here -- it is what stops ordinary,
	// never-actionable Linear traffic from costing an installation lookup
	// and an identity-resolution call (in turn a Linear GraphQL round trip
	// for any actor not already linked). Deliberately NOT logged at Warn:
	// this fires on every ordinary delivery of a category automation
	// dispatch is simply not subscribed to, not a symptom of anything
	// wrong.
	if reason := domainautomation.ClassifyLinearDispatch(eventType); reason != domainautomation.LinearDispatchNotSkipped {
		logger.Debug("linear: automation dispatch: event type not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
		return
	}

	if deps.Installations == nil {
		logger.Error("linear: automation dispatch: installations store not wired, skipping (fail closed)", "event_type", eventType)
		return
	}

	in, organizationID, actorExternalID, actorType, ok := buildLinearEventInput(eventType, rawBody)
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

	// D15/U3 audit fix: deps.IdentityLink.Users/Identities below need both
	// wired -- either nil (a minimal test rig, or any other wiring that
	// never populates IdentityLink at all) would otherwise reach a
	// nil-pointer dereference deep inside identitylink/actorauthz rather
	// than a named, logged, fail-closed skip, mirroring github's own
	// identical "identities == nil || users == nil" nil-safety check
	// (dispatchAutomationsBestEffort, automationdispatch.go). Deliberately
	// checked AFTER the installation-row lookup above, not before: an
	// uninstalled workspace must still be denied for THAT reason
	// specifically (organization_not_installed), never masked by this
	// unrelated wiring gap.
	if deps.IdentityLink.Users == nil || deps.IdentityLink.Identities == nil {
		logger.Error("linear: automation dispatch: identity link store not wired, skipping (fail closed)", "event_type", eventType)
		return
	}

	// # W2 audit fix, SECURITY: a non-"user" actor.type is denied, never routed to a substitute authorization path
	//
	// U7 originally mirrored github's own D12 split here exactly: a
	// non-"user" actor.type (domainautomation.ClassifyLinearActorOrigin ==
	// LinearEventOriginMachine) skipped this human-actor gate entirely and
	// dispatched under the automation's OWN creator's authorization
	// instead (automation.DispatchLinearWebhookEvent, lineardispatch.go).
	// Confirmed HIGH, SECURITY finding against that mirror: GitHub's split
	// is keyed on the EVENT TYPE (check_run/status), a value the sender
	// never supplies -- GitHub itself is structurally the only possible
	// actor. Linear's split was keyed on a per-PAYLOAD field (actor.type)
	// that the SAME "Issue"/"Comment" event category carries either value
	// for, depending only on how the issue was filed: an Integration such
	// as Slack, Zapier, a customer-facing intake form, or email-to-issue
	// reports something other than "user" for the identical category a
	// real person filing directly reports "user" for -- none of which
	// require any special privilege in a workspace that already has this
	// app installed. Routing THAT field's value to a DIFFERENT, weaker
	// authorization path (the automation creator's, almost always
	// satisfied) let any such actor bypass the human-actor gate entirely.
	//
	// This deployment has also never observed Linear's real wire value for
	// a non-"user" actor.type (Linear's own docs name "OAuth client" and
	// "Integration" as the two non-human kinds but do not publish their
	// exact strings) -- a security decision resting on an unobserved
	// external value is not a decision, it is a hope. So this now fails
	// CLOSED for LinearEventOriginMachine: denied outright, exactly like
	// an unlinked/unauthorized human actor below, never dispatched via any
	// path. The functional cost -- a genuinely machine-originated Linear
	// event (if Linear ever exposes one this deployment could distinguish
	// STRUCTURALLY, the way check_run/status are) cannot fire an
	// automation today -- is recorded in docs/DECISIONS.md's D-06 entry,
	// with an evaluable reopen condition, rather than silently accepted.
	// dispatchOneLinearAutomation (lineardispatch.go) no longer carries
	// any per-automation, creator-authorizing machine-origin gate at all
	// (that branch is now unreachable dead code with this adapter denying
	// upstream, so it was removed rather than left as a footgun a future
	// change could silently re-enable).
	//
	// The ok==false "actor deleted, origin unknown" case (Actor == nil, or
	// a present Actor with an empty Type) falls through to the human gate
	// below UNCHANGED -- the safe, narrower default, exactly like GitHub's
	// own "anything not explicitly classified Machine falls through to the
	// human-origin sender check" precedent.
	if origin, known := domainautomation.ClassifyLinearActorOrigin(actorType); known && origin == domainautomation.LinearEventOriginMachine {
		logger.Info("linear: automation dispatch: actor reported as non-\"user\" origin, denied -- fail closed (no structural equivalent of GitHub's event-type split observed for Linear, see docs/DECISIONS.md D-06)", "event_type", eventType, "reason", "linear_machine_origin_not_authorized")
		return
	}

	// U3 audit fix, SECURITY (confirmed HIGH finding: "the gate creates the
	// identity it then checks"): identitylink.LookupLinkedUserID is a PURE
	// lookup -- see this file's own linearAutomationEventEnvelope doc
	// comment (above) for the full "why", mirroring github's own
	// resolveCommenterActor exactly. A lookup FAILURE (as opposed to a
	// genuine "not linked" verdict) fails closed here too, never silently
	// treated as "not linked" -- mirroring github's own identical
	// resolveCommenterActor error-handling split (identity.go's own doc
	// comment there).
	actorUserID, _, err := identitylink.LookupLinkedUserID(ctx, deps.IdentityLink, sqlcgen.IdentityProviderLinear, actorExternalID)
	if err != nil {
		logger.Error("linear: automation dispatch: look up actor identity failed, skipping (fail closed)", "error", err, "event_type", eventType)
		return
	}
	if !actorauthz.AuthorizeLinkedActor(ctx, logger, authzSurface, deps.IdentityLink.Users, actorUserID, authz.ActionCreateSession, authz.Resource{}) {
		logger.Info("linear: automation dispatch: actor not authorized, skipping", "event_type", eventType, "reason", "actor_unlinked_or_unauthorized")
		return
	}

	automation.DispatchLinearWebhookEvent(ctx, logger, deps.Automations, deps.AutomationInvocations, deps.Timeouts, eventType, deliveryID, in)
}
