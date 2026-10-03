import { describe, expect, it } from 'vitest'

import type { EventEnvelope } from '../../ws/types'
import { asExecutionComplete, asStepFinish, asSubTaskStart, asToken, asToolCall, asToolResult } from '../eventPayloads'
import { MALFORMED_CUT } from '../tokenCut'

function env(type: string, payload: unknown): EventEnvelope {
  return { id: 1, type, payload, createdAt: '2026-08-20T10:00:00Z' }
}

describe('eventPayloads type guards', () => {
  it('accepts a well-formed tool_call', () => {
    const e = env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Read', input: { path: 'x' } })
    expect(asToolCall(e)).not.toBeNull()
  })

  it('rejects a tool_call whose type does not match', () => {
    const e = env('tool_result', { messageId: 'm1', callId: 'c1', toolName: 'Read', input: {} })
    expect(asToolCall(e)).toBeNull()
  })

  it('rejects a tool_call with a wrong-typed field (input not an object)', () => {
    const e = env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Read', input: 'not-an-object' })
    expect(asToolCall(e)).toBeNull()
  })

  it('rejects a tool_call missing a required field entirely', () => {
    const e = env('tool_call', { messageId: 'm1', toolName: 'Read', input: {} })
    expect(asToolCall(e)).toBeNull()
  })

  it('accepts a tool_call whose payload is a hostile object with extra/prototype-polluting-looking keys, without throwing', () => {
    const e = env('tool_call', JSON.parse('{"messageId":"m1","callId":"c1","toolName":"Read","input":{},"__proto__":{"polluted":true}}'))
    expect(() => asToolCall(e)).not.toThrow()
    expect(asToolCall(e)).not.toBeNull()
  })

  it('rejects a payload that is not even a plain object (an array, or a primitive)', () => {
    expect(asToolCall(env('tool_call', ['not', 'an', 'object']))).toBeNull()
    expect(asToolCall(env('tool_call', 'a string'))).toBeNull()
    expect(asToolCall(env('tool_call', null))).toBeNull()
    expect(asToolCall(env('tool_call', undefined))).toBeNull()
  })

  it('accepts a well-formed tool_result and rejects one with isError as a non-boolean', () => {
    expect(asToolResult(env('tool_result', { messageId: 'm1', callId: 'c1', output: {}, isError: false }))).not.toBeNull()
    expect(asToolResult(env('tool_result', { messageId: 'm1', callId: 'c1', output: {}, isError: 'false' }))).toBeNull()
  })

  it('rejects step_finish whose cost.tokens is a bare number instead of an object -- §6.1\'s own pinned regression', () => {
    const e = env('step_finish', { messageId: 'm1', stepId: 's1', cost: { tokens: 42 } })
    expect(asStepFinish(e)).toBeNull()
  })

  it('accepts a well-formed step_finish', () => {
    const e = env('step_finish', { messageId: 'm1', stepId: 's1', cost: { tokens: { input: 10, output: 5 }, usd: 0.02 } })
    expect(asStepFinish(e)).not.toBeNull()
  })

  it('rejects sub_task_start missing parentMessageId', () => {
    const e = env('sub_task_start', { messageId: 'm1', subTaskId: 'st1', label: 'x' })
    expect(asSubTaskStart(e)).toBeNull()
  })

  it('rejects execution_complete with an outcome outside the closed taxonomy', () => {
    const e = env('execution_complete', { messageId: 'm1', outcome: 'exploded', reason: null })
    expect(asExecutionComplete(e)).toBeNull()
  })

  it('accepts execution_complete with a null reason and rejects a non-string, non-null reason', () => {
    expect(asExecutionComplete(env('execution_complete', { messageId: 'm1', outcome: 'completed', reason: null }))).not.toBeNull()
    expect(asExecutionComplete(env('execution_complete', { messageId: 'm1', outcome: 'failed', reason: 42 }))).toBeNull()
  })

  it('accepts a token event and rejects one whose text field is missing', () => {
    expect(asToken(env('token', { messageId: 'm1', text: 'hello' }))).not.toBeNull()
    expect(asToken(env('token', { messageId: 'm1' }))).toBeNull()
  })
})

// A token frame's `cut` (technical plan §6.1) reaches readers as tokenCut.ts
// reads it: kept when well formed, the fail-closed sentinel when present and
// unreadable, and absent when absent or null.
describe('asToken -- the cut property', () => {
  it('carries a well-formed cut', () => {
    const t = asToken(env('token', { messageId: 'm1', text: 'ab\n[text cut at 2 of 9 bytes on its way from the sandbox]', cut: { kept: 2, total: 9 } }))
    expect(t?.cut).toEqual({ kept: 2, total: 9 })
  })

  it('reads a malformed cut as the fail-closed sentinel, never dropping it', () => {
    for (const cut of [{ kept: '2', total: 9 }, { kept: 9, total: 9 }, 'cut', [2, 9]]) {
      const t = asToken(env('token', { messageId: 'm1', text: 'ab', cut }))
      expect(t).not.toBeNull()
      expect(t!.cut).toEqual(MALFORMED_CUT)
    }
  })

  it('a null or absent cut is a whole frame', () => {
    expect(asToken(env('token', { messageId: 'm1', text: 'ab', cut: null }))).not.toHaveProperty('cut')
    expect(asToken(env('token', { messageId: 'm1', text: 'ab' }))).not.toHaveProperty('cut')
  })
})
