import { describe, expect, it } from 'vitest'

import { additionCheckReason, additionsCheckLabel, additionsRunSummary, findingSourceBucketLabel, isUnverifiedAddition, sentinelFixLabel, sentinelFixTone, visualQaTone } from '../reviewFormat'

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

describe('isUnverifiedAddition (§26.6)', () => {
  it('only a counter-review addition the server checked is verified', () => {
    expect(isUnverifiedAddition({ source: 'counter_review', additionCheck: 'checked' })).toBe(false)
    expect(isUnverifiedAddition({ source: 'counter_review', additionCheck: 'not_run' })).toBe(true)
    expect(isUnverifiedAddition({ source: 'counter_review', additionCheck: 'not_found' })).toBe(true)
    expect(isUnverifiedAddition({ source: 'counter_review', additionCheck: 'unconfirmed' })).toBe(true)
    expect(isUnverifiedAddition({ source: 'counter_review', additionCheck: null })).toBe(true)
    expect(isUnverifiedAddition({ source: 'counter_review', additionCheck: 'some-future-value' })).toBe(true)
  })

  it('a primary finding, or one with no source recorded, is never an unverified addition', () => {
    expect(isUnverifiedAddition({ source: 'primary', additionCheck: null })).toBe(false)
    expect(isUnverifiedAddition({ source: null, additionCheck: null })).toBe(false)
    expect(isUnverifiedAddition({})).toBe(false)
  })
})

describe('additionCheckReason / additionsCheckLabel / findingSourceBucketLabel', () => {
  it('names each reason, and "could not be confirmed" claims nothing about the trace', () => {
    expect(additionCheckReason('not_run')).toBe('no fact-check run over it after the counter-review was reported')
    expect(additionCheckReason('not_found')).toContain('was found in this turn')
    expect(additionCheckReason('unconfirmed')).toContain('could not be confirmed')
    expect(additionCheckReason('unconfirmed')).not.toContain('was found')
    expect(additionCheckReason(null)).toBe('no fact-check run over it was confirmed')
  })

  it('labels the verdict resolution, with a dash when there was nothing to resolve', () => {
    expect(additionsCheckLabel('checked')).toBe('additions checked')
    expect(additionsCheckLabel('not_found')).toBe('additions unverified')
    expect(additionsCheckLabel('unconfirmed')).toBe('additions unverified (could not be confirmed)')
    expect(additionsCheckLabel(null)).toBe('—')
  })

  it('labels every source bucket, falling back to the raw value', () => {
    expect(findingSourceBucketLabel('primary')).toBe('primary reviewer')
    expect(findingSourceBucketLabel('counter_review')).toBe('counter-review, checked')
    expect(findingSourceBucketLabel('counter_review_unverified')).toBe('counter-review, unverified')
    expect(findingSourceBucketLabel('not_recorded')).toBe('source not recorded')
    expect(findingSourceBucketLabel('later')).toBe('later')
  })
})

describe('additionsRunSummary (§26.6)', () => {
  it('reports the second run beside the resolution of the additions the verdict published', () => {
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 1, additionsCheck: 'checked' })).toBe('done (1 killed) · additions checked')
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 0, additionsCheck: 'not_found' })).toBe('done (0 killed) · additions unverified')
    expect(additionsRunSummary({ additionsFactCheck: 'skipped', additionsFactCheckKilled: 0, additionsCheck: 'not_run' })).toBe('skipped (0 killed) · additions unverified')
    expect(additionsRunSummary({ additionsFactCheck: null, additionsFactCheckKilled: null, additionsCheck: 'not_run' })).toBe('not reported · additions unverified')
  })

  it('a second run that removed every addition published none, and never reads unverified', () => {
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 2, additionsCheck: null })).toBe('done (2 killed) · no addition published')
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 2 })).not.toContain('unverified')
  })

  it('never claims no addition was published while a published finding has no recorded source', () => {
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 0, additionsCheck: null }, true)).toBe('done (0 killed) · not resolved -- a finding has no recorded source')
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 0, additionsCheck: null }, true)).not.toContain('no addition published')
    expect(additionsRunSummary({ additionsFactCheck: 'done', additionsFactCheckKilled: 1, additionsCheck: 'checked' }, true)).toBe('done (1 killed) · additions checked')
  })

  it('a dash when there was neither a second run nor an addition', () => {
    expect(additionsRunSummary({ additionsFactCheck: null, additionsFactCheckKilled: null, additionsCheck: null })).toBe('—')
    expect(additionsRunSummary({})).toBe('—')
    expect(additionsRunSummary(null)).toBe('—')
  })
})
