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

export function asSubTaskStart(env: EventEnvelope): SubTaskStart | null {
  if (env.type !== 'sub_task_start' || !isPlainObject(env.payload)) return null
  const p = env.payload
  if (!isString(p.messageId) || !isString(p.subTaskId) || !isString(p.label) || !isString(p.parentMessageId)) return null
  return env.payload as unknown as SubTaskStart
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
