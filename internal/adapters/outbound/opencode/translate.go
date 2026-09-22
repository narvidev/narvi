package opencode

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// This file holds pure translation functions: one OpenCode part/message/
// outcome shape (types.go) in, one wire sandboxws.* event out. Every
// function stamps SessionId/Gen from cmd (Narvi's OWN session id and spawn
// generation, sandboxws.Prompt's own fields — NOT to be confused with
// OpenCode's own "ses_..." session id, which only ever appears as the wire
// conversationId, never as a wire event's sessionId/gen).
//
// messageId convention (§7's own "correlate by generated
// lexicographically-ascending message IDs" quirk): translateToken is the
// ONE exception that uses the part's own id ("prt_...") as the wire
// messageId — every other part-derived event (step_start, step_finish,
// tool_call, tool_result, and sub_task_start's own parentMessageId) uses
// the ENCLOSING OpenCode message's id ("msg_...", p.MessageID) instead,
// with the part's own id carried separately in the field that is each
// event's real per-instance correlator (StepId for step_start/step_finish,
// CallId for tool_call/tool_result). This split is deliberate: messageId
// answers "which conversational message did this happen under" (useful
// for grouping/ordering across an entire turn), while Step/CallId answers
// "which specific instance of a step/tool this is" (needed for the dedup
// logic in sse.go's dispatchTool) — collapsing both onto the same id would
// lose one of those two distinct pieces of information. Adapter-
// SYNTHESIZED events with no single originating OpenCode id
// (execution_complete, sub_task_start, sub_task_finish) mint a fresh
// uuid instead, via newEventID — mirroring
// internal/sandboxagent/wsbridge.Bridge's own newMessageID precedent for
// bridge-originated events.

func newEventID() string { return uuid.NewString() }

// subTaskIDPtr converts a subTaskID string into the wire shape every one of
// the six generated ...SubTaskId types (Token/ToolCall/ToolResult/
// StepStart/StepFinish/ExecutionComplete) share (§6.1/§7.1: OPTIONAL,
// nullable) -- an empty string (the turn's main lane, the overwhelming
// common case) becomes a nil pointer, which json.Marshal's own omitempty
// tag on that field then omits entirely; a non-empty subTaskID becomes a
// pointer to it. Every translate* call site below passes subTaskID == ""
// for main-lane events and a real OpenCode-derived id (today: a task
// tool's own spawned child session id, see maybeStartTaskSubtask in sse.go)
// for a sub-task's own nested events.
func subTaskIDPtr(subTaskID string) *string {
	if subTaskID == "" {
		return nil
	}
	return &subTaskID
}

func translateToken(cmd sandboxws.Prompt, p textPart, subTaskID string) sandboxws.Token {
	return sandboxws.Token{
		Type:      "token",
		MessageId: p.ID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		Text:      p.Text,
		SubTaskId: sandboxws.TokenSubTaskId(subTaskIDPtr(subTaskID)),
	}
}

func translateStepStart(cmd sandboxws.Prompt, p partEnvelope, subTaskID string) sandboxws.StepStart {
	return sandboxws.StepStart{
		Type:      "step_start",
		MessageId: p.MessageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		StepId:    p.ID,
		SubTaskId: sandboxws.StepStartSubTaskId(subTaskIDPtr(subTaskID)),
	}
}

// translateStepFinish maps OpenCode's own step-finish token breakdown onto
// wire StepFinish.Cost — THE schema §6.1/§9.1 dedicate their own explicit
// warning to: cost.tokens MUST be an object, never a bare number.
// StepFinishCostTokens.Cached is populated from tokens.cache.read
// (per this Step's own instructions: "cost.tokens = {input, output,
// cached: tokens.cache.read}"). Usd carries p.Cost through UNCHANGED --
// nil stays nil (§25.15: OpenCode's own "cost" key was genuinely absent,
// types.go's own stepFinishPart.Cost doc comment) rather than being
// coerced into a fabricated 0, so a control-plane consumer can finally
// tell "no cost arrived" apart from "this step cost $0".
func translateStepFinish(cmd sandboxws.Prompt, p stepFinishPart, subTaskID string) sandboxws.StepFinish {
	cached := int(p.Tokens.Cache.Read)
	return sandboxws.StepFinish{
		Type:      "step_finish",
		MessageId: p.MessageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		StepId:    p.ID,
		SubTaskId: sandboxws.StepFinishSubTaskId(subTaskIDPtr(subTaskID)),
		Cost: sandboxws.StepFinishCost{
			Tokens: sandboxws.StepFinishCostTokens{
				Input:  int(p.Tokens.Input),
				Output: int(p.Tokens.Output),
				Cached: &cached,
			},
			Usd: p.Cost,
		},
	}
}

// decodeToolInput unmarshals a tool part's own "input" object (always
// present, possibly "{}") into wire ToolCall's own freeform map shape.
func decodeToolInput(raw json.RawMessage) (sandboxws.ToolCallInput, error) {
	m, err := decodeToolObject(raw)
	if err != nil {
		return nil, err
	}
	return sandboxws.ToolCallInput(m), nil
}

func decodeToolObject(raw json.RawMessage) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return map[string]interface{}{}, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]interface{}{}
	}
	return m, nil
}

// toolInputEmpty reports whether a tool part's own "input" is the
// empty-object placeholder OpenCode sends while a call is still "pending"
// (§7's own "skip the empty-input pending state -- it has nothing real to
// report yet" instruction). Malformed input is treated as empty (nothing
// real to report yet either) rather than erroring here — translateToolCall
// surfaces a genuine decode error separately, for the case a NON-empty but
// malformed input sneaks through.
func toolInputEmpty(raw json.RawMessage) bool {
	m, err := decodeToolObject(raw)
	if err != nil {
		return true
	}
	return len(m) == 0
}

func translateToolCall(cmd sandboxws.Prompt, p toolPart, subTaskID string) (sandboxws.ToolCall, error) {
	input, err := decodeToolInput(p.State.Input)
	if err != nil {
		return sandboxws.ToolCall{}, err
	}
	return sandboxws.ToolCall{
		Type:      "tool_call",
		MessageId: p.MessageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		CallId:    p.CallID,
		ToolName:  p.Tool,
		Input:     input,
		SubTaskId: sandboxws.ToolCallSubTaskId(subTaskIDPtr(subTaskID)),
	}, nil
}

// translateToolResult wraps OpenCode's own STRING-shaped completed/error
// output (VERIFIED live: ToolStateCompleted.output and ToolStateError.error
// are both plain strings, never objects) into wire ToolResult.Output's own
// object shape ({"output": "..."} or {"error": "..."} respectively) — this
// adapter's own, documented convention for satisfying the wire contract's
// "freeform, tool-specific output" object requirement from a tool state
// that is itself just a string.
func translateToolResult(cmd sandboxws.Prompt, p toolPart, subTaskID string) sandboxws.ToolResult {
	isError := p.State.Status == "error"

	output := sandboxws.ToolResultOutput{}
	switch {
	case isError && p.State.Error != nil:
		output["error"] = *p.State.Error
	case p.State.Output != nil:
		output["output"] = *p.State.Output
	}

	return sandboxws.ToolResult{
		Type:      "tool_result",
		MessageId: p.MessageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		CallId:    p.CallID,
		Output:    output,
		IsError:   isError,
		SubTaskId: sandboxws.ToolResultSubTaskId(subTaskIDPtr(subTaskID)),
	}
}

// translateCompactionOverflow maps an "overflow" compaction (§7's own
// "handle compaction events" quirk — the context window genuinely ran out
// of room and had to be force-compacted mid-turn) onto a wire warning
// event -- there is no dedicated wire event for compaction itself, and
// warning is the closest existing slot for an operationally meaningful,
// non-fatal degradation signal. A non-overflow (ordinary/auto) compaction
// is NOT translated at all — see dispatchPart's "compaction" case
// (sse.go) for that deliberate asymmetry.
func translateCompactionOverflow(cmd sandboxws.Prompt, p compactionPart) sandboxws.Warning {
	return sandboxws.Warning{
		Type:      "warning",
		MessageId: p.MessageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		Message:   "opencode: context window overflow forced a mid-turn compaction",
	}
}

func translateSubTaskStart(cmd sandboxws.Prompt, p subtaskPart) sandboxws.SubTaskStart {
	return sandboxws.SubTaskStart{
		Type:            "sub_task_start",
		MessageId:       newEventID(),
		SessionId:       cmd.SessionId,
		Gen:             cmd.Gen,
		SubTaskId:       p.ID,
		Label:           p.Description,
		ParentMessageId: p.MessageID,
	}
}

// translateSubTaskStartFromTask builds sub_task_start from a "task" tool
// call's own toolPart, once maybeStartTaskSubtask (sse.go) has confirmed
// its state.metadata carries a real child session id -- the REAL,
// empirically-verified §7.1 sub-task announcement path this Step's own
// investigation found (see this package's own Adapter doc comment and
// taskToolMetadata's own doc comment, types.go). ParentMessageId is the
// enclosing "task" tool_call's own messageID (p.MessageID) -- exactly the
// "messageId of the main-lane tool_call event whose invocation spawned
// this sub-task" the wire schema's own SubTaskStart.parentMessageId
// documents. Label is best-effort from the task call's own "description"
// input field (taskInputDescription below) -- a human-readable label, not
// a correctness-bearing value. SubAgentType (§26.4/§7.1) is the
// SAME task call's own "subagent_type" input field (taskInputSubAgentType
// below) -- unlike Label, this IS the engine's own reliable dispatch
// parameter, and is what post-hoc sub-task corroboration
// (reviewverdict.CounterReviewCorroborated) keys off once this event lands
// in Postgres (sessionactor's unconditional raw-event persistence, no
// further wiring needed on that side).
func translateSubTaskStartFromTask(cmd sandboxws.Prompt, p toolPart, subTaskID string) sandboxws.SubTaskStart {
	return sandboxws.SubTaskStart{
		Type:            "sub_task_start",
		MessageId:       newEventID(),
		SessionId:       cmd.SessionId,
		Gen:             cmd.Gen,
		SubTaskId:       subTaskID,
		Label:           taskInputDescription(p.State.Input),
		ParentMessageId: p.MessageID,
		SubAgentType:    subAgentTypePtr(taskInputSubAgentType(p.State.Input)),
	}
}

// taskInputDescription best-effort extracts the "task" tool's own
// "description" input field — VERIFIED LIVE: {"description","prompt",
// "subagent_type"} — for use as sub_task_start's own human-readable Label.
// Falls back to a generic label if absent/malformed; never an error, since
// this only feeds a display label, not a correctness-bearing field.
func taskInputDescription(raw json.RawMessage) string {
	m, err := decodeToolObject(raw)
	if err != nil {
		return "task"
	}
	if desc, ok := m["description"].(string); ok && desc != "" {
		return desc
	}
	return "task"
}

// taskInputSubAgentType best-effort extracts the "task" tool's own
// "subagent_type" input field — the SAME VERIFIED-LIVE input shape
// taskInputDescription documents above ({"description","prompt",
// "subagent_type"}), just the third key rather than the first. This is
// OpenCode's own real dispatch parameter: the literal value the model
// passes to invoke a specific named custom agent (e.g.
// review.CounterReviewerAgentName's "counter-reviewer"), not freeform
// text — which is exactly why §26.4's post-hoc corroboration
// (reviewverdict.CounterReviewCorroborated) keys off this field rather
// than Label. Returns "" on absent/malformed input, never an error: this
// only feeds a display/correlation field on the wire event, the same
// "this only feeds a display label" reasoning taskInputDescription's own
// doc comment already gives — an empty SubAgentType simply means
// corroboration will never find a matching starts record for this
// sub-task, never a hard failure anywhere in this adapter.
func taskInputSubAgentType(raw json.RawMessage) string {
	m, err := decodeToolObject(raw)
	if err != nil {
		return ""
	}
	if agentType, ok := m["subagent_type"].(string); ok {
		return agentType
	}
	return ""
}

// subAgentTypePtr converts a subAgentType string into sub_task_start's own
// wire shape for SubAgentType (§6.1: OPTIONAL, additive) --
// mirrors subTaskIDPtr's own identical "empty string becomes a nil
// pointer, non-empty becomes a pointer to it" convention immediately
// above, so json.Marshal's own omitempty tag on that field omits it
// entirely whenever there was nothing real to report (translateSubTaskStart's
// legacy/unverified-live subtaskPart fallback path, which has no task-tool
// input to extract this from at all, and any malformed/absent
// "subagent_type" on the real task-tool path).
func subAgentTypePtr(subAgentType string) *string {
	if subAgentType == "" {
		return nil
	}
	return &subAgentType
}

func translateSubTaskFinish(cmd sandboxws.Prompt, subTaskID string, outcome sandboxws.ExecutionCompleteOutcome) sandboxws.SubTaskFinish {
	messageID := newEventID()
	return sandboxws.SubTaskFinish{
		Type:      "sub_task_finish",
		MessageId: messageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		AckId:     "sub_task_finish:" + messageID,
		SubTaskId: subTaskID,
		Outcome:   subTaskOutcome(outcome),
	}
}

// translateExecutionComplete's own subTaskID parameter exists purely for
// the six-event-types-gain-subTaskId schema uniformity (§6.1) — in
// practice this adapter only ever calls it with "" (empty): a turn's own
// execution_complete is, by definition, always the MAIN lane's own
// terminal event; a sub-task's own terminus is sub_task_finish, never a
// second execution_complete.
func translateExecutionComplete(cmd sandboxws.Prompt, out turnOutcome, subTaskID string) sandboxws.ExecutionComplete {
	messageID := newEventID()
	return sandboxws.ExecutionComplete{
		Type:       "execution_complete",
		MessageId:  messageID,
		SessionId:  cmd.SessionId,
		Gen:        cmd.Gen,
		AckId:      "execution_complete:" + messageID,
		Outcome:    out.Outcome,
		Reason:     sandboxws.ExecutionCompleteReason(out.Reason),
		SubTaskId:  sandboxws.ExecutionCompleteSubTaskId(subTaskIDPtr(subTaskID)),
		Diagnostic: diagnosticToWire(out.Diagnostic),
	}
}

// diagnosticToWire converts §7.3's own internal, allowlisted
// ProviderFailureDiagnostic (diagnostic.go) into the generated wire type
// -- the ONLY function in this package that ever constructs a
// sandboxws.ExecutionCompleteDiagnostic, so there is exactly one place
// that could ever populate the wire shape, and it reads named fields off
// ProviderFailureDiagnostic one at a time rather than copying anything
// wholesale. nil in, nil out: an absent diagnostic (no tagged-union error
// was ever observed for this failure) stays entirely absent on the wire
// (the field's own "omitempty" -- see ExecutionComplete.Diagnostic,
// sandboxws.go), never an empty object.
func diagnosticToWire(d *ProviderFailureDiagnostic) *sandboxws.ExecutionCompleteDiagnostic {
	if d == nil {
		return nil
	}
	return &sandboxws.ExecutionCompleteDiagnostic{
		Message:           emptyStringToNilPtr(d.Message),
		UnionMember:       emptyStringToNilPtr(d.UnionMember),
		StatusCode:        d.StatusCode,
		ProviderRequestId: emptyStringToNilPtr(d.ProviderRequestID),
		Model:             emptyStringToNilPtr(d.Model),
		RuntimeVersion:    emptyStringToNilPtr(d.RuntimeVersion),
		SandboxId:         emptyStringToNilPtr(d.SandboxID),
	}
}

// emptyStringToNilPtr mirrors subTaskIDPtr's own identical "empty string
// becomes a nil pointer" convention (above) -- every
// ProviderFailureDiagnostic string field is "" when OpenCode/this adapter
// never supplied a value, and the wire type's own "omitempty" only omits
// a nil pointer, never a non-nil pointer to an empty string.
func emptyStringToNilPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
