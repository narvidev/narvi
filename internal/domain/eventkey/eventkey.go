// Package eventkey derives the key a sandbox event is stored under in the
// session event log (technical plan §6.1): events.message_id, unique per
// session (migrations/000019), on which a resent event dedupes, first wins.
//
// The key is the event's wire messageId, except for three types. The
// runtime adapter gives every event it derives from a part of an assistant
// message that message's id (translateStepStart, translateStepFinish,
// translateToolCall and translateToolResult, internal/adapters/outbound/
// opencode/translate.go), and the message's `step_start` comes first, so
// under the wire messageId alone each `tool_call`, `tool_result` and
// `step_finish` of a real turn deduped onto its message's `step_start` row
// and was never stored. Each of those three carries the id of its own
// instance -- `callId` for a tool call and its result, `stepId` for a
// step's end -- so each is stored under its message's id, its type and that
// correlator: one row per call, per result and per step, and a resend,
// which carries the same correlator, still dedupes.
//
// Every other type keeps its wire messageId: re-keying a type already
// stored would make a sandbox's replay after the deploy store each such
// event a second time, under its new key, at the tail of the log. These
// three have no stored rows to collide with, since none was ever stored. A
// `token` frame's later keys are the session actor's own
// (sessionactor/tokenframe.go); its first frame keeps its wire messageId.
//
// Readers never see the key: they read the payload, whose messageId is
// untouched. Nothing here does I/O.
package eventkey

// The three event types stored under a key derived from their correlator.
const (
	TypeToolCall   = "tool_call"
	TypeToolResult = "tool_result"
	TypeStepFinish = "step_finish"
)

// KeyedByCorrelator reports whether eventType is one of the three types
// StorageKey derives a key for: `tool_call`, `tool_result` and
// `step_finish`. The session actor stores these only while their turn is
// live (sessionactor/toolevent.go).
func KeyedByCorrelator(eventType string) bool {
	switch eventType {
	case TypeToolCall, TypeToolResult, TypeStepFinish:
		return true
	default:
		return false
	}
}

// StorageKey returns the events.message_id an event of eventType is stored
// under, given its wire messageId and the correlators its envelope carries:
// callID (`callId`, on a tool call and its result) and stepID (`stepId`, on
// a step's start and end).
//
//   - `tool_call`: messageID + "#tool_call:" + callID
//   - `tool_result`: messageID + "#tool_result:" + callID
//   - `step_finish`: messageID + "#step_finish:" + stepID
//
// One of those three whose correlator is empty -- an agent that left it out,
// which the wire schema does not allow -- keeps the bare messageID, as
// every event did before. Every other type keeps messageID, whatever
// correlator it carries.
func StorageKey(eventType, messageID, callID, stepID string) string {
	var correlator string
	switch eventType {
	case TypeToolCall, TypeToolResult:
		correlator = callID
	case TypeStepFinish:
		correlator = stepID
	default:
		return messageID
	}
	if correlator == "" {
		return messageID
	}
	return messageID + "#" + eventType + ":" + correlator
}
