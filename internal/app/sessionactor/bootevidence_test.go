package sessionactor

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// bootTimingFrame is a schema-valid boot_timing frame for metric, with
// failedJSON as its "failed" value ("" leaves the field out).
func bootTimingFrame(metric, failedJSON string) json.RawMessage {
	raw := `{"type":"boot_timing","messageId":"bt-1","sessionId":"s","gen":1,"metric":"` + metric + `","seconds":12.5`
	if failedJSON != "" {
		raw += `,"failed":` + failedJSON
	}
	return json.RawMessage(raw + `}`)
}

// TestBootEvidence pins which events show that a generation's boot has
// actually run (§3.2's boot-evidence rule): a reported phase, directly or
// on a heartbeat, or the agent's own successful boot_duration -- and
// nothing that only says the agent is connected, or that its boot is
// still running, or failed.
func TestBootEvidence(t *testing.T) {
	t.Parallel()

	phase := "web:starting"
	initial := "starting"

	tests := []struct {
		name string
		cmd  SandboxEvent
		want bool
	}{
		{
			name: "boot_progress",
			cmd:  SandboxEvent{Type: "boot_progress", Raw: json.RawMessage(`{"type":"boot_progress","phase":"web:starting"}`)},
			want: true,
		},
		{
			name: "heartbeat carrying a service phase",
			cmd:  SandboxEvent{Type: "heartbeat", LastBootPhase: &phase},
			want: true,
		},
		{
			name: "heartbeat carrying a fixed agent's initial phase",
			cmd:  SandboxEvent{Type: "heartbeat", LastBootPhase: &initial},
			want: true,
		},
		{
			name: "heartbeat with a null phase is not evidence: it is what the rule gates",
			cmd:  SandboxEvent{Type: "heartbeat"},
			want: false,
		},
		{
			name: "successful boot_duration",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: bootTimingFrame("boot_duration", "false")},
			want: true,
		},
		{
			name: "failed boot_duration",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: bootTimingFrame("boot_duration", "true")},
			want: false,
		},
		{
			name: "boot_duration without a failed field",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: bootTimingFrame("boot_duration", "")},
			want: false,
		},
		{
			name: "boot_duration with a null failed field",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: bootTimingFrame("boot_duration", "null")},
			want: false,
		},
		{
			name: "hook_rerun_duration speaks to a boot still running",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: bootTimingFrame("hook_rerun_duration", "false")},
			want: false,
		},
		{
			name: "git_checkout_duration speaks to a boot still running",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: bootTimingFrame("git_checkout_duration", "false")},
			want: false,
		},
		{
			name: "boot_timing failing its schema decode",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: json.RawMessage(`{"type":"boot_timing","metric":"boot_duration","failed":false}`)},
			want: false,
		},
		{
			name: "boot_timing that is not JSON",
			cmd:  SandboxEvent{Type: "boot_timing", Raw: json.RawMessage(`not json`)},
			want: false,
		},
		{
			name: "ready only says the agent is connected",
			cmd:  SandboxEvent{Type: "ready", Raw: json.RawMessage(`{"type":"ready"}`)},
			want: false,
		},
		{
			name: "git_sync speaks to a boot still running",
			cmd:  SandboxEvent{Type: "git_sync", Raw: json.RawMessage(`{"type":"git_sync","status":"synced"}`)},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bootEvidence(tc.cmd); got != tc.want {
				t.Errorf("bootEvidence(%s %s) = %v, want %v", tc.cmd.Type, tc.cmd.Raw, got, tc.want)
			}
		})
	}
}

// TestHasBootEvidence: evidence counts only for the generation it was
// recorded for.
func TestHasBootEvidence(t *testing.T) {
	t.Parallel()

	gen := func(g int32) *int32 { return &g }

	tests := []struct {
		name string
		row  sqlcgen.Sandbox
		want bool
	}{
		{name: "none recorded", row: sqlcgen.Sandbox{Gen: 2}, want: false},
		{name: "recorded for this gen", row: sqlcgen.Sandbox{Gen: 2, BootEvidenceGen: gen(2)}, want: true},
		{name: "recorded for the previous gen", row: sqlcgen.Sandbox{Gen: 2, BootEvidenceGen: gen(1)}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasBootEvidence(tc.row); got != tc.want {
				t.Errorf("hasBootEvidence(gen=%d, evidence=%v) = %v, want %v", tc.row.Gen, tc.row.BootEvidenceGen, got, tc.want)
			}
		})
	}
}
