package outbox_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/outbox"
)

func TestEvaluateFailure(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	policy := outbox.Policy{
		Backoff: outbox.BackoffConfig{
			BaseDelay: 30 * time.Second,
			MaxDelay:  5 * time.Minute,
		},
		MaxConsecutiveInterruptions: 2,
		InterruptedSettleDelay:      45 * time.Second,
	}

	// interrupted is a repeatable delivery the shutdown cut short, within
	// its own delivery timeout -- the one shape that keeps its attempt.
	interrupted := func(attempts, run int) outbox.Failure {
		return outbox.Failure{AttemptCount: attempts, ConsecutiveInterruptions: run, ShutdownBegun: true, Repeatable: true}
	}

	tests := []struct {
		name           string
		failure        outbox.Failure
		wantClass      outbox.FailureClass
		wantRule       outbox.Rule
		wantDelay      time.Duration
		wantDeadLetter bool
		wantRun        int
	}{
		{
			name:      "a failure with no shutdown counts and resets the run",
			failure:   outbox.Failure{AttemptCount: 1, ConsecutiveInterruptions: 2, Repeatable: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleFailed, wantDelay: 30 * time.Second, wantRun: 0,
		},
		{
			name:      "a cancelled or timed-out delivery with no shutdown still counts",
			failure:   outbox.Failure{AttemptCount: 3, OutlivedDeliveryTimeout: true, Repeatable: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleFailed, wantDelay: 2 * time.Minute, wantRun: 0,
		},
		{
			name:      "a failure with no shutdown at MaxAttempts dead-letters",
			failure:   outbox.Failure{AttemptCount: outbox.MaxAttempts, Repeatable: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleFailed, wantDeadLetter: true, wantRun: 0,
		},
		{
			name:      "an interrupted delivery keeps its attempt and is due after the settle delay",
			failure:   interrupted(1, 0),
			wantClass: outbox.ClassDeferred, wantRule: outbox.RuleShutdownInterrupted, wantDelay: 45 * time.Second, wantRun: 1,
		},
		{
			name:      "an interruption at the bound still keeps its attempt",
			failure:   interrupted(1, 1),
			wantClass: outbox.ClassDeferred, wantRule: outbox.RuleShutdownInterrupted, wantDelay: 45 * time.Second, wantRun: 2,
		},
		{
			name:      "an interruption past the bound counts, due no sooner than the settle delay",
			failure:   interrupted(1, 2),
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownPastBound, wantDelay: 45 * time.Second, wantRun: 3,
		},
		{
			name:      "an interruption past the bound backing off longer than the settle delay keeps its backoff",
			failure:   interrupted(4, 2),
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownPastBound, wantDelay: 4 * time.Minute, wantRun: 3,
		},
		{
			name:      "an interruption past the bound at MaxAttempts dead-letters",
			failure:   interrupted(outbox.MaxAttempts, 5),
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownPastBound, wantDeadLetter: true, wantRun: 6,
		},
		{
			name:      "an interrupted delivery on its tenth attempt is not dead-lettered: the attempt is given back",
			failure:   interrupted(outbox.MaxAttempts, 0),
			wantClass: outbox.ClassDeferred, wantRule: outbox.RuleShutdownInterrupted, wantDelay: 45 * time.Second, wantRun: 1,
		},
		{
			name:      "an interruption of a kind not safe to repeat counts, due no sooner than the settle delay",
			failure:   outbox.Failure{AttemptCount: 1, ShutdownBegun: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownNotRepeatable, wantDelay: 45 * time.Second, wantRun: 1,
		},
		{
			name:      "an interruption of a kind not safe to repeat backing off longer than the settle delay keeps its backoff",
			failure:   outbox.Failure{AttemptCount: 3, ShutdownBegun: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownNotRepeatable, wantDelay: 2 * time.Minute, wantRun: 1,
		},
		{
			name:      "an interruption of a kind not safe to repeat at MaxAttempts dead-letters",
			failure:   outbox.Failure{AttemptCount: outbox.MaxAttempts, ShutdownBegun: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownNotRepeatable, wantDeadLetter: true, wantRun: 1,
		},
		{
			name:      "a delivery that outlived its own timeout counts, even when shutdown stopped it",
			failure:   outbox.Failure{AttemptCount: 2, ShutdownBegun: true, OutlivedDeliveryTimeout: true, Repeatable: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownOutlivedDeliveryTimeout, wantDelay: time.Minute, wantRun: 1,
		},
		{
			name:      "not repeatable takes precedence over the delivery timeout and the bound",
			failure:   outbox.Failure{AttemptCount: 1, ConsecutiveInterruptions: 9, ShutdownBegun: true, OutlivedDeliveryTimeout: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownNotRepeatable, wantDelay: 45 * time.Second, wantRun: 10,
		},
		{
			name:      "the delivery timeout takes precedence over the bound",
			failure:   outbox.Failure{AttemptCount: 1, ConsecutiveInterruptions: 9, ShutdownBegun: true, OutlivedDeliveryTimeout: true, Repeatable: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleShutdownOutlivedDeliveryTimeout, wantDelay: 45 * time.Second, wantRun: 10,
		},
		{
			name:      "a row the shutdown reached before its delivery started keeps its attempt and its run",
			failure:   outbox.Failure{AttemptCount: 1, ConsecutiveInterruptions: 2, ShutdownBegun: true, NotStarted: true},
			wantClass: outbox.ClassDeferred, wantRule: outbox.RuleShutdownBeforeStart, wantDelay: 0, wantRun: 2,
		},
		{
			name:      "a row never started is deferred past the bound too: nothing was sent",
			failure:   outbox.Failure{AttemptCount: outbox.MaxAttempts, ConsecutiveInterruptions: 7, ShutdownBegun: true, NotStarted: true},
			wantClass: outbox.ClassDeferred, wantRule: outbox.RuleShutdownBeforeStart, wantDelay: 0, wantRun: 7,
		},
		{
			name:      "NotStarted means nothing without a shutdown",
			failure:   outbox.Failure{AttemptCount: 1, NotStarted: true},
			wantClass: outbox.ClassCounted, wantRule: outbox.RuleFailed, wantDelay: 30 * time.Second, wantRun: 0,
		},
		{
			name:      "a negative run reads as zero",
			failure:   interrupted(1, -4),
			wantClass: outbox.ClassDeferred, wantRule: outbox.RuleShutdownInterrupted, wantDelay: 45 * time.Second, wantRun: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := outbox.EvaluateFailure(tc.failure, policy, now)

			if got.Class != tc.wantClass || got.Rule != tc.wantRule {
				t.Fatalf("EvaluateFailure(%+v) = class %d rule %q, want class %d rule %q", tc.failure, got.Class, got.Rule, tc.wantClass, tc.wantRule)
			}
			if got.ConsecutiveInterruptions != tc.wantRun {
				t.Fatalf("ConsecutiveInterruptions = %d, want %d", got.ConsecutiveInterruptions, tc.wantRun)
			}
			if got.DeadLetter != tc.wantDeadLetter {
				t.Fatalf("DeadLetter = %v, want %v", got.DeadLetter, tc.wantDeadLetter)
			}
			if tc.wantDeadLetter {
				return
			}
			if want := now.Add(tc.wantDelay); !got.NextRetryAt.Equal(want) {
				t.Fatalf("NextRetryAt = %v, want %v", got.NextRetryAt, want)
			}
		})
	}
}

// TestEvaluateFailure_CountedMatchesEvaluateBackoff pins that the counted
// class is EvaluateBackoff's schedule and dead-letter rule, not a copy of
// them: for every attempt count, a counted failure decides exactly what
// EvaluateBackoff decides.
func TestEvaluateFailure_CountedMatchesEvaluateBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := outbox.BackoffConfig{BaseDelay: 30 * time.Second, MaxDelay: 5 * time.Minute}

	for attempts := 0; attempts <= outbox.MaxAttempts+1; attempts++ {
		got := outbox.EvaluateFailure(outbox.Failure{AttemptCount: attempts}, outbox.Policy{Backoff: cfg, MaxConsecutiveInterruptions: 3}, now)
		want := outbox.EvaluateBackoff(attempts, cfg, now)
		if got.BackoffDecision != want {
			t.Fatalf("attempts %d: EvaluateFailure = %+v, EvaluateBackoff = %+v", attempts, got.BackoffDecision, want)
		}
	}
}
