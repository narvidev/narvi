package automation

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// CreateInvocation is this Step's own minimal, durable "an invocation now
// exists, fan it out" entry point -- see this package's own doc.go for why
// this is deliberately as small as internal/app/releasereview.Enqueue: a
// fast, cheap, single INSERT, never the real fan-out work itself (that
// happens later, on Engine's own background pump, entirely decoupled from
// whatever caller decided this automation should fire right now).
//
// §8.4 ("automations: triggers & extras", §8.4) owns the actual trigger-
// condition evaluation that decides WHEN to call this; this Step's own
// callers are its integration tests. targets is validated (automation.
// ValidateTargets, §3.5's own "fan-out ≤10" cap) before anything is
// persisted -- an invalid target list returns an error and writes nothing.
func CreateInvocation(ctx context.Context, invocations InvocationCreator, automationID pgtype.UUID, targets []domainautomation.Target) (sqlcgen.AutomationInvocation, error) {
	if err := domainautomation.ValidateTargets(targets); err != nil {
		return sqlcgen.AutomationInvocation{}, fmt.Errorf("automation: create invocation: %w", err)
	}

	targetsJSON, err := MarshalTargets(targets)
	if err != nil {
		return sqlcgen.AutomationInvocation{}, fmt.Errorf("automation: create invocation: %w", err)
	}

	inv, err := invocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
		AutomationID: automationID,
		Targets:      targetsJSON,
		TotalRuns:    int32(len(targets)),
	})
	if err != nil {
		return sqlcgen.AutomationInvocation{}, fmt.Errorf("automation: create invocation: insert: %w", err)
	}
	return inv, nil
}

// InvocationCreator is the narrow slice of
// *postgres.AutomationInvocationStore CreateInvocation needs -- mirrors
// internal/app/releasereview's own PendingEnqueuer/OutboxEnqueuer
// precedent: a small, locally-defined interface so a unit test can inject
// a fake with no real DB round trip.
type InvocationCreator interface {
	Create(ctx context.Context, arg sqlcgen.CreateAutomationInvocationParams) (sqlcgen.AutomationInvocation, error)
}

// DeliveryInvocationCreator is InvocationCreator's own delivery-idempotent
// counterpart -- D1 audit fix's own live GitHub/Linear webhook dispatch
// callers (githubdispatch.go/lineardispatch.go) need BOTH CreateForDelivery
// (CreateInvocationForDelivery below) and CountRecentInvocations (D8's own
// per-automation dispatch throttle, EvaluateDispatchThrottle) -- bundled
// into one interface rather than two, since every real caller of either
// (*postgres.AutomationInvocationStore) always needs both together, and a
// test fake standing in for "the live webhook dispatch path's own
// invocation store" naturally implements both or neither.
type DeliveryInvocationCreator interface {
	CreateForDelivery(ctx context.Context, arg sqlcgen.CreateAutomationInvocationForDeliveryParams) (sqlcgen.CreateAutomationInvocationForDeliveryRow, error)
	CountRecentInvocations(ctx context.Context, automationID pgtype.UUID, since pgtype.Timestamptz) (int64, error)
}

// CreateInvocationForDelivery is D1's own idempotent entry point for live
// GitHub/Linear webhook dispatch -- mirrors CreateInvocation's own
// target-validation/marshal steps exactly, but creates (or finds) the
// invocation keyed on (automationID, provider, deliveryID) via
// invocations.CreateForDelivery's own ON CONFLICT idiom (migrations/
// 000136_automation_invocations_source_delivery.up.sql), so a redelivery
// of the SAME (provider, deliveryID) for the SAME automation can NEVER
// create a second invocation.
//
// This is the fix for D1's own confirmed finding: the @mention/
// AgentSessionEvent pipelines sharing this SAME delivery release their own
// webhook_deliveries claim specifically so a GitHub/Linear redelivery can
// retry THEIR OWN failed processing -- "this delivery was claimed but
// never actually acted on" (handler.go's own comment, quoted verbatim in
// the audit finding) is a statement about THEIR processing, not this
// package's. Before this fix, automation dispatch had NO idempotency of
// its own and silently relied on that SAME claim never being released --
// so a delivery that failed in a LATER lane (releasing the claim) and was
// then redelivered created a SECOND invocation, and thus a second run and
// a second agent session, even though the automation dispatch itself had
// already fully succeeded on the first delivery. The claim's lifetime is
// owned by those other consumers and is not this package's to change --
// see doc.go's own "Deduplication reuses the ALREADY-ESTABLISHED
// webhookDeliveryStore.Claim/Release mechanism" section for what remains
// true after this fix (the CLAIM still dedupes the delivery from ever
// reaching this package twice while unclaimed; THIS function is what
// makes reaching it MORE than once, after a claim release elsewhere,
// harmless).
//
// created reports whether THIS call is the one that actually created the
// invocation (false means an earlier delivery of the identical
// (automationID, provider, deliveryID) already did -- the caller should
// log and skip, never treat this as an error).
func CreateInvocationForDelivery(ctx context.Context, invocations DeliveryInvocationCreator, automationID pgtype.UUID, targets []domainautomation.Target, provider, deliveryID string) (inv sqlcgen.AutomationInvocation, created bool, err error) {
	if err := domainautomation.ValidateTargets(targets); err != nil {
		return sqlcgen.AutomationInvocation{}, false, fmt.Errorf("automation: create invocation for delivery: %w", err)
	}

	targetsJSON, err := MarshalTargets(targets)
	if err != nil {
		return sqlcgen.AutomationInvocation{}, false, fmt.Errorf("automation: create invocation for delivery: %w", err)
	}

	row, err := invocations.CreateForDelivery(ctx, sqlcgen.CreateAutomationInvocationForDeliveryParams{
		AutomationID:     automationID,
		Targets:          targetsJSON,
		TotalRuns:        int32(len(targets)),
		SourceProvider:   &provider,
		SourceDeliveryID: &deliveryID,
	})
	if err != nil {
		return sqlcgen.AutomationInvocation{}, false, fmt.Errorf("automation: create invocation for delivery: insert: %w", err)
	}

	return sqlcgen.AutomationInvocation{
		ID:               row.ID,
		AutomationID:     row.AutomationID,
		Status:           row.Status,
		Targets:          row.Targets,
		TotalRuns:        row.TotalRuns,
		FannedOutAt:      row.FannedOutAt,
		FailureCountedAt: row.FailureCountedAt,
		ClosedAt:         row.ClosedAt,
		CreatedAt:        row.CreatedAt,
		SourceProvider:   row.SourceProvider,
		SourceDeliveryID: row.SourceDeliveryID,
	}, row.Inserted, nil
}
