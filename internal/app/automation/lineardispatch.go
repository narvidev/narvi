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
// httpapi's own linearTriggerConfigWire). OrganizationID is W3 audit fix's
// own required addition -- see domainautomation.LinearTriggerConfig.
// OrganizationID's own doc comment.
type linearTriggerConfigJSON struct {
	EventType      string `json:"eventType"`
	Action         string `json:"action,omitempty"`
	TeamKey        string `json:"teamKey,omitempty"`
	OrganizationID string `json:"organizationId,omitempty"`
}

func unmarshalLinearTriggerConfig(raw []byte) (domainautomation.LinearTriggerConfig, error) {
	var wire linearTriggerConfigJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return domainautomation.LinearTriggerConfig{}, fmt.Errorf("automation: unmarshal linear trigger config: %w", err)
		}
	}
	return domainautomation.LinearTriggerConfig{EventType: wire.EventType, Action: wire.Action, TeamKey: wire.TeamKey, OrganizationID: wire.OrganizationID}, nil
}

// LinearTriggerLister mirrors GitHubTriggerLister's own "narrow slice of
// *postgres.AutomationStore" shape, for Linear. Unlike GitHubTriggerLister,
// this carries no MarkCreatorUnauthorized/ClearCreatorUnauthorized: W2
// audit fix removed dispatchOneLinearAutomation's own per-automation,
// machine-origin gate entirely (see DispatchLinearWebhookEvent's own doc
// comment) -- nothing in this file writes automations.
// creator_unauthorized_since for the Linear path any more, so requiring
// those two methods here would be dead surface, never called.
// ListActiveLinearAutomations now takes organizationID (W3 audit fix,
// confirmed HIGH, TENANT ISOLATION finding) -- see the generated query's
// own doc comment (queries/automations.sql) for the full "why".
type LinearTriggerLister interface {
	ListActiveLinearAutomations(ctx context.Context, organizationID string) ([]sqlcgen.Automation, error)
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
//
// D18 audit fix: mirrors DispatchGitHubWebhookEvent's own identical fix
// too -- ctx is wrapped in a single context.WithTimeout(ctx, timeouts.
// AutomationDispatchTotalBudget) covering the list call AND the entire
// per-automation loop below, so every platform.Retry call inside shares
// ONE deadline instead of the single-call bound multiplying by however
// many Linear-triggered automations this delivery evaluates.
//
// # W2 audit fix: no per-automation, machine-origin authorization lookup -- unlike GitHub's D12
//
// U7 originally threaded actorType (the event's own top-level "actor.type",
// buildLinearEventInput, internal/adapters/inbound/linear/
// automationdispatch.go) and a *postgres.UserStore down to THIS function
// specifically to authorize domainautomation.ClassifyLinearActorOrigin's
// own machine-origin bucket against each matching automation's OWN
// creator, mirroring GitHubEventOriginMachine's structurally-justified
// reasoning (dispatchOneGitHubAutomation, githubdispatch.go). W2 audit fix
// (SECURITY, confirmed HIGH) found that mirror unsound for Linear: unlike
// GitHub's check_run/status (an event TYPE only GitHub itself can ever
// emit), Linear's actor.type is a per-payload field the SAME "Issue"/
// "Comment" category carries either value for, depending only on how the
// issue was filed -- not something the sender is structurally prevented
// from influencing. Routing that field's value to a substitute
// (automation-creator) authorization path was a real bypass of the
// human-actor gate, not a legitimate structural necessity. That gate is
// now denied entirely, one layer up, before any automation is even listed
// (internal/adapters/inbound/linear's own dispatchAutomationsBestEffort) --
// so this function is called ONLY for an already-authorized human-origin
// actor (or the "actor deleted, origin unknown" case, which was always
// routed through the ordinary human-actor gate). There is therefore
// nothing left for a per-automation gate here to do: no actorType/users
// parameter, no machine-origin branch. See docs/DECISIONS.md's D-07 entry
// for the resulting, accepted functional limitation and its reopen
// condition, and domain/automation/dispatch.go's own LinearEventOrigin doc
// comment for the full "why".
func DispatchLinearWebhookEvent(ctx context.Context, logger *slog.Logger, automations LinearTriggerLister, invocations DeliveryInvocationCreator, timeouts platform.Timeouts, eventType string, deliveryID string, in domainautomation.LinearEventInput) {
	if reason := domainautomation.ClassifyLinearDispatch(eventType); reason != domainautomation.LinearDispatchNotSkipped {
		logger.Debug("automation: linear event not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
		return
	}

	// W3 audit fix (confirmed HIGH, TENANT ISOLATION finding): an empty
	// in.OrganizationID must never be treated as "list every organization's
	// own automations" -- fails closed, loudly, before ever querying.
	// Structurally unreachable on the real webhook path (D9's own
	// installation lookup, internal/adapters/inbound/linear/
	// automationdispatch.go, already requires a non-empty organizationId
	// to resolve an installation before this function is ever called), but
	// this function is also called directly by this package's own tests,
	// and a defensive fail-closed default here costs nothing.
	if in.OrganizationID == "" {
		logger.Error("automation: linear dispatch: empty organization id, skipping (fail closed)", "event_type", eventType)
		return
	}

	// See dispatchTotalBudgetContext's own doc comment (githubdispatch.go)
	// for why an unconfigured (zero-value) budget must NOT be handed to
	// context.WithTimeout directly.
	ctx, cancel := dispatchTotalBudgetContext(ctx, timeouts.AutomationDispatchTotalBudget)
	defer cancel()

	var rows []sqlcgen.Automation
	retryErr := platform.Retry(ctx, timeouts.AutomationDispatchMaxAttempts, timeouts.AutomationDispatchRetryBaseDelay, timeouts.AutomationDispatchRetryMaxDelay, func() error {
		var err error
		rows, err = automations.ListActiveLinearAutomations(ctx, in.OrganizationID)
		return err
	})
	if retryErr != nil {
		if dispatchBudgetExhausted(retryErr) {
			// Y2 audit fix, mirroring DispatchGitHubWebhookEvent's own
			// identical fix (githubdispatch.go): the shared budget expiring
			// HERE, before the automation list is even read, drops EVERY
			// automation that would have matched this delivery -- not one,
			// like the throttle_check/create stages below. See
			// recordAutomationDispatchDropped's own doc comment
			// (dispatchmetrics.go) for why this is a third, distinct stage.
			logger.Warn("automation: list active linear automations: total time budget exhausted before this call could complete, skipping every matching automation for this delivery (fail closed) -- NOT a throttle decision, see platform.Timeouts.AutomationDispatchTotalBudget", "reason", "dispatch_budget_exhausted", "error", retryErr)
			recordAutomationDispatchDropped(ctx, "list")
		} else {
			logger.Error("automation: list active linear automations failed", "error", retryErr)
		}
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

	if verdict := checkDispatchThrottle(ctx, logger, invocations, timeouts, row.ID); verdict != dispatchGateAllowed {
		return
	}

	created, err := createInvocationForDeliveryWithRetry(ctx, invocations, timeouts, row.ID, targets, linearDeliveryProvider, deliveryID)
	if err != nil {
		if dispatchBudgetExhausted(err) {
			logger.Warn("automation: create invocation for linear dispatch: total time budget exhausted before this call could complete, skipping (fail closed) -- NOT a throttle decision, see platform.Timeouts.AutomationDispatchTotalBudget", "reason", "dispatch_budget_exhausted", "error", err, "event_type", eventType)
			recordAutomationDispatchDropped(ctx, "create") // W4 audit fix, see dispatchmetrics.go
		} else {
			logger.Error("automation: create invocation for linear dispatch failed", "error", err, "event_type", eventType)
		}
		return
	}
	if !created {
		logger.Info("automation: linear dispatch already created an invocation for this exact delivery, skipping", "event_type", eventType, "delivery_id", deliveryID)
	}
}
