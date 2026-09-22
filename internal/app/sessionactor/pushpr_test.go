package sessionactor

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestExecutionOutcomeTrigger is table-driven over every
// sandboxws.ExecutionCompleteOutcome value its own generated
// UnmarshalJSON accepts, plus one unrecognized value, proving the exact
// (outcome -> trigger) mapping completeProcessingTurn relies on.
func TestExecutionOutcomeTrigger(t *testing.T) {
	tests := []struct {
		name    string
		outcome sandboxws.ExecutionCompleteOutcome
		want    turn.Trigger
		wantOK  bool
	}{
		{"completed", sandboxws.ExecutionCompleteOutcomeCompleted, turn.TriggerComplete, true},
		{"failed", sandboxws.ExecutionCompleteOutcomeFailed, turn.TriggerFail, true},
		{"cancelled", sandboxws.ExecutionCompleteOutcomeCancelled, turn.TriggerCancel, true},
		{"unrecognized", sandboxws.ExecutionCompleteOutcome("bogus"), 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := executionOutcomeTrigger(tc.outcome)
			if ok != tc.wantOK {
				t.Fatalf("executionOutcomeTrigger(%q) ok = %v, want %v", tc.outcome, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("executionOutcomeTrigger(%q) = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}
}

// TestLogProviderFailureDiagnostic covers §7.3's own
// correlation-id-scoped operator log surface directly: every non-nil
// wire field must appear on the log line, under its own documented
// attribute name, and every nil field must be OMITTED rather than logged
// as an empty string or a literal "<nil>" -- an omitted attribute and a
// present-but-empty one look identical to a human reading the line, but
// only the first is honest about what OpenCode/this adapter actually
// supplied (see ProviderFailureDiagnostic's own "omitempty" fields,
// diagnostic.go). Also covers the identity fields C4 added
// (session_id/message_id/turn_id/correlation_id): session_id/message_id
// are unconditional (always sourced off the wire event); turn_id/
// correlation_id are each omitted exactly when the caller passes their
// own zero value ("" / nil) -- the "no Processing turn found" case
// completeProcessingTurn's own doc comment describes.
func TestLogProviderFailureDiagnostic(t *testing.T) {
	statusCode := 429
	corrID := "corr-turn-own-abc123"

	tests := []struct {
		name          string
		diagnostic    *sandboxws.ExecutionCompleteDiagnostic
		turnID        string
		correlationID *string
		wantAttrs     map[string]string // attr -> substring expected in the line
		wantAbsent    []string          // attr keys that must NOT appear at all
	}{
		{
			name: "every field present",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{
				Message:           strPtr("rate limited"),
				UnionMember:       strPtr("APIError"),
				StatusCode:        &statusCode,
				ProviderRequestId: strPtr("req_abc123"),
				Model:             strPtr("anthropic/claude-sonnet-4-5"),
				RuntimeVersion:    strPtr("1.17.15"),
				SandboxId:         strPtr("sbx-live-0001"),
			},
			turnID:        "trn-11111111-1111-1111-1111-111111111111",
			correlationID: &corrID,
			wantAttrs: map[string]string{
				"session_id":                     "ses-fixture-1",
				"message_id":                     "msg-fixture-1",
				"turn_id":                        "trn-11111111-1111-1111-1111-111111111111",
				"correlation_id":                 corrID,
				"diagnostic_message":             "rate limited",
				"diagnostic_union_member":        "APIError",
				"diagnostic_status_code":         "429",
				"diagnostic_provider_request_id": "req_abc123",
				"diagnostic_model":               "anthropic/claude-sonnet-4-5",
				"diagnostic_runtime_version":     "1.17.15",
				"diagnostic_sandbox_id":          "sbx-live-0001",
			},
			// F7: a KNOWN model must never also carry the "unknown" flag --
			// the two are mutually exclusive.
			wantAbsent: []string{"diagnostic_model_unknown"},
		},
		{
			name:       "every field nil is omitted, not logged empty",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{},
			// turnID/correlationID left zero -- the "no Processing turn
			// found for this delivery" case.
			wantAttrs: map[string]string{
				"session_id": "ses-fixture-1",
				"message_id": "msg-fixture-1",
				// F7: Model is nil here same as every other field, but
				// unlike every other field this ABSENCE must be logged
				// explicitly rather than silently omitted -- "we could not
				// determine which model ran" must never be indistinguishable
				// from "this build does not record the model at all".
				"diagnostic_model_unknown": "true",
			},
			wantAbsent: []string{
				"diagnostic_message", "diagnostic_union_member", "diagnostic_status_code",
				"diagnostic_provider_request_id", "diagnostic_model", "diagnostic_runtime_version",
				"diagnostic_sandbox_id", "turn_id", "correlation_id",
			},
		},
		{
			name: "only the allowlisted request id is present -- no message/status this time",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{
				ProviderRequestId: strPtr("req_only_this_survived"),
				Model:             strPtr("openai/gpt-5"),
			},
			turnID: "trn-22222222-2222-2222-2222-222222222222",
			wantAttrs: map[string]string{
				"session_id":                     "ses-fixture-1",
				"message_id":                     "msg-fixture-1",
				"turn_id":                        "trn-22222222-2222-2222-2222-222222222222",
				"diagnostic_provider_request_id": "req_only_this_survived",
				"diagnostic_model":               "openai/gpt-5",
			},
			wantAbsent: []string{
				"diagnostic_message", "diagnostic_union_member", "diagnostic_status_code",
				"diagnostic_runtime_version", "diagnostic_sandbox_id", "correlation_id",
				"diagnostic_model_unknown",
			},
		},
		{
			// F7's own motivating scenario, VERIFIED LIVE
			// (ProviderFailureDiagnostic.Model's own doc comment,
			// internal/adapters/outbound/opencode/diagnostic.go): a genuine
			// "model not found" session.error fires with no assistant
			// message.updated ever created at all, so the diagnostic is
			// otherwise populated (message/unionMember) but Model stays "".
			// Before F7, this log line was silent about the model on
			// exactly this genuinely-unknown case, indistinguishable from a
			// build that simply never wired up model reporting.
			name: "model genuinely unknown alongside an otherwise-populated diagnostic",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{
				Message:     strPtr("ProviderModelNotFoundError: Model not found: bogus-model"),
				UnionMember: strPtr("UnknownError"),
			},
			turnID: "trn-33333333-3333-3333-3333-333333333333",
			wantAttrs: map[string]string{
				"session_id":               "ses-fixture-1",
				"message_id":               "msg-fixture-1",
				"turn_id":                  "trn-33333333-3333-3333-3333-333333333333",
				"diagnostic_message":       "ProviderModelNotFoundError: Model not found: bogus-model",
				"diagnostic_union_member":  "UnknownError",
				"diagnostic_model_unknown": "true",
			},
			wantAbsent: []string{
				"diagnostic_model", "diagnostic_status_code", "diagnostic_provider_request_id",
				"diagnostic_runtime_version", "diagnostic_sandbox_id", "correlation_id",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			logProviderFailureDiagnostic(logger, tc.diagnostic, "ses-fixture-1", "msg-fixture-1", tc.turnID, tc.correlationID)

			line := buf.String()
			for attr, want := range tc.wantAttrs {
				if !strings.Contains(line, attr+"="+want) && !strings.Contains(line, attr+`="`+want+`"`) {
					t.Errorf("log line missing %s=%s; got: %s", attr, want, line)
				}
			}
			for _, attr := range tc.wantAbsent {
				if strings.Contains(line, attr+"=") {
					t.Errorf("log line unexpectedly carries %s; got: %s", attr, line)
				}
			}
		})
	}
}

// parseOwnerRepo's own table-driven test used to live here -- audit-
// remediation batch B3 moved both this file's own parseOwnerRepo AND
// internal/app/imagebuild/builder.go's byte-for-byte fork of it into
// internal/domain/reposource.ParseOwnerRepo, and moved this exact test
// table with it: see TestParseOwnerRepo in
// internal/domain/reposource/reposource_test.go, which covers every edge
// case (https URL, with/without a trailing ".git", with/without a
// trailing slash, a non-GitHub host parsed generically, malformed inputs)
// both of this file's and builder.go's own pre-existing tests relied on.
