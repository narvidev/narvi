package automation

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// This file is W4 audit fix's own required visibility half (confirmed
// MEDIUM finding: "budget exhaustion drops automations permanently and
// invisibly"). When platform.Timeouts.AutomationDispatchTotalBudget runs
// out mid-loop (DispatchGitHubWebhookEvent/DispatchLinearWebhookEvent, one
// shared context.WithTimeout covering every matching automation's own
// throttle-check and create-invocation calls, D18/U1), every REMAINING
// matching automation is dropped -- and, unlike an ordinary transient
// Postgres error (D6's own bounded inline retry self-heals those WITHIN
// the same request), this drop is permanent: the webhook delivery's own
// claim is already taken (internal/app/automation's own doc.go,
// CreateInvocationForDelivery), so a provider redelivery returns at the
// duplicate check before dispatch ever runs again, and no reconciler
// exists over automation_invocations.source_delivery_id to notice a
// matching automation that never got its own invocation row. Before this
// fix, the ONLY trace of a dropped automation was a single Warn log line
// (dispatchGateBudgetExhausted/dispatchBudgetExhausted, githubdispatch.go)
// inside a best-effort dispatch lane -- indistinguishable, to an operator
// with no log-search habit built around this exact phrase, from nothing
// having happened at all. See docs/DECISIONS.md's D-08 entry for why this
// batch ships VISIBILITY rather than a durable fix (a genuinely
// unbounded-N design needs dispatch off the request path entirely) and
// its own reopen condition.

// automationDispatchMeterName mirrors internal/adapters/inbound/httpapi's
// own cloudIdentityMeterName precedent exactly (one named meter per major
// subsystem, §5.3).
const automationDispatchMeterName = "narvi/automation-dispatch"

// automationDispatchDroppedTotalCounter is resolved LAZILY, on first use
// (sync.OnceValue) -- mirrors cloudIdentityMintTotalCounter's own identical
// reasoning (internal/adapters/inbound/httpapi/cloudidentitymetrics.go):
// DispatchGitHubWebhookEvent/DispatchLinearWebhookEvent are free functions
// with no per-process constructor object to anchor eager resolution to,
// and eager package-init resolution would permanently bind this instrument
// to whatever MeterProvider happens to be globally registered before
// main.go's own real OTel SDK setup (or a test's own TestMain) runs.
var automationDispatchDroppedTotalCounter = sync.OnceValue(newAutomationDispatchDroppedTotalCounter)

func newAutomationDispatchDroppedTotalCounter() metric.Int64Counter {
	c, err := otel.Meter(automationDispatchMeterName).Int64Counter(
		"automation_dispatch_dropped_total",
		metric.WithDescription("Count of every automation dispatch attempt (a specific automation matched, or -- for the \"list\" stage, WOULD have matched, a live GitHub/Linear webhook delivery's trigger) abandoned because platform.Timeouts.AutomationDispatchTotalBudget ran out before its own list, throttle-check, or create-invocation call could complete -- W4 audit fix, \"list\" stage added by Y2 audit fix. This drop is PERMANENT (the delivery's own claim is already taken; no redelivery or reconciler reaches it again), unlike an ordinary transient Postgres error, which platform.Retry already self-heals within the same request. Tagged by the \"stage\" attribute (\"list\" | \"throttle_check\" | \"create\") naming which Postgres call the budget ran out on -- \"list\" fires at most once per delivery and stands for every automation that delivery would have matched, since the list that would have named them never completed."),
		metric.WithUnit("{automation}"),
	)
	if err != nil {
		// An Int64Counter construction call can only ever fail for a
		// malformed static instrument name -- this one is a fixed,
		// well-formed literal, so this is not a runtime condition; logged
		// rather than silently swallowed, mirroring cloudIdentityMintTotalCounter's
		// own identical defensive-logging precedent.
		slog.Error("automation: construct automation_dispatch_dropped_total counter failed", "error", err)
	}
	return c
}

// recordAutomationDispatchDropped increments the drop counter by one --
// called from DispatchGitHubWebhookEvent/DispatchLinearWebhookEvent's own
// list-call dispatchBudgetExhausted(err) branch (stage "list",
// githubdispatch.go/lineardispatch.go -- Y2 audit fix: the shared budget
// expiring HERE drops every automation that delivery would have matched,
// not one, since the list that would have named them never completed),
// from checkDispatchThrottle's own dispatchGateBudgetExhausted branch
// (stage "throttle_check") and from dispatchOneGitHubAutomation/
// dispatchOneLinearAutomation's own create-call dispatchBudgetExhausted(err)
// branch (stage "create"), githubdispatch.go/lineardispatch.go -- these
// are the three call sites that actually increment this counter, each
// firing specifically because the shared budget expired, as opposed to a
// genuine business verdict (no match, throttled, denied) or an ordinary
// Postgres error platform.Retry already retried through.
//
// # A fourth site the shared budget can also abandon a dispatch at, left uncounted on purpose
//
// dispatchOneGitHubAutomation's own machine-origin authorization lookup
// (githubdispatch.go, the `actorauthz.AuthorizeLinkedActorVerdict`
// (internal/app/actorauthz/authorize.go) switch's `default:` branch --
// verdict `LinkedActorError` (internal/app/actorauthz/authorize.go))
// returns without ever calling this function when the shared budget
// expiring mid-lookup turns users.GetByID into a lookup failure: that
// automation is dropped for the identical underlying reason -- the budget
// ran out -- but no stage records it. This is not an oversight:
// `AuthorizeLinkedActorVerdict` collapses every lookup failure (a budget-
// expired context, an ordinary transient Postgres error, anything else)
// into the SAME `LinkedActorError` verdict, discarding the underlying
// error -- so telling "budget expired" apart from "an ordinary transient
// lookup failure" at that call site would need either an ad-hoc
// context-deadline probe (which would mislabel a genuinely transient
// lookup failure that merely races the budget's own expiry as a budget
// drop) or widening `AuthorizeLinkedActorVerdict`'s own signature, shared
// verbatim by the GitHub, Linear, and Slack identity call sites, to carry
// a distinction none of ITS other callers need. Production behaviour at
// this site is already correct regardless of the miscount: it fails
// closed exactly like every other actorauthz call site in this codebase,
// creates no invocation, and writes no creator_unauthorized_since mark
// either way (W9 audit fix, see dispatchOneGitHubAutomation's own doc
// comment) -- only this counter's own coverage stops short of it. Filed
// as its own entry in docs/DECISIONS.md's Deferred table, with an
// evaluable reopen condition, rather than left to be rediscovered as a
// surprise the next time this counter is reconciled against a real
// delivery.
func recordAutomationDispatchDropped(ctx context.Context, stage string) {
	automationDispatchDroppedTotalCounter().Add(ctx, 1, metric.WithAttributes(attribute.String("stage", stage)))
}
