import { describe, expect, it } from 'vitest'

import type { EventEnvelope } from '../../ws/types'
import { buildTimelineModel } from '../timelineModel'

let nextId = 1
function env(type: string, payload: unknown, createdAt = '2026-08-20T10:00:00Z'): EventEnvelope {
  return { id: nextId++, type, payload, createdAt }
}

describe('buildTimelineModel', () => {
  it('returns an empty model for an empty log (the "no events yet" state)', () => {
    const model = buildTimelineModel([])
    expect(model.turns).toEqual([])
    expect(model.warnings).toEqual([])
    expect(model.errors).toEqual([])
    expect(model.orphanedSubTasks).toEqual([])
    expect(model.latestTitle).toBeNull()
  })

  it('groups a step_start -> tool_call -> tool_result -> step_finish -> execution_complete sequence into one closed turn', () => {
    const events = [
      env('step_start', { messageId: 'm1', stepId: 's1' }),
      env('tool_call', { messageId: 'm2', callId: 'c1', toolName: 'Read', input: { path: 'a.go' } }),
      env('tool_result', { messageId: 'm3', callId: 'c1', output: { ok: true }, isError: false }),
      env('step_finish', { messageId: 'm4', stepId: 's1', cost: { tokens: { input: 100, output: 20 }, usd: 0.01 } }),
      env('execution_complete', { messageId: 'm5', outcome: 'completed', reason: null }),
    ]
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(1)
    const turn = model.turns[0]!
    expect(turn.live).toBe(false)
    expect(turn.outcome).toEqual({ outcome: 'completed', reason: null })
    expect(turn.steps).toHaveLength(1)
    const step = turn.steps[0]!
    expect(step.live).toBe(false)
    expect(step.cost).toEqual({ inputTokens: 100, outputTokens: 20, cachedTokens: null, usd: 0.01 })
    expect(step.toolCalls).toHaveLength(1)
    expect(step.toolCalls[0]!.toolName).toBe('Read')
    expect(step.toolCalls[0]!.result).toEqual({ output: { ok: true }, isError: false, finishedAt: expect.any(String) })
  })

  it('starts a fresh turn after execution_complete -- a second turn on the same session is never merged into the first', () => {
    const events = [
      env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Read', input: {} }),
      env('execution_complete', { messageId: 'm2', outcome: 'completed', reason: null }),
      env('tool_call', { messageId: 'm3', callId: 'c2', toolName: 'Edit', input: {} }),
    ]
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(2)
    expect(model.turns[0]!.outcome?.outcome).toBe('completed')
    expect(model.turns[1]!.live).toBe(true)
    expect(model.turns[1]!.outcome).toBeNull()
  })

  it('leaves the trailing turn live with no outcome when execution_complete has not arrived yet', () => {
    const events = [env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Bash', input: { cmd: 'go test ./...' } })]
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(1)
    expect(model.turns[0]!.live).toBe(true)
    expect(model.turns[0]!.outcome).toBeNull()
  })

  it('auto-opens an implicit step when a tool_call arrives with no step_start (never dropped)', () => {
    const events = [env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Grep', input: {} })]
    const model = buildTimelineModel(events)
    expect(model.turns[0]!.steps).toHaveLength(1)
    expect(model.turns[0]!.steps[0]!.toolCalls).toHaveLength(1)
  })

  it('nests a sub-task under the spawning tool call, by messageId (§7.1)', () => {
    const events = [
      env('tool_call', { messageId: 'parent-msg', callId: 'c1', toolName: 'Task', input: {} }),
      env('sub_task_start', { messageId: 'm2', subTaskId: 'st1', label: 'counter-reviewer', parentMessageId: 'parent-msg' }),
      env('sub_task_finish', { messageId: 'm3', subTaskId: 'st1', outcome: 'completed' }),
    ]
    const model = buildTimelineModel(events)
    const toolCall = model.turns[0]!.steps[0]!.toolCalls[0]!
    expect(toolCall.subTasks).toHaveLength(1)
    expect(toolCall.subTasks[0]!).toMatchObject({ subTaskId: 'st1', label: 'counter-reviewer', status: 'completed' })
    expect(model.orphanedSubTasks).toEqual([])
  })

  it('buffers a sub_task_start that arrives BEFORE its parent tool_call and attaches it once the parent appears (out-of-order delivery)', () => {
    const events = [
      env('sub_task_start', { messageId: 'm1', subTaskId: 'st1', label: 'fact-check', parentMessageId: 'parent-msg' }),
      env('tool_call', { messageId: 'parent-msg', callId: 'c1', toolName: 'Task', input: {} }),
    ]
    const model = buildTimelineModel(events)
    const toolCall = model.turns[0]!.steps[0]!.toolCalls[0]!
    expect(toolCall.subTasks).toHaveLength(1)
    expect(toolCall.subTasks[0]!.subTaskId).toBe('st1')
  })

  it('surfaces a sub_task_start whose parent tool_call never appears as orphaned-under-no-one -- never silently dropped, but also never crashes', () => {
    // The sub-task itself is only visible once ITS OWN parent tool_call
    // is found; if it never is, it simply never renders anywhere (not
    // tracked in orphanedSubTasks either, since a "sub_task_start with an
    // unmatched parent" is architecturally indistinguishable from "the
    // parent just hasn't arrived in this page yet" -- only a FINISH with
    // no matching START, a structurally impossible ordering in a
    // well-formed producer, is treated as evidence of corruption/hostile
    // input and surfaced in orphanedSubTasks). This test pins that this
    // case does not throw and does not fabricate a phantom tool call.
    const events = [env('sub_task_start', { messageId: 'm1', subTaskId: 'st1', label: 'x', parentMessageId: 'never-appears' })]
    expect(() => buildTimelineModel(events)).not.toThrow()
    const model = buildTimelineModel(events)
    expect(model.orphanedSubTasks).toEqual([])
  })

  it('surfaces a sub_task_finish with no matching sub_task_start in orphanedSubTasks (finish-before-start / corrupt producer)', () => {
    const events = [env('sub_task_finish', { messageId: 'm1', subTaskId: 'st-ghost', outcome: 'failed' })]
    const model = buildTimelineModel(events)
    expect(model.orphanedSubTasks).toHaveLength(1)
    expect(model.orphanedSubTasks[0]!).toMatchObject({ subTaskId: 'st-ghost', status: 'failed' })
  })

  it('skips events carrying a subTaskId in the main lane -- a sub-task never interleaves into the parent step it runs alongside', () => {
    const events = [
      env('step_start', { messageId: 'm1', stepId: 's1' }),
      env('tool_call', { messageId: 'm2', callId: 'c1', toolName: 'Read', input: {}, subTaskId: 'st1' }),
      env('token', { messageId: 'm3', text: 'sub-task chatter', subTaskId: 'st1' }),
    ]
    const model = buildTimelineModel(events)
    const step = model.turns[0]!.steps[0]!
    expect(step.toolCalls).toEqual([])
    expect(step.tokens).toEqual([])
  })

  it('upserts token text by messageId (cumulative replace, never append) -- §6.1', () => {
    const events = [env('token', { messageId: 'm1', text: 'Hello' }), env('token', { messageId: 'm1', text: 'Hello world' })]
    const model = buildTimelineModel(events)
    const step = model.turns[0]!.steps[0]!
    expect(step.tokens).toEqual([{ messageId: 'm1', text: 'Hello world' }])
  })

  // The event log's own shape since each distinct cumulative frame became its
  // own row (internal/app/sessionactor/tokenframe.go): the pinned runtime
  // sends every text part twice -- empty when the part opens, full when it
  // closes -- as two rows sharing the part's messageId, both inside the step
  // that produced them. The step must show one entry per part, holding the
  // newest frame.
  it('folds a text part stored as several rows (empty first frame, then the full text) into one entry per part, per step', () => {
    const events = [
      env('step_start', { messageId: 'msg_1', stepId: 's1' }),
      env('token', { messageId: 'prt_a', text: '' }),
      env('token', { messageId: 'prt_a', text: 'Let me check the tests.' }),
      env('tool_call', { messageId: 'msg_1', callId: 'c1', toolName: 'bash', input: { command: 'go test' } }),
      env('tool_result', { messageId: 'msg_1', callId: 'c1', output: { ok: true }, isError: false }),
      env('step_finish', { messageId: 'msg_1', stepId: 's1', cost: { tokens: { input: 10, output: 5 } } }),
      env('step_start', { messageId: 'msg_2', stepId: 's2' }),
      env('token', { messageId: 'prt_b', text: '' }),
      env('token', { messageId: 'prt_b', text: 'All tests pass.' }),
      env('step_finish', { messageId: 'msg_2', stepId: 's2', cost: { tokens: { input: 10, output: 5 } } }),
      env('execution_complete', { messageId: 'm9', outcome: 'completed', reason: null }),
    ]
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(1)
    const [first, second] = model.turns[0]!.steps
    expect(first!.tokens).toEqual([{ messageId: 'prt_a', text: 'Let me check the tests.' }])
    expect(second!.tokens).toEqual([{ messageId: 'prt_b', text: 'All tests pass.' }])
  })

  it('routes session_title/warning/error without opening a spurious turn', () => {
    const events = [
      env('session_title', { messageId: 'm1', title: 'Fix the scheduler' }),
      env('warning', { messageId: 'm2', message: 'context nearing limit' }),
      env('error', { messageId: 'm3', message: 'boom', fatal: true }),
    ]
    const model = buildTimelineModel(events)
    expect(model.turns).toEqual([])
    expect(model.latestTitle).toBe('Fix the scheduler')
    expect(model.warnings).toHaveLength(1)
    expect(model.errors).toHaveLength(1)
    expect(model.errors[0]!.fatal).toBe(true)
  })

  it('ignores a malformed payload for a recognized type without throwing (wrong field types)', () => {
    const events = [
      env('tool_call', { messageId: 123, callId: 'c1', toolName: 'Read', input: {} }), // messageId not a string
      env('step_finish', { messageId: 'm2', stepId: 's1', cost: { tokens: 42 } }), // tokens not an object -- §6.1's own explicit warning
    ]
    expect(() => buildTimelineModel(events)).not.toThrow()
    const model = buildTimelineModel(events)
    expect(model.turns).toEqual([])
  })

  it('ignores a completely unrecognized event type without throwing', () => {
    const events = [env('some_future_event_type', { anything: 'goes' })]
    expect(() => buildTimelineModel(events)).not.toThrow()
    expect(buildTimelineModel(events).turns).toEqual([])
  })

  it('handles a step_finish with no matching step_start by surfacing a step anyway (never dropped)', () => {
    const events = [env('step_finish', { messageId: 'm1', stepId: 'ghost-step', cost: { tokens: { input: 1, output: 1 } } })]
    const model = buildTimelineModel(events)
    expect(model.turns[0]!.steps).toHaveLength(1)
    expect(model.turns[0]!.steps[0]!.cost).not.toBeNull()
  })

  it('tracks the latest boot_progress phase and clears it once ready is seen (the "session still booting" empty-state signal)', () => {
    const events = [env('boot_progress', { messageId: 'm1', phase: 'installing deps', timestamp: '2026-08-20T10:00:00Z' })]
    let model = buildTimelineModel(events)
    expect(model.latestBootPhase).toBe('installing deps')
    expect(model.sawReady).toBe(false)
    expect(model.turns).toEqual([]) // session-lifecycle events never open a turn

    model = buildTimelineModel([...events, env('ready', { messageId: 'm2', timestamp: '2026-08-20T10:01:00Z' })])
    expect(model.sawReady).toBe(true)
    expect(model.latestBootPhase).toBeNull()
  })

  it('does not treat an artifact event as a turn-opening event and does not throw on a very large tool_call input', () => {
    const bigInput = { blob: 'x'.repeat(50_000) }
    const events = [
      env('artifact', { messageId: 'm1', artifactType: 'pr', url: 'https://github.com/acme/example/pull/1', metadata: {} }),
      env('tool_call', { messageId: 'm2', callId: 'c1', toolName: 'Bash', input: bigInput }),
    ]
    expect(() => buildTimelineModel(events)).not.toThrow()
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(1) // the artifact must not have opened its own empty turn
    expect(model.turns[0]!.steps[0]!.toolCalls[0]!.input).toEqual(bigInput)
  })
})
