// eventPayloads.ts -- turns one EventEnvelope (web/src/ws/types.ts) --
// `{id, type, payload: unknown, createdAt}` -- into a typed SandboxEvent
// (contracts/gen/ts/sandbox-ws-events.ts) IF, and only if, `payload`
// actually shapes up as the variant `type` claims. `payload` is verbatim,
// untrusted wire content (this Step's own defining risk) -- every field
// read off it below is checked for its expected primitive type before
// this module ever hands it to a caller as that type; a payload that
// lies about its own shape (a hostile or corrupted producer) is dropped,
// never trusted past this boundary and never thrown.
//
// No type here is redeclared -- every return type below is imported
// directly from @narvi/contracts/sandbox-ws-events (web/scripts/
// check-no-dto-redeclaration.mjs enforces exactly this); this module adds
// only the RUNTIME narrowing that schema has no mechanism of its own to
// perform client-side (§12.1: "the UI merely renders the generated
// contracts" -- rendering safely from an untyped wire still needs code
// somewhere, and this is that code, not a second copy of the schema).
import type {
  Artifact,
  BootProgress,
  ExecutionComplete,
  PromptReceived,
  Ready,
  SandboxErrorEvent,
  SessionTitle,
  StepFinish,
  StepStart,
  SubTaskFinish,
  SubTaskStart,
  Token,
  ToolCall,
  ToolResult,
  Warning,
} from '@narvi/contracts/sandbox-ws-events'

import type { EventEnvelope } from '../ws/types'
import { isPlainObject } from '../ws/util'
import { decodeCut } from './tokenCut'

function isString(v: unknown): v is string {
  return typeof v === 'string'
}
function isOptionalNullableString(v: unknown): v is string | null | undefined {
  return v === undefined || v === null || typeof v === 'string'
}
function isBoolean(v: unknown): v is boolean {
  return typeof v === 'boolean'
}
function isNumber(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v)
}

/** asToolCall narrows env.payload to a ToolCall iff env.type === 'tool_call' and every required field is present with the right primitive type. */
export function asToolCall(env: EventEnvelope): ToolCall | null {
  if (env.type !== 'tool_call' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.callId) || !isString(p.toolName) || !isPlainObject(p.input)) return null
  if (!isOptionalNullableString(p.subTaskId)) return null
  return env.payload as unknown as ToolCall
}

export function asToolResult(env: EventEnvelope): ToolResult | null {
  if (env.type !== 'tool_result' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.callId) || !isPlainObject(p.output) || !isBoolean(p.isError)) return null
  if (!isOptionalNullableString(p.subTaskId)) return null
  return env.payload as unknown as ToolResult
}

export function asStepStart(env: EventEnvelope): StepStart | null {
  if (env.type !== 'step_start' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.stepId)) return null
  if (!isOptionalNullableString(p.subTaskId)) return null
  return env.payload as unknown as StepStart
}

export function asStepFinish(env: EventEnvelope): StepFinish | null {
  if (env.type !== 'step_finish' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.stepId) || !isPlainObject(p.cost)) return null
  const tokens = p.cost.tokens
  // §6.1's own explicit warning, pinned client-side too: tokens MUST be an
  // object, never a bare number (a number here would otherwise silently
  // read as "no cost data" below rather than corrupting a running total,
  // but is still rejected outright as a malformed event -- never coerced).
  if (!isPlainObject(tokens) || !isNumber(tokens.input) || !isNumber(tokens.output)) return null
  if (!isOptionalNullableString(p.subTaskId)) return null
  return env.payload as unknown as StepFinish
}

/**
 * asSubTaskStart narrows a `sub_task_start`. Its optional parentCallId
 * (technical plan §6.1) is kept when it is a non-empty string and dropped
 * otherwise -- never failing the event, so a sub-task whose call cannot be
 * named still shows, paired by its message instead (timelineModel.ts).
 */
export function asSubTaskStart(env: EventEnvelope): SubTaskStart | null {
  if (env.type !== 'sub_task_start' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.subTaskId) || !isString(p.label) || !isString(p.parentMessageId)) return null
  if (p.parentCallId === undefined || (isString(p.parentCallId) && p.parentCallId !== '')) return env.payload as unknown as SubTaskStart
  const start = { ...(env.payload as unknown as SubTaskStart) }
  delete start.parentCallId
  return start
}

export function asSubTaskFinish(env: EventEnvelope): SubTaskFinish | null {
  if (env.type !== 'sub_task_finish' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.subTaskId) || !isString(p.outcome)) return null
  if (p.outcome !== 'completed' && p.outcome !== 'failed' && p.outcome !== 'cancelled') return null
  return env.payload as unknown as SubTaskFinish
}

/**
 * asToken narrows a `token` frame, carrying its `cut` (technical plan §6.1)
 * as tokenCut.ts's decodeCut reads it: absent or null is a whole frame (no
 * `cut` on the result), a well-formed one is kept, and anything else present
 * is MALFORMED_CUT -- never dropped, so a frame whose cut cannot be read is
 * still read as cut, never as whole.
 */
export function asToken(env: EventEnvelope): Token | null {
  if (env.type !== 'token' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.text)) return null
  if (!isOptionalNullableString(p.subTaskId)) return null
  if (p.cut === undefined) return env.payload as unknown as Token
  const token = { ...(env.payload as unknown as Token) }
  const cut = decodeCut(p.cut)
  if (cut === null) delete token.cut
  else token.cut = cut
  return token
}

export function asExecutionComplete(env: EventEnvelope): ExecutionComplete | null {
  if (env.type !== 'execution_complete' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.outcome)) return null
  if (p.outcome !== 'completed' && p.outcome !== 'failed' && p.outcome !== 'cancelled') return null
  if (p.reason !== null && !isString(p.reason)) return null
  if (!isOptionalNullableString(p.subTaskId)) return null
  return env.payload as unknown as ExecutionComplete
}

/**
 * The reason the control plane writes on the turn end of a stop
 * (internal/app/sessionactor/stop.go): the one synthetic end of a
 * cancelled turn. Every other synthetic end -- turn_deadline, a dispatch
 * whose prompt never got through, a refused spawn, the credential gate --
 * ends a failed turn.
 */
const SYNTHETIC_STOP_REASON = 'stopped'

/** A turn's end, as the timeline and the cost rollup read it: the agent's own execution_complete, or the one the control plane writes itself. */
export interface TurnEnd {
  outcome: 'completed' | 'failed' | 'cancelled'
  reason: string | null
  /** Set only on an agent's execution_complete of a sub-task lane, which ends no turn. */
  subTaskId: string | null
  /** True for the control plane's own synthetic end, false for the agent's. */
  synthetic: boolean
  /**
   * False only on the control plane's end of a turn whose prompt certainly
   * never reached the sandbox (`"delivered": false`,
   * internal/app/sessionactor/dispatch.go's failDispatchedTurn): its agent
   * never ran.
   */
  delivered: boolean
}

/**
 * asTurnEnd reads an `execution_complete` as the end of a turn, from either
 * writer. The agent's carries its outcome (asExecutionComplete). The one the
 * control plane writes when it ends a turn itself -- its deadline passes, it
 * is stopped, its dispatch or spawn is refused -- carries `{turn_id,
 * synthetic: true, reason}` (technical plan §3.3, "a synthetic
 * execution_complete"), so its outcome is read from its reason: cancelled
 * for a stop, failed for every other.
 *
 * A synthetic end is a turn end here only when it also carries `dispatched:
 * true` (internal/app/sessionactor/syntheticend.go): the turn it ends was
 * the session's one turn in flight, whose events the log holds and the page
 * is showing. A turn ended from pending -- queued behind a running one and
 * cancelled by a stop, refused at the credential gate, abandoned on a
 * refused spawn -- never dispatched and has no event in the log, and ending
 * the page's turn on its end ended another turn, the one still running. So
 * does a synthetic end stored before the stamp existed: it reads as it did
 * before the page read synthetic ends at all, as no end. The page cannot
 * match an end to its turn by `turn_id`, which no other event names.
 */
export function asTurnEnd(env: EventEnvelope): TurnEnd | null {
  const agent = asExecutionComplete(env)
  if (agent !== null) return { outcome: agent.outcome, reason: agent.reason, subTaskId: agent.subTaskId ?? null, synthetic: false, delivered: true }
  if (env.type !== 'execution_complete' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (p.synthetic !== true || p.dispatched !== true || !isString(p.reason)) return null
  return {
    outcome: p.reason === SYNTHETIC_STOP_REASON ? 'cancelled' : 'failed',
    reason: p.reason,
    subTaskId: null,
    synthetic: true,
    delivered: p.delivered !== false,
  }
}

export function asWarning(env: EventEnvelope): Warning | null {
  if (env.type !== 'warning' || !isPlainObject(env.payload)) return null
  if (!isString(env.payload.message)) return null
  return env.payload as unknown as Warning
}

export function asSandboxError(env: EventEnvelope): SandboxErrorEvent | null {
  if (env.type !== 'error' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.message) || !isBoolean(p.fatal)) return null
  return env.payload as unknown as SandboxErrorEvent
}

export function asArtifact(env: EventEnvelope): Artifact | null {
  if (env.type !== 'artifact' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.url) || !isPlainObject(p.metadata)) return null
  if (p.artifactType !== 'pr' && p.artifactType !== 'preview' && p.artifactType !== 'upload') return null
  return env.payload as unknown as Artifact
}

export function asSessionTitle(env: EventEnvelope): SessionTitle | null {
  if (env.type !== 'session_title' || !isPlainObject(env.payload)) return null
  if (!isString(env.payload.title)) return null
  return env.payload as unknown as SessionTitle
}

/** asBootProgress/asReady back the "session still booting" empty-timeline state (Timeline.tsx) -- both are session-lifecycle events (never turn-scoped, §6.1), so this module's own turn-building callers never need to consume them beyond deriving that one honest status line. */
export function asBootProgress(env: EventEnvelope): BootProgress | null {
  if (env.type !== 'boot_progress' || !isPlainObject(env.payload)) return null
  if (!isString(env.payload.phase)) return null
  return env.payload as unknown as BootProgress
}

export function asReady(env: EventEnvelope): Ready | null {
  if (env.type !== 'ready' || !isPlainObject(env.payload)) return null
  return env.payload as unknown as Ready
}

/** readyGen is the sandbox generation a `ready` names, or null when its `gen` is not a number. */
export function readyGen(ready: Ready): number | null {
  const gen: unknown = ready.gen
  return isNumber(gen) ? gen : null
}

/** asPromptReceived narrows the agent's receipt of a prompt (technical plan §3.3, prompt receipts). */
export function asPromptReceived(env: EventEnvelope): PromptReceived | null {
  if (env.type !== 'prompt_received' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.promptMessageId)) return null
  return env.payload as unknown as PromptReceived
}
