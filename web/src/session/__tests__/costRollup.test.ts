// costRollup.test.ts -- §12.2's own "cost incl. sub-task roll-up" (§7.1).
// The defining property under test: a step_finish tagged with a subTaskId
// (sub-task spend) is included in the totals here, unlike
// timelineModel.ts's own per-step cost, which deliberately EXCLUDES it
// from the main lane it renders (that module's own top comment).
import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

import type { EventEnvelope } from '../../ws/types'
import { buildCostRollup } from '../costRollup'

function stepFinish(id: number, usd: number | null, input: number, output: number, subTaskId?: string): EventEnvelope {
  return {
    id,
    type: 'step_finish',
    payload: { type: 'step_finish', messageId: `m${id}`, sessionId: 's', gen: 1, stepId: `st${id}`, subTaskId: subTaskId ?? null, cost: { tokens: { input, output }, usd } },
    createdAt: `2026-08-20T10:00:0${id}Z`,
  }
}

function executionComplete(id: number, subTaskId?: string): EventEnvelope {
  return {
    id,
    type: 'execution_complete',
    payload: { type: 'execution_complete', messageId: `m${id}`, sessionId: 's', gen: 1, ackId: `execution_complete:m${id}`, outcome: 'completed', reason: null, subTaskId: subTaskId ?? null },
    createdAt: `2026-08-20T10:00:0${id}Z`,
  }
}

describe('buildCostRollup', () => {
  it('with no step_finish events, both totals are null (never a fabricated 0 "measured, and free")', () => {
    const rollup = buildCostRollup([])
    expect(rollup.turnUsd).toBeNull()
    expect(rollup.sessionUsd).toBeNull()
    expect(rollup.turnInputTokens).toBe(0)
    expect(rollup.sessionInputTokens).toBe(0)
  })

  it('sums main-lane step_finish cost/tokens into both turn and session totals', () => {
    const rollup = buildCostRollup([stepFinish(1, 0.1, 100, 50), stepFinish(2, 0.2, 200, 75)])
    expect(rollup.turnUsd).toBeCloseTo(0.3)
    expect(rollup.sessionUsd).toBeCloseTo(0.3)
    expect(rollup.turnInputTokens).toBe(300)
    expect(rollup.turnOutputTokens).toBe(125)
  })

  // THE defining property §12.2 names: sub-task-tagged step_finish
  // events roll up into cost too -- unlike timelineModel.ts's own main-
  // lane-only per-step cost. MUTATION TEST: change buildCostRollup to
  // reuse timelineModel.ts's own step-attribution logic (or add a
  // `if (stepFinish.subTaskId) continue` guard mirroring that module's)
  // and this test fails -- sub-task spend silently disappears from the
  // total exactly the way §7.1 warns against.
  it('includes sub-task-tagged step_finish cost in the roll-up (the row\'s own named requirement)', () => {
    const rollup = buildCostRollup([
      stepFinish(1, 0.10, 100, 50), // main lane
      stepFinish(2, 0.05, 40, 10, 'subtask-a'), // sub-task spend -- must still count
      stepFinish(3, 0.07, 30, 15, 'subtask-b'),
    ])
    expect(rollup.sessionUsd).toBeCloseTo(0.22)
    expect(rollup.sessionInputTokens).toBe(170)
    expect(rollup.sessionOutputTokens).toBe(75)
  })

  it('a step_finish with no usd figure contributes tokens but leaves usd unmeasured (until another step supplies one)', () => {
    const rollup = buildCostRollup([stepFinish(1, null, 10, 5)])
    expect(rollup.sessionUsd).toBeNull()
    expect(rollup.sessionInputTokens).toBe(10)
  })

  it('the MAIN turn\'s own execution_complete resets the current-turn accumulator but never the session total', () => {
    const events = [
      stepFinish(1, 0.10, 10, 5),
      executionComplete(2), // turn 1 ends
      stepFinish(3, 0.20, 20, 8), // turn 2's own spend
    ]
    const rollup = buildCostRollup(events)
    expect(rollup.turnUsd).toBeCloseTo(0.2) // only turn 2's own spend
    expect(rollup.sessionUsd).toBeCloseTo(0.3) // both turns
  })

  it('the control plane\'s synthetic execution_complete ends the turn too', () => {
    const events = [
      stepFinish(1, 0.1, 10, 5),
      { id: 2, type: 'execution_complete', payload: { turn_id: 't1', synthetic: true, dispatched: true, reason: 'timeout' }, createdAt: '2026-08-20T10:00:02Z' },
      stepFinish(3, 0.2, 20, 8),
    ]
    const rollup = buildCostRollup(events)
    expect(rollup.turnUsd).toBeCloseTo(0.2)
    expect(rollup.sessionUsd).toBeCloseTo(0.3)
  })

  // S1: turn B, queued behind A, is cancelled at once by a person's stop
  // (a pending turn's synthetic end: no `dispatched` stamp), while A runs on
  // and completes. B's end resets nothing mid-A; A's own end does.
  it('a queued turn\'s synthetic end mid-turn resets nothing; the running turn\'s own end does', () => {
    const events: EventEnvelope[] = [
      stepFinish(1, 0.1, 10, 5),
      { id: 2, type: 'execution_complete', payload: { turn_id: 'turnB', synthetic: true, reason: 'stopped' }, createdAt: '2026-08-20T10:00:02Z' },
      stepFinish(3, 0.5, 20, 8),
    ]
    const mid = buildCostRollup(events)
    expect(mid.turnUsd).toBeCloseTo(0.6)
    expect(mid.turnInputTokens).toBe(30)
    const ended = buildCostRollup([...events, executionComplete(4)])
    expect(ended.turnUsd).toBeNull()
    expect(ended.sessionUsd).toBeCloseTo(0.6)
  })

  it('a SUB-TASK-tagged execution_complete never closes the main turn boundary', () => {
    const events = [
      stepFinish(1, 0.10, 10, 5),
      executionComplete(2, 'subtask-a'), // must NOT reset the turn accumulator
      stepFinish(3, 0.05, 5, 2),
    ]
    const rollup = buildCostRollup(events)
    expect(rollup.turnUsd).toBeCloseTo(0.15) // both steps still in the SAME turn
  })
})

// The rows one assistant message leaves once the server stores its tool
// events (technical plan §6.1): its step_start, tool_calls, tool_results
// and step_finish all share one msg_ id, as the runtime adapter gives it,
// and the server keys each by its own correlator. Written by
// TestResilience_ToolEventsOfOneMessage_EachStoredOnce (test/resilience)
// from the rows the real handler stores. A rendering check: this module
// never reads messageId, so it cannot see a loss on the server -- before
// the server stored the step_finish, there was no row here to sum.
describe('buildCostRollup -- the rows one message leaves once stored', () => {
  const rows = (): EventEnvelope[] => JSON.parse(readFileSync(new URL('./fixtures/toolEventsOfOneMessage.json', import.meta.url), 'utf8')) as EventEnvelope[]

  it('the turn and session totals and the tokens come from that step_finish while the turn runs', () => {
    const live = rows().filter((e) => e.type !== 'execution_complete')
    expect(live.filter((e) => e.type === 'step_finish')).toHaveLength(1)
    const rollup = buildCostRollup(live)
    expect(rollup).toEqual({ turnUsd: 0.42, turnInputTokens: 1200, turnOutputTokens: 80, sessionUsd: 0.42, sessionInputTokens: 1200, sessionOutputTokens: 80 })
  })

  it('once the turn has ended, the session keeps them', () => {
    const rollup = buildCostRollup(rows())
    expect([rollup.sessionUsd, rollup.sessionInputTokens, rollup.sessionOutputTokens]).toEqual([0.42, 1200, 80])
  })
})
