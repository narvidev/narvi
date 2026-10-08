// autonomyFreezeFormat.test.ts -- the autonomy freeze's shared wording
// (autonomyFreezeFormat.ts, technical plan §40.2): who froze it when no one
// is on record, a held row's age, the held-advances count once the 100
// bound cuts the list, and the bounds every "held actions start again"
// carries.
import { describe, expect, it } from 'vitest'

import { FREEZE_RESUME_BOUNDS, frozenByText, heldAdvancesCountText, heldAgoText } from '../autonomyFreezeFormat'

describe('frozenByText', () => {
  it('names the administrator on record', () => {
    expect(frozenByText({ frozenByDisplayName: 'Ada Admin' })).toBe('Ada Admin')
  })

  it('says no one is on record -- a deleted user, or a freeze written by hand -- never that a user was deleted', () => {
    expect(frozenByText({ frozenByDisplayName: null })).toBe('someone not on record')
  })
})

describe('heldAgoText', () => {
  const now = new Date('2026-10-08T12:00:00Z')

  it('reads "held just now" under a minute, and for a time a little ahead of this clock', () => {
    expect(heldAgoText('2026-10-08T11:59:30Z', now)).toBe('held just now')
    expect(heldAgoText('2026-10-08T12:00:20Z', now)).toBe('held just now')
  })

  it('reads "held N ago" from a minute on', () => {
    expect(heldAgoText('2026-10-08T11:57:00Z', now)).toBe('held 3 min ago')
  })
})

describe('heldAdvancesCountText', () => {
  it('says how many are held, and that the oldest are shown, when the bound cut the list', () => {
    expect(heldAdvancesCountText(100, 250)).toBe('250 held · the oldest 100 shown · workflow runs whose next step starts once autonomy is unfrozen')
  })

  it('counts what it shows when nothing was cut, or the server sent no total', () => {
    expect(heldAdvancesCountText(3, 3)).toBe('3 · workflow runs whose next step starts once autonomy is unfrozen')
    expect(heldAdvancesCountText(3, undefined)).toBe('3 · workflow runs whose next step starts once autonomy is unfrozen')
  })
})

describe('FREEZE_RESUME_BOUNDS', () => {
  it('names the two held actions a lifted freeze does not bring back', () => {
    expect(FREEZE_RESUME_BOUNDS).toContain('a scheduled automation run held more than ten minutes waits for its next occurrence')
    expect(FREEZE_RESUME_BOUNDS).toContain('a sentinel fix held from merging is not merged automatically')
  })
})
