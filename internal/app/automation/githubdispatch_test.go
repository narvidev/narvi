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

	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, timeouts, "pull_request", "delivery-retry-transient-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

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

	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, timeouts, "pull_request", "delivery-retry-exhausted-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

	if lister.calls != timeouts.AutomationDispatchMaxAttempts {
		t.Fatalf("ListActiveGitHubAutomations call count = %d, want exactly %d (bounded, not unbounded retry)", lister.calls, timeouts.AutomationDispatchMaxAttempts)
	}
}
