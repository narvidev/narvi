// timelineModel.ts -- turns an id-ordered EventEnvelope[] (always
// SessionStream.getSnapshot().events, web/src/ws/sessionStream.ts) into
// the render model Timeline.tsx draws: turns, each holding steps, each
// holding tool calls, each optionally holding sub-task lanes (§7.1) --
// decision 4 ("A timeline of typed events... the UI merely renders the
// generated contracts") and §12.2 item 1's own "sub-task fan-out, as
// collapsed labeled sub-lanes nested under the spawning tool call, never
// interleaved, with a live count while active and a distinct color/icon
// for a failed or cancelled sub-lane vs. completed."
//
// # Why "collapsed" sub-lanes carry no tool-call content of their own
//
// §12.2 item 1 says "COLLAPSED labeled sub-lanes" -- a sub-task's own
// label + live/completed/failed/cancelled state, not a full nested
// transcript. This module therefore does not track a sub-task's own
// tool_call/tool_result/token events at all: every event that carries a
// non-null subTaskId is routed away from the main lane the instant it is
// classified, and dropped once classified (never attached to the
// sub-task, never rendered) -- §7.1's own "the model is flat, a sub-task
// cannot itself spawn a further-nested sub-task" is exactly what makes
// this safe: there is no deeper structure being lost by not tracking it.
//
// # Turn/step correlation, and what this module does NOT trust
//
// A turn boundary is inferred from execution_complete (the turn's own
// terminal event, §3.3) -- everything from the end of the PREVIOUS
// execution_complete (or session start) up to and including the next one
// is one turn; a trailing turn with no execution_complete yet is the
// live, in-progress one (Timeline.tsx's own ".stream" card). A tool_call
// carries no stepId of its own (only step_start/step_finish do) -- this
// module attributes a tool_call/tool_result/token to whichever step is
// currently OPEN (the most recent step_start not yet closed by a
// matching step_finish) within the current turn, auto-opening an
// implicit step if one somehow arrives with none open (a producer that
// skips step_start is not this module's contract to enforce -- dropping
// the event would be a worse failure than showing it under a synthetic
// step).
//
// A text part is the exception, once it has a row: a `token` whose
// messageId (the part id) already has an entry in the current turn updates
// THAT entry, in whatever step it sits, rather than landing in the open
// step. A part is placed where its first frame appeared. Its later frames
// are normally right behind it, but not always: the turn still running
// when the control plane is deployed onto per-frame storage gets its
// parts' later frames back at the tail of the turn, after the steps that
// followed them (internal/app/sessionactor/tokenframe.go, "The turn
// running at deploy"). Placed by the open step, that text would show under
// the wrong step, or a synthesized one, and its own step would stay blank.
//
// sub_task_start.parentMessageId correlates against a tool_call's own
// `messageId` (NOT `callId` -- callId is what tool_result correlates
// against instead, a distinct id on the very same event, §6.1). A
// sub-task whose parent hasn't been seen yet (out-of-order delivery) is
// buffered and attached retroactively the moment its parent tool_call
// does arrive; one that NEVER finds a parent (a malformed/hostile
// producer) surfaces in `orphanedSubTasks` rather than being silently
// dropped -- see this Step's own PR description for why silent data loss
// is treated as a worse failure mode than an honest "unattributed"
// bucket.
import type { EventEnvelope } from '../ws/types'
import {
  asArtifact,
  asBootProgress,
  asExecutionComplete,
  asReady,
  asSandboxError,
  asSessionTitle,
  asStepFinish,
  asStepStart,
  asSubTaskFinish,
  asSubTaskStart,
  asToken,
  asToolCall,
  asToolResult,
  asWarning,
} from './eventPayloads'

export interface SubTaskNode {
  subTaskId: string
  label: string
  subAgentType: string | null
  startedAt: string | null
  finishedAt: string | null
  status: 'running' | 'completed' | 'failed' | 'cancelled'
}

export interface ToolCallNode {
  callId: string
  messageId: string
  toolName: string
  input: unknown
  startedAt: string
  result: { output: unknown; isError: boolean; finishedAt: string } | null
  subTasks: SubTaskNode[]
}

export interface TokenStream {
  messageId: string
  text: string
}

export interface StepCost {
  inputTokens: number
  outputTokens: number
  cachedTokens: number | null
  usd: number | null
}

export interface StepNode {
  stepId: string
  toolCalls: ToolCallNode[]
  tokens: TokenStream[]
  cost: StepCost | null
  startedAt: string | null
  finishedAt: string | null
  live: boolean
}

export interface TurnOutcome {
  outcome: 'completed' | 'failed' | 'cancelled'
  reason: string | null
}

export interface TurnNode {
  /** The id of the first event folded into this turn -- a stable React key (event ids are strictly monotonic, never reused, eventLog.ts's own top comment). */
  firstEventId: number
  steps: StepNode[]
  outcome: TurnOutcome | null
  /** True while this turn has no outcome yet AND is the last turn in the log -- the ".stream" card. */
  live: boolean
}

export interface TimelineNotice {
  id: number
  createdAt: string
  message: string
}

export interface TimelineModel {
  turns: TurnNode[]
  warnings: TimelineNotice[]
  errors: (TimelineNotice & { fatal: boolean })[]
  latestTitle: string | null
  orphanedSubTasks: SubTaskNode[]
  /** The most recent named boot_progress phase seen, or null once a 'ready' event has been seen (or none was ever reported) -- Timeline.tsx's own "session still booting" empty-state signal. */
  latestBootPhase: string | null
  /** True once a 'ready' event (sandbox finished booting) has been seen anywhere in the log. */
  sawReady: boolean
}

function subTaskStatusFromOutcome(outcome: 'completed' | 'failed' | 'cancelled'): SubTaskNode['status'] {
  return outcome
}

export function buildTimelineModel(events: readonly EventEnvelope[]): TimelineModel {
  const turns: TurnNode[] = []
  const warnings: TimelineNotice[] = []
  const errors: (TimelineNotice & { fatal: boolean })[] = []
  const orphanedSubTasks: SubTaskNode[] = []
  let latestTitle: string | null = null
  let latestBootPhase: string | null = null
  let sawReady = false

  let currentTurn: TurnNode | null = null
  // Per-turn correlation state -- reset every time a new turn opens
  // (this module's own top comment: turn-scoped, never bled across a
  // turn boundary even if a producer somehow reused an id).
  let toolCallsByMessageId = new Map<string, ToolCallNode>()
  let toolCallsByCallId = new Map<string, ToolCallNode>()
  let stepsByStepId = new Map<string, StepNode>()
  let pendingSubTasksByParent = new Map<string, SubTaskNode[]>()
  let subTasksById = new Map<string, SubTaskNode>()
  let tokensByMessageId = new Map<string, TokenStream>()
  let openStepId: string | null = null

  function resetTurnState(): void {
    toolCallsByMessageId = new Map()
    toolCallsByCallId = new Map()
    stepsByStepId = new Map()
    pendingSubTasksByParent = new Map()
    subTasksById = new Map()
    tokensByMessageId = new Map()
    openStepId = null
  }

  function ensureTurn(firstEventId: number): TurnNode {
    if (currentTurn === null) {
      currentTurn = { firstEventId, steps: [], outcome: null, live: true }
      turns.push(currentTurn)
      resetTurnState()
    }
    return currentTurn
  }

  function ensureOpenStep(turn: TurnNode, event: EventEnvelope): StepNode {
    if (openStepId !== null) {
      const existing = stepsByStepId.get(openStepId)
      if (existing) return existing
    }
    // No open step (either none ever started, or the tracked one is
    // missing from the map, which cannot really happen but is guarded
    // anyway) -- synthesize one so this event is never silently dropped.
    const implicit: StepNode = {
      stepId: `implicit:${event.id}`,
      toolCalls: [],
      tokens: [],
      cost: null,
      startedAt: event.createdAt,
      finishedAt: null,
      live: true,
    }
    stepsByStepId.set(implicit.stepId, implicit)
    turn.steps.push(implicit)
    openStepId = implicit.stepId
    return implicit
  }

  function attachSubTask(node: SubTaskNode, parentMessageId: string): void {
    const parent = toolCallsByMessageId.get(parentMessageId)
    if (parent) {
      parent.subTasks.push(node)
      return
    }
    const pending = pendingSubTasksByParent.get(parentMessageId) ?? []
    pending.push(node)
    pendingSubTasksByParent.set(parentMessageId, pending)
  }

  for (const event of events) {
    // Session/connection-lifecycle events never carry a subTaskId and
    // are never turn-scoped (§6.1's own "session/connection-lifecycle
    // events ... never populate it" list) -- handled first, before any
    // ensureTurn call, so a session_title/warning/error never spuriously
    // opens an empty turn.
    const title = asSessionTitle(event)
    if (title !== null) {
      latestTitle = title.title
      continue
    }
    const warning = asWarning(event)
    if (warning !== null) {
      warnings.push({ id: event.id, createdAt: event.createdAt, message: warning.message })
      continue
    }
    const sandboxError = asSandboxError(event)
    if (sandboxError !== null) {
      errors.push({ id: event.id, createdAt: event.createdAt, message: sandboxError.message, fatal: sandboxError.fatal })
      continue
    }
    const bootProgress = asBootProgress(event)
    if (bootProgress !== null) {
      latestBootPhase = bootProgress.phase
      continue
    }
    if (asReady(event) !== null) {
      sawReady = true
      latestBootPhase = null // "Null once boot has completed" -- Heartbeat.lastBootPhase's own doc comment, mirrored here
      continue
    }
    // Artifact events (pr/preview/upload) are the rail's own content
    // (§12.2 item 1's own "right rail... artifacts: PR / preview /
    // uploads", built out fully in a later Step) -- parsed here only to
    // recognize and skip them explicitly, never falling through to the
    // "unrecognized event type" no-op below by coincidence.
    if (asArtifact(event) !== null) continue

    const subTaskStart = asSubTaskStart(event)
    if (subTaskStart !== null) {
      // Opens/reuses the current turn BEFORE touching pendingSubTasksByParent/
      // subTasksById below -- both are turn-scoped state that resetTurnState()
      // (inside ensureTurn) would otherwise wipe out from under a sub-task
      // that arrived before its own turn had opened any OTHER event yet
      // (e.g. a sub_task_start as the very first event of a turn).
      ensureTurn(event.id)
      const node: SubTaskNode = {
        subTaskId: subTaskStart.subTaskId,
        label: subTaskStart.label,
        subAgentType: subTaskStart.subAgentType ?? null,
        startedAt: event.createdAt,
        finishedAt: null,
        status: 'running',
      }
      subTasksById.set(node.subTaskId, node)
      attachSubTask(node, subTaskStart.parentMessageId)
      continue
    }
    const subTaskFinish = asSubTaskFinish(event)
    if (subTaskFinish !== null) {
      ensureTurn(event.id) // see the identical call in the sub_task_start branch above for why
      const existing = subTasksById.get(subTaskFinish.subTaskId)
      if (existing) {
        existing.status = subTaskStatusFromOutcome(subTaskFinish.outcome)
        existing.finishedAt = event.createdAt
      } else {
        // A finish with no matching start anywhere in this turn's own
        // history -- either genuinely out-of-order beyond this reducer's
        // single pass, or hostile/corrupt. Surfaced honestly rather than
        // dropped (this module's own top comment).
        orphanedSubTasks.push({
          subTaskId: subTaskFinish.subTaskId,
          label: '(sub-task start not seen)',
          subAgentType: null,
          startedAt: null,
          finishedAt: event.createdAt,
          status: subTaskStatusFromOutcome(subTaskFinish.outcome),
        })
      }
      continue
    }

    // Every remaining recognized type below is turn-scoped -- open (or
    // reuse) the current turn before routing further. Every one of these
    // ALSO carries an optional subTaskId (§6.1/§7.1): a non-null value
    // means this event belongs to a sub-task lane this module does not
    // render the internals of (this file's own top comment) -- skip it
    // once the turn/parsing has been acknowledged, never attach it to
    // the main lane.
    const toolCall = asToolCall(event)
    if (toolCall !== null) {
      const turn = ensureTurn(event.id)
      if (toolCall.subTaskId) continue
      const step = ensureOpenStep(turn, event)
      const pending = pendingSubTasksByParent.get(toolCall.messageId) ?? []
      pendingSubTasksByParent.delete(toolCall.messageId)
      const node: ToolCallNode = {
        callId: toolCall.callId,
        messageId: toolCall.messageId,
        toolName: toolCall.toolName,
        input: toolCall.input,
        startedAt: event.createdAt,
        result: null,
        subTasks: pending,
      }
      toolCallsByMessageId.set(node.messageId, node)
      toolCallsByCallId.set(node.callId, node)
      step.toolCalls.push(node)
      continue
    }
    const toolResult = asToolResult(event)
    if (toolResult !== null) {
      ensureTurn(event.id)
      if (toolResult.subTaskId) continue
      const node = toolCallsByCallId.get(toolResult.callId)
      if (node) {
        node.result = { output: toolResult.output, isError: toolResult.isError, finishedAt: event.createdAt }
      }
      continue
    }
    const stepStart = asStepStart(event)
    if (stepStart !== null) {
      const turn = ensureTurn(event.id)
      if (stepStart.subTaskId) continue
      let step = stepsByStepId.get(stepStart.stepId)
      if (!step) {
        step = { stepId: stepStart.stepId, toolCalls: [], tokens: [], cost: null, startedAt: event.createdAt, finishedAt: null, live: true }
        stepsByStepId.set(step.stepId, step)
        turn.steps.push(step)
      }
      openStepId = step.stepId
      continue
    }
    const stepFinish = asStepFinish(event)
    if (stepFinish !== null) {
      const turn = ensureTurn(event.id)
      if (stepFinish.subTaskId) continue
      let step = stepsByStepId.get(stepFinish.stepId)
      if (!step) {
        // step_finish with no matching step_start -- still surfaced, not
        // dropped (this module's own top comment on honest handling of
        // out-of-order/adversarial input).
        step = { stepId: stepFinish.stepId, toolCalls: [], tokens: [], cost: null, startedAt: null, finishedAt: null, live: true }
        stepsByStepId.set(step.stepId, step)
        turn.steps.push(step)
      }
      step.cost = {
        inputTokens: stepFinish.cost.tokens.input,
        outputTokens: stepFinish.cost.tokens.output,
        cachedTokens: stepFinish.cost.tokens.cached ?? null,
        usd: stepFinish.cost.usd ?? null,
      }
      step.finishedAt = event.createdAt
      step.live = false
      if (openStepId === step.stepId) openStepId = null
      continue
    }
    const token = asToken(event)
    if (token !== null) {
      const turn = ensureTurn(event.id)
      if (token.subTaskId) continue
      // Upsert-by-messageId, cumulative replace (§6.1), wherever in this
      // turn the part's first frame placed it (this file's top comment).
      const existing = tokensByMessageId.get(token.messageId)
      if (existing) {
        existing.text = token.text
        continue
      }
      const stream: TokenStream = { messageId: token.messageId, text: token.text }
      ensureOpenStep(turn, event).tokens.push(stream)
      tokensByMessageId.set(stream.messageId, stream)
      continue
    }
    const executionComplete = asExecutionComplete(event)
    if (executionComplete !== null) {
      const turn = ensureTurn(event.id)
      if (executionComplete.subTaskId) continue
      turn.outcome = { outcome: executionComplete.outcome, reason: executionComplete.reason }
      turn.live = false
      for (const step of turn.steps) step.live = false
      currentTurn = null // the NEXT turn-scoped event (if any) starts fresh
      continue
    }
    // Every other recognized/unrecognized type (ready, heartbeat,
    // boot_progress, git_sync, boot_timing, push_complete, push_error,
    // snapshot_ready, or a genuinely unknown future type) is not part of
    // this timeline model -- session-workspace-wide status (the rail) or
    // simply out of this file's own rendering scope. Never a
    // crash either way: an unrecognized type is a documented no-op, not
    // a thrown error.
  }

  return { turns, warnings, errors, latestTitle, orphanedSubTasks, latestBootPhase, sawReady }
}
