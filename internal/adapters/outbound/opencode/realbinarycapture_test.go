package opencode

import (
	"context"
	"encoding/json"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// This file pins E1 (turn.go's own modelForOutcome) and part of E2/E5
// against the REAL event ordering and REAL wire bytes the pinned OpenCode
// 1.17.15 binary actually emits on its ordinary provider-failure path --
// captured live, not guessed, during this Step's own research pass: a
// real `opencode serve` process (npm-installed opencode-ai@1.17.15, run
// under an isolated XDG_DATA_HOME/XDG_CONFIG_HOME so it could never touch
// a real credential store), a real, deliberately-invalid, obviously
// synthetic Anthropic-shaped API key set via PUT /auth/anthropic, a real
// POST /session/{id}/prompt_async call, and the real resulting GET /event
// trace, captured with `curl -sN http://127.0.0.1:<port>/event` and
// inspected directly. Reproduced 3/3 trials, byte-identical event
// ordering and responseHeaders key set every time.
//
// The real captured ordering (session id elided here, see the constants
// below for the exact bytes):
//
//	message.updated  role=assistant  modelID/providerID SET, error ABSENT
//	session.error    APIError  (statusCode 401, real responseHeaders)
//	session.idle                        <- the adapter finalizes HERE
//	message.updated  role=assistant  error=APIError (SAME message, now late)
//	session.idle
//
// The captured raw JSON constants below are the EXACT "properties" bytes
// OpenCode itself emitted for the first three of those events (the
// session id substituted for this package's own fixed fake-server
// session, "ses_fake" -- see fake_server_test.go's own
// sessionResponse{ID: "ses_fake"} -- and the real machine-local temp
// directory this was captured under substituted for the inert,
// unmodeled "path.cwd" value -- openCodeMessageInfo has no "path" field
// at all, so that value is never decoded into anything; nothing else
// altered, spacing included), broadcast through sseLineRaw (below)
// rather than through this package's own messageUpdatedProps/
// openCodeMessageInfo struct literals the way this package's OTHER
// fixture helpers do (apiErrorMessageUpdatedWithModel,
// overflowMessageUpdatedWithModel): encoding a fixture by marshaling the
// SAME struct that later decodes it can never catch a wrong JSON tag
// (E2) -- these fixtures are decoded by the real production dispatchEvent
// path from literal bytes a real OpenCode process actually sent, so a
// future typo or rename in openCodeMessageInfo/openCodeErrorData's own
// `json:"..."` tags fails THIS test, not just the round-trip ones.
//
// No credential ever appears in these bytes: the captured 401 response
// body is "API key is invalid." -- the synthetic key itself was never
// echoed back by the (real, live) provider-shaped endpoint OpenCode
// called; its own JSON "request_id" is genuinely null. The one opaque
// token present, "cf-ray", is a per-request CDN trace id, not a
// credential or account identifier (see requestIDHeaderCandidates' own
// doc comment, diagnostic.go, for why it was added to the allowlist).

// capturedAssistantMessageUpdatedModelOnlyJSON is the FIRST captured
// message.updated: role=assistant, modelID="claude-opus-5",
// providerID="anthropic", NO error field at all yet.
const capturedAssistantMessageUpdatedModelOnlyJSON = `{"sessionID":"ses_fake","info":{"id":"msg_0c904ce700019mxKFd63d5Tdxy","parentID":"msg_0c904ce000013WrZ9O6KEEiDYZ","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/workspace","root":"/"},"cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"modelID":"claude-opus-5","providerID":"anthropic","time":{"created":1790078930544},"sessionID":"ses_fake"}}`

// capturedSessionErrorAPIErrorJSON is the captured session.error: a real
// 401 APIError, with the real responseHeaders key set (9 keys, no
// "x-request-id"/"request-id", but a real "cf-ray") and the real
// responseBody (its own "request_id" is null).
const capturedSessionErrorAPIErrorJSON = `{"sessionID":"ses_fake","error":{"name":"APIError","data":{"message":"API key is invalid.","statusCode":401,"isRetryable":false,"responseHeaders":{"cf-cache-status":"DYNAMIC","cf-ray":"a3f1328d4e1be195-MRS","connection":"keep-alive","content-length":"106","content-security-policy":"default-src 'none'; frame-ancestors 'none'","content-type":"application/json","date":"Tue, 22 Sep 2026 12:08:52 GMT","server":"cloudflare","x-robots-tag":"none"},"responseBody":"{\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"API key is invalid.\"},\"request_id\":null}","metadata":{"url":"https://api.anthropic.com/v1/messages"}}}}`

// capturedAssistantMessageUpdatedWithErrorJSON is the SECOND, LATE
// message.updated for the SAME message id as
// capturedAssistantMessageUpdatedModelOnlyJSON above -- now carrying the
// identical APIError session.error already reported, plus a "completed"
// timestamp. In the real trace this arrives strictly AFTER the first
// session.idle (i.e. after the adapter has already finalized).
const capturedAssistantMessageUpdatedWithErrorJSON = `{"sessionID":"ses_fake","info":{"id":"msg_0c904ce700019mxKFd63d5Tdxy","parentID":"msg_0c904ce000013WrZ9O6KEEiDYZ","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/workspace","root":"/"},"cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"modelID":"claude-opus-5","providerID":"anthropic","time":{"created":1790078930544,"completed":1790078931960},"sessionID":"ses_fake","error":{"name":"APIError","data":{"message":"API key is invalid.","statusCode":401,"isRetryable":false,"responseHeaders":{"cf-cache-status":"DYNAMIC","cf-ray":"a3f1328d4e1be195-MRS","connection":"keep-alive","content-length":"106","content-security-policy":"default-src 'none'; frame-ancestors 'none'","content-type":"application/json","date":"Tue, 22 Sep 2026 12:08:52 GMT","server":"cloudflare","x-robots-tag":"none"},"responseBody":"{\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"API key is invalid.\"},\"request_id\":null}","metadata":{"url":"https://api.anthropic.com/v1/messages"}}}}}`

// sseLineRaw builds one raw "data: <json>\n\n" line exactly like sseLine
// (fake_server_test.go), except properties is a LITERAL raw JSON string
// (captured real bytes, above) rather than a Go value this package's own
// struct types would marshal -- buildSSELine's own props any parameter
// accepts a json.RawMessage transparently (its MarshalJSON returns the
// bytes unchanged), so this broadcasts the captured bytes byte-for-byte,
// decoded on the receiving end only by the REAL production
// json.Unmarshal(env.Properties, &props) call in dispatchEvent (sse.go).
func sseLineRaw(t *testing.T, eventType string, rawProperties string) string {
	t.Helper()
	if !json.Valid([]byte(rawProperties)) {
		t.Fatalf("sseLineRaw: rawProperties is not valid JSON: %s", rawProperties)
	}
	line, err := buildSSELine(eventType, json.RawMessage(rawProperties))
	if err != nil {
		t.Fatalf("sseLineRaw: %v", err)
	}
	return line
}

// TestRealOrdering_ModelSurvivesLateArrivingAssistantError is E1's own
// pin: replays the REAL captured ordering above through the real
// Adapter/dispatchEvent path (fakeOpenCodeServer, exactly like this
// package's own other full-round-trip tests), and asserts the model
// survives into the finalized diagnostic even though, at the moment the
// FIRST session.idle fires and finalize runs, the assistant message's own
// error field has not arrived yet -- only lastAssistantModel (from the
// earlier, error-less message.updated) and ts.sessionError (from
// session.error) have.
//
// Mutation-verified: reverting modelForOutcome (turn.go) to its prior
// `if ts.lastAssistantError != nil { return ts.lastAssistantModel };
// return ""` form makes this test fail with
// Diagnostic.Model = <nil>, want "anthropic/claude-opus-5" -- while the
// REST of this package's own suite (including
// TestTransientRetry_PermanentAPIErrorNeverRetried, which plants the
// error directly ON the message.updated and so never exercises the real
// ordering) stays green.
func TestRealOrdering_ModelSurvivesLateArrivingAssistantError(t *testing.T) {
	f := newFakeOpenCodeServer(t)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-real-ordering-1", Gen: 1,
		Text: "say hello in one word",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, "ses_fake")

	// The REAL captured ordering, in the REAL captured order: the
	// model-bearing, error-less message.updated FIRST, then session.error,
	// then session.idle -- this is where the adapter must finalize, and
	// must do so with the model already in hand.
	f.broadcast(sseLineRaw(t, "message.updated", capturedAssistantMessageUpdatedModelOnlyJSON))
	f.broadcast(sseLineRaw(t, "session.error", capturedSessionErrorAPIErrorJSON))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	// unregisterTurn (adapter.go) is deferred inside StartTurn itself, so
	// by the time group.Wait() returns above, "ses_fake" is guaranteed
	// already removed from the turn registry -- broadcasting the real
	// trace's own LATE events now exercises the exact same "arrives too
	// late" path the real binary produced, and dispatchEvent's own
	// resolveEvent (sse.go) will find no registered turn and drop them,
	// deterministically, regardless of scheduling.
	f.broadcast(sseLineRaw(t, "message.updated", capturedAssistantMessageUpdatedWithErrorJSON))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	events := collector.snapshot()
	final := lastExecutionComplete(t, events)
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}

	if final.Diagnostic == nil {
		t.Fatal("execution_complete.Diagnostic = nil, want the allowlisted provider-failure record")
	}
	wantModel := "anthropic/claude-opus-5"
	if final.Diagnostic.Model == nil || *final.Diagnostic.Model != wantModel {
		got := "<nil>"
		if final.Diagnostic.Model != nil {
			got = *final.Diagnostic.Model
		}
		t.Errorf("Diagnostic.Model = %s, want %q -- the model WAS already known "+
			"(the earlier, error-less message.updated reported it) at the moment the "+
			"FIRST session.idle finalized this turn; it must not be discarded just "+
			"because the error arrived on session.error instead of directly on the "+
			"assistant message", got, wantModel)
	}
	if final.Diagnostic.UnionMember == nil || *final.Diagnostic.UnionMember != "APIError" {
		t.Errorf("Diagnostic.UnionMember = %v, want %q", final.Diagnostic.UnionMember, "APIError")
	}
	if final.Diagnostic.StatusCode == nil || *final.Diagnostic.StatusCode != 401 {
		t.Errorf("Diagnostic.StatusCode = %v, want 401", final.Diagnostic.StatusCode)
	}
	// E5: the real captured responseHeaders carry no "x-request-id"/
	// "request-id", only "cf-ray" -- see requestIDHeaderCandidates' own
	// doc comment (diagnostic.go) for why that was added to the
	// allowlist. Pin it here against the REAL captured bytes, not a
	// synthetic header map.
	wantRequestID := "a3f1328d4e1be195-MRS"
	if final.Diagnostic.ProviderRequestId == nil || *final.Diagnostic.ProviderRequestId != wantRequestID {
		got := "<nil>"
		if final.Diagnostic.ProviderRequestId != nil {
			got = *final.Diagnostic.ProviderRequestId
		}
		t.Errorf("Diagnostic.ProviderRequestId = %s, want %q (the real captured \"cf-ray\" header)", got, wantRequestID)
	}

	// The late, second wave of events must not have produced any
	// additional execution_complete -- exactly one turn, exactly one
	// terminal event, no matter how late OpenCode's own trailing
	// message.updated/session.idle pair arrives.
	if got := len(collector.snapshot()); got != len(events) {
		t.Errorf("collector observed %d events after the late trailing wave, want exactly %d (unchanged) -- "+
			"the late message.updated/session.idle must be silently dropped, not produce a second execution_complete",
			got, len(events))
	}
}
