package sessionactor

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/continuity"
	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// TestBuildPromptPayload_RecapOnlyForItsGen: a prompt opens with the recap
// stored on its turn only when that recap was built for the gen the
// prompt goes to (technical plan §35.5), joined to the request by
// continuity.Separator; a recap built for another gen, or none, leaves the
// text the request alone.
func TestBuildPromptPayload_RecapOnlyForItsGen(t *testing.T) {
	t.Parallel()
	request := "carry on with the migration"
	recap := continuity.LineHeader + "\n" + continuity.LineClosing
	gen := func(g int32) *int32 { return &g }

	tests := []struct {
		name     string
		recap    *string
		recapGen *int32
		want     string
	}{
		{"no recap", nil, nil, request},
		{"a recap built for this gen", &recap, gen(3), recap + continuity.Separator + request},
		{"a recap built for an earlier gen", &recap, gen(2), request},
		{"a recap built for a later gen", &recap, gen(4), request},
		{"a recap with no gen", &recap, nil, request},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := sqlcgen.Turn{Prompt: &request, ContinuityRecap: tc.recap, ContinuityRecapGen: tc.recapGen}
			raw, err := BuildPromptPayload("session-1", sqlcgen.Session{}, sqlcgen.Sandbox{Gen: 3}, target, "msg-1", false)
			if err != nil {
				t.Fatalf("BuildPromptPayload: %v", err)
			}
			var got sandboxws.Prompt
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Text != tc.want {
				t.Fatalf("Text = %q, want %q", got.Text, tc.want)
			}
		})
	}
}

// TestPromptReachEvidence: only a prompt_received that names its prompt,
// or an event of a turn the agent is running, is evidence that a prompt
// reached its gen; a frame of the boot or of the connection never is.
func TestPromptReachEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		cmd          SandboxEvent
		wantEvidence bool
		wantPrompt   string
	}{
		{"prompt_received", SandboxEvent{Type: "prompt_received", Raw: json.RawMessage(`{"type":"prompt_received","promptMessageId":"p-1"}`)}, true, "p-1"},
		{"prompt_received naming no prompt", SandboxEvent{Type: "prompt_received", Raw: json.RawMessage(`{"type":"prompt_received"}`)}, false, ""},
		{"prompt_received that does not decode", SandboxEvent{Type: "prompt_received", Raw: json.RawMessage(`{"promptMessageId":7}`)}, false, ""},
		{"step_start", SandboxEvent{Type: "step_start"}, true, ""},
		{"token", SandboxEvent{Type: "token"}, true, ""},
		{"tool_call", SandboxEvent{Type: "tool_call"}, true, ""},
		{"tool_result", SandboxEvent{Type: "tool_result"}, true, ""},
		{"step_finish", SandboxEvent{Type: "step_finish"}, true, ""},
		{"sub_task_start", SandboxEvent{Type: "sub_task_start"}, true, ""},
		{"sub_task_finish", SandboxEvent{Type: "sub_task_finish"}, true, ""},
		{"execution_complete", SandboxEvent{Type: "execution_complete"}, true, ""},
		{"ready", SandboxEvent{Type: "ready"}, false, ""},
		{"heartbeat", SandboxEvent{Type: "heartbeat"}, false, ""},
		{"boot_progress", SandboxEvent{Type: "boot_progress"}, false, ""},
		{"git_sync", SandboxEvent{Type: "git_sync"}, false, ""},
		{"boot_timing", SandboxEvent{Type: "boot_timing"}, false, ""},
		{"warning", SandboxEvent{Type: "warning"}, false, ""},
		{"snapshot_ready", SandboxEvent{Type: "snapshot_ready"}, false, ""},
		{"checkout_result", SandboxEvent{Type: "checkout_result"}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evidence, prompt := promptReachEvidence(tc.cmd)
			if evidence != tc.wantEvidence {
				t.Fatalf("evidence = %v, want %v", evidence, tc.wantEvidence)
			}
			if got := stringOrEmpty(prompt); got != tc.wantPrompt || (prompt == nil) != (tc.wantPrompt == "") {
				t.Fatalf("prompt message id = %v, want %q", prompt, tc.wantPrompt)
			}
		})
	}
}

// TestFreshLineageFacts: the facts a fresh-spawn claim reads off the row
// it replaces -- the refusal's kind, a live gen given up on, a mark that
// counts only for its own gen.
func TestFreshLineageFacts(t *testing.T) {
	t.Parallel()
	t.Run("refusal", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			refusal *snapshotRefusal
			want    sandbox.LineageRefusal
		}{
			{nil, sandbox.LineageRefusalNone},
			{&snapshotRefusal{clear: true}, sandbox.LineageRefusalCleared},
			{&snapshotRefusal{clear: false}, sandbox.LineageRefusalKept},
		} {
			if got := lineageRefusal(tc.refusal); got != tc.want {
				t.Errorf("lineageRefusal(%+v) = %v, want %v", tc.refusal, got, tc.want)
			}
		}
	})
	t.Run("replaced live", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			status sandbox.State
			want   bool
		}{
			{sandbox.StateSpawning, true}, {sandbox.StateConnecting, true}, {sandbox.StateBooting, true}, {sandbox.StateReady, true},
			{sandbox.StatePending, false}, {sandbox.StateStopped, false}, {sandbox.StateFailed, false}, {sandbox.StateStale, false},
			{sandbox.StateSuspect, false}, {sandbox.StateSnapshotting, false},
		} {
			if got := replacedLive(tc.status); got != tc.want {
				t.Errorf("replacedLive(%s) = %v, want %v", tc.status, got, tc.want)
			}
		}
	})
	t.Run("pending mark", func(t *testing.T) {
		t.Parallel()
		gen := func(g int32) *int32 { return &g }
		for _, tc := range []struct {
			name string
			row  sqlcgen.Sandbox
			want bool
		}{
			{"no mark", sqlcgen.Sandbox{Gen: 2}, false},
			{"the live gen's mark", sqlcgen.Sandbox{Gen: 2, FreshLineageGen: gen(2)}, true},
			{"an earlier gen's mark", sqlcgen.Sandbox{Gen: 3, FreshLineageGen: gen(2)}, false},
		} {
			if got := freshLineagePending(tc.row); got != tc.want {
				t.Errorf("%s: freshLineagePending = %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}
