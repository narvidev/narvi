package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// sseDataPrefix is the one SSE field this adapter cares about — VERIFIED
// live: every line on the global /event stream is `data: <json>\n\n`. Any
// other SSE field (event:, id:, retry:, comments) or a blank separator
// line is silently ignored by processSSELine.
const sseDataPrefix = "data: "

// runEventLoop maintains ONE persistent connection to the global GET
// /event SSE stream for the adapter's whole lifetime, opened once here
// rather than per-turn (§7: "filter the global event stream by session
// id"), reconnecting on any failure until ctx is canceled
// (Adapter.Close). Every parsed event is dispatched to whichever turnState
// is currently registered for its OpenCode sessionID, if any (dispatchEvent
// below) — an event for a session with no registered turn is silently
// dropped; this can only happen for a session this adapter never started
// a turn for, or one whose turn already finalized and unregistered.
// StartTurn always registers a turnState BEFORE POSTing prompt_async (see
// adapter.go), so no live turn's own events can ever race this.
//
// Reconnect delay uses a.reconnectInterval (platform.Timeouts.
// OpenCodeSSEReconnectInterval) — a DEDICATED, deliberately short field
// (Finding 2), NOT a.sseInactivityTimeout: the two used to be the same
// value, which meant a dropped connection didn't even START reconnecting
// until waitForTurn's own per-turn fallback had already had its own full
// window to fire, making reconnection structurally unable to ever win that
// race. a.reconnectInterval is tuned independently, short enough that a
// genuine reconnect has a real chance to land well before
// a.sseInactivityTimeout elapses.
//
// Records every GENUINE disconnect via a.recordDisconnect, BEFORE the
// reconnect-delay wait below — Finding 1/2's own liveness disambiguation
// (Adapter.shouldFinalizeByFallback) depends on this reflecting the exact
// moment each outage STARTED, not when reconnection later succeeds.
// Deliberately guarded by the same ctx.Err() == nil check as the warning
// log: a connectAndConsume error that coincides with ctx already being
// canceled (Adapter.Close, or the process shutting down) is an ordinary,
// expected shutdown, not a genuine outage worth recording.
func (a *Adapter) runEventLoop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := a.connectAndConsume(ctx); err != nil && ctx.Err() == nil {
			a.recordDisconnect()
			slog.Warn("opencode: event stream connection lost, reconnecting", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.reconnectInterval):
		}
	}
}

// connectAndConsume performs one GET /event handshake and reads it line by
// line until the connection ends or ctx is canceled. Uses bufio.Reader.
// ReadString rather than bufio.Scanner specifically because Scanner's
// default token buffer has a fixed cap that a long tool-output-carrying
// SSE line could exceed; ReadString has no such limit.
func (a *Adapter) connectAndConsume(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/event", nil)
	if err != nil {
		return fmt.Errorf("opencode: build GET /event request: %w", err)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: GET /event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode: GET /event: http %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			a.processSSELine(line)
		}
		if err != nil {
			return err
		}
	}
}

// processSSELine parses one line of the SSE stream, dispatching it if it
// is a "data:" line carrying a well-formed sseEnvelope. A malformed line
// is logged and skipped — never torn down the whole connection over one
// bad frame: a single corrupt/unexpected SSE line from OpenCode must not
// take down every other in-flight turn sharing this one persistent
// connection.
//
// Deliberately does NOT touch any whole-connection-level liveness signal
// here (Finding 1/2's own disambiguation moved off of that approach: see
// runEventLoop's own recordDisconnect call and Adapter.shouldFinalizeByFallback's
// own doc comment for why a point-in-time "is the stream fresh right now"
// signal — which a bare reconnect handshake line would immediately refresh
// — cannot distinguish a turn whose own outcome is actually ready from one
// that merely shares a just-reconnected connection).
func (a *Adapter) processSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}

	data, ok := strings.CutPrefix(line, sseDataPrefix)
	if !ok {
		return
	}

	var env sseEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		slog.Warn("opencode: malformed SSE event, skipping", "error", err)
		return
	}

	a.dispatchEvent(env)
}

// dispatchEvent routes one parsed SSE envelope by its own "type" field —
// the concrete shapes and verification status of each are documented on
// their own properties struct in types.go. Every case below resolves
// props.SessionID via a.resolveEvent, NOT a.lookupTurn directly, so an
// event whose sessionID belongs to a registered SUB-TASK's own child
// OpenCode session (see registerSubtaskSession/maybeStartTaskSubtask,
// this file, and Adapter's own package doc comment for the real correlator
// this implements) is routed to the same enclosing turnState instead of
// being silently dropped as "no registered turn for this session".
func (a *Adapter) dispatchEvent(env sseEnvelope) {
	switch env.Type {
	case "server.connected":
		a.connectOnce.Do(func() { close(a.connectedCh) })

	case "message.updated":
		var props messageUpdatedProps
		if err := json.Unmarshal(env.Properties, &props); err != nil {
			slog.Warn("opencode: malformed message.updated event, skipping", "error", err)
			return
		}
		ts, subTaskID, ok := a.resolveEvent(props.SessionID)
		if !ok {
			return
		}
		ts.touch()
		if ts.isCompacting() {
			// §7.2's own VERIFIED LIVE finding: a synchronous POST
			// /summarize call still produces its own message.updated
			// events (a NEW assistant message with info.mode=="compaction")
			// for the SAME sessionID while it's in flight -- every message
			// arriving during this window is guaranteed compaction-
			// internal, so it must not pollute assistantMessageIDs/
			// lastAssistantError for the turn's own real outcome (see
			// turnState's own compacting field comment, turn.go, and
			// forceCompaction's own doc comment, compact.go, for the full
			// captured event sequence this guards against).
			return
		}
		if props.Info.Role == "assistant" {
			// markAssistantMessageID is shared across the main lane and
			// every sub-task lane alike -- dispatchPart's own
			// isAssistantMessage gate (keyed by messageID, globally
			// unique across OpenCode) needs a sub-task's own assistant
			// messages marked too, or its own text parts would never
			// translate. lastAssistantError, in contrast, feeds the
			// ENCLOSING turn's own deriveOutcome (below) -- a sub-task's
			// own internal error must NOT leak into the main turn's own
			// outcome (see maybeFinishTaskSubtask for the real signal
			// that governs a sub-task's OWN outcome instead), so that is
			// only set for the main lane.
			ts.markAssistantMessageID(props.Info.ID)
			if subTaskID == "" {
				ts.setLastAssistantError(props.Info.Error)
			}
		}

	case "message.part.updated":
		var props messagePartUpdatedProps
		if err := json.Unmarshal(env.Properties, &props); err != nil {
			slog.Warn("opencode: malformed message.part.updated event, skipping", "error", err)
			return
		}
		ts, subTaskID, ok := a.resolveEvent(props.SessionID)
		if !ok {
			return
		}
		ts.touch()
		if ts.isCompacting() {
			// Mirrors message.updated's own guard above -- a compaction's
			// own message.part.updated wave (step-start, then "text"
			// carrying the real cumulative summary text, then step-finish)
			// must not be translated/counted toward this turn's own real
			// output (dispatchPart's own "text" case would otherwise call
			// ts.markSawText, polluting hasText for whatever outcome
			// eventually gets computed).
			//
			// Cost is the one deliberate exception to that suppression
			// (D1 fix): forceCompaction's own POST
			// /summarize call (compact.go) is a real, synchronous,
			// billed call on this SAME session, and it emits its own
			// genuine step-finish with a real, non-zero p.Cost as part
			// of this exact wave. Silently dropping it here would mean
			// ts.spentUSD (§26.7/§7.1) under-reports true spend by the
			// compaction call's own cost -- backwards for a
			// fail-toward-caution cost gate, where GET
			// /review-cost-budget must never report shouldSkip:false
			// while real spend has already crossed the ceiling. Peek
			// the part's own type discriminator the same way
			// dispatchPart does, and if it's a step-finish, add its
			// cost directly -- WITHOUT calling the full dispatchPart,
			// so hasText/wire-event suppression for every other part
			// type (and step-finish's own wire translation) is
			// unchanged.
			var partEnv partEnvelope
			if err := json.Unmarshal(props.Part, &partEnv); err != nil {
				slog.Warn("opencode: malformed part envelope during compaction, skipping cost check", "error", err)
				return
			}
			if partEnv.Type == "step-finish" {
				var p stepFinishPart
				if err := json.Unmarshal(props.Part, &p); err != nil {
					slog.Warn("opencode: malformed step-finish part during compaction, skipping cost", "error", err)
					return
				}
				ts.addCost(p.Cost)
			}
			return
		}
		a.dispatchPart(ts, subTaskID, props.Part)

	case "session.idle":
		var props sessionIdleProps
		if err := json.Unmarshal(env.Properties, &props); err != nil {
			slog.Warn("opencode: malformed session.idle event, skipping", "error", err)
			return
		}
		ts, subTaskID, ok := a.resolveEvent(props.SessionID)
		if !ok {
			return
		}
		ts.touch()
		if subTaskID != "" {
			// A sub-task's own child session going idle is NOT this
			// turn's own terminal signal -- see maybeFinishTaskSubtask
			// (this file) for the real, per-sub-task completion signal
			// this adapter uses instead (the enclosing "task" tool
			// call's own "completed"/"error" transition, delivered on
			// the MAIN lane). touch() above already recorded this turn
			// as still alive.
			return
		}
		if ts.isCompacting() {
			// §7.2's own VERIFIED LIVE finding, the load-bearing guard: a
			// synchronous POST /summarize call's OWN internal agentic
			// sub-turn genuinely fires session.idle for this SAME
			// sessionID while the /summarize HTTP call is still in
			// flight (confirmed by capturing real GET /event traffic
			// during a live /summarize call). Without this guard, THAT
			// session.idle would reach the exact same finalize call
			// below and finalize using whatever ts.errorForOutcome()
			// still holds at that moment -- the ORIGINAL
			// ContextOverflowError, since nothing has cleared it yet --
			// finalizing the whole turn as failed BEFORE the retry
			// prompt is even sent. touch() above already recorded this
			// turn as still alive.
			return
		}
		err := ts.errorForOutcome()
		hasText, hasToolCall := ts.outcomeInputs()
		outcome := deriveOutcome(err, hasText, hasToolCall)
		// §7.3: built here, not inside deriveOutcome itself --
		// see turnOutcome.Diagnostic's own doc comment (outcome.go) for
		// why deriveOutcome stays pure/Adapter-free.
		outcome.Diagnostic = a.buildProviderFailureDiagnostic(err, ts)
		// "now": nothing intervenes between ts.touch()/the isCompacting
		// guard above and the reads that produced err/hasText/hasToolCall
		// (all synchronous, no I/O, same goroutine) -- see
		// finalizeOrRecoverFromOverflow's own doc comment (adapter.go) for
		// what this snapshotTime means to turnState.resolveOverflowAction.
		a.finalizeOrRecoverFromOverflow(props.SessionID, ts, outcome, err, time.Now())

	case "session.error":
		var props sessionErrorProps
		if err := json.Unmarshal(env.Properties, &props); err != nil {
			slog.Warn("opencode: malformed session.error event, skipping", "error", err)
			return
		}
		ts, subTaskID, ok := a.resolveEvent(props.SessionID)
		if !ok {
			return
		}
		ts.touch()
		if subTaskID != "" {
			// Mirrors session.idle above -- a sub-task's own internal
			// error must not set the ENCLOSING turn's own sessionError;
			// the task tool call's own "error" status is the real signal
			// (maybeFinishTaskSubtask).
			return
		}
		if ts.isCompacting() {
			// Mirrors message.updated/session.idle above -- a
			// compaction-internal error must not corrupt ts.sessionError
			// for the turn's own real outcome (§7.2's own VERIFIED LIVE
			// finding: see turnState's own compacting field comment,
			// turn.go).
			return
		}
		ts.setSessionError(props.Error)

	default:
		// server.heartbeat, session.updated, session.status, session.diff,
		// message.part.delta (a redundant delta view of the SAME text
		// message.part.updated already reports cumulatively -- translating
		// it too would double-report the same content), catalog.updated,
		// bare busy/idle, plugin.*, integration.*, reference.updated, and
		// anything else a future OpenCode version adds: silently ignored,
		// exactly matching §7's own "no wire event exists for it, this is
		// normal/expected" bucket -- OpenCode's own event surface is far
		// wider than Narvi's own wire contract has slots for.
	}
}

// dispatchPart decodes one message.part.updated's own "part" object by its
// "type" discriminator (partEnvelope) and translates it, when a wire event
// slot exists for that part type. subTaskID is "" for the turn's own main
// lane, or the enclosing sub-task's own subTaskId (see
// Adapter.resolveEvent) — threaded into every translate* call for the six
// event types Finding 1's own schema change gave a subTaskId field to.
func (a *Adapter) dispatchPart(ts *turnState, subTaskID string, raw json.RawMessage) {
	var env partEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		slog.Warn("opencode: malformed part envelope, skipping", "error", err)
		return
	}

	switch env.Type {
	case "text":
		var p textPart
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Warn("opencode: malformed text part, skipping", "error", err)
			return
		}
		// VERIFIED LIVE: message.part.updated fires for the USER's own
		// message too (its prompt echoed back as a "text" part), not just
		// the assistant's reply. Only an assistant-owned text part may be
		// translated/counted -- see assistantMessageIDs' own field comment
		// (turn.go) for the full story of the bug this check fixes.
		if !ts.isAssistantMessage(p.MessageID) {
			return
		}
		if p.Text != "" {
			// Shared across the main lane and every sub-task lane alike:
			// a turn that delegated its whole real output to a sub-task
			// genuinely did produce output, so it must count toward
			// deriveOutcome's own "treat no output as failure" check
			// (§7) exactly as if the main lane had produced it directly.
			ts.markSawText()
		}
		ts.emit(translateToken(ts.cmd, p, subTaskID))

	case "step-start":
		ts.emit(translateStepStart(ts.cmd, env, subTaskID))

	case "step-finish":
		var p stepFinishPart
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Warn("opencode: malformed step-finish part, skipping", "error", err)
			return
		}
		// §26.7/§7.1: sum p.Cost into this turn's own running
		// total -- unconditionally, regardless of subTaskID, since this
		// SAME turnState is shared by the main lane and every sub-task
		// alike (see turnState.spentUSD's own field doc comment, turn.go,
		// for the full reasoning).
		ts.addCost(p.Cost)
		ts.emit(translateStepFinish(ts.cmd, p, subTaskID))

	case "tool":
		var p toolPart
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Warn("opencode: malformed tool part, skipping", "error", err)
			return
		}
		a.dispatchTool(ts, p, subTaskID)

	case "subtask":
		var p subtaskPart
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Warn("opencode: malformed subtask part, skipping", "error", err)
			return
		}
		a.dispatchSubtaskStart(ts, p)

	case "compaction":
		// §7's own "handle compaction events" quirk. An ordinary/automatic
		// compaction (Auto true, Overflow false) is routine housekeeping —
		// no wire-event slot exists for it and none is warranted. An
		// OVERFLOW compaction (the context window genuinely ran out of
		// room mid-turn) is an operationally meaningful degradation this
		// adapter surfaces as a wire warning (translateCompactionOverflow)
		// — the closest existing slot, since the wire contract has no
		// dedicated compaction event. "warning" is one of the session/
		// connection-lifecycle event types §6.1 explicitly excludes from
		// ever carrying subTaskId, so no subTaskID is threaded here even
		// when this fires for a sub-task's own compaction.
		var p compactionPart
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Warn("opencode: malformed compaction part, skipping", "error", err)
			return
		}
		if p.Overflow {
			ts.emit(translateCompactionOverflow(ts.cmd, p))
		}

	default:
		// reasoning, patch, snapshot, file, agent, retry, and any future
		// part type: no wire-event slot exists for these (§7); silently
		// skipped, not an error.
	}
}

// dispatchTool implements §7's own "dedupe tool states by sid:callID:
// status" quirk — the single most important correctness property in this
// Step: emit tool_call the FIRST time a callID's own input is seen
// non-empty (skipping the empty-input "pending" state), and tool_result
// exactly ONCE, on the first "completed" or "error" status for that
// callID, ignoring every intermediate "running" update in between.
// subTaskID (see dispatchPart) is threaded onto the emitted tool_call/
// tool_result. When p.Tool == "task", this ALSO drives the real §7.1
// sub-task lifecycle (maybeStartTaskSubtask/maybeFinishTaskSubtask below)
// — see Adapter's own package doc comment for the full writeup of the
// correlator this implements.
func (a *Adapter) dispatchTool(ts *turnState, p toolPart, subTaskID string) {
	switch p.State.Status {
	case "pending":
		// Nothing real to report yet (§7: empty input).

	case "running":
		if p.Tool == "task" {
			a.maybeStartTaskSubtask(ts, p)
		}
		if ts.alreadySentCall(p.CallID) || toolInputEmpty(p.State.Input) {
			return
		}
		a.emitToolCall(ts, p, subTaskID)

	case "completed", "error":
		if p.Tool == "task" {
			// Covers the symmetric "resolved so fast we never saw
			// running" case for the sub-task's own lifecycle too -- a
			// call that jumps straight from "pending" to
			// "completed"/"error" still owes a sub_task_start (idempotent
			// regardless: see ts.subtaskAlreadyStarted's own doc comment)
			// BEFORE its own enclosing tool_call, matching the "running"
			// branch's own ordering above.
			a.maybeStartTaskSubtask(ts, p)
		}
		if !ts.alreadySentCall(p.CallID) {
			// This call resolved so fast we never saw a non-empty
			// "running" state first -- still owe a tool_call (the wire
			// contract pairs call+result) using this same final state's
			// own input.
			a.emitToolCall(ts, p, subTaskID)
		}
		if ts.alreadySentResult(p.CallID) {
			return
		}
		ts.markResultSent(p.CallID)
		if p.Tool == "task" {
			a.maybeFinishTaskSubtask(ts, p)
		}
		ts.emit(translateToolResult(ts.cmd, p, subTaskID))

	default:
		slog.Warn("opencode: unrecognized tool state status, skipping",
			"status", p.State.Status, "callID", p.CallID)
	}
}

func (a *Adapter) emitToolCall(ts *turnState, p toolPart, subTaskID string) {
	event, err := translateToolCall(ts.cmd, p, subTaskID)
	if err != nil {
		slog.Warn("opencode: malformed tool input, skipping tool_call", "callID", p.CallID, "error", err)
		return
	}
	ts.markCallSent(p.CallID)
	ts.markSawToolCall()
	ts.emit(event)
}

// dispatchSubtaskStart implements the LEGACY, still-unverified-live
// "subtask" part path — see types.go's own subtaskPart doc comment and
// Adapter's own package doc comment for why maybeStartTaskSubtask/
// maybeFinishTaskSubtask below are now the PRIMARY, empirically-verified
// §7.1 sub-task path. Kept as an extra fallback in case some other
// OpenCode-internal path emits this part type; harmless to keep alongside
// the task-tool path since the two use disjoint id spaces (a subtaskPart's
// own "prt_..." id here vs. a task tool call's own spawned "ses_..." child
// session id there).
func (a *Adapter) dispatchSubtaskStart(ts *turnState, p subtaskPart) {
	if ts.subtaskAlreadyStarted(p.ID) {
		return
	}
	ts.markSubtaskStarted(p.ID)
	ts.emit(translateSubTaskStart(ts.cmd, p))
}

// maybeStartTaskSubtask implements the "sub_task_start" half of §7.1's
// sub-task fan-out for OpenCode's own "task" tool — the REAL,
// empirically-verified correlator this Step's own live investigation found
// (see Adapter's own package doc comment for the full writeup): the task
// tool's own toolPart.State carries a "metadata" object once status
// reaches "running" (taskToolMetadata, types.go), naming the freshly-
// spawned sub-agent's own distinct OpenCode session id — used directly as
// this sub-task's own subTaskId (the same "derived from whatever
// correlator the engine itself exposes" convention SubTaskStart's own
// wire-schema doc comment already establishes). Idempotent via
// ts.subtaskAlreadyStarted/markSubtaskStarted: repeated "running"
// deliveries for the same call (or a "completed"/"error" one, for a call
// that resolved before any "running" update ever arrived — see
// dispatchTool's own "completed","error" branch) only start it once.
// Registers the child session's own routing entry (registerSubtaskSession)
// so its own subsequent inner events (dispatchEvent, via resolveEvent) get
// tagged with this same subTaskId -- ONLY once ts.emit itself confirms this
// turn hasn't already finalized on a concurrent goroutine (ts.emit's own
// return value, gating this exact race -- see its own doc comment): a
// "running" delivery processed after a concurrent finalize has already
// drained this turn's open sub-tasks must not leave behind a routing entry
// finalize will never revisit, on an Adapter-lifetime map nothing else
// would ever clean up.
func (a *Adapter) maybeStartTaskSubtask(ts *turnState, p toolPart) {
	meta, ok := decodeTaskMetadata(p.State.Metadata)
	if !ok || ts.subtaskAlreadyStarted(meta.SessionID) {
		return
	}
	ts.markSubtaskStarted(meta.SessionID)
	if !ts.emit(translateSubTaskStartFromTask(ts.cmd, p, meta.SessionID)) {
		return
	}
	a.registerSubtaskSession(meta.SessionID, ts, meta.SessionID)
}

// maybeFinishTaskSubtask implements the "sub_task_finish" half — called
// from dispatchTool's own "completed"/"error" branch, gated by the SAME
// per-callID alreadySentResult/markResultSent dedup ordinary tool_result
// already uses (§7's "dedupe tool states by sid:callID:status" quirk), so
// a repeated terminal delivery for the same call can never double-close
// the same sub-task. This is a genuinely MORE PRECISE signal than the
// enclosing turn's own eventual finalize (still the fallback for a
// sub-task whose own task tool call never reaches a terminal status
// before the turn itself ends — see Adapter.finalize's own
// drainOpenSubtasks call): "error" maps to SubTaskFinish's own "failed"
// outcome, anything else ("completed") to "completed" — there is no
// "cancelled" outcome from here, since a genuinely cancelled TURN closes
// its own still-open sub-tasks via the turn-level finalize path instead,
// and a task tool call reaching "error"/"completed" on its own is never
// itself a cancellation. No-ops if this subTaskId was never (or is no
// longer) open — e.g. metadata absent/malformed, or already closed.
func (a *Adapter) maybeFinishTaskSubtask(ts *turnState, p toolPart) {
	meta, ok := decodeTaskMetadata(p.State.Metadata)
	if !ok || !ts.subtaskAlreadyStarted(meta.SessionID) {
		return
	}
	outcome := sandboxws.ExecutionCompleteOutcomeCompleted
	if p.State.Status == "error" {
		outcome = sandboxws.ExecutionCompleteOutcomeFailed
	}
	ts.markSubtaskFinished(meta.SessionID)
	a.unregisterSubtaskSession(meta.SessionID)
	ts.emit(translateSubTaskFinish(ts.cmd, meta.SessionID, outcome))
}

// decodeTaskMetadata best-effort decodes a task toolPart's own
// State.Metadata into taskToolMetadata, reporting ok=false for absent,
// malformed, or session-id-less metadata (a NON-task tool's own
// differently-shaped, or entirely absent, metadata included) — never an
// error path, since metadata is a schema-generic `object` field this
// adapter has no business rejecting a whole event over.
func decodeTaskMetadata(raw json.RawMessage) (taskToolMetadata, bool) {
	if len(raw) == 0 {
		return taskToolMetadata{}, false
	}
	var meta taskToolMetadata
	if err := json.Unmarshal(raw, &meta); err != nil || meta.SessionID == "" {
		return taskToolMetadata{}, false
	}
	return meta, true
}
