package opencode

import (
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
)

// These tests exercise dispatchEvent/dispatchPart directly with synthetic
// SSE envelopes -- fast and deterministic, unlike eliciting the same
// scenarios from a real model (non-deterministic, slow, and in the role-
// filtering case below, dependent on the live provider producing exactly
// the right shape). A prior adversarial review reproduced the role-
// filtering bug live; these tests pin the fix without that dependency.

// newDispatchTestAdapter builds an Adapter whose background SSE loop
// points at an unreachable address -- irrelevant here, since these tests
// call dispatchEvent/dispatchPart directly rather than going through a
// real connection.
func newDispatchTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	a := New("http://127.0.0.1:1", testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)
	return a
}

func spyEventSink(t *testing.T) (ports.EventSink, func() []ports.AgentEvent) {
	t.Helper()
	var events []ports.AgentEvent
	return func(e ports.AgentEvent) {
			events = append(events, e)
		}, func() []ports.AgentEvent {
			return events
		}
}

// TestDispatchPart_UserMessageTextIsNeverTranslated is the regression test
// for the live-verified bug: message.part.updated fires for the USER's
// own message (its prompt echoed back as a "text" part), not just the
// assistant's. Without role filtering, that text part was (a) emitted as a
// spurious wire token event indistinguishable from real assistant output,
// and (b) marked sawText true before the assistant ever said anything,
// silently defeating §7's own "treat 'no output' as failure" quirk on the
// live SSE path.
func TestDispatchPart_UserMessageTextIsNeverTranslated(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-1", Gen: 1}
	ts := newTurnState(cmd, sink, "")
	a.registerTurn("ses_test", ts)

	// message.updated for the USER's own message -- must NOT be recorded
	// as an assistant message id.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.updated",
		Properties: []byte(`{"sessionID":"ses_test","info":{"id":"msg_user","role":"user"}}`),
	})

	// message.part.updated: a "text" part belonging to the USER's own
	// message (the user's own prompt, echoed back by OpenCode).
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"ses_test","part":{"id":"prt_user","messageID":"msg_user","type":"text","text":"the user's own prompt"}}`),
	})

	if got := events(); len(got) != 0 {
		t.Fatalf("events = %+v, want zero events for a user-owned text part", got)
	}
	if hasText, _ := ts.outcomeInputs(); hasText {
		t.Error("sawText = true after only a USER-owned text part; the assistant never said anything yet")
	}

	// Now the assistant's own message + text part -- THIS must translate.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.updated",
		Properties: []byte(`{"sessionID":"ses_test","info":{"id":"msg_asst","role":"assistant"}}`),
	})
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"ses_test","part":{"id":"prt_asst","messageID":"msg_asst","type":"text","text":"real reply"}}`),
	})

	got := events()
	if len(got) != 1 {
		t.Fatalf("events = %+v, want exactly 1 (the assistant's own text part)", got)
	}
	token, ok := got[0].Payload.(sandboxws.Token)
	if !ok {
		t.Fatalf("events[0].Payload = %T, want sandboxws.Token", got[0].Payload)
	}
	if token.Text != "real reply" {
		t.Errorf("token.Text = %q, want %q", token.Text, "real reply")
	}
	if hasText, _ := ts.outcomeInputs(); !hasText {
		t.Error("sawText = false after a real assistant text part, want true")
	}
}

// TestDispatchPart_TextFromUnknownMessageIDIsSkipped proves the fail-
// closed default: a text part whose MessageID this turnState has never
// seen a message.updated for at all (role unknown) is treated the same as
// "not assistant" -- skipped, not translated.
func TestDispatchPart_TextFromUnknownMessageIDIsSkipped(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-1", Gen: 1}
	ts := newTurnState(cmd, sink, "")
	a.registerTurn("ses_test", ts)

	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"ses_test","part":{"id":"prt_x","messageID":"msg_never_seen","type":"text","text":"???"}}`),
	})

	if got := events(); len(got) != 0 {
		t.Fatalf("events = %+v, want zero events for a text part with an unknown messageID", got)
	}
}

// TestDispatchPart_CompactionOverflowEmitsWarning proves §7's own "handle
// compaction events" quirk: an overflow compaction (the context window
// genuinely ran out of room mid-turn) is surfaced as a wire warning; an
// ordinary/auto compaction is not.
func TestDispatchPart_CompactionOverflowEmitsWarning(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-1", Gen: 1}
	ts := newTurnState(cmd, sink, "")
	a.registerTurn("ses_test", ts)

	// Ordinary/auto compaction, no overflow -- must NOT emit anything.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"ses_test","part":{"id":"prt_c1","messageID":"msg_asst","type":"compaction","auto":true,"overflow":false}}`),
	})
	if got := events(); len(got) != 0 {
		t.Fatalf("events = %+v, want zero events for a non-overflow compaction", got)
	}

	// Overflow compaction -- must emit a warning.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"ses_test","part":{"id":"prt_c2","messageID":"msg_asst","type":"compaction","auto":true,"overflow":true}}`),
	})
	got := events()
	if len(got) != 1 {
		t.Fatalf("events = %+v, want exactly 1 (the overflow warning)", got)
	}
	warning, ok := got[0].Payload.(sandboxws.Warning)
	if !ok {
		t.Fatalf("events[0].Payload = %T, want sandboxws.Warning", got[0].Payload)
	}
	if warning.Message == "" {
		t.Error("warning.Message is empty, want a non-empty description")
	}
	if got[0].Critical {
		t.Error("warning event reported Critical = true, want false (warning is not one of the 6 critical types)")
	}
}
