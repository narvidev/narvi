package contractstest

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

func TestSandboxCommandsRoundTrip(t *testing.T) {
	sch := compileSchema(t, "sandbox-ws/v1/commands.schema.json", "")

	t.Run("Prompt", func(t *testing.T) {
		conversationID := "conv-123"
		model := "claude-sonnet-5"
		effort := "high"

		roundTrip(t, sch, sandboxws.Prompt{
			Type:           "prompt",
			MessageId:      "m1",
			SessionId:      testSessionID,
			Gen:            1,
			ConversationId: &conversationID,
			Text:           "fix the failing test",
			Model:          &model,
			Effort:         &effort,
			ScmName:        "Ada Lovelace",
			ScmEmail:       "ada@example.com",
			PlanMode:       true,
		})
	})

	t.Run("Prompt_OmittedConversationId", func(t *testing.T) {
		// §3.3 / commands.schema.json description: omitting conversationId
		// means "start a fresh conversation" — the one field in /contracts
		// where omission and explicit null are deliberately synonymous.
		// model/effort are still required keys, but their value may be null
		// (use the session/plan default).
		roundTrip(t, sch, sandboxws.Prompt{
			Type:      "prompt",
			MessageId: "m2",
			SessionId: testSessionID,
			Gen:       1,
			Text:      "fix the failing test",
			Model:     nil,
			Effort:    nil,
			ScmName:   "Ada Lovelace",
			ScmEmail:  "ada@example.com",
		})
	})

	t.Run("Prompt_ReceiptRequested", func(t *testing.T) {
		// Technical plan §3.3, prompt receipts: receiptRequested is optional
		// and carries no default, so a prompt that asks for no receipt omits
		// the key (Prompt_OmittedConversationId above) and stays byte-identical
		// to one an older control plane sends.
		requested := true
		roundTrip(t, sch, sandboxws.Prompt{
			Type:             "prompt",
			MessageId:        "m2r",
			SessionId:        testSessionID,
			Gen:              1,
			Text:             "fix the failing test",
			ScmName:          "Ada Lovelace",
			ScmEmail:         "ada@example.com",
			ReceiptRequested: &requested,
		})
	})

	t.Run("Stop", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Stop{
			Type:      "stop",
			MessageId: "m3",
			SessionId: testSessionID,
			Gen:       1,
		})
	})

	t.Run("Push", func(t *testing.T) {
		remote := "upstream"
		roundTrip(t, sch, sandboxws.Push{
			Type:      "push",
			MessageId: "m4",
			SessionId: testSessionID,
			Gen:       1,
			Repos: []sandboxws.PushReposElem{
				{Name: "narvi", Branch: "session/abc", Remote: &remote},
				{Name: "docs", Branch: "session/abc", Remote: nil},
			},
		})
	})

	t.Run("Snapshot", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Snapshot{
			Type:      "snapshot",
			MessageId: "m5",
			SessionId: testSessionID,
			Gen:       1,
		})
	})

	t.Run("Shutdown", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Shutdown{
			Type:      "shutdown",
			MessageId: "m6",
			SessionId: testSessionID,
			Gen:       1,
		})
	})

	t.Run("Ack", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Ack{
			Type:      "ack",
			MessageId: "m7",
			SessionId: testSessionID,
			Gen:       1,
			AckId:     "execution_complete:m0",
		})
	})

	t.Run("GitSyncComplete", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.GitSyncComplete{
			Type:      "git_sync_complete",
			MessageId: "m8",
			SessionId: testSessionID,
			Gen:       1,
		})
	})

	t.Run("Checkout", func(t *testing.T) {
		// Technical plan §21.1, §30.4: the commit a review turn records, read
		// from its pull request's ref in the base repository -- a SHA-1 and a
		// SHA-256 name alike.
		roundTrip(t, sch, sandboxws.Checkout{
			Type:      "checkout",
			MessageId: "m9",
			SessionId: testSessionID,
			Gen:       1,
			Repos: []sandboxws.CheckoutReposElem{
				{Name: "widgets", Ref: "refs/pull/7/head", Sha: "0123456789abcdef0123456789abcdef01234567"},
				{Name: "docs", Ref: "refs/pull/12/head", Sha: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			},
		})
	})

	// The ref and the sha reach the agent's git argument list, so the
	// schema and the generated decoder alike refuse any other shape.
	t.Run("Checkout_RefOrShaOutOfShapeRejected", func(t *testing.T) {
		const goodRef, goodSha = "refs/pull/7/head", "0123456789abcdef0123456789abcdef01234567"
		tests := []struct{ name, ref, sha string }{
			{name: "a branch for ref", ref: "refs/heads/main", sha: goodSha},
			{name: "an option for ref", ref: "--upload-pack=x", sha: goodSha},
			{name: "an abbreviated sha", ref: goodRef, sha: goodSha[:12]},
			{name: "an uppercase sha", ref: goodRef, sha: "0123456789ABCDEF0123456789ABCDEF01234567"},
			{name: "a revision expression for sha", ref: goodRef, sha: "HEAD~1"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				payload := []byte(`{"type":"checkout","messageId":"m10","sessionId":"` + testSessionID +
					`","gen":1,"repos":[{"name":"widgets","ref":"` + tt.ref + `","sha":"` + tt.sha + `"}]}`)
				if err := validateJSON(t, sch, payload); err == nil {
					t.Fatal("expected schema validation to fail, got nil error")
				}
				var cmd sandboxws.Checkout
				if err := json.Unmarshal(payload, &cmd); err == nil {
					t.Fatal("expected Go unmarshal to fail, got nil error")
				}
			})
		}
	})

	t.Run("Checkout_NoRepoRejected", func(t *testing.T) {
		payload := []byte(`{"type":"checkout","messageId":"m11","sessionId":"` + testSessionID + `","gen":1,"repos":[]}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected a checkout naming no repo to fail validation, got nil error")
		}
	})
}

func TestSandboxCommandsRejectUnknownType(t *testing.T) {
	sch := compileSchema(t, "sandbox-ws/v1/commands.schema.json", "")

	// "restart" is not one of the 8 §6.1 commands; the oneOf must reject it
	// (every branch's "type" const fails to match).
	data := []byte(`{"type":"restart","messageId":"m1","sessionId":"` + testSessionID + `","gen":1}`)
	if err := validateJSON(t, sch, data); err == nil {
		t.Fatal("expected schema validation to reject an unknown command type, got nil error")
	}
}
