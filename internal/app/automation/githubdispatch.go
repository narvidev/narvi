package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/domain/authz"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// githubAutomationAuthzSurface is D12 audit fix's own "surface" label for
// actorauthz.AuthorizeLinkedActor calls made from THIS package (the
// per-automation, machine-origin gate below) -- kept distinct from
// internal/adapters/inbound/github's own "github" constant (identity.go)
// so a log reader can tell "the adapter denied the human sender, once per
// delivery" apart from "the per-automation creator-authorization gate
// denied a machine-originated event for THIS ONE automation", even though
// both ultimately gate the identical authz.ActionCreateSession row.
const githubAutomationAuthzSurface = "automation-github"

// githubTriggerConfigJSON is this package's OWN small, private, decode-only
// copy of the github trigger_config wire shape -- see
// cronTriggerConfigJSON's own doc comment (triggerpump.go) for why this is
// never shared with internal/adapters/inbound/httpapi's own
// githubTriggerConfigWire (an import cycle: this package already imports
// httpapi, for CreateSessionOnTx). Field names/json tags match that wire
// struct exactly, so a value httpapi wrote decodes here unchanged.
type githubTriggerConfigJSON struct {
	Event      string `json:"event"`
	Action     string `json:"action,omitempty"`
	Label      string `json:"label,omitempty"`
	Name       string `json:"name,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

func unmarshalGitHubTriggerConfig(raw []byte) (domainautomation.GitHubTriggerConfig, error) {
	var wire githubTriggerConfigJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return domainautomation.GitHubTriggerConfig{}, fmt.Errorf("automation: unmarshal github trigger config: %w", err)
		}
	}
	return domainautomation.GitHubTriggerConfig{
		Event:      wire.Event,
		Action:     wire.Action,
		Label:      wire.Label,
		Name:       wire.Name,
		Conclusion: wire.Conclusion,
	}, nil
}

// GitHubTriggerLister is the narrow slice of *postgres.AutomationStore
// DispatchGitHubWebhookEvent needs -- mirrors InvocationCreator's own
// "small, locally-defined interface so a unit/integration test can inject
// a fake with no real DB round trip, or one that deliberately panics to
// prove dispatch failure isolation" reasoning (invocationenqueue.go).
//
// MarkCreatorUnauthorized/ClearCreatorUnauthorized (U8 audit fix) widen
// this beyond a pure "lister": dispatchOneGitHubAutomation's own
// machine-origin gate (below) needs to WRITE this automation's own
// creator_unauthorized_since the moment it denies or re-authorizes, and
// *postgres.AutomationStore is already the SAME concrete type every real
// caller passes as GitHubTriggerLister, so widening this interface costs
// production wiring nothing.
type GitHubTriggerLister interface {
	ListActiveGitHubAutomations(ctx context.Context) ([]sqlcgen.Automation, error)
	MarkCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error)
	ClearCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error)
}

// githubDeliveryProvider is the SAME literal
// internal/adapters/inbound/github's own handler.go already uses for
// WebhookDeliveryStore.Claim -- this package's own copy (D1 audit fix,
// automation_invocations.source_provider), never imported from there (that
// package imports THIS one, for GitHubTriggerLister/InvocationCreator --
// importing back would be a cycle).
const githubDeliveryProvider = "github"

// DispatchGitHubWebhookEvent is §8.4's own live-dispatch entry point
// for a real, already-claimed (deduplicated -- see this package's own
// doc.go) GitHub webhook delivery: called inline, synchronously, from
// internal/adapters/inbound/github's own handler.go, for EVERY delivery
// that handler processes -- an ADDITIONAL, independent consumer of the
// SAME delivery the @mention/merge-gate lanes already process, never a
// replacement for either (see that adapter's own doc comment on why a
// panic/error here must never suppress them).
//
// domainautomation.ClassifyGitHubDispatch(eventType) is checked first --
// see that function's own doc comment for the closed, typed allowlist it
// enforces; an eventType outside it returns immediately, before ever
// listing an automation.
//
// For each active, TriggerTypeGitHub automation whose own trigger_config
// (domainautomation.MatchesGitHubTrigger) matches in, this narrows that
// automation's own configured target repos to exactly the ones
// domainautomation.TargetMatchesGitHubEvent accepts (repo scoping, and --
// §8.4's own named trap -- branch-TIP scoping, never containment) and,
// only when at least one target survives that narrowing, calls
// CreateInvocationForDelivery with that narrowed target list, keyed on
// (row.ID, githubDeliveryProvider, deliveryID) -- D1 audit fix: idempotent
// regardless of how many times this exact delivery is re-dispatched (a
// real GitHub redelivery, or this package's OWN bounded retry below).
// D8 audit fix: gated behind a per-automation throttle
// (domainautomation.EvaluateDispatchThrottle) BEFORE that call, so one
// automation's own noisy event stream cannot create unbounded invocations.
// One automation's own failure (a malformed trigger_config, a Postgres
// error) is isolated: logged, and does NOT abort evaluating the rest of
// the batch -- mirrors evaluateCronAutomation's own identical per-row
// isolation (triggerpump.go).
//
// # D6 audit fix: transient vs. permanent, and a bounded retry that never touches the shared claim
//
// The two genuine Postgres round trips below (ListActiveGitHubAutomations
// here, CreateForDelivery/CountRecentInvocations in
// dispatchOneGitHubAutomation) are each wrapped in platform.Retry, bounded
// by timeouts.AutomationDispatchMaxAttempts/
// AutomationDispatchRetryBaseDelay/AutomationDispatchRetryMaxDelay -- a
// genuinely TRANSIENT failure (a dropped connection, a momentary Postgres
// blip) now self-heals within THIS SAME request. This is the other half of
// D1/D6's shared design: this package does not, and must not, touch the
// webhook_deliveries claim in either direction (see CreateInvocationForDelivery's
// own doc comment, invocationenqueue.go) -- so a transient failure's ONLY
// possible retry path is an inline one, bounded by this handler's own
// request budget, never a claim-release-triggered external redelivery. A
// PERMANENT/business outcome (no matching automation, the trigger's own
// filter/target narrowing rejects this event, a malformed per-row
// trigger_config, the throttle denies) is NOT retried -- redelivering the
// identical event would only ever reproduce the identical, deterministic
// verdict. A PANIC (recovered one layer up, by the adapter's own
// dispatchAutomationsBestEffort) is also not retried here -- a
// programming bug reproduces itself identically on a retry; only this
// package's OWN two Postgres calls are.
//
// # D18 audit fix: one shared total budget, not a per-automation multiple
//
// Both of those platform.Retry-wrapped round trips are bounded per CALL --
// but dispatchOneGitHubAutomation runs once per row in the `for _, row :=
// range rows` loop below, and checkDispatchThrottle/
// createInvocationForDeliveryWithRetry (each itself another
// platform.Retry call) run inside THAT function -- so, before this fix,
// this function's own total worst-case sleep was the single-call bound
// multiplied by however many automations this delivery's trigger type had
// configured, not the single-call bound itself (confirmed, MEDIUM
// finding). ctx is now wrapped in a single context.WithTimeout(ctx,
// timeouts.AutomationDispatchTotalBudget) covering the list call AND the
// entire per-automation loop -- every platform.Retry call below shares
// this ONE deadline and returns ctx.Err() promptly once it expires
// (platform.Retry's own doc comment), so the total time this function may
// spend retrying is now bounded regardless of how many automations match.
// See AutomationDispatchTotalBudget's own doc comment (platform/
// timeouts.go) for the chosen value and the full "why".
//
// # D12 audit fix: a per-automation, machine-origin authorization lookup
//
// dispatchOneGitHubAutomation also calls actorauthz.AuthorizeLinkedActor
// against row.CreatedBy for a GitHubEventOriginMachine event (check_run,
// status) -- a THIRD genuine Postgres round trip this file makes, but
// deliberately NOT wrapped in platform.Retry: it fails closed on any
// error, exactly like every other actorauthz call site in this codebase
// (github/linear/slack's own identity.go files), none of which retry
// either -- this is a consistent, established choice, not a gap this fix
// introduces.
func DispatchGitHubWebhookEvent(ctx context.Context, logger *slog.Logger, automations GitHubTriggerLister, invocations DeliveryInvocationCreator, users *postgres.UserStore, timeouts platform.Timeouts, eventType string, deliveryID string, in domainautomation.GitHubEventInput) {
	if reason := domainautomation.ClassifyGitHubDispatch(eventType); reason != domainautomation.GitHubDispatchNotSkipped {
		// U10 audit fix: downgraded from Warn -- after U6/U10's own
		// adapter-side reordering (internal/adapters/inbound/github's own
		// dispatchAutomationsBestEffort), this event type is now already
		// filtered out BEFORE this function is ever called on the live
		// path; this check stays only as a defensive, direct-call-safe
		// backstop (this package's own unit tests call this function
		// directly), and firing on ordinary, never-subscribed-to traffic
		// was never actually a WARN-worthy symptom in the first place.
		logger.Debug("automation: github event not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
		return
	}

	// D18 audit fix (confirmed finding: "the retry budget multiplies
	// inside a loop"): every platform.Retry call below -- the list call
	// here, plus checkDispatchThrottle/createInvocationForDeliveryWithRetry
	// called ONCE PER MATCHING AUTOMATION inside the loop below -- now
	// shares this ONE deadline, so the total time this function may spend
	// retrying is bounded regardless of how many automations match, not
	// the single-call bound multiplied by that count. See
	// platform.Timeouts.AutomationDispatchTotalBudget's own doc comment
	// for the full "why", and dispatchTotalBudgetContext's own doc comment
	// for why an UNCONFIGURED (zero-value) budget must NOT be handed to
	// context.WithTimeout directly.
	ctx, cancel := dispatchTotalBudgetContext(ctx, timeouts.AutomationDispatchTotalBudget)
	defer cancel()

	var rows []sqlcgen.Automation
	retryErr := platform.Retry(ctx, timeouts.AutomationDispatchMaxAttempts, timeouts.AutomationDispatchRetryBaseDelay, timeouts.AutomationDispatchRetryMaxDelay, func() error {
		var err error
		rows, err = automations.ListActiveGitHubAutomations(ctx)
		return err
	})
	if retryErr != nil {
		logger.Error("automation: list active github automations failed", "error", retryErr)
		return
	}

	for _, row := range rows {
		dispatchOneGitHubAutomation(ctx, logger, automations, invocations, users, timeouts, row, eventType, deliveryID, in)
	}
}

// dispatchTotalBudgetContext wraps ctx in a context.WithTimeout bounded by
// budget -- D18 audit fix's own total-budget cap, shared by
// DispatchGitHubWebhookEvent/DispatchLinearWebhookEvent. budget <= 0 (the
// Go zero value -- an UNCONFIGURED platform.Timeouts, which this exact
// package's own test fixtures and this codebase's other minimal-wiring
// test rigs deliberately leave zero for fields "nothing yet cares about")
// is treated as "no additional cap": returns ctx completely UNCHANGED (a
// no-op cancel), never context.WithTimeout(ctx, 0) -- which creates a
// context whose deadline is effectively already past, silently failing
// EVERY dispatch closed regardless of event type, human- or
// machine-origin, the instant it is used. That degenerate input was
// caught by this batch's own new machine-origin dispatch test
// (TestGitHubIntegration_AutomationDispatchFiresOnCheckRunWithAuthorizedCreator,
// github/automationdispatch_integration_test.go) failing with "list
// active github automations failed: context deadline exceeded" against a
// test rig that -- like every OTHER pre-existing test in that file --
// never sets Config.Timeouts at all. Mirrors platform.Retry's own
// identical "attempts < 1 is treated as 1, never zero calls" convention
// for its own degenerate input: a zero/invalid budget falls back to a
// safe default (here, "whatever deadline ctx already carries, unchanged")
// rather than the literal, silently-catastrophic zero-value behavior.
func dispatchTotalBudgetContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, budget)
}

func dispatchOneGitHubAutomation(ctx context.Context, logger *slog.Logger, automations GitHubTriggerLister, invocations DeliveryInvocationCreator, users *postgres.UserStore, timeouts platform.Timeouts, row sqlcgen.Automation, eventType string, deliveryID string, in domainautomation.GitHubEventInput) {
	logger = logger.With("automation_id", row.ID.String())

	cfg, err := unmarshalGitHubTriggerConfig(row.TriggerConfig)
	if err != nil {
		logger.Error("automation: decode github trigger config failed", "error", err)
		return
	}
	if !domainautomation.MatchesGitHubTrigger(cfg, in) {
		return
	}

	// D12 audit fix: a machine-originated event type (check_run, status --
	// GitHub itself is always the actor, never a human account) has no
	// sender identity for the adapter to have authorized; instead, THIS
	// automation's own creator must be a linked, non-disabled account
	// still holding authz.ActionCreateSession -- the maintainer who
	// created this automation and deliberately chose this trigger is the
	// human decision being honoured. row.CreatedBy is nullable (ON DELETE
	// SET NULL, migrations/000051_automations.up.sql: "an automation, like
	// a session, can outlive the user who created it") --
	// actorauthz.AuthorizeLinkedActor denies immediately when it is
	// invalid, exactly like an unlinked GitHub sender is denied on the
	// human-origin path (internal/adapters/inbound/github's own
	// dispatchAutomationsBestEffort). Human-originated events are
	// UNAFFECTED: their sender was already authorized once, upstream, by
	// that SAME adapter function, before this automation was even listed
	// -- see GitHubEventOrigin's own doc comment (domain/automation/
	// dispatch.go) for why the two origins are deliberately gated at
	// different layers rather than forced onto one shared call site. This
	// lookup is NOT wrapped in platform.Retry (unlike this file's other
	// Postgres round trips, D6) -- it fails closed on any error, exactly
	// like every other actorauthz.AuthorizeLinkedActor/AuthorizeResolvedActor
	// call site across this codebase (github/linear/slack's own identity.go
	// files), none of which retry either.
	if origin, known := domainautomation.ClassifyGitHubEventOrigin(eventType); known && origin == domainautomation.GitHubEventOriginMachine {
		if !actorauthz.AuthorizeLinkedActor(ctx, logger, githubAutomationAuthzSurface, users, row.CreatedBy, authz.ActionCreateSession, authz.Resource{}) {
			logger.Info("automation: github dispatch: automation creator not authorized for a machine-originated event, skipping", "event_type", eventType, "reason", "creator_unlinked_or_unauthorized")
			markCreatorUnauthorizedBestEffort(ctx, logger, automations, row.ID)
			return
		}
		// U8 audit fix: a machine-origin dispatch that JUST authorized
		// means this automation's own creator is linked/authorized again --
		// clear any earlier denial's own mark (a no-op, one guarded UPDATE
		// matching zero rows, when there was nothing to clear).
		clearCreatorUnauthorizedBestEffort(ctx, logger, automations, row.ID)
	}

	targets, err := UnmarshalTargets(row.Repos)
	if err != nil {
		logger.Error("automation: decode automation repos for github dispatch failed", "error", err)
		return
	}

	matched := make([]domainautomation.Target, 0, len(targets))
	for _, target := range targets {
		if domainautomation.TargetMatchesGitHubEvent(target, in) {
			matched = append(matched, target)
		}
	}
	if len(matched) == 0 {
		// The trigger's own Event/Action/Label/Name/Conclusion filter
		// matched, but none of this automation's own configured target
		// repos/branches actually concern this event (wrong repo, or --
		// §8.4's own named trap -- a branch that merely contains the
		// commit rather than being its tip). Not an error: this
		// automation simply has nothing to do for this delivery.
		logger.Debug("automation: github trigger matched but no configured target repo/branch concerns this event", "event_type", eventType)
		return
	}

	if verdict := checkDispatchThrottle(ctx, logger, invocations, timeouts, row.ID); verdict != dispatchGateAllowed {
		return
	}

	created, err := createInvocationForDeliveryWithRetry(ctx, invocations, timeouts, row.ID, matched, githubDeliveryProvider, deliveryID)
	if err != nil {
		if dispatchBudgetExhausted(err) {
			logger.Warn("automation: create invocation for github dispatch: total time budget exhausted before this call could complete, skipping (fail closed) -- NOT a throttle decision, see platform.Timeouts.AutomationDispatchTotalBudget", "reason", "dispatch_budget_exhausted", "error", err, "event_type", eventType)
		} else {
			logger.Error("automation: create invocation for github dispatch failed", "error", err, "event_type", eventType)
		}
		return
	}
	if !created {
		logger.Info("automation: github dispatch already created an invocation for this exact delivery, skipping", "event_type", eventType, "delivery_id", deliveryID)
	}
}

// dispatchGateVerdict is checkDispatchThrottle's own return type -- U1
// audit fix (confirmed HIGH finding: "the total budget is smaller than the
// retry chain it contains... [exhaustion] is logged as a throttle, which
// is a different thing and sends whoever reads it looking in the wrong
// place"). A plain bool could not tell a caller (or a log reader)
// EvaluateDispatchThrottle's own genuine "this automation has created too
// many invocations recently" verdict apart from "the shared
// AutomationDispatchTotalBudget ran out before this automation's own
// Postgres call could even complete" (an infrastructure/capacity
// condition an operator should size the budget or automation count
// against, not the anti-abuse control doing its job) apart from an
// ordinary, non-time-budget Postgres failure. All three still deny (fail
// closed -- no invocation is created in any case); this type exists
// purely for OBSERVABILITY, so a log reader -- or a test asserting on this
// return value -- is pointed at the right cause.
type dispatchGateVerdict int

const (
	dispatchGateAllowed dispatchGateVerdict = iota
	dispatchGateThrottled
	dispatchGateBudgetExhausted
	dispatchGateError
)

// dispatchBudgetExhausted reports whether err is ctx's OWN expiry -- either
// DispatchGitHubWebhookEvent/DispatchLinearWebhookEvent's own
// dispatchTotalBudgetContext deadline (D18/U1), or a deadline the caller's
// own incoming ctx already carried -- as opposed to a genuine, repeated
// Postgres error unrelated to time budget. Neither
// ListActiveGitHubAutomations/ListActiveLinearAutomations nor
// CountRecentInvocations/CreateForDelivery wraps its OWN call with any
// separate context.WithTimeout (each runs directly against whatever ctx
// this package's own callers already pass in) -- so a DeadlineExceeded/
// Canceled surfacing from any of them can only ever be attributed to a
// shared budget/caller ctx, never a private per-call timeout of their own.
func dispatchBudgetExhausted(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// creatorUnauthorizedMarker is the narrow slice of GitHubTriggerLister/
// LinearTriggerLister markCreatorUnauthorizedBestEffort/
// clearCreatorUnauthorizedBestEffort (below) need -- shared VERBATIM
// between both dispatch paths' own machine-origin gates
// (dispatchOneGitHubAutomation here, dispatchOneLinearAutomation,
// lineardispatch.go), U8 audit fix.
type creatorUnauthorizedMarker interface {
	MarkCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error)
	ClearCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error)
}

// markCreatorUnauthorizedBestEffort/clearCreatorUnauthorizedBestEffort are
// U8 audit fix's own required surface (confirmed LOW finding: "a
// machine-origin automation can become permanently dead with no way to
// revive it... An automation that is active and structurally incapable of
// firing is a lie in the product's own state"). Called from each
// provider's own per-automation machine-origin gate, immediately after
// its own actorauthz.AuthorizeLinkedActor verdict -- see migrations/
// 000138_automations_creator_unauthorized.up.sql's own doc comment for
// the full "why" this specific state (rather than re-attribution
// tooling) is this fix's chosen scope.
//
// Deliberately BEST EFFORT, never retried, never fail-closed: this is
// OBSERVABILITY state, not an authorization decision -- the
// AuthorizeLinkedActor verdict immediately above already decided whether
// to dispatch; a failure writing creator_unauthorized_since must never
// retroactively change that, and must never cost this delivery another
// entry in the U1 retry-chain budget math (AutomationDispatchTotalBudget
// is sized against list+throttle+create -- this is a FOURTH, genuinely
// optional call, logged and dropped on error, not added to that budget's
// own required floor).
func markCreatorUnauthorizedBestEffort(ctx context.Context, logger *slog.Logger, automations creatorUnauthorizedMarker, id pgtype.UUID) {
	if _, err := automations.MarkCreatorUnauthorized(ctx, id); err != nil {
		logger.Error("automation: mark automation creator_unauthorized_since failed (best effort, not retried)", "error", err, "automation_id", id.String())
	}
}

func clearCreatorUnauthorizedBestEffort(ctx context.Context, logger *slog.Logger, automations creatorUnauthorizedMarker, id pgtype.UUID) {
	if _, err := automations.ClearCreatorUnauthorized(ctx, id); err != nil {
		logger.Error("automation: clear automation creator_unauthorized_since failed (best effort, not retried)", "error", err, "automation_id", id.String())
	}
}

// checkDispatchThrottle is D8's own gate, shared verbatim (via
// createInvocationForDeliveryWithRetry's own sibling call in
// lineardispatch.go) between the GitHub and Linear dispatch paths --
// counts row's own invocations created within
// timeouts.AutomationDispatchThrottleWindow (retried the SAME bounded way
// as every other Postgres call in this file, D6) and reports whether
// ANOTHER may be created.
func checkDispatchThrottle(ctx context.Context, logger *slog.Logger, invocations DeliveryInvocationCreator, timeouts platform.Timeouts, automationID pgtype.UUID) dispatchGateVerdict {
	since := time.Now().Add(-timeouts.AutomationDispatchThrottleWindow)
	var count int64
	retryErr := platform.Retry(ctx, timeouts.AutomationDispatchMaxAttempts, timeouts.AutomationDispatchRetryBaseDelay, timeouts.AutomationDispatchRetryMaxDelay, func() error {
		var err error
		count, err = invocations.CountRecentInvocations(ctx, automationID, pgtype.Timestamptz{Time: since, Valid: true})
		return err
	})
	if retryErr != nil {
		if dispatchBudgetExhausted(retryErr) {
			logger.Warn("automation: dispatch: total time budget exhausted before this automation's own recent-invocation count could complete, skipping (fail closed) -- NOT a throttle decision, see platform.Timeouts.AutomationDispatchTotalBudget", "reason", "dispatch_budget_exhausted", "error", retryErr)
			return dispatchGateBudgetExhausted
		}
		logger.Error("automation: count recent invocations for dispatch throttle failed, skipping (fail closed)", "error", retryErr)
		return dispatchGateError
	}
	if !domainautomation.EvaluateDispatchThrottle(int(count)) {
		logger.Warn("automation: dispatch throttled, this automation has created too many invocations recently", "reason", "dispatch_throttled", "count_in_window", count, "threshold", domainautomation.DispatchThrottleThreshold)
		return dispatchGateThrottled
	}
	return dispatchGateAllowed
}

// createInvocationForDeliveryWithRetry wraps CreateInvocationForDelivery
// (invocationenqueue.go) in the SAME bounded platform.Retry every other
// Postgres call in this file uses (D6) -- shared between the GitHub and
// Linear dispatch paths (lineardispatch.go's own identical call).
func createInvocationForDeliveryWithRetry(ctx context.Context, invocations DeliveryInvocationCreator, timeouts platform.Timeouts, automationID pgtype.UUID, targets []domainautomation.Target, provider, deliveryID string) (created bool, err error) {
	retryErr := platform.Retry(ctx, timeouts.AutomationDispatchMaxAttempts, timeouts.AutomationDispatchRetryBaseDelay, timeouts.AutomationDispatchRetryMaxDelay, func() error {
		_, wasCreated, createErr := CreateInvocationForDelivery(ctx, invocations, automationID, targets, provider, deliveryID)
		if createErr != nil {
			return createErr
		}
		created = wasCreated
		return nil
	})
	return created, retryErr
}
