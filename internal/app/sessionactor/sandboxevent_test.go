package sessionactor

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// TestSandboxTransitionTrigger is table-driven over every (event type,
// LastBootPhase, status, boot evidence) combination that matters: the two mappings this
// function itself implements, each exercised both ON its precondition and
// OFF it (proving the precondition check, not just the trigger kind,
// gates the decision), plus a handful of event types/status pairs this
// function deliberately does not map -- including a Suspect status,
// proving sandboxTransitionTrigger ITSELF never speculatively fires a
// recovery transition. Suspect-recovery (§3.2, "two-phase
// terminalization") is real and wired, but lives entirely in
// handleSandboxEvent's own dedicated branch (sandboxevent.go, see that
// file's top comment), which runs BEFORE this function is ever called and
// uses sandbox.RecoverTrigger with a stored pre_suspect_status -- not a
// third case added to this function's own two-mapping table.
func TestSandboxTransitionTrigger(t *testing.T) {
	t.Parallel()

	bootPhase := "web:starting"

	tests := []struct {
		name          string
		eventType     string
		lastBootPhase *string
		status        sandbox.State
		bootEvidence  bool
		wantOK        bool
		wantKind      sandbox.TriggerKind
	}{
		{
			name:      "ready while connecting fires WSConnected",
			eventType: "ready",
			status:    sandbox.StateConnecting,
			wantOK:    true,
			wantKind:  sandbox.TriggerWSConnected,
		},
		{
			name:      "ready while already ready is a no-op",
			eventType: "ready",
			status:    sandbox.StateReady,
			wantOK:    false,
		},
		{
			name:      "ready while suspect is a no-op (recovery, if any, is handled elsewhere -- see this file's own doc comment)",
			eventType: "ready",
			status:    sandbox.StateSuspect,
			wantOK:    false,
		},
		{
			name:      "ready while pending is a no-op",
			eventType: "ready",
			status:    sandbox.StatePending,
			wantOK:    false,
		},
		{
			name:         "heartbeat with nil LastBootPhase while booting, after boot evidence, fires BootComplete",
			eventType:    "heartbeat",
			status:       sandbox.StateBooting,
			bootEvidence: true,
			wantOK:       true,
			wantKind:     sandbox.TriggerBootComplete,
		},
		{
			name:      "heartbeat with nil LastBootPhase while booting, before any boot evidence, is a no-op (§3.2: absence of information)",
			eventType: "heartbeat",
			status:    sandbox.StateBooting,
			wantOK:    false,
		},
		{
			name:          "heartbeat with a non-nil LastBootPhase while booting is a no-op",
			eventType:     "heartbeat",
			lastBootPhase: &bootPhase,
			status:        sandbox.StateBooting,
			bootEvidence:  true,
			wantOK:        false,
		},
		{
			name:         "heartbeat with nil LastBootPhase while already ready is a no-op",
			eventType:    "heartbeat",
			status:       sandbox.StateReady,
			bootEvidence: true,
			wantOK:       false,
		},
		{
			name:         "heartbeat with nil LastBootPhase while suspect is a no-op",
			eventType:    "heartbeat",
			status:       sandbox.StateSuspect,
			bootEvidence: true,
			wantOK:       false,
		},
		{
			name:         "boot_timing while booting never transitions anything, even though it is evidence",
			eventType:    "boot_timing",
			status:       sandbox.StateBooting,
			bootEvidence: true,
			wantOK:       false,
		},
		{
			name:      "execution_complete never transitions anything",
			eventType: "execution_complete",
			status:    sandbox.StateReady,
			wantOK:    false,
		},
		{
			name:      "token event never transitions anything",
			eventType: "token",
			status:    sandbox.StateBooting,
			wantOK:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			trig, ok := sandboxTransitionTrigger(tc.eventType, tc.lastBootPhase, tc.status, tc.bootEvidence)
			if ok != tc.wantOK {
				t.Fatalf("sandboxTransitionTrigger(%q, %v, %s, evidence=%v) ok = %v, want %v",
					tc.eventType, tc.lastBootPhase, tc.status, tc.bootEvidence, ok, tc.wantOK)
			}
			if ok && trig.Kind != tc.wantKind {
				t.Errorf("sandboxTransitionTrigger(%q, %v, %s, evidence=%v) kind = %s, want %s",
					tc.eventType, tc.lastBootPhase, tc.status, tc.bootEvidence, trig.Kind, tc.wantKind)
			}
		})
	}
}

// TestPeekAckID proves the ackId extraction: present-and-non-empty on a
// synthetic critical-shaped payload, absent on a non-critical-shaped one,
// and defensively "" (never a panic) on malformed JSON.
func TestPeekAckID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "critical event carries ackId",
			raw:  json.RawMessage(`{"type":"execution_complete","messageId":"m1","ackId":"execution_complete:m1"}`),
			want: "execution_complete:m1",
		},
		{
			name: "non-critical event has no ackId field",
			raw:  json.RawMessage(`{"type":"token","messageId":"m1"}`),
			want: "",
		},
		{
			name: "ackId explicitly empty string",
			raw:  json.RawMessage(`{"type":"heartbeat","ackId":""}`),
			want: "",
		},
		{
			name: "malformed JSON never panics, returns empty",
			raw:  json.RawMessage(`not json`),
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := peekAckID(tc.raw); got != tc.want {
				t.Errorf("peekAckID(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
