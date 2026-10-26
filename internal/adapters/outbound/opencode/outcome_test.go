package opencode

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// deriveOutcome and the no-output-is-failure / error-name-to-outcome
// mapping are hard to elicit reliably from a REAL model (§7's own quirks).
// Per this Step's own testing instructions, test the DERIVATION FUNCTION
// directly against constructed values instead -- more reliable, and it
// isolates the logic from live-model non-determinism.
func TestDeriveOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         *openCodeTaggedError
		hasText     bool
		hasToolCall bool
		wantOutcome sandboxws.ExecutionCompleteOutcome
		wantReason  bool // whether a non-nil Reason is expected
	}{
		{
			name:        "aborted turn maps to cancelled with no reason",
			err:         &openCodeTaggedError{Name: "MessageAbortedError"},
			wantOutcome: sandboxws.ExecutionCompleteOutcomeCancelled,
			wantReason:  false,
		},
		{
			name:        "any other tagged error maps to failed with a reason",
			err:         &openCodeTaggedError{Name: "ProviderAuthError"},
			wantOutcome: sandboxws.ExecutionCompleteOutcomeFailed,
			wantReason:  true,
		},
		{
			name:        "context overflow error maps to failed with a reason",
			err:         &openCodeTaggedError{Name: "ContextOverflowError"},
			wantOutcome: sandboxws.ExecutionCompleteOutcomeFailed,
			wantReason:  true,
		},
		{
			name:        "no error, has text -> completed",
			hasText:     true,
			wantOutcome: sandboxws.ExecutionCompleteOutcomeCompleted,
			wantReason:  false,
		},
		{
			name:        "no error, has tool call but no text -> completed",
			hasToolCall: true,
			wantOutcome: sandboxws.ExecutionCompleteOutcomeCompleted,
			wantReason:  false,
		},
		{
			name:        "no error, has both text and tool call -> completed",
			hasText:     true,
			hasToolCall: true,
			wantOutcome: sandboxws.ExecutionCompleteOutcomeCompleted,
			wantReason:  false,
		},
		{
			name:        "no error, no text, no tool call -> failed (no output is failure)",
			wantOutcome: sandboxws.ExecutionCompleteOutcomeFailed,
			wantReason:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := deriveOutcome(tt.err, tt.hasText, tt.hasToolCall)
			if got.Outcome != tt.wantOutcome {
				t.Errorf("deriveOutcome() outcome = %q, want %q", got.Outcome, tt.wantOutcome)
			}
			if hasReason := got.Reason != nil; hasReason != tt.wantReason {
				t.Errorf("deriveOutcome() reason present = %v, want %v (reason=%v)", hasReason, tt.wantReason, got.Reason)
			}
			if got.Reason != nil {
				if *got.Reason == "" {
					t.Error("deriveOutcome() returned a non-nil but empty reason")
				}
				// Never leak the tagged error's own raw data verbatim --
				// only the short, fixed Name-derived string.
			}
		})
	}
}

// TestIsContextOverflowError mirrors TestDeriveOutcome's own table-driven
// style: isContextOverflowError is §7.2's own "classify on the typed
// discriminator already decoded" logic, hard-and-non-deterministic to
// elicit reliably from a real model (a real ContextOverflowError requires
// an actually-overflowed real context window), so tested directly against
// constructed values instead.
func TestIsContextOverflowError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *openCodeTaggedError
		want bool
	}{
		{name: "nil error is never a context overflow", err: nil, want: false},
		{name: "ContextOverflowError is a context overflow", err: &openCodeTaggedError{Name: "ContextOverflowError"}, want: true},
		{name: "MessageAbortedError is not a context overflow", err: &openCodeTaggedError{Name: "MessageAbortedError"}, want: false},
		{name: "ProviderAuthError is not a context overflow", err: &openCodeTaggedError{Name: "ProviderAuthError"}, want: false},
		{name: "empty name is not a context overflow", err: &openCodeTaggedError{Name: ""}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isContextOverflowError(tt.err); got != tt.want {
				t.Errorf("isContextOverflowError(%+v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsTransientAPIError mirrors TestIsContextOverflowError's own
// table-driven style: isTransientAPIError is this Step's own ("typed
// transient-error retry for the OpenCode adapter") "classify ONLY on the
// typed isRetryable field OpenCode itself already computed" logic, tested
// directly against constructed values (a real transient 429/529-shaped
// APIError is hard to elicit reliably on demand from a live provider).
//
// The table below is this Step's own required classification proof: a
// transient (isRetryable=true) APIError classifies true; a permanent
// (isRetryable=false) APIError, and every OTHER tagged-union member name
// (including one a "local-connection failure" could plausibly be tagged
// with, e.g. UnknownError), classify false — never on a substring of any
// error text, only err.Name and err.Data.IsRetryable.
func TestIsTransientAPIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *openCodeTaggedError
		want bool
	}{
		{name: "nil error is never transient", err: nil, want: false},
		{
			name: "APIError with isRetryable=true is transient",
			err:  &openCodeTaggedError{Name: "APIError", Data: &openCodeErrorData{IsRetryable: true}},
			want: true,
		},
		{
			name: "APIError with isRetryable=true and a statusCode is transient (statusCode is corroborating only)",
			err: &openCodeTaggedError{Name: "APIError", Data: &openCodeErrorData{
				IsRetryable: true, StatusCode: intPtr(529),
			}},
			want: true,
		},
		{
			name: "APIError with isRetryable=false is permanent, not transient",
			err:  &openCodeTaggedError{Name: "APIError", Data: &openCodeErrorData{IsRetryable: false}},
			want: false,
		},
		{
			name: "APIError with nil Data is never transient (fail closed)",
			err:  &openCodeTaggedError{Name: "APIError", Data: nil},
			want: false,
		},
		{
			name: "MessageAbortedError carrying a Data object with no real isRetryable opinion is never transient " +
				"(Name gate prevents trusting a zero-value IsRetryable from a member that never expressed one)",
			err:  &openCodeTaggedError{Name: "MessageAbortedError", Data: &openCodeErrorData{IsRetryable: false}},
			want: false,
		},
		{
			name: "ContextOverflowError is not a transient APIError (it has its own, separate recovery path)",
			err:  &openCodeTaggedError{Name: "ContextOverflowError"},
			want: false,
		},
		{
			name: "UnknownError (the shape a local-connection/sandbox-health failure would plausibly surface as, " +
				"were it ever decoded into this typed union at all -- see isTransientAPIError's own doc comment " +
				"for why it structurally never is) is not transient",
			err:  &openCodeTaggedError{Name: "UnknownError", Data: &openCodeErrorData{IsRetryable: true}},
			want: false,
		},
		{name: "empty name is not transient", err: &openCodeTaggedError{Name: ""}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTransientAPIError(tt.err); got != tt.want {
				t.Errorf("isTransientAPIError(%+v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func intPtr(i int) *int { return &i }

// TestIsTransientAPIError_DeadlineMessageWithElapsedSecondsIsNotTransient
// pins the trap technical plan §35.4 names, at the agent runtime's one
// turn-level retry classifier: a deadline message that embeds its own
// elapsed seconds, with a gateway or request timeout status, is not a
// transient provider error -- retrying it would re-submit the turn into a
// sandbox that is stopping. Only the runtime's tagged isRetryable decides:
// never the message's digits or words, and never a 5xx or 408 status.
func TestIsTransientAPIError_DeadlineMessageWithElapsedSecondsIsNotTransient(t *testing.T) {
	t.Parallel()
	messages := []string{
		"request timed out after 3600 seconds",
		"deadline exceeded: 7199.8s elapsed",
		"Gateway Timeout (504) after 3599 s",
		"the sandbox's lifetime of 7200 seconds was reached; turn context canceled before completion",
		"timed out",
		"504",
	}
	statuses := []*int{nil, intPtr(504), intPtr(408), intPtr(500), intPtr(502), intPtr(503), intPtr(529), intPtr(429)}
	for _, message := range messages {
		for _, status := range statuses {
			for _, name := range []string{"APIError", "UnknownError", "MessageAbortedError", "ProviderAuthError"} {
				for _, retryable := range []bool{false, true} {
					err := &openCodeTaggedError{Name: name, Data: &openCodeErrorData{Message: message, StatusCode: status, IsRetryable: retryable}}
					want := name == "APIError" && retryable
					if got := isTransientAPIError(err); got != want {
						t.Errorf("isTransientAPIError(%s, message %q, status %v, isRetryable %v) = %v, want %v: only an APIError's isRetryable decides",
							name, message, derefInt(status), retryable, got, want)
					}
				}
			}
		}
	}
}

func derefInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// TestEnrichReasonForFailedRecovery proves the reason-enrichment logic
// used when a compaction/retry recovery attempt itself failed
// (Adapter.attemptCompactionRetry, adapter.go): the result must reference
// BOTH the original overflow reason AND the new failure detail, so an
// operator can distinguish this from a first-time, never-retried overflow.
func TestEnrichReasonForFailedRecovery(t *testing.T) {
	t.Parallel()

	original := "opencode: ContextOverflowError"
	tests := []struct {
		name           string
		originalReason *string
		detail         string
		wantContains   []string
	}{
		{
			name:           "nil original reason falls back to a fixed default",
			originalReason: nil,
			detail:         "forceCompaction: boom",
			wantContains:   []string{"ContextOverflowError", "forceCompaction: boom"},
		},
		{
			name:           "non-nil original reason is preserved verbatim",
			originalReason: &original,
			detail:         "retry postPromptAsync: boom",
			wantContains:   []string{original, "retry postPromptAsync: boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := enrichReasonForFailedRecovery(tt.originalReason, tt.detail)
			if got == "" {
				t.Fatal("enrichReasonForFailedRecovery() returned an empty string")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("enrichReasonForFailedRecovery() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// TestEnrichReasonForRepeatedOverflow proves the reason-enrichment logic
// used when a RETRIED prompt also overflowed
// (Adapter.finalizeOrRecoverFromOverflow, adapter.go, compactionAttempted
// already true): the result must mention that a retry was already
// attempted, so the final reason string visibly differs from a first-time
// overflow (§7.2 point 3's own "never a silent double failure"
// requirement).
func TestEnrichReasonForRepeatedOverflow(t *testing.T) {
	t.Parallel()

	retryReason := "opencode: ContextOverflowError"
	tests := []struct {
		name         string
		retryReason  *string
		wantContains []string
	}{
		{
			name:         "nil retry reason falls back to a fixed default",
			retryReason:  nil,
			wantContains: []string{"ContextOverflowError", "already attempted"},
		},
		{
			name:         "non-nil retry reason is preserved verbatim",
			retryReason:  &retryReason,
			wantContains: []string{retryReason, "already attempted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := enrichReasonForRepeatedOverflow(tt.retryReason)
			if got == "" {
				t.Fatal("enrichReasonForRepeatedOverflow() returned an empty string")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("enrichReasonForRepeatedOverflow() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// TestEnrichReasonForFailedTransientRetry mirrors
// TestEnrichReasonForFailedRecovery's own style for this Step's own sibling
// function: enrichReasonForFailedTransientRetry must reference BOTH the
// original transient-error reason AND the new failure detail, and must
// NEVER reuse enrichReasonForFailedRecovery's own "compaction retry" wording
// (no compaction is ever attempted for this failure class).
func TestEnrichReasonForFailedTransientRetry(t *testing.T) {
	t.Parallel()

	original := "opencode: APIError"
	tests := []struct {
		name           string
		originalReason *string
		detail         string
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:           "nil original reason falls back to a fixed default",
			originalReason: nil,
			detail:         "backoff wait: context canceled",
			wantContains:   []string{"APIError", "backoff wait: context canceled"},
			wantNotContain: []string{"compaction"},
		},
		{
			name:           "non-nil original reason is preserved verbatim",
			originalReason: &original,
			detail:         "retry postPromptAsync: boom",
			wantContains:   []string{original, "retry postPromptAsync: boom"},
			wantNotContain: []string{"compaction"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := enrichReasonForFailedTransientRetry(tt.originalReason, tt.detail)
			if got == "" {
				t.Fatal("enrichReasonForFailedTransientRetry() returned an empty string")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("enrichReasonForFailedTransientRetry() = %q, want it to contain %q", got, want)
				}
			}
			for _, notWant := range tt.wantNotContain {
				if strings.Contains(got, notWant) {
					t.Errorf("enrichReasonForFailedTransientRetry() = %q, want it NOT to contain %q "+
						"(no compaction is ever attempted for a transient-error retry)", got, notWant)
				}
			}
		})
	}
}

// TestEnrichReasonForRepeatedTransientFailure mirrors
// TestEnrichReasonForRepeatedOverflow's own style for this Step's own
// sibling function: the result must mention that a retry was already
// attempted, and must never claim the retried prompt "also overflowed"
// (enrichReasonForRepeatedOverflow's own wording), which would simply be
// false for this failure class.
func TestEnrichReasonForRepeatedTransientFailure(t *testing.T) {
	t.Parallel()

	retryReason := "opencode: APIError"
	tests := []struct {
		name         string
		retryReason  *string
		wantContains []string
	}{
		{
			name:         "nil retry reason falls back to a fixed default",
			retryReason:  nil,
			wantContains: []string{"APIError", "already attempted"},
		},
		{
			name:         "non-nil retry reason is preserved verbatim",
			retryReason:  &retryReason,
			wantContains: []string{retryReason, "already attempted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := enrichReasonForRepeatedTransientFailure(tt.retryReason)
			if got == "" {
				t.Fatal("enrichReasonForRepeatedTransientFailure() returned an empty string")
			}
			if strings.Contains(got, "overflowed") {
				t.Errorf("enrichReasonForRepeatedTransientFailure() = %q, want it NOT to claim the prompt "+
					"\"also overflowed\" -- that is enrichReasonForRepeatedOverflow's own, different, wording", got)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("enrichReasonForRepeatedTransientFailure() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestSubTaskOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   sandboxws.ExecutionCompleteOutcome
		want sandboxws.SubTaskFinishOutcome
	}{
		{sandboxws.ExecutionCompleteOutcomeCompleted, sandboxws.SubTaskFinishOutcomeCompleted},
		{sandboxws.ExecutionCompleteOutcomeFailed, sandboxws.SubTaskFinishOutcomeFailed},
		{sandboxws.ExecutionCompleteOutcomeCancelled, sandboxws.SubTaskFinishOutcomeCancelled},
	}

	for _, tt := range tests {
		if got := subTaskOutcome(tt.in); got != tt.want {
			t.Errorf("subTaskOutcome(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
