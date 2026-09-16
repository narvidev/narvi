package sessionactor

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestHashSandboxToken_DeterministicAndDistinct proves hashSandboxToken
// (this package's own duplicate of wshub.HashSandboxToken's algorithm,
// kept separate to avoid an import cycle -- see its own doc comment)
// produces a deterministic, non-empty digest, and that distinct inputs
// produce distinct digests.
func TestHashSandboxToken_DeterministicAndDistinct(t *testing.T) {
	t.Parallel()

	a := hashSandboxToken("token-a")
	aAgain := hashSandboxToken("token-a")
	b := hashSandboxToken("token-b")

	if a == "" {
		t.Fatal("hashSandboxToken() = empty, want a real digest")
	}
	if a != aAgain {
		t.Errorf("hashSandboxToken(\"token-a\") = %q and %q, want identical", a, aAgain)
	}
	if a == b {
		t.Errorf("hashSandboxToken(\"token-a\") == hashSandboxToken(\"token-b\") = %q, want distinct", a)
	}
}

// TestBuildPromptPayload proves BuildPromptPayload marshals a real,
// schema-required-field-complete sandboxws.Prompt: ConversationId nil for
// a session with no prior conversation, or the session's own recorded
// value when present; Text/Model/PlanMode taken from the turn; ScmName/
// ScmEmail always non-empty.
func TestBuildPromptPayload(t *testing.T) {
	t.Parallel()

	var sandboxID, turnID pgtype.UUID
	_ = sandboxID.Scan("11111111-1111-1111-1111-111111111111")
	_ = turnID.Scan("22222222-2222-2222-2222-222222222222")

	sandboxRow := sqlcgen.Sandbox{ID: sandboxID, Gen: 4}
	prompt := "please do the thing"
	model := "claude-sonnet-5"
	effort := "high"

	t.Run("no prior conversation -> nil ConversationId", func(t *testing.T) {
		t.Parallel()

		sessionRow := sqlcgen.Session{OpencodeConversationID: nil}
		turn := sqlcgen.Turn{ID: turnID, Prompt: &prompt, ModelID: &model, Effort: &effort, PlanMode: true}

		raw, err := BuildPromptPayload("session-1", sessionRow, sandboxRow, turn, "msg-1")
		if err != nil {
			t.Fatalf("BuildPromptPayload() error = %v, want nil", err)
		}

		var got sandboxws.Prompt
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal as sandboxws.Prompt: %v (payload: %s)", err, raw)
		}
		if got.Type != "prompt" {
			t.Errorf("Type = %q, want %q", got.Type, "prompt")
		}
		if got.SessionId != "session-1" {
			t.Errorf("SessionId = %q, want %q", got.SessionId, "session-1")
		}
		if got.Gen != 4 {
			t.Errorf("Gen = %d, want 4", got.Gen)
		}
		if got.ConversationId != nil {
			t.Errorf("ConversationId = %v, want nil", got.ConversationId)
		}
		if got.Text != prompt {
			t.Errorf("Text = %q, want %q", got.Text, prompt)
		}
		if got.Model == nil || *got.Model != model {
			t.Errorf("Model = %v, want %q", got.Model, model)
		}
		if !got.PlanMode {
			t.Error("PlanMode = false, want true")
		}
		if got.ScmName == "" || got.ScmEmail == "" {
			t.Error("ScmName/ScmEmail must be non-empty")
		}
		// (§29.8): turns.effort now threads through end to end,
		// mirroring model_id's own assertion immediately above -- this
		// replaces the pre-existing hardcoded-nil assertion this test used
		// to make here.
		if got.Effort == nil || *got.Effort != effort {
			t.Errorf("Effort = %v, want %q", got.Effort, effort)
		}
		// MessageId (finding F3 (§21.1's amendment)) is now a caller-supplied
		// parameter, never generated inside BuildPromptPayload itself --
		// asserting the EXACT value proves it is threaded through
		// verbatim, never silently regenerated or dropped.
		if got.MessageId != "msg-1" {
			t.Errorf("MessageId = %q, want %q (the caller-supplied value, threaded through verbatim)", got.MessageId, "msg-1")
		}
	})

	t.Run("prior conversation id is carried forward", func(t *testing.T) {
		t.Parallel()

		conversationID := "conv-abc-123"
		sessionRow := sqlcgen.Session{OpencodeConversationID: &conversationID}
		turn := sqlcgen.Turn{ID: turnID, Prompt: &prompt}

		raw, err := BuildPromptPayload("session-2", sessionRow, sandboxRow, turn, "msg-2")
		if err != nil {
			t.Fatalf("BuildPromptPayload() error = %v, want nil", err)
		}

		var got sandboxws.Prompt
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal as sandboxws.Prompt: %v", err)
		}
		if got.ConversationId == nil || *got.ConversationId != conversationID {
			t.Errorf("ConversationId = %v, want %q", got.ConversationId, conversationID)
		}
	})

	t.Run("nil prompt text is the empty string, never a panic", func(t *testing.T) {
		t.Parallel()

		sessionRow := sqlcgen.Session{}
		turn := sqlcgen.Turn{ID: turnID}

		raw, err := BuildPromptPayload("session-3", sessionRow, sandboxRow, turn, "msg-3")
		if err != nil {
			t.Fatalf("BuildPromptPayload() error = %v, want nil", err)
		}
		var got sandboxws.Prompt
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal as sandboxws.Prompt: %v", err)
		}
		if got.Text != "" {
			t.Errorf("Text = %q, want empty", got.Text)
		}
		if got.Model != nil || got.Effort != nil {
			t.Errorf("Model = %v, Effort = %v, want both nil for a turn with neither set", got.Model, got.Effort)
		}
	})
}

// TestStringOrEmpty covers stringOrEmpty's own nil/non-nil cases.
func TestStringOrEmpty(t *testing.T) {
	t.Parallel()

	if got := stringOrEmpty(nil); got != "" {
		t.Errorf("stringOrEmpty(nil) = %q, want empty", got)
	}
	s := "hello"
	if got := stringOrEmpty(&s); got != "hello" {
		t.Errorf("stringOrEmpty(&%q) = %q, want %q", s, got, s)
	}
}
