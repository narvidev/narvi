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
// diagnostic.go).
func TestLogProviderFailureDiagnostic(t *testing.T) {
	statusCode := 429

	tests := []struct {
		name       string
		diagnostic *sandboxws.ExecutionCompleteDiagnostic
		wantAttrs  map[string]string // attr -> substring expected in the line
		wantAbsent []string          // attr keys that must NOT appear at all
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
			wantAttrs: map[string]string{
				"diagnostic_message":             "rate limited",
				"diagnostic_union_member":        "APIError",
				"diagnostic_status_code":         "429",
				"diagnostic_provider_request_id": "req_abc123",
				"diagnostic_model":               "anthropic/claude-sonnet-4-5",
				"diagnostic_runtime_version":     "1.17.15",
				"diagnostic_sandbox_id":          "sbx-live-0001",
			},
		},
		{
			name:       "every field nil is omitted, not logged empty",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{},
			wantAbsent: []string{
				"diagnostic_message", "diagnostic_union_member", "diagnostic_status_code",
				"diagnostic_provider_request_id", "diagnostic_model", "diagnostic_runtime_version",
				"diagnostic_sandbox_id",
			},
		},
		{
			name: "only the allowlisted request id is present -- no message/status this time",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{
				ProviderRequestId: strPtr("req_only_this_survived"),
				Model:             strPtr("openai/gpt-5"),
			},
			wantAttrs: map[string]string{
				"diagnostic_provider_request_id": "req_only_this_survived",
				"diagnostic_model":               "openai/gpt-5",
			},
			wantAbsent: []string{
				"diagnostic_message", "diagnostic_union_member", "diagnostic_status_code",
				"diagnostic_runtime_version", "diagnostic_sandbox_id",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			logProviderFailureDiagnostic(logger, tc.diagnostic)

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
