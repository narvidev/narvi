package sessionactor

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// diagnosticFieldLogAttr maps every sandboxws.ExecutionCompleteDiagnostic
// struct field name to the operator log attribute
// logProviderFailureDiagnostic (pushpr.go) emits it under. F4's own
// self-checking mechanism: pushpr_integration_test.go's
// TestHandleSandboxEvent_ExecutionCompleteFailed_OperatorLogReachesTheRealPathAndMatchesTheJournal
// walks reflect.Type.Field over the wire struct and requires an entry
// here for each one it finds, so a field with no entry fails that test
// outright rather than silently dropping out of the comparison -- exactly
// what happened to StatusCode before this map existed (the comparison
// there used to enumerate six of the struct's seven fields by hand, with
// nothing making that enumeration exhaustive).
var diagnosticFieldLogAttr = map[string]string{
	"Message":           "diagnostic_message",
	"UnionMember":       "diagnostic_union_member",
	"StatusCode":        "diagnostic_status_code",
	"ProviderRequestId": "diagnostic_provider_request_id",
	"Model":             "diagnostic_model",
	"RuntimeVersion":    "diagnostic_runtime_version",
	"SandboxId":         "diagnostic_sandbox_id",
}

// diagnosticFieldString renders one ExecutionCompleteDiagnostic field's
// own reflect.Value -- always a pointer type (every field in the struct
// is *string or *int, matching ProviderFailureDiagnostic's own
// allowlist-by-type discipline, internal/adapters/outbound/opencode/
// diagnostic.go) -- as (formatted value, present). A nil pointer is "not
// present", matching the wire's own omitempty semantics; fmt.Sprint on
// the dereferenced value handles *string and *int identically, so this
// needs no per-field-type branch and keeps working unchanged if a future
// field is some other pointer-to-scalar type.
func diagnosticFieldString(v reflect.Value) (string, bool) {
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return "", false
	}
	return fmt.Sprint(v.Elem().Interface()), true
}

// logAttrString renders one decoded JSON log attribute (entry[key]) as
// (formatted value, present) -- mirroring diagnosticFieldString's own
// shape so the two sides of a comparison built from both stay symmetric.
// JSON numbers decode to float64 through encoding/json's own
// map[string]any convention (findLogEntry, planrecord_integration_test.go):
// every diagnostic numeric field is a whole-number HTTP status, so
// strconv.FormatFloat with 'f'/-1 precision renders "429", never
// "429.000000" or the exponential form Go's default %v would pick for an
// arbitrary bare float64.
func logAttrString(entry map[string]any, key string) (string, bool) {
	v, ok := entry[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return fmt.Sprint(t), true
	}
}

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
		},
		{
			name:       "every field nil is omitted, not logged empty",
			diagnostic: &sandboxws.ExecutionCompleteDiagnostic{},
			// turnID/correlationID left zero -- the "no Processing turn
			// found for this delivery" case.
			wantAttrs: map[string]string{
				"session_id": "ses-fixture-1",
				"message_id": "msg-fixture-1",
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
