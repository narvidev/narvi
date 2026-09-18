package automation_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countingFailNTimesLister is a GitHubTriggerLister fake that fails its
// first failTimes calls, then always succeeds with an empty result -- D6
// audit fix's own required proof that a genuinely TRANSIENT failure now
// self-heals within the SAME request, via platform.Retry, without ever
// touching the shared webhook_deliveries claim.
type countingFailNTimesLister struct {
	failTimes int
	calls     int
}

func (f *countingFailNTimesLister) ListActiveGitHubAutomations(_ context.Context) ([]sqlcgen.Automation, error) {
	f.calls++
	if f.calls <= f.failTimes {
		return nil, errors.New("transient: connection reset")
	}
	return nil, nil
}

// noopDeliveryInvocationCreator satisfies automation.DeliveryInvocationCreator
// with no real Postgres round trip -- neither method is ever called in
// these tests (ListActiveGitHubAutomations always returns zero rows), but
// the interface still requires both.
type noopDeliveryInvocationCreator struct{}

func (noopDeliveryInvocationCreator) CreateForDelivery(_ context.Context, _ sqlcgen.CreateAutomationInvocationForDeliveryParams) (sqlcgen.CreateAutomationInvocationForDeliveryRow, error) {
	return sqlcgen.CreateAutomationInvocationForDeliveryRow{}, errors.New("unexpected call")
}

func (noopDeliveryInvocationCreator) CountRecentInvocations(_ context.Context, _ pgtype.UUID, _ pgtype.Timestamptz) (int64, error) {
	return 0, errors.New("unexpected call")
}

func TestDispatchGitHubWebhookEvent_RetriesTransientListFailure(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	// A genuinely transient failure on the first two attempts must not be
	// the final word -- the third (successful) attempt, still within
	// timeouts.AutomationDispatchMaxAttempts, must be what this call
	// observes.
	lister := &countingFailNTimesLister{failTimes: timeouts.AutomationDispatchMaxAttempts - 1}

	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, nil, timeouts, "pull_request", "delivery-retry-transient-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

	if lister.calls != timeouts.AutomationDispatchMaxAttempts {
		t.Fatalf("ListActiveGitHubAutomations call count = %d, want exactly %d (retried up to the budget, succeeding on the last attempt)", lister.calls, timeouts.AutomationDispatchMaxAttempts)
	}
}

func TestDispatchGitHubWebhookEvent_StopsRetryingAtMaxAttemptsOnPermanentTransientFailure(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	// Every attempt fails -- the call count must stop at EXACTLY
	// AutomationDispatchMaxAttempts, never more (a bug that retried
	// forever) and never fewer (a bug that never retried at all).
	lister := &countingFailNTimesLister{failTimes: timeouts.AutomationDispatchMaxAttempts + 10}

	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, nil, timeouts, "pull_request", "delivery-retry-exhausted-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

	if lister.calls != timeouts.AutomationDispatchMaxAttempts {
		t.Fatalf("ListActiveGitHubAutomations call count = %d, want exactly %d (bounded, not unbounded retry)", lister.calls, timeouts.AutomationDispatchMaxAttempts)
	}
}

// ctxCapturingLister is a GitHubTriggerLister fake that records the ctx it
// was actually called with -- D18's own required proof of
// dispatchTotalBudgetContext's own degenerate-zero-value handling (this
// package's own githubdispatch.go), since that helper is unexported and
// this package cannot reach it directly from _test.
type ctxCapturingLister struct {
	gotCtx context.Context
}

func (f *ctxCapturingLister) ListActiveGitHubAutomations(ctx context.Context) ([]sqlcgen.Automation, error) {
	f.gotCtx = ctx
	return nil, nil
}

// TestDispatchGitHubWebhookEvent_ZeroTotalBudgetDoesNotExpireContextImmediately
// is D18's own required, missing proof for the DEFAULT (unconfigured)
// configuration: platform.Timeouts{} (the Go zero value -- every existing
// test rig in the github/linear adapter packages that never explicitly
// sets Config.Timeouts, this package's own githubdispatch_test.go included)
// must NOT make dispatchTotalBudgetContext hand context.WithTimeout(ctx, 0)
// to the caller -- that creates an ALREADY-EXPIRED context, silently
// failing every dispatch closed the instant it is used. Caught for real
// against a live Postgres by this batch's own
// TestGitHubIntegration_AutomationDispatchFiresOnCheckRunWithAuthorizedCreator
// (github/automationdispatch_integration_test.go) before this exact
// zero-value handling was added -- pinned here at the unit level too, so
// a future regression fails fast, with no container needed.
func TestDispatchGitHubWebhookEvent_ZeroTotalBudgetDoesNotExpireContextImmediately(t *testing.T) {
	lister := &ctxCapturingLister{}

	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, nil, platform.Timeouts{}, "pull_request", "delivery-zero-budget-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

	if lister.gotCtx == nil {
		t.Fatalf("ListActiveGitHubAutomations was never called")
	}
	if _, ok := lister.gotCtx.Deadline(); ok {
		t.Fatalf("ctx passed to ListActiveGitHubAutomations carries a deadline, want none -- an unconfigured (zero-value) AutomationDispatchTotalBudget must fall back to \"no additional cap\", never context.WithTimeout(ctx, 0)")
	}
}

// TestDispatchGitHubWebhookEvent_ConfiguredTotalBudgetSetsADeadline is the
// positive half of the proof immediately above: a REAL, configured budget
// (platform.DefaultTimeouts()) must still actually bound ctx -- proving
// the zero-value fallback above did not also silently disable the cap for
// the configured case.
func TestDispatchGitHubWebhookEvent_ConfiguredTotalBudgetSetsADeadline(t *testing.T) {
	lister := &ctxCapturingLister{}

	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, nil, platform.DefaultTimeouts(), "pull_request", "delivery-configured-budget-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

	if lister.gotCtx == nil {
		t.Fatalf("ListActiveGitHubAutomations was never called")
	}
	if _, ok := lister.gotCtx.Deadline(); !ok {
		t.Fatalf("ctx passed to ListActiveGitHubAutomations carries no deadline, want one bounded by AutomationDispatchTotalBudget")
	}
}
