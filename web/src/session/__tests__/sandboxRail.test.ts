import { describe, expect, it } from 'vitest'

import type { EventEnvelope } from '../../ws/types'
import { buildSandboxRailModel, runtimeLabel, shortDigest } from '../sandboxRail'
import type { BootPhase } from '../sandboxRail'
import type { SandboxSnapshot } from '../sandboxSnapshot'

function ev(id: number, type: string, payload: unknown, createdAt: string): EventEnvelope {
  return { id, type, payload, createdAt }
}

describe('shortDigest', () => {
  it('strips a leading "algo:" prefix before truncating', () => {
    expect(shortDigest('sha256:9f31c00abcdef')).toBe('9f31c00')
  })

  it('truncates a bare digest with no prefix the same way', () => {
    expect(shortDigest('9f31c00abcdef')).toBe('9f31c00')
  })
})

describe('runtimeLabel', () => {
  it('renders "agentVersion · img shortDigest" (mockups.html\'s own "v1.4.2 · img 9f31c") when both are real', () => {
    expect(runtimeLabel('v1.4.2', 'sha256:9f31c00')).toBe('v1.4.2 · img 9f31c00')
  })

  it('returns null when agentVersion is not yet reported', () => {
    expect(runtimeLabel(null, 'sha256:9f31c00')).toBeNull()
  })

  it('returns null when imageDigest is not yet reported', () => {
    expect(runtimeLabel('v1.4.2', null)).toBeNull()
  })

  it('returns null when neither is reported yet', () => {
    expect(runtimeLabel(null, null)).toBeNull()
  })
})

describe('buildSandboxRailModel', () => {
  it('with no snapshot and no events, reports nothing (honest "not started" state)', () => {
    const model = buildSandboxRailModel([], null)
    expect(model).toEqual({ status: null, gen: null, lastSeenAt: null, bootPhases: [], transitions: [], hasSandbox: false, agentVersion: null, imageDigest: null })
  })

  it('seeds status/gen/lastSeenAt from the WS subscribe snapshot when there are no events yet', () => {
    const snapshot: SandboxSnapshot = { id: 'sb-1', gen: 2, status: 'booting', lastSeenAt: '2026-08-20T10:00:00Z', createdAt: 'x', updatedAt: 'y', agentVersion: null, imageDigest: null }
    const model = buildSandboxRailModel([], snapshot)
    expect(model.status).toBe('booting')
    expect(model.gen).toBe(2)
    expect(model.lastSeenAt).toBe('2026-08-20T10:00:00Z')
    expect(model.hasSandbox).toBe(true)
  })

  it('seeds agentVersion/imageDigest from the WS subscribe snapshot -- §12.2 item 1\'s own runtime-fingerprint gap', () => {
    const snapshot: SandboxSnapshot = { id: 'sb-1', gen: 3, status: 'ready', lastSeenAt: '2026-08-20T10:00:02Z', createdAt: 'x', updatedAt: 'y', agentVersion: 'v1.4.2', imageDigest: 'sha256:9f31c00' }
    const model = buildSandboxRailModel([], snapshot)
    expect(model.agentVersion).toBe('v1.4.2')
    expect(model.imageDigest).toBe('sha256:9f31c00')
  })

  it('agentVersion/imageDigest stay null when the snapshot has not reported them yet -- never derived from any event', () => {
    const snapshot: SandboxSnapshot = { id: 'sb-1', gen: 1, status: 'connecting', lastSeenAt: null, createdAt: 'x', updatedAt: 'y', agentVersion: null, imageDigest: null }
    const events: EventEnvelope[] = [ev(1, 'ready', { type: 'ready', messageId: 'm1', sessionId: 's', gen: 1, timestamp: 'x' }, '2026-08-20T10:00:05.000Z')]
    const model = buildSandboxRailModel(events, snapshot)
    expect(model.agentVersion).toBeNull()
    expect(model.imageDigest).toBeNull()
  })

  it('boot_progress events accumulate as phases with real, computed durations, and never set the status', () => {
    const events: EventEnvelope[] = [
      ev(1, 'boot_progress', { type: 'boot_progress', messageId: 'm1', sessionId: 's', gen: 1, phase: 'clone', timestamp: 'x' }, '2026-08-20T10:00:00.000Z'),
      ev(2, 'boot_progress', { type: 'boot_progress', messageId: 'm2', sessionId: 's', gen: 1, phase: 'deps', timestamp: 'x' }, '2026-08-20T10:00:03.500Z'),
    ]
    const model = buildSandboxRailModel(events, null)
    expect(model.status).toBeNull()
    expect(model.bootPhases).toHaveLength(2)
    expect(model.bootPhases[0]).toMatchObject({ phase: 'clone', endedAt: '2026-08-20T10:00:03.500Z', seconds: 3.5, open: false })
    // Last phase stays open (no seconds yet) until something closes it.
    expect(model.bootPhases[1]).toMatchObject({ phase: 'deps', endedAt: null, seconds: null, open: true })
    expect(model.transitions.map((t) => t.label)).toEqual(['clone', 'deps'])
  })

  // Technical plan §3.2: the agent's `ready` opens its connection, ahead
  // of the clone, the git-dir sync and the hooks; the server keeps the
  // sandbox booting until boot evidence and a null-phase heartbeat. The
  // rail's status is the server's (the snapshot, which follows every
  // change), and only the server's own sandbox_status event ends a phase.
  // Reading the boot from `ready` again makes the first two rows fail.
  describe('status and boot phases follow the server, never the agent\'s own events', () => {
    const at = (s: number) => `2026-08-20T10:00:${String(s).padStart(2, '0')}.000Z`
    const snap = (status: string, gen = 1): SandboxSnapshot => ({ id: 'sb-1', gen, status, lastSeenAt: null, createdAt: 'x', updatedAt: 'y', agentVersion: null, imageDigest: null })
    const phase = (id: number, p: string, s: number) => ev(id, 'boot_progress', { type: 'boot_progress', messageId: `m${id}`, sessionId: 's', gen: 1, phase: p }, at(s))
    const ready = (id: number, s: number) => ev(id, 'ready', { type: 'ready', messageId: `m${id}`, sessionId: 's', gen: 1 }, at(s))
    const status = (id: number, st: string, s: number, gen = 1) => ev(id, 'sandbox_status', { sandbox: { gen, status: st } }, at(s))
    const fatal = (id: number, s: number) => ev(id, 'error', { type: 'error', messageId: `m${id}`, sessionId: 's', gen: 1, ackId: `error:m${id}`, message: 'boom', fatal: true }, at(s))

    const cases: {
      name: string
      events: EventEnvelope[]
      snapshot: SandboxSnapshot | null
      wantStatus: string | null
      wantPhases: Partial<BootPhase>[]
      wantLabels: string[]
    }[] = [
      {
        name: 'a ready after the phase neither closes it nor reads ready while the server says booting',
        events: [phase(1, 'clone', 0), ready(2, 5)],
        snapshot: snap('booting'),
        wantStatus: 'booting',
        wantPhases: [{ phase: 'clone', endedAt: null, seconds: null, open: true }],
        wantLabels: ['clone', 'agent connected'],
      },
      {
        name: 'the agent connected, then reported its phase: still booting',
        events: [status(1, 'booting', 0), ready(2, 0), phase(3, 'clone', 1)],
        snapshot: snap('booting'),
        wantStatus: 'booting',
        wantPhases: [{ phase: 'clone', open: true }],
        wantLabels: ['booting', 'agent connected', 'clone'],
      },
      {
        name: 'the server marking it ready closes the phase at its own time',
        events: [phase(1, 'clone', 0), ready(2, 1), status(3, 'ready', 9)],
        snapshot: snap('ready'),
        wantStatus: 'ready',
        wantPhases: [{ phase: 'clone', endedAt: at(9), seconds: 9, open: false }],
        wantLabels: ['clone', 'agent connected', 'ready'],
      },
      {
        name: 'suspect keeps the phase open: a liveness doubt, not the end of the boot',
        events: [phase(1, 'clone', 0), status(2, 'suspect', 4)],
        snapshot: snap('suspect'),
        wantStatus: 'suspect',
        wantPhases: [{ phase: 'clone', open: true }],
        wantLabels: ['clone', 'suspect'],
      },
      {
        name: 'a fatal agent error does not make the sandbox failed: the server says what it is',
        events: [phase(1, 'clone', 0), fatal(2, 3)],
        snapshot: snap('booting'),
        wantStatus: 'booting',
        wantPhases: [{ phase: 'clone', open: true }],
        wantLabels: ['clone', 'error: boom'],
      },
      {
        name: 'a phase the server shows over without saying when: finished, duration unknown',
        events: [phase(1, 'clone', 0)],
        snapshot: snap('ready'),
        wantStatus: 'ready',
        wantPhases: [{ phase: 'clone', endedAt: null, seconds: null, open: false }],
        wantLabels: ['clone'],
      },
      {
        name: 'no snapshot: no status, however many events',
        events: [phase(1, 'clone', 0), ready(2, 1), status(3, 'ready', 2)],
        snapshot: null,
        wantStatus: null,
        wantPhases: [{ phase: 'clone', endedAt: at(2), open: false }],
        wantLabels: ['clone', 'agent connected', 'ready'],
      },
    ]
    for (const c of cases) {
      it(c.name, () => {
        const model = buildSandboxRailModel(c.events, c.snapshot)
        expect(model.status).toBe(c.wantStatus)
        expect(model.bootPhases).toHaveLength(c.wantPhases.length)
        c.wantPhases.forEach((want, i) => expect(model.bootPhases[i]).toMatchObject(want))
        expect(model.transitions.map((t) => t.label)).toEqual(c.wantLabels)
      })
    }

    it('tones the server\'s transitions: ready ok, failed crit, suspect warn, the rest neutral; the agent\'s connection is neutral, never ok', () => {
      const model = buildSandboxRailModel([status(1, 'booting', 0), ready(2, 1), status(3, 'ready', 2), status(4, 'suspect', 3), status(5, 'failed', 4)], snap('failed'))
      expect(model.transitions.map((t) => [t.label, t.tone])).toEqual([
        ['booting', 'neutral'],
        ['agent connected', 'neutral'],
        ['ready', 'ok'],
        ['suspect', 'warn'],
        ['failed', 'crit'],
      ])
    })

    // A generation only ever grows, so the rail shows the highest one
    // reported -- the snapshot's, an agent event's or a sandbox_status's --
    // never the last one read. A log can hold an older gen after a newer
    // one (an old gen's frames replayed late), and a snapshot can be ahead
    // of every event loaded (a respawn whose sandbox_status is not in the
    // page yet, or one from before the control plane reported changes).
    // Showing the last gen seen makes every row below but the first fail.
    describe('gen: the highest reported, never the last one read', () => {
      const toolCall = (id: number, gen: number, s: number) => ev(id, 'tool_call', { type: 'tool_call', messageId: `m${id}`, sessionId: 's', gen, callId: `c${id}`, toolName: 'Read', input: {} }, at(s))
      const genCases: { name: string; events: EventEnvelope[]; snapshot: SandboxSnapshot | null; want: number | null }[] = [
        { name: 'a newer event after an older one', events: [toolCall(1, 1, 0), toolCall(2, 2, 1)], snapshot: null, want: 2 },
        { name: 'the snapshot above the last gen-bearing event', events: [toolCall(1, 2, 0)], snapshot: snap('booting', 3), want: 3 },
        { name: 'an older gen after a newer one', events: [toolCall(1, 3, 0), toolCall(2, 2, 1)], snapshot: null, want: 3 },
        { name: 'an older agent event after the server\'s newer generation', events: [status(1, 'spawning', 0, 3), toolCall(2, 2, 1)], snapshot: null, want: 3 },
        { name: 'an older sandbox_status after a newer agent event', events: [toolCall(1, 3, 0), status(2, 'stopped', 1, 2)], snapshot: snap('booting', 1), want: 3 },
      ]
      for (const c of genCases) {
        it(c.name, () => {
          expect(buildSandboxRailModel(c.events, c.snapshot).gen).toBe(c.want)
        })
      }
    })

    // Rollout compatibility: a control plane older than
    // FetchHistoryResponse.sandbox stores no sandbox_status and sends no
    // row after the subscribe reply. Until a reply carries the row
    // (serverReportsSandbox false), the agent's events move the status as
    // they did before -- else the rail would keep the subscribe-time status
    // until the next subscribe. Dropping the fallback makes these rows
    // fail; applying it while the server reports the row makes the table
    // above fail.
    describe('a control plane that reports no sandbox row: the agent\'s events move the status, as before', () => {
      const rolloutCases: { name: string; events: EventEnvelope[]; want: string | null; wantPhases: Partial<BootPhase>[] }[] = [
        { name: 'the agent\'s ready ends the boot and its phase', events: [phase(1, 'clone', 0), ready(2, 4)], want: 'ready', wantPhases: [{ phase: 'clone', endedAt: at(4), seconds: 4, open: false }] },
        { name: 'a phase after the ready reads booting', events: [ready(1, 0), phase(2, 'web:ready', 1)], want: 'booting', wantPhases: [{ phase: 'web:ready', open: true }] },
        { name: 'a fatal agent error reads failed and ends the phase', events: [phase(1, 'clone', 0), fatal(2, 3)], want: 'failed', wantPhases: [{ phase: 'clone', endedAt: at(3), open: false }] },
        { name: 'no agent event: the subscribe-time status', events: [], want: 'booting', wantPhases: [] },
      ]
      for (const c of rolloutCases) {
        it(c.name, () => {
          const model = buildSandboxRailModel(c.events, snap('booting'), false)
          expect(model.status).toBe(c.want)
          expect(model.bootPhases).toHaveLength(c.wantPhases.length)
          c.wantPhases.forEach((want, i) => expect(model.bootPhases[i]).toMatchObject(want))
        })
      }
    })

    it('a sandbox_status event reports the server\'s generation but is no sign of life: gen follows it, lastSeenAt does not', () => {
      const model = buildSandboxRailModel([phase(1, 'clone', 0), status(2, 'spawning', 30, 2)], snap('ready'))
      expect(model.gen).toBe(2)
      expect(model.lastSeenAt).toBe(at(0))
      expect(model.hasSandbox).toBe(true)
    })
  })

  it('a NON-fatal sandbox_error is recorded but does not change status', () => {
    const snapshot: SandboxSnapshot = { id: 'sb-1', gen: 1, status: 'ready', lastSeenAt: null, createdAt: 'x', updatedAt: 'y', agentVersion: null, imageDigest: null }
    const events: EventEnvelope[] = [ev(1, 'error', { type: 'error', messageId: 'm1', sessionId: 's', gen: 1, ackId: 'error:m1', message: 'transient hiccup', fatal: false }, '2026-08-20T10:00:00Z')]
    const model = buildSandboxRailModel(events, snapshot)
    expect(model.status).toBe('ready')
    expect(model.transitions[0]).toMatchObject({ tone: 'warn' })
  })

  it('gen and lastSeenAt advance from ANY gen-bearing event, not just heartbeat/boot events', () => {
    const events: EventEnvelope[] = [ev(1, 'tool_call', { type: 'tool_call', messageId: 'm1', sessionId: 's', gen: 5, callId: 'c1', toolName: 'Read', input: {} }, '2026-08-20T10:05:00Z')]
    const model = buildSandboxRailModel(events, null)
    expect(model.gen).toBe(5)
    expect(model.lastSeenAt).toBe('2026-08-20T10:05:00Z')
    expect(model.hasSandbox).toBe(true)
  })

  it('a prompt_received (technical plan §3.3, prompt receipts) moves no status, transition or boot phase -- only gen and lastSeenAt, as any sandbox frame does', () => {
    const snapshot: SandboxSnapshot = { id: 'sb-1', gen: 1, status: 'ready', lastSeenAt: null, createdAt: 'x', updatedAt: 'y', agentVersion: null, imageDigest: null }
    const before: EventEnvelope[] = [
      ev(1, 'boot_progress', { type: 'boot_progress', messageId: 'm1', sessionId: 's', gen: 1, phase: 'deps', timestamp: 'x' }, '2026-08-20T10:00:00.000Z'),
    ]
    const receipt = ev(2, 'prompt_received', { type: 'prompt_received', messageId: 'prompt_received:p1', sessionId: 's', gen: 1, promptMessageId: 'p1', duplicate: false }, '2026-08-20T10:00:09.000Z')
    const without = buildSandboxRailModel(before, snapshot)
    const withReceipt = buildSandboxRailModel([...before, receipt], snapshot)
    expect(withReceipt.status).toBe(without.status)
    expect(withReceipt.transitions).toEqual(without.transitions)
    expect(withReceipt.bootPhases).toEqual(without.bootPhases)
    expect(withReceipt.gen).toBe(1)
    expect(withReceipt.lastSeenAt).toBe('2026-08-20T10:00:09.000Z')
  })

  it('a later event\'s gen wins over an earlier one (respawn bumps gen forward)', () => {
    const events: EventEnvelope[] = [
      ev(1, 'tool_call', { type: 'tool_call', messageId: 'm1', sessionId: 's', gen: 1, callId: 'c1', toolName: 'Read', input: {} }, '2026-08-20T10:00:00Z'),
      ev(2, 'tool_call', { type: 'tool_call', messageId: 'm2', sessionId: 's', gen: 2, callId: 'c2', toolName: 'Read', input: {} }, '2026-08-20T10:05:00Z'),
    ]
    const model = buildSandboxRailModel(events, null)
    expect(model.gen).toBe(2)
    expect(model.lastSeenAt).toBe('2026-08-20T10:05:00Z')
  })
})
