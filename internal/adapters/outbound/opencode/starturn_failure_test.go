package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

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

// TestStartTurn_NilModelOmitsWireModelField pins §7.3/A1: a request that
// names no model (cmd.Model nil -- the default configuration every one of
// Composer/PlanModeView/Timeline resume/DecisionInbox dispatches through,
// web/src/session) must reach OpenCode's own POST .../prompt_async with
// the "model" key ABSENT ENTIRELY, letting the engine apply its own
// configured default -- exactly origin/main's own behavior, confirmed by a
// parent-commit control run directly against both revisions during this
// audit (matching byte-for-byte: `{"parts":[{"type":"text","text":"hi"}]}`
// on both, and `{"model":{"providerID":"anthropic",
// "modelID":"claude-sonnet-4-5"},"parts":[...]}` on both for a named
// model). An earlier version of resolveModel/resolveModelForced (session.go)
// forced a hardcoded fallback model onto this exact request instead, a
// silent PRODUCTION behavior change on every default turn for every
// client -- not a diagnostic-only concern (§7.3's own "not the model that
// ran" gap is fixed a different way, see ProviderFailureDiagnostic.Model's
// own doc comment, diagnostic.go). This asserts the RAW wire bytes (not
// the parsed Go struct, which a struct-level nil check could satisfy even
// if some OTHER code path re-added the key under a different Go field),
// so a regression that reintroduces resolveModelForced at this call site
// is caught here directly, not merely inferred from the diagnostic.
func TestStartTurn_NilModelOmitsWireModelField(t *testing.T) {
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sessionResponse{ID: "ses_fake"})
		case r.Method == http.MethodPost:
			// The only OTHER POST this test's own StartTurn call can reach
			// is .../prompt_async -- captured verbatim, raw bytes, before
			// any Go struct ever gets a chance to reshape them.
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading prompt_async request body: %v", err)
			}
			select {
			case captured <- raw:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	sink, _ := spyEventSink(t)
	// Model deliberately left unset -- the default configuration.
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, sink, nil)
		return err
	})

	var raw []byte
	select {
	case raw = <-captured:
	case <-time.After(testWait):
		t.Fatal("prompt_async request was never observed -- StartTurn never reached postPromptAsync")
	}

	// Captured -- no need to let StartTurn run out its own full wait; this
	// test only cares about the request it already sent.
	cancel()
	_ = group.Wait()

	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("json.Unmarshal(prompt_async request body) = %v; body: %s", err, raw)
	}
	if _, present := asMap["model"]; present {
		t.Errorf("prompt_async request body = %s, want the \"model\" key ABSENT entirely for a request "+
			"that named no model -- installing a Narvi-side default here is a production behavior change "+
			"on every default turn, for every client, not merely a diagnostic-completeness concern "+
			"(resolveModel's own doc comment, session.go)", raw)
	}
	if _, present := asMap["parts"]; !present {
		t.Errorf("prompt_async request body = %s, want a real \"parts\" key present -- "+
			"a body that has neither key would pass the \"model\" check above vacuously", raw)
	}
}
