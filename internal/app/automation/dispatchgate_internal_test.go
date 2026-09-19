package automation

// White-box (package automation, not automation_test) unit tests for
// checkDispatchThrottle/dispatchBudgetExhausted's own U1 audit fix
// (confirmed HIGH finding: "the total budget is smaller than the retry
// chain it contains... exhaustion is logged as a throttle, which is a
// different thing"). dispatchGateVerdict/dispatchBudgetExhausted are both
// unexported, so this needs direct package access -- the same reason
// closeout_whitebox_integration_test.go needs it for closeInvocation.
// Deliberately NOT integration-tagged: every fake here is in-process, no
// real Postgres round trip, mirroring githubdispatch_test.go's own
// (external, automation_test) fakes.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedCountInvocationCreator is a DeliveryInvocationCreator fake whose
// CountRecentInvocations always returns the same (count, err) pair,
// counting how many times it was called -- CreateForDelivery is never
// exercised by these tests and panics if it is.
type fixedCountInvocationCreator struct {
	count int64
	err   error
	calls int
}

func (f *fixedCountInvocationCreator) CountRecentInvocations(_ context.Context, _ pgtype.UUID, _ pgtype.Timestamptz) (int64, error) {
	f.calls++
	return f.count, f.err
}

func (f *fixedCountInvocationCreator) CreateForDelivery(_ context.Context, _ sqlcgen.CreateAutomationInvocationForDeliveryParams) (sqlcgen.CreateAutomationInvocationForDeliveryRow, error) {
	panic("unexpected call: CreateForDelivery")
}

// smallRetryTimeouts is a fast, deterministic retry configuration for these
// unit tests -- real AutomationDispatchMaxAttempts/RetryBaseDelay/
// RetryMaxDelay would make an exhausted-retry test case sleep for real
// (750ms at production defaults); 2 attempts at 1ms keeps every case here
// well under a millisecond of real sleep.
func smallRetryTimeouts() platform.Timeouts {
	return platform.Timeouts{
		AutomationDispatchMaxAttempts:    2,
		AutomationDispatchRetryBaseDelay: time.Millisecond,
		AutomationDispatchRetryMaxDelay:  time.Millisecond,
		AutomationDispatchThrottleWindow: time.Minute,
	}
}

// TestCheckDispatchThrottle_Allowed proves the ordinary, healthy path: a
// count comfortably below domainautomation.DispatchThrottleThreshold
// returns dispatchGateAllowed, exactly once (no retry needed).
func TestCheckDispatchThrottle_Allowed(t *testing.T) {
	fake := &fixedCountInvocationCreator{count: 0, err: nil}

	verdict := checkDispatchThrottle(context.Background(), testDiscardLogger(), fake, smallRetryTimeouts(), pgtype.UUID{})

	if verdict != dispatchGateAllowed {
		t.Fatalf("checkDispatchThrottle verdict = %v, want dispatchGateAllowed", verdict)
	}
	if fake.calls != 1 {
		t.Fatalf("CountRecentInvocations calls = %d, want 1 (no retry needed on success)", fake.calls)
	}
}

// TestCheckDispatchThrottle_Throttled proves EvaluateDispatchThrottle's own
// genuine "too many invocations recently" verdict still returns
// dispatchGateThrottled -- U1's own fix must not have collapsed this,
// pre-existing, case into the new dispatchGateBudgetExhausted one.
func TestCheckDispatchThrottle_Throttled(t *testing.T) {
	fake := &fixedCountInvocationCreator{count: 999, err: nil} // comfortably >= DispatchThrottleThreshold

	verdict := checkDispatchThrottle(context.Background(), testDiscardLogger(), fake, smallRetryTimeouts(), pgtype.UUID{})

	if verdict != dispatchGateThrottled {
		t.Fatalf("checkDispatchThrottle verdict = %v, want dispatchGateThrottled", verdict)
	}
}

// TestCheckDispatchThrottle_BudgetExhaustedIsDistinctFromThrottled is U1's
// own required proof: a Postgres call that fails because the SHARED
// dispatch time budget (context.DeadlineExceeded -- either
// dispatchTotalBudgetContext's own deadline, or a deadline the caller's
// incoming ctx already carried) ran out must report dispatchGateBudgetExhausted,
// NEVER dispatchGateThrottled (a business decision this failure has nothing
// to do with) and NEVER the generic dispatchGateError (an operator reading
// logs/metrics for THIS specific, actionable cause -- "the budget is too
// small for this delivery's own fan-out" -- must be able to tell it apart
// from an ordinary repeated Postgres error).
func TestCheckDispatchThrottle_BudgetExhaustedIsDistinctFromThrottled(t *testing.T) {
	fake := &fixedCountInvocationCreator{count: 0, err: context.DeadlineExceeded}

	verdict := checkDispatchThrottle(context.Background(), testDiscardLogger(), fake, smallRetryTimeouts(), pgtype.UUID{})

	if verdict != dispatchGateBudgetExhausted {
		t.Fatalf("checkDispatchThrottle verdict = %v, want dispatchGateBudgetExhausted", verdict)
	}
	if verdict == dispatchGateThrottled {
		t.Fatalf("checkDispatchThrottle verdict must never be dispatchGateThrottled for a budget-exhaustion failure -- that is a different condition entirely")
	}
}

// TestCheckDispatchThrottle_GenuineErrorIsDistinctFromBudgetExhausted is
// the companion proof: an ordinary, REPEATED Postgres error unrelated to
// time budget (never wrapping context.DeadlineExceeded/context.Canceled)
// must report the generic dispatchGateError, never
// dispatchGateBudgetExhausted -- a real outage should not be misread as
// "the budget was merely too small".
func TestCheckDispatchThrottle_GenuineErrorIsDistinctFromBudgetExhausted(t *testing.T) {
	fake := &fixedCountInvocationCreator{count: 0, err: errors.New("connection reset by peer")}

	verdict := checkDispatchThrottle(context.Background(), testDiscardLogger(), fake, smallRetryTimeouts(), pgtype.UUID{})

	if verdict != dispatchGateError {
		t.Fatalf("checkDispatchThrottle verdict = %v, want dispatchGateError", verdict)
	}
}

// TestDispatchBudgetExhausted is a direct table-driven pin of the
// classifier itself.
func TestDispatchBudgetExhausted(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"DeadlineExceeded", context.DeadlineExceeded, true},
		{"Canceled", context.Canceled, true},
		{"wrapped DeadlineExceeded", errors.New("count recent invocations: " + context.DeadlineExceeded.Error()), false}, // string-wrapping is NOT errors.Is-wrapping
		{"genuine error", errors.New("connection reset"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatchBudgetExhausted(tc.err); got != tc.want {
				t.Errorf("dispatchBudgetExhausted(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
