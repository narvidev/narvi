package opencode

import (
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// This file pins E1's own corrected rule (modelForOutcome/turn.go): report
// this turn's own most recently recorded engine model whenever one was
// genuinely recorded, regardless of which branch errorForOutcome
// ultimately resolves to for the error text -- return "" only when no
// assistant message.updated was ever observed for this turn at all.
//
// TestModelForOutcome_Table below operates directly on turnState (the
// SAME white-box access this package's own outcome_test.go uses for
// deriveOutcome) -- deliberately lower-level than
// TestRealOrdering_ModelSurvivesLateArrivingAssistantError
// (realbinarycapture_test.go), which replays the real captured SSE bytes
// end to end through the real Adapter. This file isolates JUST the
// turnState-level rule, so every branch of it (including the ones a real
// scripted turn would be awkward or slow to elicit, e.g. "no assistant
// message ever seen") is covered directly and fast.

// apiErrTagged is a minimal, reusable APIError tagged error for tests
// below that only care that SOME error is present, not its own fields.
func apiErrTagged() *openCodeTaggedError {
	return &openCodeTaggedError{Name: "APIError", Data: &openCodeErrorData{Message: "boom"}}
}

func TestModelForOutcome_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup runs against a fresh turnState, in order, to build up the
		// exact sequence of observations this case models.
		setup     func(ts *turnState)
		wantModel string
		wantErr   bool // whether errorForOutcome() should be non-nil afterward
	}{
		{
			// VERIFIED LIVE real ordering (pinned OpenCode 1.17.15, 3/3
			// trials against a genuine 401 APIError): the assistant
			// message reporting the model arrives BEFORE session.error,
			// and that SAME message's own error field only arrives in a
			// SECOND, later message.updated -- too late for the live
			// session.idle finalize (which this setup simulates by
			// calling setSessionError, then stopping, exactly as
			// dispatchEvent's own session.idle case would observe things
			// at that moment) to see it in lastAssistantError. See
			// TestRealOrdering_ModelSurvivesLateArrivingAssistantError
			// (realbinarycapture_test.go) for the byte-for-byte replay of
			// this SAME scenario through the real Adapter/dispatchEvent.
			name: "real ordering: model-bearing message.updated then session.error, no error on the message yet",
			setup: func(ts *turnState) {
				ts.setLastAssistantMessage(openCodeMessageInfo{
					ID: "msg1", Role: "assistant",
					ModelID: "claude-opus-5", ProviderID: "anthropic",
				})
				ts.setSessionError(*apiErrTagged())
			},
			wantModel: "anthropic/claude-opus-5",
			wantErr:   true,
		},
		{
			// The OLD, narrower case this package's own previous revision
			// of modelForOutcome already handled correctly: the error IS
			// attached directly to the assistant message (no separate
			// session.error at all).
			name: "error attached directly to the assistant message",
			setup: func(ts *turnState) {
				ts.setLastAssistantMessage(openCodeMessageInfo{
					ID: "msg1", Role: "assistant",
					ModelID: "claude-opus-5", ProviderID: "anthropic",
					Error: apiErrTagged(),
				})
			},
			wantModel: "anthropic/claude-opus-5",
			wantErr:   true,
		},
		{
			// The ONE case the OLD doc comment's own example
			// (ProviderModelNotFoundError) actually describes, and the
			// only genuinely correct "" case: a session-level
			// session.error with NO assistant message.updated ever
			// observed for this turn at all. VERIFIED LIVE separately: a
			// real "model not found" prompt against the pinned binary
			// produced a session.error with no preceding assistant
			// message.updated.
			name: "session-level error with no assistant message ever seen",
			setup: func(ts *turnState) {
				ts.setSessionError(*apiErrTagged())
			},
			wantModel: "",
			wantErr:   true,
		},
		{
			// The one assistant message this turn did see carried no
			// model info at all (modelDisplayFromInfo's own empty-half
			// guard) -- still an honest "", not a wrong guess, and NOT
			// the same code path as "no assistant message ever seen"
			// (assistantMessageIDs/lastAssistantModel both still get
			// touched), but the observable result is identical.
			name: "assistant message seen but carried no model info",
			setup: func(ts *turnState) {
				ts.setLastAssistantMessage(openCodeMessageInfo{ID: "msg1", Role: "assistant"})
				ts.setSessionError(*apiErrTagged())
			},
			wantModel: "",
			wantErr:   true,
		},
		{
			// No error observed at all (a genuinely still-running or
			// successful turn) -- modelForOutcome is never actually
			// consulted on this path in production (both real call
			// sites gate on outcome.Outcome == Failed first), but the
			// method itself has no such gate of its own, so this proves
			// it degrades harmlessly rather than panicking or lying.
			name:      "nothing observed at all",
			setup:     func(*turnState) {},
			wantModel: "",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			collector := &eventCollector{}
			ts := newTurnState(sandboxws.Prompt{SessionId: testSessionID, Gen: 1}, collector.sink)
			tt.setup(ts)

			if got := ts.modelForOutcome(); got != tt.wantModel {
				t.Errorf("modelForOutcome() = %q, want %q", got, tt.wantModel)
			}
			if gotErr := ts.errorForOutcome() != nil; gotErr != tt.wantErr {
				t.Errorf("errorForOutcome() != nil = %v, want %v", gotErr, tt.wantErr)
			}
		})
	}
}

// TestModelForOutcome_ClearedAfterRetryStartsBeforeNewAssistantMessage pins
// clearErrorsForRetry's own remaining guard (turn.go's own doc comment on
// both clearErrorsForRetry and modelForOutcome): once modelForOutcome
// stopped gating on lastAssistantError, the ONLY thing left preventing a
// STALE, pre-retry model from being attributed to a session-level error
// the RETRY itself produces (before the retry's own first assistant
// message.updated ever arrives) is clearErrorsForRetry resetting
// lastAssistantModel back to "" at the retry boundary.
//
// Mutation-verified: deleting "ts.lastAssistantModel = \"\"" from
// clearErrorsForRetry (turn.go) makes this test fail with
// modelForOutcome() = "anthropic/claude-opus-4-5" (the STALE, original
// model), want "" -- while the rest of this package's own test suite
// stays green, exactly matching this finding's own "removing it leaves
// the suite green" description.
func TestModelForOutcome_ClearedAfterRetryStartsBeforeNewAssistantMessage(t *testing.T) {
	t.Parallel()

	collector := &eventCollector{}
	ts := newTurnState(sandboxws.Prompt{SessionId: testSessionID, Gen: 1}, collector.sink)

	// The ORIGINAL, pre-retry attempt: a model-bearing assistant message,
	// then a ContextOverflowError that will trigger a compaction retry.
	ts.setLastAssistantMessage(openCodeMessageInfo{
		ID: "msg-original", Role: "assistant",
		ModelID: "claude-opus-4-5", ProviderID: "anthropic",
	})
	ts.setSessionError(openCodeTaggedError{Name: "ContextOverflowError"})
	if got := ts.modelForOutcome(); got != "anthropic/claude-opus-4-5" {
		t.Fatalf("precondition: modelForOutcome() before retry = %q, want %q", got, "anthropic/claude-opus-4-5")
	}

	// The retry begins (mirrors attemptCompactionRetry/attemptTransientRetry,
	// adapter.go): errors AND the stale model are reset before re-dispatch.
	ts.clearErrorsForRetry()

	// The retry's own re-dispatch fails outright -- a session-level error
	// with NO new assistant message.updated ever having arrived for the
	// retry (e.g. the retried postPromptAsync call itself erroring, or a
	// session.error firing before the retry's own first message.updated).
	ts.setSessionError(*apiErrTagged())

	if got := ts.modelForOutcome(); got != "" {
		t.Errorf(`modelForOutcome() after a retry's own session-level error with no new assistant message = %q, want "" -- `+
			"the ORIGINAL, pre-retry model must not survive to be mis-attributed to the RETRY's own unrelated error", got)
	}
}
