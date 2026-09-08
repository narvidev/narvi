import { describe, expect, it } from 'vitest'

import { parseSandboxSnapshot } from '../sandboxSnapshot'

describe('parseSandboxSnapshot', () => {
  it('parses a well-formed snapshot (client.go\'s own sandboxWireMap shape)', () => {
    const raw = { id: 'sb-1', gen: 3, status: 'ready', lastSeenAt: '2026-08-20T10:00:02Z', createdAt: '2026-08-20T09:58:00Z', updatedAt: '2026-08-20T10:00:02Z', agentVersion: 'v1.4.2', imageDigest: 'sha256:9f31c' }
    expect(parseSandboxSnapshot(raw)).toEqual(raw)
  })

  it('accepts a null lastSeenAt (never yet reported a heartbeat)', () => {
    const raw = { id: 'sb-1', gen: 0, status: 'pending', lastSeenAt: null, createdAt: 'x', updatedAt: 'x', agentVersion: null, imageDigest: null }
    expect(parseSandboxSnapshot(raw)).toEqual(raw)
  })

  it('agentVersion/imageDigest default to null when absent -- a sandbox row that predates this gen\'s own "ready" event, or one from before this field existed', () => {
    const raw = { id: 'sb-1', gen: 1, status: 'connecting', lastSeenAt: null, createdAt: 'x', updatedAt: 'x' }
    expect(parseSandboxSnapshot(raw)).toEqual({ ...raw, agentVersion: null, imageDigest: null })
  })

  it('returns null when agentVersion/imageDigest have the wrong type -- never coerces', () => {
    const base = { id: 'sb-1', gen: 1, status: 'ready', lastSeenAt: null, createdAt: 'x', updatedAt: 'x' }
    expect(parseSandboxSnapshot({ ...base, agentVersion: 42 })).toBeNull()
    expect(parseSandboxSnapshot({ ...base, imageDigest: 42 })).toBeNull()
  })

  it('returns null for a missing sandbox (no sandbox row yet)', () => {
    expect(parseSandboxSnapshot(null)).toBeNull()
    expect(parseSandboxSnapshot(undefined)).toBeNull()
  })

  it('returns null for a non-object', () => {
    expect(parseSandboxSnapshot('not an object')).toBeNull()
    expect(parseSandboxSnapshot(42)).toBeNull()
    expect(parseSandboxSnapshot([])).toBeNull()
  })

  it('returns null when a required field has the wrong type -- never coerces', () => {
    expect(parseSandboxSnapshot({ id: 'sb-1', gen: '3', status: 'ready', lastSeenAt: null, createdAt: 'x', updatedAt: 'x' })).toBeNull()
    expect(parseSandboxSnapshot({ id: 'sb-1', gen: 3, status: 42, lastSeenAt: null, createdAt: 'x', updatedAt: 'x' })).toBeNull()
    expect(parseSandboxSnapshot({ id: 'sb-1', gen: 3, status: 'ready', lastSeenAt: 42, createdAt: 'x', updatedAt: 'x' })).toBeNull()
  })

  it('returns null when a required field is missing entirely', () => {
    expect(parseSandboxSnapshot({ gen: 3, status: 'ready', lastSeenAt: null, createdAt: 'x', updatedAt: 'x' })).toBeNull()
  })
})
