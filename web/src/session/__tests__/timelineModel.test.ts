import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

import type { EventEnvelope } from '../../ws/types'
import { asExecutionComplete, asStepStart } from '../eventPayloads'
import { buildTimelineModel } from '../timelineModel'
import { decodeCut, MALFORMED_CUT } from '../tokenCut'
import { readTokenCutVectors } from './tokenCutVectors'

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

  it('ends a turn at the synthetic execution_complete the control plane writes, with its reason, and opens the next turn fresh', () => {
    // The control plane's own turn end carries {turn_id, synthetic, reason}
    // and no messageId or outcome (timerfired.go's turn_deadline, stop.go,
    // dispatch.go, credentialgate.go): a stop's is cancelled, every other
    // failed.
    for (const [reason, outcome] of [
      ['timeout', 'failed'],
      ['stopped', 'cancelled'],
      ['the sandbox could not be spawned', 'failed'],
    ] as const) {
      const events = [
        env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Bash', input: {} }),
        env('execution_complete', { turn_id: 't1', synthetic: true, reason }),
        env('tool_call', { messageId: 'm2', callId: 'c2', toolName: 'Read', input: {} }),
      ]
      const model = buildTimelineModel(events)
      expect(model.turns.map((t) => [t.live, t.outcome]), reason).toEqual([
        [false, { outcome, reason }],
        [true, null],
      ])
    }
  })

  it('reads an execution_complete that is neither the agent\'s nor a synthetic one as no turn end', () => {
    const events = [
      env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Bash', input: {} }),
      env('execution_complete', { turn_id: 't1', synthetic: false, reason: 'timeout' }),
      env('execution_complete', { turn_id: 't1', synthetic: true, reason: 7 }),
    ]
    expect(buildTimelineModel(events).turns.map((t) => t.live)).toEqual([true])
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

  it('nests a sub-task that names no call under the task call of its message (§7.1)', () => {
    const events = [
      env('tool_call', { messageId: 'parent-msg', callId: 'c1', toolName: 'task', input: {} }),
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
      env('tool_call', { messageId: 'parent-msg', callId: 'c1', toolName: 'task', input: {} }),
    ]
    const model = buildTimelineModel(events)
    const toolCall = model.turns[0]!.steps[0]!.toolCalls[0]!
    expect(toolCall.subTasks).toHaveLength(1)
    expect(toolCall.subTasks[0]!.subTaskId).toBe('st1')
  })

  it('shows a sub_task_start whose parent tool_call never appears as a lane of the turn -- never dropped, never a phantom tool call', () => {
    // Only a FINISH with no matching START, a structurally impossible
    // ordering in a well-formed producer, is surfaced in orphanedSubTasks;
    // a start that has found no call is a lane of its turn, while the turn
    // runs and once it has ended.
    const start = env('sub_task_start', { messageId: 'm1', subTaskId: 'st1', label: 'x', parentMessageId: 'never-appears' })
    expect(() => buildTimelineModel([start])).not.toThrow()
    const live = buildTimelineModel([start])
    expect(live.orphanedSubTasks).toEqual([])
    expect(live.turns[0]!.steps).toEqual([])
    expect(live.turns[0]!.subTasks.map((s) => s.subTaskId)).toEqual(['st1'])
    const ended = buildTimelineModel([start, env('execution_complete', { messageId: 'done', outcome: 'completed', reason: null })])
    expect(ended.turns[0]!.subTasks.map((s) => s.subTaskId)).toEqual(['st1'])
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
    expect(step.tokens).toMatchObject([{ messageId: 'm1', text: 'Hello world' }])
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
    expect(first!.tokens).toMatchObject([{ messageId: 'prt_a', text: 'Let me check the tests.' }])
    expect(second!.tokens).toMatchObject([{ messageId: 'prt_b', text: 'All tests pass.' }])
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

  it('keeps prompt_received (technical plan §3.3, prompt receipts) out of the model: it opens no turn after an execution_complete and leaves the boot signal alone', () => {
    const events = [
      env('boot_progress', { messageId: 'm0', phase: 'installing deps', timestamp: '2026-08-20T10:00:00Z' }),
      env('tool_call', { messageId: 'm1', callId: 'c1', toolName: 'Read', input: {} }),
      env('execution_complete', { messageId: 'm2', outcome: 'completed', reason: null }),
      env('prompt_received', { type: 'prompt_received', messageId: 'prompt_received:p2', sessionId: 's', gen: 1, promptMessageId: 'p2', duplicate: false }),
    ]
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(1)
    expect(model.turns[0]!.live).toBe(false)
    expect(model.latestBootPhase).toBe('installing deps')
    expect(model.sawAgentReady).toBe(false)
  })

  it('reads a ready that advertises capabilities as any agent ready', () => {
    const events = [
      env('boot_progress', { messageId: 'm1', phase: 'installing deps', timestamp: '2026-08-20T10:00:00Z' }),
      env('ready', { messageId: 'm2', timestamp: '2026-08-20T10:01:00Z', capabilities: { promptReceipt: true } }),
    ]
    const model = buildTimelineModel(events)
    expect(model.sawAgentReady).toBe(true)
    expect(model.latestBootPhase).toBe('installing deps') // the agent's ready ends nothing
    expect(model.turns).toEqual([])
  })

  it('handles a step_finish with no matching step_start by surfacing a step anyway (never dropped)', () => {
    const events = [env('step_finish', { messageId: 'm1', stepId: 'ghost-step', cost: { tokens: { input: 1, output: 1 } } })]
    const model = buildTimelineModel(events)
    expect(model.turns[0]!.steps).toHaveLength(1)
    expect(model.turns[0]!.steps[0]!.cost).not.toBeNull()
  })

  // Technical plan §3.2: the agent's `ready` is the first event of its
  // connection, ahead of the clone, the git-dir sync and the hooks; the
  // server keeps the sandbox booting until boot evidence and a null-phase
  // heartbeat, and says so with a sandbox_status event. Reading the boot as
  // over at `ready` again makes the "ready ends nothing" row fail.
  describe('latestBootPhase: the phase the "session still booting" empty state names', () => {
    const phase = (p: string) => env('boot_progress', { messageId: `bp-${p}`, gen: 1, phase: p })
    const ready = () => env('ready', { messageId: 'r', gen: 1 })
    const status = (s: string, gen = 1) => env('sandbox_status', { sandbox: { gen, status: s } })
    const cases: { name: string; events: () => EventEnvelope[]; want: string | null }[] = [
      { name: 'none reported', events: () => [], want: null },
      { name: 'the latest boot_progress phase', events: () => [phase('clone'), phase('installing deps')], want: 'installing deps' },
      { name: 'the agent\'s ready ends nothing: it comes before the boot', events: () => [ready(), phase('clone')], want: 'clone' },
      { name: 'a ready after the phase ends nothing either (a reconnect mid-boot)', events: () => [phase('clone'), ready()], want: 'clone' },
      { name: 'the server reporting booting keeps it', events: () => [phase('clone'), status('booting')], want: 'clone' },
      { name: 'the server reporting suspect keeps it: a liveness doubt, not the end of the boot', events: () => [phase('clone'), status('suspect')], want: 'clone' },
      { name: 'the server reporting ready ends it', events: () => [phase('clone'), status('ready')], want: null },
      { name: 'the server reporting failed ends it', events: () => [phase('clone'), status('failed')], want: null },
      { name: 'a new generation starts over', events: () => [phase('clone'), status('stopped'), status('spawning', 2), phase('deps')], want: 'deps' },
      { name: 'a malformed sandbox_status ends nothing', events: () => [phase('clone'), env('sandbox_status', { status: 'ready' })], want: 'clone' },
    ]
    for (const c of cases) {
      it(c.name, () => {
        const model = buildTimelineModel(c.events())
        expect(model.latestBootPhase).toBe(c.want)
        expect(model.turns).toEqual([]) // session-lifecycle events never open a turn
      })
    }

    // Rollout compatibility (sessionStatus.ts's isStillBooting): read only
    // while the control plane reports no sandbox row.
    it('sawAgentReady: whether the agent\'s ready is in the log, and nothing else sets it', () => {
      expect(buildTimelineModel([phase('clone'), status('ready')]).sawAgentReady).toBe(false)
      expect(buildTimelineModel([phase('clone'), ready()]).sawAgentReady).toBe(true)
      expect(buildTimelineModel([ready(), phase('clone')]).sawAgentReady).toBe(true)
    })
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

// What a sandbox's reconnect replay can put in the log. The sandbox-agent
// keeps best-effort events in its outbound buffer and resends all of them
// on every reconnect, long after their turn's execution_complete; this
// model opens a turn at any turn-scoped event that follows one, so a
// `token` row stored there would read as a new turn still running (and,
// since the route derives hasOpenTurn from the last turn's `live`, disable
// the composer). The session actor therefore stores a `token` frame only
// while its turn is Processing (internal/app/sessionactor/tokenframe.go,
// pinned end to end by TestHandleSandboxEvent_TokenFrames_
// LateFramesOfAnEndedTurnAddNoRow): a replay after the turn ended adds no
// row, and one during the next turn adds nothing of the earlier turn's
// parts. The replay for the turn still running when the control plane was
// deployed onto per-frame storage does add rows: its parts' later frames,
// at the tail of that turn (tokenframe.go, "The turn running at deploy").
// These are the logs all that leaves, fed to this model.
describe('buildTimelineModel over the log a reconnect replay can leave', () => {
  // A turn as the server stores it: a step, its text parts (each stored as
  // one row per distinct frame), the step's end and the turn's end.
  function storedTurn(step: string, parts: Record<string, string[]>): EventEnvelope[] {
    const events = [env('step_start', { messageId: `msg_${step}`, stepId: step })]
    for (const [partId, frames] of Object.entries(parts)) {
      for (const text of frames) events.push(env('token', { messageId: partId, text }))
    }
    events.push(env('step_finish', { messageId: `msg_${step}`, stepId: step, cost: { tokens: { input: 1, output: 1 } } }))
    events.push(env('execution_complete', { messageId: `done_${step}`, outcome: 'completed', reason: null }))
    return events
  }

  // hasOpenTurn exactly as routes/session/$sessionId.tsx derives it.
  function hasOpenTurn(events: EventEnvelope[]): boolean {
    const { turns } = buildTimelineModel(events)
    return turns.length > 0 && (turns[turns.length - 1]?.live ?? false)
  }

  // What a late row stored after its turn ended does to this model: it
  // opens a turn the log never had, live until something ends it -- the
  // composer disabled meanwhile -- and the next real turn is folded into
  // it. So the log must hold exactly the turns expected, and from a turn's
  // execution_complete until the next turn's first step_start (a turn's
  // first row, as the runtime sends it), no prefix of the log -- each is a
  // state the page renders, live or on a reload -- may have a live turn.
  function expectNoPhantomTurn(events: EventEnvelope[], expectedTurns: number): void {
    expect(buildTimelineModel(events).turns, 'turns in the log').toHaveLength(expectedTurns)
    let ended = false
    for (let n = 1; n <= events.length; n++) {
      const event = events[n - 1]!
      const complete = asExecutionComplete(event)
      const start = asStepStart(event)
      if (complete !== null && !complete.subTaskId) ended = true
      else if (start !== null && !start.subTaskId) ended = false
      if (ended) expect(hasOpenTurn(events.slice(0, n)), `prefix of ${n} events, after an execution_complete`).toBe(false)
    }
  }

  // Frames of an ended turn's part stored after its execution_complete.
  // Round one's: the full frame a pre-fix part's first-wins row swallowed,
  // replayed after the turn ended.
  const lateFullFrame = (): EventEnvelope[] => [
    ...storedTurn('s1', { prt_legacy: [''] }),
    env('token', { messageId: 'prt_legacy', text: 'Turn one final answer.' }),
  ]
  // The previous control plane's, replaying a part stored with no row under
  // its bare part id: its empty first frame inserted again, after the turn.
  const previousBinaryReplay = (): EventEnvelope[] => [
    ...storedTurn('s1', { prt_1: ['', 'Turn one final answer.'] }),
    env('token', { messageId: 'prt_1', text: '' }),
  ]

  it('expectNoPhantomTurn fails on the phantom logs it exists to rule out', () => {
    expect(hasOpenTurn(lateFullFrame())).toBe(true)
    expect(() => expectNoPhantomTurn(lateFullFrame(), 1)).toThrow()
    expect(hasOpenTurn(previousBinaryReplay())).toBe(true)
    expect(() => expectNoPhantomTurn(previousBinaryReplay(), 1)).toThrow()
    // Folded into the next turn, the phantom holds the turn count the log
    // expects; the live prefix before that turn's step_start still shows it.
    const folded = [...previousBinaryReplay(), ...storedTurn('s2', { prt_next: ['', 'Turn two answer.'] })]
    expect(buildTimelineModel(folded).turns).toHaveLength(2)
    expect(() => expectNoPhantomTurn(folded, 2)).toThrow()
    // A late row that reads as a new turn's first step passes the live
    // check; the turn count still sees a turn the log never had.
    const lateStep = [...storedTurn('s1', { prt_a: ['', 'Turn one answer.'] }), env('step_start', { messageId: 'msg_s1_again', stepId: 's1' })]
    expect(() => expectNoPhantomTurn(lateStep, 1)).toThrow()
  })

  it('a replay after the turns ended leaves them closed: no live turn, the composer open', () => {
    // History stored before per-frame keys (first frame only: prt_legacy is
    // blank for good) and after (prt_fixed kept both frames); the replay of
    // either adds no row.
    const events = [...storedTurn('s1', { prt_legacy: [''] }), ...storedTurn('s2', { prt_fixed: ['', 'Turn two note.'] })]
    const model = buildTimelineModel(events)
    expect(model.turns.map((t) => [t.live, t.outcome?.outcome])).toEqual([
      [false, 'completed'],
      [false, 'completed'],
    ])
    expect(model.turns[1]!.steps[0]!.tokens).toMatchObject([{ messageId: 'prt_fixed', text: 'Turn two note.' }])
    expect(hasOpenTurn(events)).toBe(false)
    expectNoPhantomTurn(events, 2)
  })

  it('a replay during the next turn leaves that turn holding only its own text, live until its execution_complete', () => {
    const earlier = storedTurn('s1', { prt_legacy: [''] })
    const next = storedTurn('s2', { prt_next: ['', 'Turn two answer.'] })
    const running = [...earlier, ...next.slice(0, -2)] // the next turn's step still open
    let model = buildTimelineModel(running)
    expect(model.turns).toHaveLength(2)
    expect(model.turns[1]!.live).toBe(true)
    expect(model.turns[1]!.steps.flatMap((s) => s.tokens)).toMatchObject([{ messageId: 'prt_next', text: 'Turn two answer.' }])
    expectNoPhantomTurn(running, 2)

    const done = [...earlier, ...next]
    model = buildTimelineModel(done)
    expect(model.turns.every((t) => !t.live)).toBe(true)
    expect(hasOpenTurn(done)).toBe(false)
    expectNoPhantomTurn(done, 2)
  })

  // The shape the actor refuses to write, and why: frames of an ended
  // turn's part stored after its execution_complete become a turn of their
  // own that never ends, holding old text, and the next real turn is folded
  // into it.
  it('late rows of an ended turn stored after its execution_complete would read as a phantom live turn', () => {
    const late = lateFullFrame()
    const model = buildTimelineModel(late)
    expect(model.turns).toHaveLength(2)
    expect(model.turns[1]!.live).toBe(true)
    expect(model.turns[1]!.outcome).toBeNull()
    expect(model.turns[1]!.steps[0]!.tokens).toMatchObject([{ messageId: 'prt_legacy', text: 'Turn one final answer.' }])
    expect(hasOpenTurn(late)).toBe(true)

    const thenNextTurn = [...late, ...storedTurn('s2', { prt_next: ['', 'Turn two answer.'] })]
    const folded = buildTimelineModel(thenNextTurn)
    expect(folded.turns).toHaveLength(2) // the next turn is folded into the phantom one
    expect(folded.turns[1]!.steps.map((s) => s.stepId)).toEqual([expect.stringMatching(/^implicit:/), 's2'])
  })

  // The turn running at deploy. Before it, first-wins kept each part's
  // first frame only; the replay stores the later frames while the turn
  // still runs, at its tail, after the steps that followed the part. Each
  // part must show its text in the step its first frame opened it in.
  function tokensByStep(events: EventEnvelope[]): [string, [string, string][]][] {
    const [turn, ...others] = buildTimelineModel(events).turns
    expect(others).toEqual([])
    return turn!.steps.map((s) => [s.stepId, s.tokens.map((t): [string, string] => [t.messageId, t.text])])
  }

  it("a part's late frame after a step boundary updates the part in its own step", () => {
    const events = [
      env('step_start', { messageId: 'msg_s1', stepId: 's1' }),
      env('token', { messageId: 'prt_a', text: '' }),
      env('step_start', { messageId: 'msg_s2', stepId: 's2' }),
      env('token', { messageId: 'prt_b', text: '' }),
      env('token', { messageId: 'prt_a', text: 'Step one narration.' }),
      env('token', { messageId: 'prt_b', text: 'Step two answer.' }),
      env('execution_complete', { messageId: 'done', outcome: 'completed', reason: null }),
    ]
    expect(tokensByStep(events)).toEqual([
      ['s1', [['prt_a', 'Step one narration.']]],
      ['s2', [['prt_b', 'Step two answer.']]],
    ])
    expectNoPhantomTurn(events, 1)
  })

  it("a part's late frame with no step open updates the part in place and synthesizes no step", () => {
    const events = [
      env('step_start', { messageId: 'msg_s1', stepId: 's1' }),
      env('token', { messageId: 'prt_a', text: '' }),
      env('step_finish', { messageId: 'msg_s1_end', stepId: 's1', cost: { tokens: { input: 1, output: 1 } } }),
      env('token', { messageId: 'prt_a', text: 'Step one narration.' }),
      env('execution_complete', { messageId: 'done', outcome: 'completed', reason: null }),
    ]
    expect(tokensByStep(events)).toEqual([['s1', [['prt_a', 'Step one narration.']]]])
    expectNoPhantomTurn(events, 1)
  })

  it('several recovered parts across steps each land in their own step, in their own order', () => {
    const events = [
      env('step_start', { messageId: 'msg_s1', stepId: 's1' }),
      env('token', { messageId: 'prt_a1', text: '' }),
      env('token', { messageId: 'prt_a2', text: '' }),
      env('step_start', { messageId: 'msg_s2', stepId: 's2' }),
      env('token', { messageId: 'prt_b1', text: 'Step two, first' }),
      env('step_start', { messageId: 'msg_s3', stepId: 's3' }),
      env('token', { messageId: 'prt_c1', text: '' }),
      // The replay, in the order the sandbox sent the frames.
      env('token', { messageId: 'prt_a1', text: 'Step one, first part.' }),
      env('token', { messageId: 'prt_a2', text: 'Step one, second part.' }),
      env('token', { messageId: 'prt_b1', text: 'Step two, first part.' }),
      // The turn goes on in the step it is in.
      env('token', { messageId: 'prt_c1', text: 'Step three answer.' }),
      env('token', { messageId: 'prt_c2', text: 'Step three, a new part.' }),
    ]
    expect(tokensByStep(events)).toEqual([
      [
        's1',
        [
          ['prt_a1', 'Step one, first part.'],
          ['prt_a2', 'Step one, second part.'],
        ],
      ],
      ['s2', [['prt_b1', 'Step two, first part.']]],
      [
        's3',
        [
          ['prt_c1', 'Step three answer.'],
          ['prt_c2', 'Step three, a new part.'],
        ],
      ],
    ])
    expect(hasOpenTurn(events)).toBe(true) // still running: no execution_complete yet
    expectNoPhantomTurn(events, 1)
  })

  // The rows the session actor stores for that turn, through Actor.Send on
  // real Postgres, written by TestHandleSandboxEvent_TokenFrames_
  // ReplayDuringTheTurnRunningAtDeploy (internal/app/sessionactor), which
  // fails if what it stores stops matching this file.
  it('the log the actor stores for the turn running at deploy shows each part in its own step', () => {
    const fixture = new URL('./fixtures/tokenReplayDuringRunningTurn.json', import.meta.url)
    const events = JSON.parse(readFileSync(fixture, 'utf8')) as EventEnvelope[]
    expect(tokensByStep(events)).toEqual([
      ['s1', [['prt_a', 'Step one narration.']]],
      ['s2', [['prt_b', 'Step two answer.']]],
    ])
    const [turn] = buildTimelineModel(events).turns
    expect([turn!.live, turn!.outcome?.outcome]).toEqual([false, 'completed'])
    expect(hasOpenTurn(events)).toBe(false)
    expectNoPhantomTurn(events, 1)
  })
})

// A frame the sandbox-agent cut on its way to the control plane (technical
// plan §6.1) carries `cut`, and a part can hold it and the whole text it was
// taken from, in either order. The timeline reads a part as plan.FinalText
// and the session actor's storage guard do, held to the same shared vector
// file (fixtures/tokenCutFrames.json).
describe('buildTimelineModel -- cut frames', () => {
  for (const vector of readTokenCutVectors()) {
    it(`reads the part as the shared vector says: ${vector.name}`, () => {
      const events: EventEnvelope[] = vector.frames.map((f) => ({
        id: f.id,
        type: 'token',
        payload: { type: 'token', messageId: 'prt_v', sessionId: '00000000-0000-4000-8000-000000000001', gen: 1, text: f.text, ...('cut' in f ? { cut: f.cut } : {}) },
        createdAt: '2026-10-03T10:00:00Z',
      }))
      const [stream, ...others] = buildTimelineModel(events).turns[0]!.steps[0]!.tokens
      expect(others).toEqual([])
      if (vector.want === null) {
        expect([stream!.text, stream!.cut]).toEqual(['', null])
        return
      }
      const want = vector.frames.find((f) => f.id === vector.want!.id)!
      expect([stream!.text, stream!.cut]).toEqual([want.text, vector.want.cut])
      // The stream keeps the part's non-empty frames, each with its cut as
      // the timeline read it.
      expect(stream!.frames).toEqual(vector.frames.filter((f) => f.text !== '').map((f) => ({ text: f.text, cut: decodeCut(f.cut) })))
    })
  }

  it('a part stored whole and then cut reads whole, where the newest row alone would read the cut', () => {
    const whole = '1. Add the migration\n2. Wire the store\n' // 39 bytes
    const cutText = '1. Add the\n[text cut at 10 of 39 bytes on its way from the sandbox]'
    expect(new TextEncoder().encode(whole).length).toBe(39)
    const events = [
      env('step_start', { messageId: 'msg_1', stepId: 's1' }),
      env('token', { messageId: 'prt_plan', text: '' }),
      env('token', { messageId: 'prt_plan', text: whole }),
      env('token', { messageId: 'prt_plan', text: cutText, cut: { kept: 10, total: 39 } }),
      env('execution_complete', { messageId: 'm9', outcome: 'completed', reason: null }),
    ]
    const [stream] = buildTimelineModel(events).turns[0]!.steps[0]!.tokens
    expect([stream!.text, stream!.cut]).toEqual([whole, null])
  })

  // The rows the real control plane and a replica built before cuts leave
  // for a 40 KiB part written whole and then, on a reconnect to a control
  // plane that states no limit, cut -- written by
  // TestResilience_Scenario23_WholeThenCutOnRollback_FinalTextReadsWhole
  // (test/resilience), which fails if what is stored stops matching this
  // file. The cut is the part's newest row; the part reads whole.
  it('the log a rollback leaves for a part stored whole then cut reads the part whole', () => {
    const fixture = new URL('./fixtures/tokenWholeThenCutOnRollback.json', import.meta.url)
    const events = JSON.parse(readFileSync(fixture, 'utf8')) as EventEnvelope[]
    const tokens = events.filter((e) => e.type === 'token').map((e) => e.payload as { text: string; cut?: { kept: number; total: number } })
    const whole = tokens.find((p) => p.text !== '' && p.cut === undefined)!
    const wholeBytes = new TextEncoder().encode(whole.text).length
    expect(wholeBytes).toBeGreaterThan(32 * 1024)
    expect(tokens[tokens.length - 1]!.cut).toEqual({ kept: expect.any(Number), total: wholeBytes })
    const [stream, ...others] = buildTimelineModel(events).turns[0]!.steps[0]!.tokens
    expect(others).toEqual([])
    expect([stream!.text, stream!.cut]).toEqual([whole.text, null])
  })

  it('a part whose only text is cut reads as cut, its marker in the text and its cut on the stream', () => {
    const cutText = '1. Add the\n[text cut at 10 of 40 bytes on its way from the sandbox]'
    const events = [env('token', { messageId: 'prt_plan', text: '' }), env('token', { messageId: 'prt_plan', text: cutText, cut: { kept: 10, total: 40 } })]
    const [stream] = buildTimelineModel(events).turns[0]!.steps[0]!.tokens
    expect([stream!.text, stream!.cut]).toEqual([cutText, { kept: 10, total: 40 }])
  })

  it('a malformed cut reads as cut, never as whole', () => {
    const events = [env('token', { messageId: 'prt_plan', text: 'Plan.\n[text cut at 5 of 90 bytes on its way from the sandbox]', cut: { kept: '5', total: 90 } })]
    const [stream] = buildTimelineModel(events).turns[0]!.steps[0]!.tokens
    expect(stream!.cut).toEqual(MALFORMED_CUT)
  })
})

// The rows a real turn's tool activity leaves once the server stores it
// (technical plan §6.1): every event of an assistant message under that
// message's id, its step_start first, each tool_call, tool_result and
// step_finish stored under a key of its own. These are rendering checks:
// they pin what the page draws from the rows it is given, and cannot see a
// loss on the server -- the server's own tests do
// (TestResilience_ToolEventsOfOneMessage_EachStoredOnce, test/resilience,
// which writes fixtures/toolEventsOfOneMessage.json from the rows the real
// handler stores; TestEvents_ToolEventsOfOneMessage_ReturnedByEventsAndHistory).
describe('buildTimelineModel -- the tool events of a real turn', () => {
  const sub = (subTaskId: string, parentMessageId: string, parentCallId?: string) =>
    env('sub_task_start', { messageId: `start-${subTaskId}`, subTaskId, label: subTaskId, parentMessageId, ...(parentCallId ? { parentCallId } : {}) })
  const call = (callId: string, toolName: string, messageId = 'msg_1') => env('tool_call', { messageId, callId, toolName, input: {} })
  const result = (callId: string, messageId = 'msg_1') => env('tool_result', { messageId, callId, output: { output: 'ok' }, isError: false })
  const lanes = (model: ReturnType<typeof buildTimelineModel>) =>
    Object.fromEntries(model.turns[0]!.steps.flatMap((s) => s.toolCalls).map((c) => [c.callId, c.subTasks.map((st) => st.subTaskId)]))

  it('the rows the server stores for one message: each tool call with its result, the sub-task under its task call, the step closed with its cost', () => {
    const fixture = new URL('./fixtures/toolEventsOfOneMessage.json', import.meta.url)
    const events = JSON.parse(readFileSync(fixture, 'utf8')) as EventEnvelope[]
    const model = buildTimelineModel(events)
    expect(model.turns).toHaveLength(1)
    const [turn] = model.turns
    expect([turn!.live, turn!.outcome?.outcome, turn!.subTasks]).toEqual([false, 'completed', []])
    expect(turn!.steps).toHaveLength(1)
    const [step] = turn!.steps
    expect(step!.toolCalls.map((c) => [c.toolName, c.callId, c.result?.output])).toEqual([
      ['read', 'call_read', { output: 'package main' }],
      ['task', 'call_task', { output: 'Looks right.' }],
    ])
    expect(lanes(model)).toEqual({ call_read: [], call_task: ['ses_child'] })
    expect(step!.toolCalls[1]!.subTasks[0]!.status).toBe('completed')
    expect(step!.cost).toEqual({ inputTokens: 1200, outputTokens: 80, cachedTokens: 300, usd: 0.42 })
    expect(step!.live).toBe(false)
    expect(step!.tokens.map((tk) => tk.text)).toEqual(['Reading the file, then asking for a second opinion.'])
  })

  it('each tool call shows with its result, paired by callId under the one message id they share', () => {
    const events = [env('step_start', { messageId: 'msg_1', stepId: 'prt_start' }), call('c1', 'read'), call('c2', 'bash'), result('c2'), result('c1')]
    const [step] = buildTimelineModel(events).turns[0]!.steps
    expect(step!.toolCalls.map((c) => [c.callId, c.result?.isError])).toEqual([
      ['c1', false],
      ['c2', false],
    ])
  })

  it('a step closes at its step_finish with its cost, paired with its step_start by their shared message id, while the turn runs', () => {
    const events = [
      env('step_start', { messageId: 'msg_1', stepId: 'prt_start_1' }),
      call('c1', 'read'),
      env('step_finish', { messageId: 'msg_1', stepId: 'prt_finish_1', cost: { tokens: { input: 10, output: 5 }, usd: 0.02 } }),
      env('step_start', { messageId: 'msg_2', stepId: 'prt_start_2' }),
    ]
    const [turn] = buildTimelineModel(events).turns
    expect(turn!.live).toBe(true)
    expect(turn!.steps.map((s) => [s.stepId, s.live, s.cost?.usd ?? null, s.toolCalls.length])).toEqual([
      ['prt_start_1', false, 0.02, 1],
      ['prt_start_2', true, null, 0],
    ])
  })

  it('a step_finish pairs with the open step of its own message, never with another message\'s, however their steps interleave', () => {
    const start = (m: string) => env('step_start', { messageId: m, stepId: `prt_start_${m}` })
    const finish = (m: string, input: number) => env('step_finish', { messageId: m, stepId: `prt_finish_${m}`, cost: { tokens: { input, output: 1 } } })
    const steps = (events: EventEnvelope[]) => buildTimelineModel(events).turns[0]!.steps.map((s) => [s.stepId, s.live, s.cost?.inputTokens ?? null])
    // One after another.
    expect(steps([start('msg_1'), finish('msg_1', 1), start('msg_2'), finish('msg_2', 2)])).toEqual([
      ['prt_start_msg_1', false, 1],
      ['prt_start_msg_2', false, 2],
    ])
    // Interleaved, the later message finishing first, and then last.
    for (const order of [
      [start('msg_1'), start('msg_2'), finish('msg_2', 2), finish('msg_1', 1)],
      [start('msg_1'), start('msg_2'), finish('msg_1', 1), finish('msg_2', 2)],
    ]) {
      expect(steps(order)).toEqual([
        ['prt_start_msg_1', false, 1],
        ['prt_start_msg_2', false, 2],
      ])
    }
    // A later message's finish never closes an earlier message's open step.
    expect(steps([start('msg_1'), start('msg_2'), finish('msg_2', 2)])).toEqual([
      ['prt_start_msg_1', true, null],
      ['prt_start_msg_2', false, 2],
    ])
  })

  it('two steps of one message each close at their own step_finish, in order', () => {
    const events = [
      env('step_start', { messageId: 'msg_1', stepId: 'prt_start_a' }),
      env('step_finish', { messageId: 'msg_1', stepId: 'prt_finish_a', cost: { tokens: { input: 1, output: 1 } } }),
      env('step_start', { messageId: 'msg_1', stepId: 'prt_start_b' }),
      env('step_finish', { messageId: 'msg_1', stepId: 'prt_finish_b', cost: { tokens: { input: 2, output: 2 } } }),
    ]
    const [turn] = buildTimelineModel(events).turns
    expect(turn!.steps.map((s) => [s.stepId, s.live, s.cost?.inputTokens])).toEqual([
      ['prt_start_a', false, 1],
      ['prt_start_b', false, 2],
    ])
  })

  it('a sub-task hangs under its own task call by parentCallId, whether its sub_task_start comes before or after that call', () => {
    const before = buildTimelineModel([call('c1', 'read'), sub('st1', 'msg_1', 'c2'), call('c2', 'task')])
    expect(lanes(before)).toEqual({ c1: [], c2: ['st1'] })
    const after = buildTimelineModel([call('c2', 'task'), call('c3', 'bash'), sub('st1', 'msg_1', 'c2')])
    expect(lanes(after)).toEqual({ c2: ['st1'], c3: [] })
  })

  it('two parallel task calls each keep their own lane, whatever order their calls and sub-tasks come in', () => {
    for (const [name, order] of [
      ['both calls, then both sub-tasks, the second first', [call('ca', 'task'), call('cb', 'task'), sub('st_b', 'msg_1', 'cb'), sub('st_a', 'msg_1', 'ca')]],
      ['both sub-tasks, then both calls', [sub('st_b', 'msg_1', 'cb'), sub('st_a', 'msg_1', 'ca'), call('ca', 'task'), call('cb', 'task')]],
      // The adapter's own order when cb's first running update names its
      // sub-task and ca's names it later (TestDispatchTool_TaskSubtask_
      // StampsParentCallId_BothOrders): ca's call, cb's sub-task before
      // cb's call, then ca's sub-task. cb's sub-task waits for cb, never
      // falling to ca, the task call of its message already there.
      ['interleaved, as the adapter emits it', [call('ca', 'task'), sub('st_b', 'msg_1', 'cb'), call('cb', 'task'), sub('st_a', 'msg_1', 'ca')]],
    ] as const) {
      expect(lanes(buildTimelineModel([...order])), name).toEqual({ ca: ['st_a'], cb: ['st_b'] })
    }
  })

  it('a sub-task without parentCallId hangs under a task call of its message, never under a read or bash call', () => {
    // read c1, the sub-task, then task c2: under c2, the next task call.
    expect(lanes(buildTimelineModel([call('c1', 'read'), sub('st1', 'msg_1'), call('c2', 'task')]))).toEqual({ c1: [], c2: ['st1'] })
    // task c2, bash c3, then the sub-task: under c2, the latest task call.
    expect(lanes(buildTimelineModel([call('c2', 'task'), call('c3', 'bash'), sub('st1', 'msg_1')]))).toEqual({ c2: ['st1'], c3: [] })
    // task c1, task c2, then the sub-task: under the latest, c2.
    expect(lanes(buildTimelineModel([call('c1', 'task'), call('c2', 'task'), sub('st1', 'msg_1')]))).toEqual({ c1: [], c2: ['st1'] })
    // The sub-task first, then bash c1, then task c2: the bash call that
    // comes first does not take it; the task call after it does.
    expect(lanes(buildTimelineModel([sub('st1', 'msg_1'), call('c1', 'bash'), call('c2', 'task')]))).toEqual({ c1: [], c2: ['st1'] })
    // Another message's task call is not its message's, before the
    // sub-task or after it.
    for (const order of [
      [call('c9', 'task', 'msg_9'), sub('st1', 'msg_1')],
      [sub('st1', 'msg_1'), call('c9', 'task', 'msg_9')],
    ]) {
      const other = buildTimelineModel(order)
      expect(lanes(other)).toEqual({ c9: [] })
      expect(other.turns[0]!.subTasks.map((s) => s.subTaskId)).toEqual(['st1'])
    }
  })

  it('a sub-task left unattached at the turn\'s end shows as a turn-level lane, and its finish still reaches it', () => {
    const model = buildTimelineModel([
      call('c1', 'read'),
      sub('st1', 'msg_1', 'c_never'),
      env('sub_task_finish', { messageId: 'f1', subTaskId: 'st1', outcome: 'failed' }),
      env('execution_complete', { messageId: 'done', outcome: 'completed', reason: null }),
      call('c2', 'task', 'msg_next'),
    ])
    expect(model.turns).toHaveLength(2)
    expect(model.turns[0]!.subTasks.map((s) => [s.subTaskId, s.status])).toEqual([['st1', 'failed']])
    expect(lanes(model)).toEqual({ c1: [] })
    expect(model.turns[1]!.subTasks).toEqual([]) // a new turn's call never takes an ended turn's sub-task
    expect(model.orphanedSubTasks).toEqual([])
  })

  it('a parentCallId that is not a non-empty string is read as absent, never dropping the sub-task', () => {
    for (const parentCallId of [7, '', null]) {
      const model = buildTimelineModel([call('c2', 'task'), env('sub_task_start', { messageId: 's', subTaskId: 'st1', label: 'x', parentMessageId: 'msg_1', parentCallId })])
      expect(lanes(model), `parentCallId ${JSON.stringify(parentCallId)}`).toEqual({ c2: ['st1'] })
    }
  })
})
