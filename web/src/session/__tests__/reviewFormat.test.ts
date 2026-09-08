import { describe, expect, it } from 'vitest'

import { sentinelFixLabel, sentinelFixTone, visualQaTone } from '../reviewFormat'

describe('visualQaTone', () => {
  it('maps pass/fail/skip(ped) to the expected tones', () => {
    expect(visualQaTone('pass')).toBe('ok')
    expect(visualQaTone('fail')).toBe('crit')
    expect(visualQaTone('skip')).toBe('neutral')
    expect(visualQaTone('skipped')).toBe('neutral')
  })

  it('an unrecognized value (a typo, a future vocabulary) renders neutral rather than guessing', () => {
    expect(visualQaTone('Pass')).toBe('neutral')
    expect(visualQaTone('')).toBe('neutral')
  })
})

describe('sentinelFixTone', () => {
  it('fix_merged is ok', () => {
    expect(sentinelFixTone('fix_merged')).toBe('ok')
  })
  it('abandoned is neutral', () => {
    expect(sentinelFixTone('abandoned')).toBe('neutral')
  })
  it('every in-flight status (pending/spawned/fix_open) is warn', () => {
    expect(sentinelFixTone('pending')).toBe('warn')
    expect(sentinelFixTone('spawned')).toBe('warn')
    expect(sentinelFixTone('fix_open')).toBe('warn')
  })
})

describe('sentinelFixLabel', () => {
  it('renders a distinct label for every known status', () => {
    expect(sentinelFixLabel('pending')).toBe('fix pending')
    expect(sentinelFixLabel('spawned')).toBe('fix session started')
    expect(sentinelFixLabel('fix_open')).toBe('fix open, merges automatically once this PR lands')
    expect(sentinelFixLabel('fix_merged')).toBe('fix merged')
    expect(sentinelFixLabel('abandoned')).toBe('fix abandoned (this PR closed unmerged)')
  })

  it('falls back to the raw status for an unrecognized value -- never blank', () => {
    expect(sentinelFixLabel('some-future-status')).toBe('some-future-status')
  })
})
