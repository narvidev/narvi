package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// These two tests are the non-cancellation counterpart to
// starturn_cancellation_test.go's own three ctx-cancellation paths: moving
// ts := newTurnState(...) to the top of StartTurn (Finding 5) must NOT
// change behavior for a GENUINE (non-ctx-related) failure at either of
// StartTurn's first two early-return points -- each must still finalize
// with a Failed outcome (never Cancelled), exactly as before this batch.

// TestStartTurn_ResolveSessionGenuineFailureEmitsFailedTerminalEvent: the
// fake server's own POST /session returns a malformed body, so
// resolveSession fails for a real reason with ctx NOT canceled.
func TestStartTurn_ResolveSessionGenuineFailureEmitsFailedTerminalEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not valid json"))
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	sink, events := spyEventSink(t)
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	var onConversationIDCalls int
	convID, err := a.StartTurn(context.Background(), cmd, sink, func(string) { onConversationIDCalls++ })
	if err != nil {
		t.Fatalf("StartTurn() error = %v, want nil (a genuine resolveSession failure finalizes internally and returns nil, not an error)", err)
	}
	if convID != "" {
		t.Errorf("StartTurn() conversationID = %q, want empty", convID)
	}
	if onConversationIDCalls != 0 {
		t.Errorf("onConversationID called %d times, want 0 (resolveSession never resolved a real id on this path)", onConversationIDCalls)
	}

	final := lastExecutionComplete(t, events())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q (a genuine failure, not a cancellation)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
}

// TestStartTurn_PostPromptAsyncGenuineFailureEmitsFailedTerminalEvent:
// resolveSession succeeds (a valid session is created), but the fake
// server's own POST .../prompt_async returns a real 500, so
// postPromptAsync fails for a real reason with ctx NOT canceled.
func TestStartTurn_PostPromptAsyncGenuineFailureEmitsFailedTerminalEvent(t *testing.T) {
	// /session (session creation) succeeds; every other path -- in
	// particular prompt_async, and /event, irrelevant to this test -- fails
	// with a real 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sessionResponse{ID: "ses_fake"})
			return
		}
		http.Error(w, "simulated internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	sink, events := spyEventSink(t)
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	var reportedID string
	var onConversationIDCalls int
	onConversationID := func(id string) {
		onConversationIDCalls++
		reportedID = id
	}

	convID, err := a.StartTurn(context.Background(), cmd, sink, onConversationID)
	if err != nil {
		t.Fatalf("StartTurn() error = %v, want nil (a genuine postPromptAsync failure finalizes internally and returns nil, not an error)", err)
	}
	if convID != "ses_fake" {
		t.Errorf("StartTurn() conversationID = %q, want %q (resolveSession succeeded before prompt_async failed)", convID, "ses_fake")
	}
	// resolveSession DID succeed on this path (unlike the sibling test
	// above) -- onConversationID must have fired exactly once, with the
	// SAME resolved id, even though the turn as a whole still ends in a
	// genuine failure moments later.
	if onConversationIDCalls != 1 {
		t.Errorf("onConversationID called %d times, want exactly 1", onConversationIDCalls)
	}
	if reportedID != "ses_fake" {
		t.Errorf("onConversationID reported %q, want %q", reportedID, "ses_fake")
	}

	final := lastExecutionComplete(t, events())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q (a genuine failure, not a cancellation)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
}
