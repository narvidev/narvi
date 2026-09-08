import { describe, expect, it } from 'vitest'

import type { EventEnvelope } from '../../ws/types'
import { buildSandboxRailModel, runtimeLabel, shortDigest } from '../sandboxRail'
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

  it('boot_progress events accumulate as phases with real, computed durations, and set status booting', () => {
    const events: EventEnvelope[] = [
      ev(1, 'boot_progress', { type: 'boot_progress', messageId: 'm1', sessionId: 's', gen: 1, phase: 'clone', timestamp: 'x' }, '2026-08-20T10:00:00.000Z'),
      ev(2, 'boot_progress', { type: 'boot_progress', messageId: 'm2', sessionId: 's', gen: 1, phase: 'deps', timestamp: 'x' }, '2026-08-20T10:00:03.500Z'),
    ]
    const model = buildSandboxRailModel(events, null)
    expect(model.status).toBe('booting')
    expect(model.bootPhases).toHaveLength(2)
    expect(model.bootPhases[0]).toMatchObject({ phase: 'clone', endedAt: '2026-08-20T10:00:03.500Z', seconds: 3.5 })
    // Last phase stays open (no seconds yet) until something closes it.
    expect(model.bootPhases[1]).toMatchObject({ phase: 'deps', endedAt: null, seconds: null })
    expect(model.transitions.map((t) => t.label)).toEqual(['clone', 'deps'])
  })

  it('a ready event closes the last open phase and flips status to ready', () => {
    const events: EventEnvelope[] = [
      ev(1, 'boot_progress', { type: 'boot_progress', messageId: 'm1', sessionId: 's', gen: 1, phase: 'agent', timestamp: 'x' }, '2026-08-20T10:00:00.000Z'),
      ev(2, 'ready', { type: 'ready', messageId: 'm2', sessionId: 's', gen: 1, timestamp: 'x' }, '2026-08-20T10:00:05.000Z'),
    ]
    const model = buildSandboxRailModel(events, null)
    expect(model.status).toBe('ready')
    expect(model.bootPhases[0]).toMatchObject({ endedAt: '2026-08-20T10:00:05.000Z', seconds: 5 })
    expect(model.transitions.at(-1)).toMatchObject({ label: 'ready', tone: 'ok' })
  })

  it('a FATAL sandbox_error flips status to failed and tags the transition crit', () => {
    const events: EventEnvelope[] = [ev(1, 'error', { type: 'error', messageId: 'm1', sessionId: 's', gen: 1, ackId: 'error:m1', message: 'boom', fatal: true }, '2026-08-20T10:00:00Z')]
    const model = buildSandboxRailModel(events, null)
    expect(model.status).toBe('failed')
    expect(model.transitions[0]).toMatchObject({ tone: 'crit' })
    expect(model.transitions[0]?.label).toContain('boom')
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
