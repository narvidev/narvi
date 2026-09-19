package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/domain/authz"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// linearAutomationAuthzSurface mirrors githubAutomationAuthzSurface's own
// doc comment exactly (githubdispatch.go), for Linear's own per-automation,
// machine-origin gate below (U7 audit fix).
const linearAutomationAuthzSurface = "automation-linear"

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

// LinearTriggerLister mirrors GitHubTriggerLister, for Linear -- including
// MarkCreatorUnauthorized/ClearCreatorUnauthorized (U8 audit fix).
type LinearTriggerLister interface {
	ListActiveLinearAutomations(ctx context.Context) ([]sqlcgen.Automation, error)
	MarkCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error)
	ClearCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error)
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
// # U7 audit fix: a per-automation, machine-origin authorization lookup, mirroring GitHub's D12
//
// actorType is the event's own top-level "actor.type" (buildLinearEventInput,
// internal/adapters/inbound/linear/automationdispatch.go) -- users is the
// SAME *postgres.UserStore the human-origin gate one layer up
// (dispatchAutomationsBestEffort) already resolves its own actor against,
// threaded down here specifically for domainautomation.
// ClassifyLinearActorOrigin's own machine-origin bucket: an "Issue"/
// "Comment" event actor reported as something other than a real Linear
// account (Linear's own docs: "Could be a User, OAuth client, or
// Integration") has no human identity for the adapter to have authorized;
// instead, THIS automation's own creator must be a linked, non-disabled
// account still holding authz.ActionCreateSession -- the maintainer who
// created this automation is the human decision being honoured, exactly
// like GitHubEventOriginMachine's own identical reasoning
// (dispatchOneGitHubAutomation, githubdispatch.go). Human-origin events
// (and the ok == false "actor deleted, origin unknown" case --
// ClassifyLinearActorOrigin's own doc comment) are UNAFFECTED: their actor
// was already authorized once, upstream, by dispatchAutomationsBestEffort,
// before this automation was even listed.
func DispatchLinearWebhookEvent(ctx context.Context, logger *slog.Logger, automations LinearTriggerLister, invocations DeliveryInvocationCreator, users *postgres.UserStore, timeouts platform.Timeouts, eventType string, deliveryID string, in domainautomation.LinearEventInput, actorType string) {
	if reason := domainautomation.ClassifyLinearDispatch(eventType); reason != domainautomation.LinearDispatchNotSkipped {
		logger.Debug("automation: linear event not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
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
		rows, err = automations.ListActiveLinearAutomations(ctx)
		return err
	})
	if retryErr != nil {
		logger.Error("automation: list active linear automations failed", "error", retryErr)
		return
	}

	for _, row := range rows {
		dispatchOneLinearAutomation(ctx, logger, automations, invocations, users, timeouts, row, eventType, deliveryID, in, actorType)
	}
}

func dispatchOneLinearAutomation(ctx context.Context, logger *slog.Logger, automations LinearTriggerLister, invocations DeliveryInvocationCreator, users *postgres.UserStore, timeouts platform.Timeouts, row sqlcgen.Automation, eventType string, deliveryID string, in domainautomation.LinearEventInput, actorType string) {
	logger = logger.With("automation_id", row.ID.String())

	cfg, err := unmarshalLinearTriggerConfig(row.TriggerConfig)
	if err != nil {
		logger.Error("automation: decode linear trigger config failed", "error", err)
		return
	}
	if !domainautomation.MatchesLinearTrigger(cfg, in) {
		return
	}

	// U7 audit fix: see this function's own caller's doc comment
	// (DispatchLinearWebhookEvent, above) for the full "why". Not wrapped
	// in platform.Retry (unlike this file's other Postgres round trips,
	// D6) -- it fails closed on any error, exactly like every other
	// actorauthz.AuthorizeLinkedActor call site across this codebase,
	// mirroring dispatchOneGitHubAutomation's own identical choice.
	if origin, known := domainautomation.ClassifyLinearActorOrigin(actorType); known && origin == domainautomation.LinearEventOriginMachine {
		if !actorauthz.AuthorizeLinkedActor(ctx, logger, linearAutomationAuthzSurface, users, row.CreatedBy, authz.ActionCreateSession, authz.Resource{}) {
			logger.Info("automation: linear dispatch: automation creator not authorized for a machine-originated actor, skipping", "event_type", eventType, "reason", "creator_unlinked_or_unauthorized")
			markCreatorUnauthorizedBestEffort(ctx, logger, automations, row.ID)
			return
		}
		// U8 audit fix: see githubdispatch.go's own identical call site
		// doc comment (dispatchOneGitHubAutomation) for the full "why".
		clearCreatorUnauthorizedBestEffort(ctx, logger, automations, row.ID)
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
		} else {
			logger.Error("automation: create invocation for linear dispatch failed", "error", err, "event_type", eventType)
		}
		return
	}
	if !created {
		logger.Info("automation: linear dispatch already created an invocation for this exact delivery, skipping", "event_type", eventType, "delivery_id", deliveryID)
	}
}
