//go:build integration

// Covers invocationenqueue.go's own CreateInvocationForDelivery against a
// real Postgres instance -- specifically D1's own "INSERT ... ON CONFLICT
// ... DO UPDATE ... RETURNING (xmax = 0) AS inserted" idempotency idiom
// (migrations/000136_automation_invocations_source_delivery.up.sql),
// which the redelivery-dedup fix this whole live-dispatch feature was
// built around rests on.
//
// D9 (round 2) audit finding: no existing test anywhere in this
// repository actually exercises the CONFLICT path this idiom exists for.
// The two "DedupesRedeliveredDelivery" integration tests (github/linear
// adapter packages) never reach it at all -- the webhook_deliveries claim
// dedupes their own second POST first, so dispatch is never even
// attempted a second time. The one test that DOES reach dispatch twice
// for the identical delivery (github's own
// TestGitHubIntegration_AutomationDispatchSurvivesClaimReleasedByALaterLaneFailure)
// only ever asserts the FINAL row count (1), which a raw, unhandled
// Postgres unique-violation error on the second insert satisfies just as
// well as a graceful ON CONFLICT dedupe does: either way, the second
// INSERT never adds a second row, so both the correct mechanism and its
// absence are observably identical from "how many invocations exist now".
//
// This test instead calls CreateInvocationForDelivery directly, twice,
// with the IDENTICAL (automationID, provider, deliveryID), and asserts on
// ITS OWN return values -- the one place "gracefully found the existing
// row" and "the raw INSERT itself failed" are actually distinguishable.
package automation_test

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/internal/app/automation"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// TestCreateInvocationForDelivery_IdempotentOnRedelivery is D1's own
// required, missing proof: calling CreateInvocationForDelivery twice with
// the SAME (automationID, provider, deliveryID) must succeed BOTH times,
// with the SECOND call reporting created=false and returning the SAME
// invocation row the first call created -- never a second row, and never
// an error. Mutation-verified: removing the "ON CONFLICT ... DO UPDATE"
// clause from CreateAutomationInvocationForDelivery (queries/
// automationinvocations.sql) makes this test's own second call fail with
// a raw Postgres unique-violation error instead.
func TestCreateInvocationForDelivery_IdempotentOnRedelivery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	auto := f.createGitHubAutomation(t, "idempotent-on-redelivery", domainautomation.GitHubTriggerConfig{Event: "issue_comment"}, target)

	const deliveryID = "delivery-conflict-path-1"
	const provider = "github"

	first, createdFirst, err := automation.CreateInvocationForDelivery(ctx, f.invocations, auto.ID, []domainautomation.Target{target}, provider, deliveryID)
	if err != nil {
		t.Fatalf("first call: CreateInvocationForDelivery returned an error, want nil: %v", err)
	}
	if !createdFirst {
		t.Fatalf("first call: created = false, want true (this is a brand-new delivery)")
	}

	second, createdSecond, err := automation.CreateInvocationForDelivery(ctx, f.invocations, auto.ID, []domainautomation.Target{target}, provider, deliveryID)
	if err != nil {
		t.Fatalf("second call with the IDENTICAL (automation_id, provider, delivery_id): got error %v, want nil -- a redelivery must gracefully find the existing row via ON CONFLICT, never surface a raw duplicate-key error", err)
	}
	if createdSecond {
		t.Fatalf("second call: created = true, want false (this is a redelivery of the SAME (automation_id, provider, delivery_id), not a new invocation)")
	}
	if second.ID != first.ID {
		t.Fatalf("second call returned invocation id %s, want the SAME id the first call created (%s) -- a redelivery must never create a second row", second.ID.String(), first.ID.String())
	}

	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM automation_invocations WHERE automation_id = $1 AND source_provider = $2 AND source_delivery_id = $3`, auto.ID, provider, deliveryID).Scan(&count); err != nil {
		t.Fatalf("count rows for this (automation_id, provider, delivery_id): %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for (automation_id, provider, delivery_id) = %d, want exactly 1 (never two, no matter how many times the identical delivery is redelivered)", count)
	}
}
